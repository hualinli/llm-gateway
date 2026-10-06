// Package organization owns organizations and memberships (ADR-0002 §2.2,
// §3). It is framework-free: standard library and database/sql only.
//
// 隔离原则：任何跨组织读取都由 SQL 的 WHERE org_id = ... 与服务层成员校验
// 双重约束；不属于调用者的组织一律 ErrNotFound，不泄露存在性。
package organization

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Organization 是 organizations 表的对外视图。
type Organization struct {
	ID             int64
	Type           string
	Name           string
	Status         string
	LegacyTenantID *string
	CreatedAt      time.Time
}

// Membership 是 memberships 表的对外视图。
type Membership struct {
	ID                int64
	OrgID             int64
	UserID            int64
	Role              string
	Status            string
	MonthlyLimitMicro *int64
	CreatedAt         time.Time
}

// MemberOrg 是“调用者可见组织 + 自己在其中的角色”。
type MemberOrg struct {
	Organization      Organization
	MembershipID      int64
	Role              string
	Status            string
	MonthlyLimitMicro *int64
}

var (
	ErrNotFound              = errors.New("organization: not found")
	ErrNotMember             = errors.New("organization: not an active member")
	ErrNameInvalid           = errors.New("organization: invalid name")
	ErrPersonalTeam          = errors.New("organization: personal accounts do not support team membership")
	ErrLimitRequired         = errors.New("organization: monthly_limit_micro must be explicitly provided")
	ErrOrganizationSuspended = errors.New("organization: account is suspended")
	ErrMemberStatusInvalid   = errors.New("organization: member status must be active or suspended")
)

const (
	typePersonal   = "personal"
	typeEnterprise = "enterprise"

	statusActive    = "active"
	statusSuspended = "suspended"

	memberActive    = "active"
	memberSuspended = "suspended"
	memberRemoved   = "removed"

	roleOwner  = "owner"
	roleAdmin  = "admin"
	roleMember = "member"

	defaultListLimit = 20
	maxListLimit     = 100
	maxNameRunes     = 80
)

// Service 是组织/成员事实源的唯一入口。db 由调用方持有与关闭。
type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service { return &Service{db: db} }

// EnsurePersonalOrganization 幂等地保证用户恰有一个个人组织，并在同一事务里
// 保证该用户在其中的 owner 成员关系：个人组织和企业组织共用一套隔离体系，
// 缺了成员关系 ListForUser / GetForUser 就看不到自己的个人组织。
//
// 并发安全依赖 idx_organizations_personal_owner 部分唯一索引：INSERT ... ON
// CONFLICT DO NOTHING 只有一方会写入，另一方回读同一行；因此并发调用只会留下
// 一行。displayName 为空时用 email 的 local part 兜底（1..80 字符）。
func (s *Service) EnsurePersonalOrganization(ctx context.Context, userID int64, displayName string) (Organization, error) {
	name, err := personalOrgName(displayName)
	if err != nil {
		return Organization{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Organization{}, err
	}
	defer func() { _ = tx.Rollback() }()

	org, err := scanOrganization(tx.QueryRowContext(ctx, queryInsertPersonalOrganization, userID, name, now).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		// 没有返回行有两种可能：并发/重复插入命中唯一索引，或用户不存在。
		// 两种情况都回读一次：读不到就是用户不存在。
		org, err = scanOrganization(tx.QueryRowContext(ctx, queryGetPersonalOrganization, userID).Scan)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	if err != nil {
		return Organization{}, err
	}
	if _, err := tx.ExecContext(ctx, queryUpsertMembership, org.ID, userID, roleOwner, nil, now); err != nil {
		return Organization{}, err
	}
	if err := tx.Commit(); err != nil {
		return Organization{}, err
	}
	return org, nil
}

// CreateEnterprise 在同一事务里创建企业组织并把创建者写成 owner 成员。
func (s *Service) CreateEnterprise(ctx context.Context, ownerUserID int64, name string) (Organization, error) {
	clean, err := normalizeOrgName(name)
	if err != nil {
		return Organization{}, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Organization{}, err
	}
	defer func() { _ = tx.Rollback() }()

	org, err := scanOrganization(tx.QueryRowContext(ctx, queryInsertOrganization, typeEnterprise, clean, ownerUserID, now).Scan)
	if isForeignKeyViolation(err) {
		return Organization{}, ErrNotFound
	}
	if err != nil {
		return Organization{}, err
	}
	if _, err := tx.ExecContext(ctx, queryInsertMembership, org.ID, ownerUserID, roleOwner, nil, now); err != nil {
		return Organization{}, err
	}
	if err := tx.Commit(); err != nil {
		return Organization{}, err
	}
	return org, nil
}

// ListForUser 按 id ASC 列出调用者的 active 组织，cursor 是上一页最后一个 id。
// limit <= 0 取 20，最大 100；返回非 nil 切片，next_cursor 为空表示没有下一页。
func (s *Service) ListForUser(ctx context.Context, userID int64, limit int, cursor int64) ([]MemberOrg, string, error) {
	limit = normalizeLimit(limit)
	// 多取一行只用于判断“还有没有下一页”，不返回给调用方。
	rows, err := s.db.QueryContext(ctx, queryListMemberOrgs, userID, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	items := make([]MemberOrg, 0, limit)
	for rows.Next() {
		item, err := scanMemberOrg(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = strconv.FormatInt(items[limit-1].Organization.ID, 10)
	}
	return items, next, nil
}

// GetForUser 返回调用者在指定组织里的角色。非成员（含 org 不存在）一律
// ErrNotFound，不区分“不存在”和“无权限”，避免存在性泄露。
func (s *Service) GetForUser(ctx context.Context, userID, orgID int64) (MemberOrg, error) {
	item, err := scanMemberOrg(s.db.QueryRowContext(ctx, queryGetMemberOrg, userID, orgID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return MemberOrg{}, ErrNotFound
	}
	if err != nil {
		return MemberOrg{}, err
	}
	return item, nil
}

// Membership 读取成员关系：没有记录返回 ErrNotFound；记录存在但不是 active
// 返回 ErrNotMember（调用方拿到的一定是 active 成员）。这里不做存在性泄露
// 处理是刻意的：它只服务于已知组织的授权细节查询，对外可见性由 GetForUser 决定。
func (s *Service) Membership(ctx context.Context, userID, orgID int64) (Membership, error) {
	var m Membership
	err := scanMembership(s.db.QueryRowContext(ctx, queryGetMembership, orgID, userID).Scan, &m)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, err
	}
	if m.Status != memberActive {
		return Membership{}, ErrNotMember
	}
	return m, nil
}

// AuthorizeOwnBillingHistory permits a suspended member to inspect their own
// historical usage and quota state. It deliberately has no caller-supplied
// membership ID and never grants organization-wide billing visibility.
func (s *Service) AuthorizeOwnBillingHistory(ctx context.Context, userID, orgID int64) (Membership, error) {
	var m Membership
	err := scanMembership(s.db.QueryRowContext(ctx, queryGetMembership, orgID, userID).Scan, &m)
	if errors.Is(err, sql.ErrNoRows) || m.Status == memberRemoved {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, err
	}
	if m.Status != memberActive && m.Status != memberSuspended {
		return Membership{}, ErrNotFound
	}
	if !Allowed(m.Role, ActionViewOwnUsage) {
		return Membership{}, ErrForbidden
	}
	return m, nil
}

// AddMember 以 upsert 写入成员关系：已存在则改角色并恢复 active，created_at
// 保持首次加入时间。role 必须在受限取值内，否则返回普通校验错误。
//
// 这是**平台预置路径**（bootstrap 与 POST /api/v1/admin/users 的 organizations
// 字段），调用者不是以组织成员身份行事：它不做权限矩阵检查、不递增 org_revision
// 也不写 policy_publications。面向组织成员的管理入口必须走 T03 的
// ChangeMemberRole / RemoveMember / TransferOwnership / CreateInvitation
// （见 members.go、invitations.go），它们全部在同一个事务里读取调用者的成员关系
// 并执行 ADR-0003 §2.3 的矩阵。
func (s *Service) AddMember(ctx context.Context, orgID, userID int64, role string) (Membership, error) {
	role = strings.TrimSpace(role)
	if !validRole(role) {
		return Membership{}, fmt.Errorf("organization: invalid role %q", role)
	}
	var m Membership
	err := scanMembership(s.db.QueryRowContext(ctx, queryUpsertMembership, orgID, userID, role, nil, time.Now().UTC()).Scan, &m)
	if isForeignKeyViolation(err) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, err
	}
	return m, nil
}

// LinkLegacyTenant 把历史 tenant 字符串映射到组织（可空列 + 部分唯一索引），
// 不创建任何用户或成员归属。重复指向同一组织是幂等的；该 tenant 已属于别的
// 组织、或该组织已经映射了别的 tenant，返回普通冲突错误（含占用方 id）。
func (s *Service) LinkLegacyTenant(ctx context.Context, orgID int64, legacyTenantID string) (Organization, error) {
	tenant := strings.TrimSpace(legacyTenantID)
	if tenant == "" {
		return Organization{}, errors.New("organization: legacy tenant id is required")
	}
	org, err := scanOrganization(s.db.QueryRowContext(ctx, queryLinkLegacyTenant, orgID, tenant, time.Now().UTC()).Scan)
	if err == nil {
		return org, nil
	}
	if isUniqueViolation(err) {
		var holder int64
		if qerr := s.db.QueryRowContext(ctx, queryFindLegacyTenantHolder, tenant).Scan(&holder); qerr == nil {
			return Organization{}, fmt.Errorf("organization: legacy tenant already linked to organization %d", holder)
		}
		return Organization{}, fmt.Errorf("organization: legacy tenant already linked to another organization: %w", err)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Organization{}, err
	}
	current, gerr := scanOrganization(s.db.QueryRowContext(ctx, queryGetOrganization, orgID).Scan)
	if errors.Is(gerr, sql.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	if gerr != nil {
		return Organization{}, gerr
	}
	if current.LegacyTenantID != nil && *current.LegacyTenantID != tenant {
		return Organization{}, fmt.Errorf("organization: organization %d already linked to legacy tenant %q", orgID, *current.LegacyTenantID)
	}
	return Organization{}, fmt.Errorf("organization: cannot link legacy tenant to organization %d", orgID)
}

func normalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultListLimit
	case limit > maxListLimit:
		return maxListLimit
	default:
		return limit
	}
}

// normalizeOrgName 用于企业组织：trim 后必须 1..80 个字符。
func normalizeOrgName(name string) (string, error) {
	clean := strings.TrimSpace(name)
	if n := len([]rune(clean)); n == 0 || n > maxNameRunes {
		return "", ErrNameInvalid
	}
	return clean, nil
}

// personalOrgName 允许空名字（由 SQL 用 email local part 兜底），但拒绝超长。
func personalOrgName(name string) (string, error) {
	clean := strings.TrimSpace(name)
	if len([]rune(clean)) > maxNameRunes {
		return "", ErrNameInvalid
	}
	return clean, nil
}

func validRole(role string) bool {
	switch role {
	case roleOwner, roleAdmin, roleMember:
		return true
	default:
		return false
	}
}

func scanOrganization(scan func(dest ...any) error) (Organization, error) {
	var (
		org    Organization
		legacy sql.NullString
	)
	if err := scan(&org.ID, &org.Type, &org.Name, &org.Status, &legacy, &org.CreatedAt); err != nil {
		return Organization{}, err
	}
	if legacy.Valid {
		value := legacy.String
		org.LegacyTenantID = &value
	}
	return org, nil
}

func scanMemberOrg(scan func(dest ...any) error) (MemberOrg, error) {
	var (
		item   MemberOrg
		legacy sql.NullString
	)
	if err := scan(&item.Organization.ID, &item.Organization.Type, &item.Organization.Name,
		&item.Organization.Status, &legacy, &item.Organization.CreatedAt, &item.MembershipID,
		&item.Role, &item.Status, &item.MonthlyLimitMicro); err != nil {
		return MemberOrg{}, err
	}
	if legacy.Valid {
		value := legacy.String
		item.Organization.LegacyTenantID = &value
	}
	return item, nil
}

func scanMembership(scan func(dest ...any) error, membership *Membership) error {
	var cap sql.NullInt64
	if err := scan(&membership.ID, &membership.OrgID, &membership.UserID, &membership.Role,
		&membership.Status, &cap, &membership.CreatedAt); err != nil {
		return err
	}
	if cap.Valid {
		value := cap.Int64
		membership.MonthlyLimitMicro = &value
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func ensureEnterpriseInTx(ctx context.Context, tx *sql.Tx, orgID int64) error {
	var typ, status string
	err := tx.QueryRowContext(ctx, queryGetOrganizationType, orgID).Scan(&typ, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if typ != typeEnterprise {
		return ErrPersonalTeam
	}
	if status != statusActive {
		return ErrOrganizationSuspended
	}
	return nil
}

func ensureEnterprise(ctx context.Context, db *sql.DB, orgID int64) error {
	var typ, status string
	err := db.QueryRowContext(ctx, queryGetOrganizationType, orgID).Scan(&typ, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if typ != typeEnterprise {
		return ErrPersonalTeam
	}
	if status != statusActive {
		return ErrOrganizationSuspended
	}
	return nil
}
