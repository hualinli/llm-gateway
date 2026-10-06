package iam

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// Register creates the user, personal organization, owner membership, zero
// balance wallet, and first session in one transaction. A registration failure
// cannot leave a partially provisioned account behind.
func (s *Service) Register(ctx context.Context, email, password, clientKey string) (User, IssuedSession, error) {
	normalized := normalizeEmail(email)
	if err := validateEmail(normalized); err != nil {
		return User{}, IssuedSession{}, err
	}
	if err := validatePassword(password); err != nil {
		return User{}, IssuedSession{}, err
	}
	throttleKey := normalized
	if client := strings.TrimSpace(clientKey); client != "" {
		throttleKey += "|" + client
	}
	if !s.throttle.allow("register|" + throttleKey) {
		return User{}, IssuedSession{}, ErrRateLimited
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.opts.BcryptCost)
	if err != nil {
		return User{}, IssuedSession{}, fmt.Errorf("iam: hash password: %w", err)
	}
	token, err := newToken()
	if err != nil {
		return User{}, IssuedSession{}, err
	}
	csrfToken, err := newToken()
	if err != nil {
		return User{}, IssuedSession{}, err
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, IssuedSession{}, err
	}
	defer func() { _ = tx.Rollback() }()
	user, err := scanUser(tx.QueryRowContext(ctx, queryInsertUser, normalized, string(hash), false, now).Scan)
	if isUniqueViolation(err) {
		return User{}, IssuedSession{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, IssuedSession{}, err
	}
	var orgID int64
	err = tx.QueryRowContext(ctx, queryRegisterPersonalOrganization, user.ID, now).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, IssuedSession{}, ErrNotFound
	}
	if err != nil {
		return User{}, IssuedSession{}, err
	}
	if _, err := tx.ExecContext(ctx, queryRegisterOwnerMembership, orgID, user.ID, now); err != nil {
		return User{}, IssuedSession{}, err
	}
	if _, err := tx.ExecContext(ctx, queryRegisterBillingWallet, orgID, now); err != nil {
		return User{}, IssuedSession{}, err
	}
	session := Session{
		UserID:      user.ID,
		TokenHash:   hashToken(token),
		ExpiresAt:   now.Add(s.opts.SessionTTL),
		AuthVersion: user.AuthVersion,
		CreatedAt:   now,
	}
	err = tx.QueryRowContext(ctx, queryInsertSession, session.TokenHash, session.UserID, hashToken(csrfToken),
		session.ExpiresAt, session.AuthVersion, session.CreatedAt).Scan(&session.ID)
	if err != nil {
		return User{}, IssuedSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, IssuedSession{}, err
	}
	return user, IssuedSession{Session: session, Token: token, CSRFToken: csrfToken}, nil
}

// CreateUser 创建受控用户（无自助注册）。email 规范化后唯一，冲突返回
// ErrEmailTaken；密码只存 bcrypt 哈希。
func (s *Service) CreateUser(ctx context.Context, email, password string, platformAdmin bool) (User, error) {
	normalized := normalizeEmail(email)
	if err := validateEmail(normalized); err != nil {
		return User{}, err
	}
	if err := validatePassword(password); err != nil {
		return User{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.opts.BcryptCost)
	if err != nil {
		return User{}, fmt.Errorf("iam: hash password: %w", err)
	}
	now := s.now()
	user, err := scanUser(s.db.QueryRowContext(ctx, queryInsertUser, normalized, string(hash), platformAdmin, now).Scan)
	if isUniqueViolation(err) {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// GetUser 按 id 读取用户；不存在返回 ErrNotFound。
func (s *Service) GetUser(ctx context.Context, id int64) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx, queryGetUser, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// FindUserByEmail 按规范化 email 读取用户；不存在返回 ErrNotFound。
func (s *Service) FindUserByEmail(ctx context.Context, email string) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx, queryFindUserByEmail, normalizeEmail(email)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// SetUserStatus 切换 active/disabled。转入 disabled 时 auth_version +1，
// 使该用户所有既有会话在校验时立即失效；重复 disable 不再递增（幂等）。
func (s *Service) SetUserStatus(ctx context.Context, userID int64, status string) (User, error) {
	if err := validateStatus(status); err != nil {
		return User{}, err
	}
	user, err := scanUser(s.db.QueryRowContext(ctx, querySetUserStatus, userID, status, s.now()).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// ChangePassword replaces one user's bcrypt hash and increments auth_version.
// It revokes every existing browser session in the same transaction; even a
// concurrent login based on the old hash receives the previous auth_version
// and becomes invalid as soon as the password change commits.
func (s *Service) ChangePassword(ctx context.Context, userID int64, oldPassword, newPassword string) (User, error) {
	if userID <= 0 {
		return User{}, ErrNotFound
	}
	if err := validatePassword(oldPassword); err != nil {
		return User{}, err
	}
	if err := validatePassword(newPassword); err != nil {
		return User{}, err
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.opts.BcryptCost)
	if err != nil {
		return User{}, fmt.Errorf("iam: hash password: %w", err)
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Revoke sessions first, matching ResolveSession's sessions-row lock order.
	if _, err := tx.ExecContext(ctx, queryRevokeUserSessions, userID, now); err != nil {
		return User{}, err
	}
	var currentHash string
	err = tx.QueryRowContext(ctx, queryLockUserPassword, userID).Scan(&currentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(oldPassword)) != nil {
		return User{}, ErrPasswordIncorrect
	}
	user, err := scanUser(tx.QueryRowContext(ctx, queryChangePassword, userID, string(newHash), now).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

// Login 校验密码并签发一个会话。
//
// 顺序（避免用户枚举）：
//  1. 进程内节流：bucket key 固定为 “规范化 email|clientKey”，由本方法自己拼接，
//     调用方只传客户端标识（通常是 IP）。这样同一 IP 上攻击者爆破 A 账号不会
//     锁死 B 账号；clientKey 为空时退化为按 email 计数（明确记录在 ADR-0002 §2.5，
//     首版不声称抗分布式爆破，IP 级总量限制由部署侧 WAF/网关承担）；
//  2. 未知 email 也做一次 bcrypt 假比对，抹平“存在/不存在”的时间差；
//  3. 密码正确但与 status != active：返回 ErrUserDisabled（密码先于状态校验，
//     否则不知道密码的人也能探测被禁用账号）。
//
// 失败与成功都会消耗节流令牌：突发的 5 次/分钟包含成功登录，符合 ADR §5。
func (s *Service) Login(ctx context.Context, email, password, clientKey string) (User, IssuedSession, error) {
	normalized := normalizeEmail(email)
	key := normalized
	if client := strings.TrimSpace(clientKey); client != "" {
		key = normalized + "|" + client
	}
	if !s.throttle.allow(key) {
		return User{}, IssuedSession{}, ErrRateLimited
	}
	if len(password) > maxPasswordBytes {
		// bcrypt 会静默截断超长输入，拒绝而不是让前缀密码通过。
		return User{}, IssuedSession{}, ErrInvalidCredentials
	}

	var (
		user User
		hash string
	)
	err := s.db.QueryRowContext(ctx, queryFindUserCredentials,
		normalized).Scan(&user.ID, &user.Email, &user.Status, &user.PlatformAdmin,
		&user.AuthVersion, &user.CreatedAt, &user.UpdatedAt, &hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.compareDummyPassword(password)
		return User{}, IssuedSession{}, ErrInvalidCredentials
	case err != nil:
		return User{}, IssuedSession{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return User{}, IssuedSession{}, ErrInvalidCredentials
	}
	if user.Status != statusActive {
		return User{}, IssuedSession{}, ErrUserDisabled
	}
	issued, err := s.issueSession(ctx, user)
	if err != nil {
		return User{}, IssuedSession{}, err
	}
	return user, issued, nil
}

// issueSession 生成明文 token + csrf token，库里只写 SHA-256 摘要。
func (s *Service) issueSession(ctx context.Context, user User) (IssuedSession, error) {
	token, err := newToken()
	if err != nil {
		return IssuedSession{}, err
	}
	csrfToken, err := newToken()
	if err != nil {
		return IssuedSession{}, err
	}
	now := s.now()
	session := Session{
		UserID:      user.ID,
		TokenHash:   hashToken(token),
		ExpiresAt:   now.Add(s.opts.SessionTTL),
		AuthVersion: user.AuthVersion,
		CreatedAt:   now,
	}
	// last_seen_at 与 created_at 同为签发时刻，SQL 里共用 $6。
	err = s.db.QueryRowContext(ctx, queryInsertSession,
		session.TokenHash, session.UserID, hashToken(csrfToken), session.ExpiresAt,
		session.AuthVersion, session.CreatedAt).Scan(&session.ID)
	if err != nil {
		return IssuedSession{}, err
	}
	return IssuedSession{Session: session, Token: token, CSRFToken: csrfToken}, nil
}

// ResolveSession 用明文 token 解析会话。token 篡改/未知、已撤销、已过期、
// auth_version 不匹配、用户非 active 一律返回 ErrSessionInvalid，不区分原因。
// 解析成功会顺手刷新 last_seen_at（同一语句内完成）。
func (s *Service) ResolveSession(ctx context.Context, rawToken string) (User, Session, error) {
	if rawToken == "" {
		return User{}, Session{}, ErrSessionInvalid
	}
	var (
		session   Session
		revokedAt sql.NullTime
		user      User
	)
	now := s.now()
	err := s.db.QueryRowContext(ctx, queryResolveSession, hashToken(rawToken), now).Scan(
		&session.ID, &session.UserID, &session.TokenHash, &session.ExpiresAt, &revokedAt,
		&session.AuthVersion, &session.CreatedAt,
		&user.ID, &user.Email, &user.Status, &user.PlatformAdmin, &user.AuthVersion,
		&user.CreatedAt, &user.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Session{}, ErrSessionInvalid
	}
	if err != nil {
		return User{}, Session{}, err
	}
	if revokedAt.Valid {
		t := revokedAt.Time
		session.RevokedAt = &t
	}
	return user, session, nil
}

// RevokeSession 撤销一个会话（登出）。未知或已撤销的 token 返回
// ErrSessionInvalid：撤销是服务端事实，重复撤销不算成功。
func (s *Service) RevokeSession(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return ErrSessionInvalid
	}
	res, err := s.db.ExecContext(ctx, queryRevokeSession, hashToken(rawToken), s.now())
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrSessionInvalid
	}
	return nil
}

// VerifyCSRF 校验会话绑定的 CSRF token：比较的是 CSRF token 的 SHA-256 摘要，
// 使用常量时间比较。会话已撤销/过期或 token 不匹配都返回 ErrCSRF。
func (s *Service) VerifyCSRF(ctx context.Context, session Session, csrfToken string) error {
	if session.ID == 0 || csrfToken == "" {
		return ErrCSRF
	}
	var stored sql.NullString
	err := s.db.QueryRowContext(ctx, querySessionCSRFHash, session.ID, s.now()).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCSRF
	}
	if err != nil {
		return err
	}
	if !stored.Valid || stored.String == "" {
		return ErrCSRF
	}
	if !constantTimeEqual(hashToken(csrfToken), stored.String) {
		return ErrCSRF
	}
	return nil
}

// CountActiveSessions 统计未撤销且未过期的会话数（测试与运维观察用）。
func (s *Service) CountActiveSessions(ctx context.Context, userID int64) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, queryCountActiveSessions, userID, s.now()).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// compareDummyPassword 在未知 email 时跑一次同等成本的 bcrypt 比对，
// 让“用户不存在”和“密码错误”的耗时接近。哈希在首次使用时按配置 cost 生成。
func (s *Service) compareDummyPassword(password string) {
	s.dummyOnce.Do(func() {
		const seed = "iam: dummy password for constant-time login"
		hash, err := bcrypt.GenerateFromPassword([]byte(seed), s.opts.BcryptCost)
		if err != nil {
			hash, err = bcrypt.GenerateFromPassword([]byte(seed), bcrypt.MinCost)
		}
		if err == nil {
			s.dummyHash = hash
		}
	})
	if len(s.dummyHash) == 0 {
		return
	}
	_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
}

// isUniqueViolation 判定 PostgreSQL 唯一约束/唯一索引冲突（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

//go:embed queries/insert_user.sql
var queryInsertUser string

//go:embed queries/get_user.sql
var queryGetUser string

//go:embed queries/find_user_by_email.sql
var queryFindUserByEmail string

//go:embed queries/find_user_credentials.sql
var queryFindUserCredentials string

//go:embed queries/set_user_status.sql
var querySetUserStatus string

//go:embed queries/insert_session.sql
var queryInsertSession string

//go:embed queries/resolve_session.sql
var queryResolveSession string

//go:embed queries/revoke_session.sql
var queryRevokeSession string

//go:embed queries/session_csrf_hash.sql
var querySessionCSRFHash string

//go:embed queries/count_active_sessions.sql
var queryCountActiveSessions string

const queryRegisterPersonalOrganization = `
INSERT INTO organizations (type, name, status, owner_user_id, created_at, updated_at)
SELECT 'personal', left(split_part(u.email, '@', 1), 80), 'active', u.id, $2, $2
FROM users u WHERE u.id = $1
RETURNING id`

const queryRegisterOwnerMembership = `
INSERT INTO memberships (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
VALUES ($1, $2, 'owner', 'active', NULL, $3, $3)`

const queryRegisterBillingWallet = `
INSERT INTO billing_accounts
	(org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
VALUES ($1, 'CNY', 0, 0, 'active', 1, $2, $2)`

//go:embed queries/lock_user_password.sql
var queryLockUserPassword string

//go:embed queries/revoke_user_sessions.sql
var queryRevokeUserSessions string

//go:embed queries/change_password.sql
var queryChangePassword string
