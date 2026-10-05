package organization

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 邀请（ADR-0003 §2.4）：管理员复制链接，不自动发邮件；明文 token 只返回一次，
// 库里只存 SHA-256；email 与 role 在创建时固定，接受时不可被请求覆盖。
const (
	// DefaultInvitationTTL 是 HTTP 层未指定时的邀请有效期。
	DefaultInvitationTTL = 72 * time.Hour
	// MaxInvitationTTL 是邀请可以被授予的最长有效期（防止永久有效的邀请链接）。
	MaxInvitationTTL = 30 * 24 * time.Hour

	invitationTokenBytes    = 32
	maxInvitationEmailBytes = 254

	statusInvitationPending  = "pending"
	statusInvitationAccepted = "accepted"
	statusInvitationRevoked  = "revoked"
)

// Invitation 是 invitations 表的对外视图：**不含** token 与 token_hash，
// 元数据里只有固定下来的 email/role 与生命周期时间。
type Invitation struct {
	ID         int64
	OrgID      int64
	Email      string
	Role       string
	Status     string
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	CreatedBy  *int64
	CreatedAt  time.Time
}

var (
	// ErrInvitationNotFound：token 不存在、已被清理，或参数为空。
	ErrInvitationNotFound = errors.New("organization: invitation not found")
	// ErrInvitationUsed：邀请已被接受，不能重复使用。
	ErrInvitationUsed = errors.New("organization: invitation was already accepted")
	// ErrInvitationRevoked：邀请已被撤销。
	ErrInvitationRevoked = errors.New("organization: invitation was revoked")
	// ErrInvitationExpired：邀请已过期。
	ErrInvitationExpired = errors.New("organization: invitation has expired")
	// ErrInvitationEmailMismatch：接受者的登录 email 与邀请固定 email 不一致。
	ErrInvitationEmailMismatch = errors.New("organization: invitation was issued for a different email")
	// ErrInvitationConflict：已有活跃成员，或邀请早于最近一次成员移除。
	ErrInvitationConflict = errors.New("organization: invitation conflicts with membership state")
	// ErrInvitationEmailInvalid：邀请 email 形状非法。
	ErrInvitationEmailInvalid = errors.New("organization: invitation email is invalid")
)

// CreateInvitation 创建一条 pending 邀请并返回**一次**明文 token（库里只有
// SHA-256）。调用者必须拥有 manage_members；邀请 owner 只有 owner 可以；
// 邀请 admin 与 ChangeMemberRole 一样需要 create_admin；platform_admin 永远
// 不能被邀请（它不是组织角色）。
//
// email 与 role 在创建时固定：接受路径不接受任何来自请求的覆盖值。
func (s *Service) CreateInvitation(ctx context.Context, actorUserID, orgID int64, email, role string, ttl time.Duration) (Invitation, string, error) {
	normalized := normalizeInvitationEmail(email)
	if err := validateInvitationEmail(normalized); err != nil {
		return Invitation{}, "", err
	}
	cleanRole := strings.TrimSpace(role)
	if cleanRole == RolePlatformAdmin {
		return Invitation{}, "", ErrPlatformRole
	}
	if !validRole(cleanRole) {
		return Invitation{}, "", ErrRoleInvalid
	}
	switch ttl {
	case 0:
		ttl = DefaultInvitationTTL
	default:
		if ttl < 0 {
			return Invitation{}, "", fmt.Errorf("organization: invitation ttl must not be negative")
		}
		if ttl > MaxInvitationTTL {
			ttl = MaxInvitationTTL
		}
	}

	rawToken, tokenHash, err := newInvitationToken()
	if err != nil {
		return Invitation{}, "", err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Invitation{}, "", err
	}
	defer func() { _ = tx.Rollback() }()

	// 同一事务里读取调用者的成员关系并执行矩阵检查（ADR-0003 §2.3）。
	actor, err := actorMembershipInTx(ctx, tx, orgID, actorUserID)
	if err != nil {
		return Invitation{}, "", err
	}
	if !Allowed(actor.Role, ActionManageMembers) {
		return Invitation{}, "", ErrForbidden
	}
	if err := checkInvitationGrant(actor.Role, cleanRole); err != nil {
		return Invitation{}, "", err
	}

	invitation, err := scanInvitation(tx.QueryRowContext(ctx, queryInsertInvitation,
		orgID, normalized, cleanRole, tokenHash, expiresAt, actorUserID, now).Scan)
	if err != nil {
		return Invitation{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Invitation{}, "", err
	}
	return invitation, rawToken, nil
}

// AcceptInvitation 在**一个事务**里完成：锁定邀请行 → 校验状态/有效期/email
// → 锁定组织并校验邀请者权限 → 以邀请固定的角色创建或恢复成员关系 → 标记 accepted → 递增 org_revision
// → 插入 policy_publications(scope=org, status=pending)。返回值是新建/恢复的
// 成员关系；rawToken 是唯一凭据，username/email/role 都不可由请求覆盖。
//
// 拒绝语义（类型化错误）：
//   - token 为空或查不到：ErrInvitationNotFound；
//   - 已被接受：ErrInvitationUsed；已撤销：ErrInvitationRevoked；
//   - 已过期：ErrInvitationExpired；
//   - 登录 email（规范化后）与邀请 email 不一致：ErrInvitationEmailMismatch。
func (s *Service) AcceptInvitation(ctx context.Context, rawToken string, userID int64, userEmail string) (Membership, error) {
	token := strings.TrimSpace(rawToken)
	if token == "" {
		return Membership{}, ErrInvitationNotFound
	}
	tokenHash := hashInvitationToken(token)
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Membership{}, err
	}
	defer func() { _ = tx.Rollback() }()

	invitation, err := scanInvitation(tx.QueryRowContext(ctx, queryLockInvitation, tokenHash).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrInvitationNotFound
	}
	if err != nil {
		return Membership{}, err
	}
	if err := checkInvitationUsable(invitation, userEmail, now); err != nil {
		return Membership{}, err
	}

	// Serialize with role changes/removals and other invitation acceptances.
	// An invitation grants membership, never a role change for an active member.
	var lockedID int64
	if err := tx.QueryRowContext(ctx, queryLockOrganization, invitation.OrgID).Scan(&lockedID); err != nil {
		return Membership{}, err
	}
	// Lock waits may cross the expiry boundary; validate against the write time.
	now = time.Now().UTC()
	if err := checkInvitationUsable(invitation, userEmail, now); err != nil {
		return Membership{}, err
	}
	if invitation.CreatedBy == nil {
		return Membership{}, ErrInvitationRevoked
	}
	actor, err := actorMembershipInTx(ctx, tx, invitation.OrgID, *invitation.CreatedBy)
	if errors.Is(err, ErrNotFound) {
		return Membership{}, ErrInvitationRevoked
	}
	if err != nil {
		return Membership{}, err
	}
	if !Allowed(actor.Role, ActionManageMembers) || checkInvitationGrant(actor.Role, invitation.Role) != nil {
		return Membership{}, ErrInvitationRevoked
	}

	// Restore a removed member only with an invitation issued after removal.
	// The conditional upsert also prevents older, unconsumed grants from
	// overwriting a membership established by a newer invitation.
	var m Membership
	err = tx.QueryRowContext(ctx, queryAcceptMembership, invitation.OrgID, userID, invitation.Role, now, invitation.CreatedAt).
		Scan(&m.OrgID, &m.UserID, &m.Role, &m.Status, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrInvitationConflict
	}
	if isForeignKeyViolation(err) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, err
	}

	result, err := tx.ExecContext(ctx, queryAcceptInvitation, invitation.ID, now)
	if err != nil {
		return Membership{}, err
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		// 行锁下不该发生：说明状态被并发修改。宁可失败也不重复发成员。
		return Membership{}, ErrInvitationUsed
	}

	if err := bumpOrgRevisionInTx(ctx, tx, invitation.OrgID, now); err != nil {
		return Membership{}, err
	}
	if err := tx.Commit(); err != nil {
		return Membership{}, err
	}
	return m, nil
}

// RevokeInvitation 撤销一条 pending 邀请。调用者需要 manage_members；撤销不
// 改变任何成员的既有权限，因此**不**递增 org_revision，也不写发布记录。
// 已被接受的邀请返回 ErrInvitationUsed；重复撤销是幂等的（返回 nil）。
func (s *Service) RevokeInvitation(ctx context.Context, actorUserID, orgID, invitationID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	actor, err := actorMembershipInTx(ctx, tx, orgID, actorUserID)
	if err != nil {
		return err
	}
	if !Allowed(actor.Role, ActionManageMembers) {
		return ErrForbidden
	}

	result, err := tx.ExecContext(ctx, queryRevokeInvitation, invitationID, orgID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		var status string
		err := tx.QueryRowContext(ctx, queryInvitationStatus, invitationID, orgID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvitationNotFound
		}
		if err != nil {
			return err
		}
		switch status {
		case statusInvitationAccepted:
			return ErrInvitationUsed
		case statusInvitationRevoked:
			// 幂等：重复撤销同一邀请不算错误。
		default:
			return ErrInvitationNotFound
		}
	}
	return tx.Commit()
}

// ListInvitations 返回组织邀请的元数据（无 token、无 token_hash）。调用者需要
// manage_members；非成员返回 ErrNotFound，不泄露组织存在性。按 id DESC（最新
// 在前）返回。
func (s *Service) ListInvitations(ctx context.Context, actorUserID, orgID int64) ([]Invitation, error) {
	actor, err := s.actorMembership(ctx, orgID, actorUserID)
	if err != nil {
		return nil, err
	}
	if !Allowed(actor.Role, ActionManageMembers) {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, queryListInvitations, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Invitation, 0, 8)
	for rows.Next() {
		item, err := scanInvitation(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// checkInvitationGrant 复用在成员角色变更上的同一套授予规则，唯一的区别是
// owner 可以被 owner 邀请（受邀者接受后才成为 owner，仍然走专用语义）。
func checkInvitationGrant(actorRole, inviteRole string) error {
	if inviteRole == roleOwner {
		if actorRole != roleOwner {
			return ErrOwnerGrant
		}
		return nil
	}
	return checkRoleGrant(actorRole, inviteRole)
}

// checkInvitationUsable 是纯函数形式的接受前置条件，便于表驱动单测：
// 状态、有效期与 email 匹配三者都满足才返回 nil。
func checkInvitationUsable(invitation Invitation, userEmail string, now time.Time) error {
	switch invitation.Status {
	case statusInvitationPending:
	case statusInvitationAccepted:
		return ErrInvitationUsed
	case statusInvitationRevoked:
		return ErrInvitationRevoked
	default:
		// 未知状态（数据库 CHECK 之外的值）一律拒绝，不猜测语义。
		return ErrInvitationNotFound
	}
	if !now.Before(invitation.ExpiresAt) {
		return ErrInvitationExpired
	}
	if normalizeInvitationEmail(userEmail) != invitation.Email {
		return ErrInvitationEmailMismatch
	}
	return nil
}

// normalizeInvitationEmail 与 internal/iam 的 email 规范化保持一致：trim +
// 小写。邀请的 email 比较因此天然大小写不敏感。
func normalizeInvitationEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validateInvitationEmail 只做形状校验（与 iam.validateEmail 同规则），不发送
// 邮件、不验证邮箱可达性——API 文案必须写明“邮箱未验证”。
func validateInvitationEmail(email string) error {
	if email == "" || len(email) > maxInvitationEmailBytes {
		return ErrInvitationEmailInvalid
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.IndexByte(email[at+1:], '@') >= 0 {
		return ErrInvitationEmailInvalid
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return ErrInvitationEmailInvalid
	}
	return nil
}

// newInvitationToken 生成 32 字节随机 token（base64url，43 字符）及其 SHA-256
// 十六进制摘要。明文只出现在 CreateInvitation 的返回值里。
func newInvitationToken() (string, string, error) {
	buf := make([]byte, invitationTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("organization: generate invitation token: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashInvitationToken(raw), nil
}

func hashInvitationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// bumpOrgRevisionInTx 递增组织 revision 并插入 pending 发布记录。两个写入必须
// 与触发它们的成员变更在同一个事务里，调用方负责传同一个 tx。
func bumpOrgRevisionInTx(ctx context.Context, tx *sql.Tx, orgID int64, now time.Time) error {
	var revision int64
	if err := tx.QueryRowContext(ctx, queryBumpOrgRevision, orgID, now).Scan(&revision); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, queryInsertPolicyPublication, orgID, revision, now); err != nil {
		return err
	}
	return nil
}

func scanInvitation(scan func(dest ...any) error) (Invitation, error) {
	var (
		invitation Invitation
		acceptedAt sql.NullTime
		createdBy  sql.NullInt64
	)
	if err := scan(&invitation.ID, &invitation.OrgID, &invitation.Email, &invitation.Role,
		&invitation.Status, &invitation.ExpiresAt, &acceptedAt, &createdBy, &invitation.CreatedAt); err != nil {
		return Invitation{}, err
	}
	if acceptedAt.Valid {
		value := acceptedAt.Time
		invitation.AcceptedAt = &value
	}
	if createdBy.Valid {
		value := createdBy.Int64
		invitation.CreatedBy = &value
	}
	return invitation, nil
}
