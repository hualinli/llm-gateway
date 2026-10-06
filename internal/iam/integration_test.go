package iam_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"

	"llm-gateway/internal/iam"
	"llm-gateway/internal/observability"
	"llm-gateway/migrations"
)

const testPassword = "correct horse battery staple"

// testClock 是可推进的注入时钟；Service 可能被并发调用，因此加锁。
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// isolatedDSN 为每个测试建独立 schema 并迁移到最新版本；永远不会碰业务库。
func isolatedDSN(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("IAM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("IAM_TEST_POSTGRES_DSN must be a PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "iam_test_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	scoped := u.String()
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, scoped
}

// newService 用 bcrypt.MinCost 加速测试，节流阈值放宽以免互相干扰。
func newService(t *testing.T, db *sql.DB, clock *testClock) *iam.Service {
	t.Helper()
	return iam.NewService(db, iam.Options{
		SessionTTL:         time.Hour,
		BcryptCost:         bcrypt.MinCost,
		LoginRatePerMinute: 600,
		LoginBurst:         100,
		Now:                clock.Now,
	})
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestMigrationCreatesIAMSchemaAndFabricatesNoRows(t *testing.T) {
	db, dsn := isolatedDSN(t)
	if err := migrations.Check(db); err != nil {
		t.Fatalf("schema version after Up: %v", err)
	}
	tables := []string{"users", "sessions", "organizations", "memberships"}
	for _, table := range tables {
		var name sql.NullString
		if err := db.QueryRow(`SELECT to_regclass($1)::text`, table).Scan(&name); err != nil || !name.Valid {
			t.Fatalf("missing table %s: %v %v", table, name, err)
		}
	}
	for _, index := range []string{
		"idx_organizations_personal_owner",
		"idx_organizations_legacy_tenant_id",
		"idx_memberships_active_user",
		"idx_memberships_active_org",
		"idx_sessions_active_user",
	} {
		var name sql.NullString
		if err := db.QueryRow(`SELECT to_regclass($1)::text`, index).Scan(&name); err != nil || !name.Valid {
			t.Fatalf("missing index %s: %v %v", index, name, err)
		}
	}
	// ADR-0002 §7：迁移不得伪造历史用户/成员归属。
	for _, table := range tables {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s has %d rows after migration, want 0", table, count)
		}
	}

	// 已有库升级路径：3 → 4 的 down/up 必须可逆且不破坏 v1..v3 的表。
	m, err := migrations.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// 固定迁移到 000004 之前（users/sessions/organizations/memberships 由 000004 建）：
	// 后续任务会继续追加 000005+，用 Steps(-1) 只会回滚最新那个迁移。
	if err := m.Migrate(3); err != nil {
		t.Fatalf("migrate down to 3: %v", err)
	}
	for _, table := range tables {
		var name sql.NullString
		if err := db.QueryRow(`SELECT to_regclass($1)::text`, table).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name.Valid {
			t.Fatalf("down migration left table %s behind", table)
		}
	}
	for _, table := range []string{"metering_events", "control_config", "provider_credentials"} {
		var name sql.NullString
		if err := db.QueryRow(`SELECT to_regclass($1)::text`, table).Scan(&name); err != nil || !name.Valid {
			t.Fatalf("down migration dropped earlier table %s: %v %v", table, name, err)
		}
	}
	if err := migrations.Up(dsn); err != nil {
		t.Fatalf("re-up after down: %v", err)
	}
	if err := migrations.Check(db); err != nil {
		t.Fatalf("schema version after re-up: %v", err)
	}
}

func TestCreateUserNormalizesEmailAndEnforcesUniqueness(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	svc := newService(t, db, clock)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, "  Alice@Example.COM ", testPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "alice@example.com" {
		t.Fatalf("Email = %q, want normalized alice@example.com", user.Email)
	}
	if user.Status != "active" || user.PlatformAdmin || user.AuthVersion != 1 {
		t.Fatalf("unexpected fresh user: %+v", user)
	}
	if !user.CreatedAt.Equal(clock.Now()) || !user.UpdatedAt.Equal(clock.Now()) {
		t.Fatalf("timestamps = %v/%v, want injected clock %v", user.CreatedAt, user.UpdatedAt, clock.Now())
	}

	// 大小写/空白差异也必须撞唯一约束。
	for _, duplicate := range []string{"alice@example.com", " ALICE@EXAMPLE.com ", "Alice@Example.com"} {
		if _, err := svc.CreateUser(ctx, duplicate, testPassword, false); !errors.Is(err, iam.ErrEmailTaken) {
			t.Fatalf("CreateUser(%q) = %v, want ErrEmailTaken", duplicate, err)
		}
	}

	found, err := svc.FindUserByEmail(ctx, "ALICE@EXAMPLE.com")
	if err != nil || found.ID != user.ID {
		t.Fatalf("FindUserByEmail = (%+v, %v), want user %d", found, err, user.ID)
	}
	got, err := svc.GetUser(ctx, user.ID)
	if err != nil || got.ID != user.ID || got.Email != user.Email {
		t.Fatalf("GetUser = (%+v, %v), want user %d", got, err, user.ID)
	}

	// 只存 bcrypt 哈希，明文绝不落库。
	var hash string
	if err := db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = $1`, user.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == testPassword || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("password_hash = %q, want a bcrypt hash", hash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPassword)); err != nil {
		t.Fatalf("stored hash does not verify the password: %v", err)
	}
	if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost != bcrypt.MinCost {
		t.Fatalf("bcrypt cost = %d (%v), want %d from Options", cost, err, bcrypt.MinCost)
	}

	if _, err := svc.GetUser(ctx, 9999999); !errors.Is(err, iam.ErrNotFound) {
		t.Fatalf("GetUser(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := svc.FindUserByEmail(ctx, "nobody@example.com"); !errors.Is(err, iam.ErrNotFound) {
		t.Fatalf("FindUserByEmail(unknown) = %v, want ErrNotFound", err)
	}

	admin, err := svc.CreateUser(ctx, "Admin@Example.com", testPassword, true)
	if err != nil {
		t.Fatal(err)
	}
	if !admin.PlatformAdmin || admin.Email != "admin@example.com" {
		t.Fatalf("admin user = %+v", admin)
	}

	// 输入校验错误没有哨兵值：HTTP 边界必须先做 400 级校验。
	if _, err := svc.CreateUser(ctx, "not-an-email", testPassword, false); err == nil {
		t.Fatal("CreateUser(invalid email) = nil, want validation error")
	}
	if _, err := svc.CreateUser(ctx, "empty@example.com", "", false); err == nil {
		t.Fatal("CreateUser(empty password) = nil, want validation error")
	}
}

func TestSetUserStatusBumpsAuthVersionOnce(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	svc := newService(t, db, clock)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, "alice@example.com", testPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := svc.SetUserStatus(ctx, user.ID, "disabled")
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != "disabled" || disabled.AuthVersion != user.AuthVersion+1 {
		t.Fatalf("disabled = %+v, want status disabled and auth_version %d", disabled, user.AuthVersion+1)
	}
	again, err := svc.SetUserStatus(ctx, user.ID, "disabled")
	if err != nil {
		t.Fatal(err)
	}
	if again.AuthVersion != disabled.AuthVersion {
		t.Fatalf("repeated disable bumped auth_version to %d, want idempotent %d", again.AuthVersion, disabled.AuthVersion)
	}
	active, err := svc.SetUserStatus(ctx, user.ID, "active")
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != "active" || active.AuthVersion != disabled.AuthVersion {
		t.Fatalf("re-enabled = %+v, want active and auth_version %d", active, disabled.AuthVersion)
	}
	if _, err := svc.SetUserStatus(ctx, 9999999, "disabled"); !errors.Is(err, iam.ErrNotFound) {
		t.Fatalf("SetUserStatus(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := svc.SetUserStatus(ctx, user.ID, "suspended"); err == nil {
		t.Fatal("SetUserStatus(suspended) = nil, want validation error")
	}
}

func TestLoginAndSessionLifecycle(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	svc := newService(t, db, clock)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, "alice@example.com", testPassword, false)
	if err != nil {
		t.Fatal(err)
	}

	// 未知 email 与错误密码都是 ErrInvalidCredentials（不泄露用户是否存在）。
	if _, _, err := svc.Login(ctx, "alice@example.com", "wrong password", "alice|10.0.0.1"); !errors.Is(err, iam.ErrInvalidCredentials) {
		t.Fatalf("wrong password = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := svc.Login(ctx, "nobody@example.com", "wrong password", "nobody|10.0.0.1"); !errors.Is(err, iam.ErrInvalidCredentials) {
		t.Fatalf("unknown email = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := svc.Login(ctx, "alice@example.com", strings.Repeat("x", 100), "alice|10.0.0.1"); !errors.Is(err, iam.ErrInvalidCredentials) {
		t.Fatalf("over-long password = %v, want ErrInvalidCredentials", err)
	}

	logged, issued, err := svc.Login(ctx, "  ALICE@example.com ", testPassword, "alice|10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if logged.ID != user.ID || logged.Email != user.Email {
		t.Fatalf("logged = %+v, want %+v", logged, user)
	}
	if issued.Token == "" || issued.CSRFToken == "" || issued.Token == issued.CSRFToken {
		t.Fatalf("issued tokens invalid: %+v", issued)
	}
	if issued.Session.TokenHash != sha256Hex(issued.Token) {
		t.Fatalf("TokenHash = %q, want sha256(%q)", issued.Session.TokenHash, issued.Token)
	}
	if issued.Session.AuthVersion != user.AuthVersion {
		t.Fatalf("session auth_version = %d, want %d", issued.Session.AuthVersion, user.AuthVersion)
	}
	if !issued.Session.ExpiresAt.Equal(clock.Now().Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want %v", issued.Session.ExpiresAt, clock.Now().Add(time.Hour))
	}

	// 库里只有摘要，明文 token/CSRF 不落库。
	var storedToken, storedCSRF string
	if err := db.QueryRowContext(ctx, `SELECT token_hash, csrf_hash FROM sessions WHERE id = $1`, issued.Session.ID).
		Scan(&storedToken, &storedCSRF); err != nil {
		t.Fatal(err)
	}
	if storedToken != sha256Hex(issued.Token) || storedToken == issued.Token {
		t.Fatalf("token_hash = %q, want a sha256 digest", storedToken)
	}
	if storedCSRF != sha256Hex(issued.CSRFToken) || storedCSRF == issued.CSRFToken {
		t.Fatalf("csrf_hash = %q, want a sha256 digest", storedCSRF)
	}

	resolvedUser, resolved, err := svc.ResolveSession(ctx, issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedUser.ID != user.ID || resolved.ID != issued.Session.ID || resolved.UserID != user.ID {
		t.Fatalf("resolved = (%+v, %+v)", resolvedUser, resolved)
	}
	if resolved.RevokedAt != nil {
		t.Fatalf("fresh session revoked_at = %v, want nil", resolved.RevokedAt)
	}

	// 解析会刷新 last_seen_at。
	clock.Advance(5 * time.Minute)
	if _, _, err := svc.ResolveSession(ctx, issued.Token); err != nil {
		t.Fatal(err)
	}
	var lastSeen time.Time
	if err := db.QueryRowContext(ctx, `SELECT last_seen_at FROM sessions WHERE id = $1`, issued.Session.ID).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	if !lastSeen.UTC().Equal(clock.Now()) {
		t.Fatalf("last_seen_at = %v, want %v", lastSeen.UTC(), clock.Now())
	}

	if err := svc.VerifyCSRF(ctx, resolved, issued.CSRFToken); err != nil {
		t.Fatalf("VerifyCSRF(correct) = %v, want nil", err)
	}
	if err := svc.VerifyCSRF(ctx, resolved, issued.CSRFToken+"x"); !errors.Is(err, iam.ErrCSRF) {
		t.Fatalf("VerifyCSRF(wrong) = %v, want ErrCSRF", err)
	}
	if err := svc.VerifyCSRF(ctx, resolved, ""); !errors.Is(err, iam.ErrCSRF) {
		t.Fatalf("VerifyCSRF(empty) = %v, want ErrCSRF", err)
	}
	if err := svc.VerifyCSRF(ctx, iam.Session{}, issued.CSRFToken); !errors.Is(err, iam.ErrCSRF) {
		t.Fatalf("VerifyCSRF(zero session) = %v, want ErrCSRF", err)
	}

	// 篡改/未知 token 一律 ErrSessionInvalid。
	for _, raw := range []string{"", "not-a-token", issued.Token + "x", issued.CSRFToken} {
		if _, _, err := svc.ResolveSession(ctx, raw); !errors.Is(err, iam.ErrSessionInvalid) {
			t.Fatalf("ResolveSession(%q) = %v, want ErrSessionInvalid", raw, err)
		}
	}

	if count, err := svc.CountActiveSessions(ctx, user.ID); err != nil || count != 1 {
		t.Fatalf("CountActiveSessions = (%d, %v), want (1, nil)", count, err)
	}

	// 登出：撤销是服务端事实，旧 token 立即失效且不能重复撤销。
	if err := svc.RevokeSession(ctx, issued.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveSession(ctx, issued.Token); !errors.Is(err, iam.ErrSessionInvalid) {
		t.Fatalf("ResolveSession after logout = %v, want ErrSessionInvalid", err)
	}
	if err := svc.VerifyCSRF(ctx, resolved, issued.CSRFToken); !errors.Is(err, iam.ErrCSRF) {
		t.Fatalf("VerifyCSRF after logout = %v, want ErrCSRF", err)
	}
	if err := svc.RevokeSession(ctx, issued.Token); !errors.Is(err, iam.ErrSessionInvalid) {
		t.Fatalf("second logout = %v, want ErrSessionInvalid", err)
	}
	if err := svc.RevokeSession(ctx, "never-issued"); !errors.Is(err, iam.ErrSessionInvalid) {
		t.Fatalf("RevokeSession(unknown) = %v, want ErrSessionInvalid", err)
	}
	if count, err := svc.CountActiveSessions(ctx, user.ID); err != nil || count != 0 {
		t.Fatalf("CountActiveSessions after logout = (%d, %v), want (0, nil)", count, err)
	}
}

func TestRegisterAtomicallyCreatesPersonalAccountWalletAndSession(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	svc := newService(t, db, clock)
	ctx := context.Background()

	user, issued, err := svc.Register(ctx, "  NEW.USER@example.com  ", testPassword, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "new.user@example.com" || user.PlatformAdmin || issued.Token == "" || issued.CSRFToken == "" {
		t.Fatalf("Register returned user/session = %+v / %+v", user, issued)
	}
	var orgID, ownerID, membershipUserID int64
	var orgType, orgStatus, role, membershipStatus, currency, walletStatus string
	var membershipLimit sql.NullInt64
	var balance, reserved int64
	if err := db.QueryRowContext(ctx, `SELECT o.id, o.type, o.status, o.owner_user_id,
		m.user_id, m.role, m.status, m.monthly_limit_micro,
		b.currency, b.posted_balance_micro, b.reserved_micro, b.status
		FROM organizations o JOIN memberships m ON m.org_id=o.id
		JOIN billing_accounts b ON b.org_id=o.id WHERE o.owner_user_id=$1`, user.ID).
		Scan(&orgID, &orgType, &orgStatus, &ownerID, &membershipUserID, &role, &membershipStatus,
			&membershipLimit, &currency, &balance, &reserved, &walletStatus); err != nil {
		t.Fatal(err)
	}
	if orgType != "personal" || orgStatus != "active" || ownerID != user.ID || membershipUserID != user.ID ||
		role != "owner" || membershipStatus != "active" || membershipLimit.Valid ||
		currency != "CNY" || balance != 0 || reserved != 0 || walletStatus != "active" {
		t.Fatalf("registration provisioning = org %d %s/%s owner=%d membership=%d %s/%s cap=%+v wallet=%s %d/%d %s",
			orgID, orgType, orgStatus, ownerID, membershipUserID, role, membershipStatus, membershipLimit,
			currency, balance, reserved, walletStatus)
	}
	if _, _, err := svc.ResolveSession(ctx, issued.Token); err != nil {
		t.Fatalf("new registration session does not resolve: %v", err)
	}
	for _, table := range []string{"users", "organizations", "memberships", "billing_accounts", "sessions"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s row count=%d, want 1", table, count)
		}
	}
	if _, _, err := svc.Register(ctx, "NEW.USER@example.com", testPassword, "127.0.0.1"); !errors.Is(err, iam.ErrEmailTaken) {
		t.Fatalf("duplicate Register = %v, want ErrEmailTaken", err)
	}
	var users, orgs, wallets int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM users),
		(SELECT count(*) FROM organizations), (SELECT count(*) FROM billing_accounts)`).
		Scan(&users, &orgs, &wallets); err != nil {
		t.Fatal(err)
	}
	if users != 1 || orgs != 1 || wallets != 1 {
		t.Fatalf("duplicate registration left partial rows: users=%d orgs=%d wallets=%d", users, orgs, wallets)
	}
}

func TestChangePasswordRevokesAllSessionsOnlyAfterOldPasswordMatches(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	svc := newService(t, db, clock)
	ctx := context.Background()
	user, first, err := svc.Register(ctx, "password-change@example.com", testPassword, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := svc.Login(ctx, user.Email, testPassword, "127.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	if count, err := svc.CountActiveSessions(ctx, user.ID); err != nil || count != 2 {
		t.Fatalf("active sessions before change = %d, %v; want 2", count, err)
	}
	if _, err := svc.ChangePassword(ctx, user.ID, "wrong old password", "new password"); !errors.Is(err, iam.ErrPasswordIncorrect) {
		t.Fatalf("ChangePassword(wrong old) = %v, want ErrPasswordIncorrect", err)
	}
	if count, err := svc.CountActiveSessions(ctx, user.ID); err != nil || count != 2 {
		t.Fatalf("wrong old password changed session state: count=%d err=%v", count, err)
	}
	if _, _, err := svc.ResolveSession(ctx, first.Token); err != nil {
		t.Fatalf("wrong old password invalidated first session: %v", err)
	}
	changed, err := svc.ChangePassword(ctx, user.ID, testPassword, "new password")
	if err != nil {
		t.Fatal(err)
	}
	if changed.AuthVersion != user.AuthVersion+1 {
		t.Fatalf("auth_version after change=%d, want %d", changed.AuthVersion, user.AuthVersion+1)
	}
	if count, err := svc.CountActiveSessions(ctx, user.ID); err != nil || count != 0 {
		t.Fatalf("active sessions after change = %d, %v; want 0", count, err)
	}
	for _, token := range []string{first.Token, second.Token} {
		if _, _, err := svc.ResolveSession(ctx, token); !errors.Is(err, iam.ErrSessionInvalid) {
			t.Fatalf("ResolveSession(old token) = %v, want ErrSessionInvalid", err)
		}
	}
	if _, _, err := svc.Login(ctx, user.Email, testPassword, "127.0.0.3"); !errors.Is(err, iam.ErrInvalidCredentials) {
		t.Fatalf("Login(old password) = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := svc.Login(ctx, user.Email, "new password", "127.0.0.3"); err != nil {
		t.Fatalf("Login(new password) = %v, want nil", err)
	}
}

func TestSessionExpiryUsesInjectedClock(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	svc := iam.NewService(db, iam.Options{
		SessionTTL:         30 * time.Minute,
		BcryptCost:         bcrypt.MinCost,
		LoginRatePerMinute: 600,
		LoginBurst:         100,
		Now:                clock.Now,
	})
	ctx := context.Background()
	user, err := svc.CreateUser(ctx, "alice@example.com", testPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	_, issued, err := svc.Login(ctx, "alice@example.com", testPassword, "alice|10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !issued.Session.ExpiresAt.Equal(clock.Now().Add(30 * time.Minute)) {
		t.Fatalf("ExpiresAt = %v, want +30m", issued.Session.ExpiresAt)
	}

	clock.Advance(29 * time.Minute)
	if _, _, err := svc.ResolveSession(ctx, issued.Token); err != nil {
		t.Fatalf("session before expiry = %v, want nil", err)
	}
	clock.Advance(2 * time.Minute)
	if _, _, err := svc.ResolveSession(ctx, issued.Token); !errors.Is(err, iam.ErrSessionInvalid) {
		t.Fatalf("expired session = %v, want ErrSessionInvalid", err)
	}
	if err := svc.VerifyCSRF(ctx, issued.Session, issued.CSRFToken); !errors.Is(err, iam.ErrCSRF) {
		t.Fatalf("VerifyCSRF on expired session = %v, want ErrCSRF", err)
	}
	if count, err := svc.CountActiveSessions(ctx, user.ID); err != nil || count != 0 {
		t.Fatalf("CountActiveSessions = (%d, %v), want (0, nil)", count, err)
	}
}

func TestDisableInvalidatesSessionsAndLogin(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	svc := newService(t, db, clock)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, "alice@example.com", testPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	_, issued, err := svc.Login(ctx, "alice@example.com", testPassword, "alice|10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := svc.SetUserStatus(ctx, user.ID, "disabled")
	if err != nil {
		t.Fatal(err)
	}
	if disabled.AuthVersion != user.AuthVersion+1 {
		t.Fatalf("auth_version = %d, want %d", disabled.AuthVersion, user.AuthVersion+1)
	}

	// 旧 cookie 立即 401（auth_version 不匹配），且禁用账号不能再登录。
	if _, _, err := svc.ResolveSession(ctx, issued.Token); !errors.Is(err, iam.ErrSessionInvalid) {
		t.Fatalf("session of disabled user = %v, want ErrSessionInvalid", err)
	}
	if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, "alice|10.0.0.1"); !errors.Is(err, iam.ErrUserDisabled) {
		t.Fatalf("login disabled = %v, want ErrUserDisabled", err)
	}
	if _, _, err := svc.Login(ctx, "alice@example.com", "wrong password", "alice|10.0.0.1"); !errors.Is(err, iam.ErrInvalidCredentials) {
		t.Fatalf("disabled user with wrong password = %v, want ErrInvalidCredentials (不泄露状态)", err)
	}

	// 重新启用不会把已作废的会话“救活”，但可以重新登录。
	if _, err := svc.SetUserStatus(ctx, user.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveSession(ctx, issued.Token); !errors.Is(err, iam.ErrSessionInvalid) {
		t.Fatalf("session after re-enable = %v, want ErrSessionInvalid", err)
	}
	_, fresh, err := svc.Login(ctx, "alice@example.com", testPassword, "alice|10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveSession(ctx, fresh.Token); err != nil {
		t.Fatalf("fresh session = %v, want nil", err)
	}
}

func TestLoginThrottleReturnsRateLimited(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	// 60/分钟 = 每秒 1 个令牌，整数回填；burst 2。
	svc := iam.NewService(db, iam.Options{
		SessionTTL:         time.Hour,
		BcryptCost:         bcrypt.MinCost,
		LoginRatePerMinute: 60,
		LoginBurst:         2,
		Now:                clock.Now,
	})
	ctx := context.Background()
	if _, err := svc.CreateUser(ctx, "alice@example.com", testPassword, false); err != nil {
		t.Fatal(err)
	}
	const key = "alice@example.com|10.0.0.1"
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, key); err != nil {
			t.Fatalf("attempt %d inside burst: %v", i+1, err)
		}
	}
	// 即使密码正确，超出突发也必须 429。
	if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, key); !errors.Is(err, iam.ErrRateLimited) {
		t.Fatalf("third attempt = %v, want ErrRateLimited", err)
	}
	// 节流先于凭据校验：错误密码也只返回 ErrRateLimited。
	if _, _, err := svc.Login(ctx, "alice@example.com", "wrong password", key); !errors.Is(err, iam.ErrRateLimited) {
		t.Fatalf("throttled wrong password = %v, want ErrRateLimited", err)
	}
	// 其它 client key 互不影响。
	if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, "alice@example.com|10.0.0.2"); err != nil {
		t.Fatalf("independent client key = %v, want nil", err)
	}
	// 空 clientKey 退化为 email 维度，是一个独立的桶（burst 2）。
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, ""); err != nil {
			t.Fatalf("email-key attempt %d: %v", i+1, err)
		}
	}
	if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, ""); !errors.Is(err, iam.ErrRateLimited) {
		t.Fatalf("exhausted email key = %v, want ErrRateLimited", err)
	}
	// 一秒后回填一个令牌。
	clock.Advance(time.Second)
	if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, key); err != nil {
		t.Fatalf("attempt after refill = %v, want nil", err)
	}
}

// TestLoginThrottleIsScopedToEmailAndClient 是 BUG-1 的回归测试：T02 黑盒验收发现
// 调用方只传 IP 时节流桶以 IP 为键，导致共享出口 IP 上任意账号爆破会锁死同 IP 的
// 其它账号。ADR-0002 §2.5 要求 bucket 为 email+client，因此组合必须发生在
// Login 内部，而不能依赖调用方。
func TestLoginThrottleIsScopedToEmailAndClient(t *testing.T) {
	db, _ := isolatedDSN(t)
	clock := newTestClock()
	svc := iam.NewService(db, iam.Options{
		SessionTTL:         time.Hour,
		BcryptCost:         bcrypt.MinCost,
		LoginRatePerMinute: 60,
		LoginBurst:         2,
		Now:                clock.Now,
	})
	ctx := context.Background()
	for _, email := range []string{"alice@example.com", "bob@example.com"} {
		if _, err := svc.CreateUser(ctx, email, testPassword, false); err != nil {
			t.Fatal(err)
		}
	}
	const clientIP = "203.0.113.7" // 调用方只传 IP，与 HTTP 层一致
	for i := 0; i < 2; i++ {
		if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, clientIP); err != nil {
			t.Fatalf("alice attempt %d inside burst: %v", i+1, err)
		}
	}
	if _, _, err := svc.Login(ctx, "alice@example.com", testPassword, clientIP); !errors.Is(err, iam.ErrRateLimited) {
		t.Fatalf("alice third attempt = %v, want ErrRateLimited", err)
	}
	// 同一客户端标识上的另一个账号不能被连坐。
	if _, _, err := svc.Login(ctx, "bob@example.com", testPassword, clientIP); err != nil {
		t.Fatalf("bob on the same client key = %v, want nil (cross-account lockout regression)", err)
	}
	// 同一 email 的大小写变体共享同一个桶（规范化）。
	if _, _, err := svc.Login(ctx, "ALICE@example.com", testPassword, clientIP); !errors.Is(err, iam.ErrRateLimited) {
		t.Fatalf("case-varied alice = %v, want ErrRateLimited", err)
	}
}
