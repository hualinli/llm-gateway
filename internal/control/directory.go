package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// Directory is the source of user IDs and tenant membership. A future login
// service calls this boundary; neither login nor API-key creation invents IDs.
type Directory interface {
	CreateUser(context.Context) (userID int64, personalTenantID string, err error)
	CreateEnterprise(context.Context, int64) (tenantID string, err error)
	AddMember(context.Context, string, int64, MembershipRole) error
	Membership(context.Context, string, int64) (MembershipRole, error)
}

type MembershipRole string

const (
	MembershipOwner  MembershipRole = "owner"
	MembershipAdmin  MembershipRole = "admin"
	MembershipMember MembershipRole = "member"
)

var ErrMembership = errors.New("active tenant membership required")

func validMembershipRole(role MembershipRole) bool {
	return role == MembershipOwner || role == MembershipAdmin || role == MembershipMember
}

func personalTenantID(userID int64) string { return "personal-" + strconv.FormatInt(userID, 10) }

func enterpriseTenantID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "enterprise-" + hex.EncodeToString(b[:]), nil
}

type PGDirectory struct{ db *sql.DB }

func NewPGDirectory(db *sql.DB) *PGDirectory { return &PGDirectory{db: db} }

func (d *PGDirectory) CreateUser(ctx context.Context) (int64, string, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, queryCreateUser).Scan(&id); err != nil {
		return 0, "", err
	}
	tenantID := personalTenantID(id)
	if _, err := tx.ExecContext(ctx, queryCreatePersonalTenant, tenantID, id); err != nil {
		return 0, "", err
	}
	if _, err := tx.ExecContext(ctx, queryAddMembership, tenantID, id, MembershipOwner); err != nil {
		return 0, "", err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", err
	}
	return id, tenantID, nil
}

func (d *PGDirectory) CreateEnterprise(ctx context.Context, adminUserID int64) (string, error) {
	if adminUserID <= 0 {
		return "", errors.New("enterprise admin user_id is required")
	}
	id, err := enterpriseTenantID()
	if err != nil {
		return "", err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, queryCreateEnterpriseTenant, id); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, queryAddMembership, id, adminUserID, MembershipOwner); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (d *PGDirectory) AddMember(ctx context.Context, tenantID string, userID int64, role MembershipRole) error {
	if tenantID == "" || userID <= 0 || !validMembershipRole(role) || role == MembershipOwner {
		return errors.New("valid tenant_id, user_id and member/admin role required")
	}
	var kind string
	if err := d.db.QueryRowContext(ctx, "SELECT kind FROM tenants WHERE id=$1", tenantID).Scan(&kind); err != nil {
		return err
	}
	if kind != "enterprise" {
		return errors.New("members can only join enterprise tenants")
	}
	result, err := d.db.ExecContext(ctx, queryAddMembership, tenantID, userID, role)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return errors.New("owner role cannot be replaced")
	}
	return nil
}

func (d *PGDirectory) Membership(ctx context.Context, tenantID string, userID int64) (MembershipRole, error) {
	if tenantID == "" || userID <= 0 {
		return "", ErrMembership
	}
	var role MembershipRole
	if err := d.db.QueryRowContext(ctx, queryMembershipRole, tenantID, userID).Scan(&role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrMembership
		}
		return "", err
	}
	return role, nil
}

// MemoryDirectory keeps local/demo provisioning usable without PostgreSQL.
type MemoryDirectory struct {
	mu      sync.Mutex
	nextID  int64
	users   map[int64]struct{}
	members map[string]map[int64]MembershipRole
}

func NewMemoryDirectory() *MemoryDirectory {
	return &MemoryDirectory{users: map[int64]struct{}{}, members: map[string]map[int64]MembershipRole{}}
}

func (d *MemoryDirectory) CreateUser(_ context.Context) (int64, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	id := d.nextID
	tenantID := personalTenantID(id)
	d.users[id] = struct{}{}
	d.members[tenantID] = map[int64]MembershipRole{id: MembershipOwner}
	return id, tenantID, nil
}

func (d *MemoryDirectory) CreateEnterprise(_ context.Context, adminUserID int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.users[adminUserID]; !ok {
		return "", fmt.Errorf("admin user %d does not exist", adminUserID)
	}
	id, err := enterpriseTenantID()
	if err != nil {
		return "", err
	}
	d.members[id] = map[int64]MembershipRole{adminUserID: MembershipOwner}
	return id, nil
}

func (d *MemoryDirectory) AddMember(_ context.Context, tenantID string, userID int64, role MembershipRole) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !validMembershipRole(role) || role == MembershipOwner {
		return errors.New("member/admin role required")
	}
	if _, ok := d.users[userID]; !ok {
		return errors.New("user does not exist")
	}
	members, ok := d.members[tenantID]
	if !ok {
		return errors.New("tenant does not exist")
	}
	if len(tenantID) < len("enterprise-") || tenantID[:len("enterprise-")] != "enterprise-" {
		return errors.New("members can only join enterprise tenants")
	}
	if members[userID] == MembershipOwner {
		return errors.New("owner role cannot be replaced")
	}
	members[userID] = role
	return nil
}

func (d *MemoryDirectory) Membership(_ context.Context, tenantID string, userID int64) (MembershipRole, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	role, ok := d.members[tenantID][userID]
	if !ok {
		return "", ErrMembership
	}
	return role, nil
}

//go:embed queries/create_user.sql
var queryCreateUser string

//go:embed queries/create_personal_tenant.sql
var queryCreatePersonalTenant string

//go:embed queries/create_enterprise_tenant.sql
var queryCreateEnterpriseTenant string

//go:embed queries/add_membership.sql
var queryAddMembership string

//go:embed queries/membership_role.sql
var queryMembershipRole string
