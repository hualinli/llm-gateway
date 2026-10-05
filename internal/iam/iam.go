// Package iam owns users, password hashing, browser sessions and CSRF
// verification (ADR-0002 §2.3–§2.5, §3). It is framework-free: standard
// library, database/sql and bcrypt only. The HTTP layer maps the exported
// sentinel errors to status codes; nothing here knows about Hertz, cookies or
// request contexts.
//
// 不变量：
//   - email 一律 trim + 小写后存储与查询，users.email 唯一；
//   - 明文密码永不落库、永不进日志，库里只有 bcrypt 哈希；
//   - 会话与 CSRF token 是 32 字节随机值，库里只存 SHA-256 十六进制摘要。
//
// 会话有效的判定：未撤销 + 未过期 + sessions.auth_version == users.auth_version
// 且 users.status = 'active'；用户禁用会递增 auth_version 使旧会话立即失效。
package iam

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// User 是 users 表的对外视图，不含 password_hash。
type User struct {
	ID            int64
	Email         string
	Status        string
	PlatformAdmin bool
	AuthVersion   int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Session 是 sessions 表的对外视图。TokenHash 是明文 token 的 SHA-256
// 十六进制摘要，明文只在 IssuedSession 里返回一次。
type Session struct {
	ID          int64
	UserID      int64
	TokenHash   string
	ExpiresAt   time.Time
	RevokedAt   *time.Time
	AuthVersion int64
	CreatedAt   time.Time
}

// IssuedSession 只在 Login 返回值里携带明文 token / csrf token，二者仅此一次
// 出现；服务端存储的只有摘要。
type IssuedSession struct {
	Session   Session
	Token     string
	CSRFToken string
}

var (
	ErrInvalidCredentials = errors.New("iam: invalid credentials")
	ErrUserDisabled       = errors.New("iam: user disabled")
	ErrEmailTaken         = errors.New("iam: email already registered")
	ErrSessionInvalid     = errors.New("iam: session invalid or expired")
	ErrCSRF               = errors.New("iam: csrf token mismatch")
	ErrRateLimited        = errors.New("iam: too many login attempts")
	ErrNotFound           = errors.New("iam: not found")
)

// 服务层输入校验错误。它们没有哨兵值：HTTP 边界必须先做 400 级别的输入校验，
// 这里的错误只用于防御性拒绝（例如直接调用服务层时）。
var (
	errEmailRequired    = errors.New("iam: email is required")
	errEmailInvalid     = errors.New("iam: email is invalid")
	errPasswordRequired = errors.New("iam: password is required")
	errPasswordTooLong  = errors.New("iam: password exceeds 72 bytes")
	errStatusInvalid    = errors.New("iam: unknown user status")
)

const (
	// statusActive / statusDisabled 是 users.status 的受限取值。
	statusActive   = "active"
	statusDisabled = "disabled"

	tokenBytes = 32

	defaultSessionTTL         = 12 * time.Hour
	defaultBcryptCost         = 12
	defaultLoginRatePerMinute = 5
	defaultLoginBurst         = 5

	// maxPasswordBytes 是 bcrypt 的输入上限；超过会被静默截断，必须拒绝。
	maxPasswordBytes = 72
	maxEmailBytes    = 254
)

// Options 覆盖服务默认值。零值即生产默认：12h 会话、bcrypt cost 12、
// 每 email+IP 每分钟 5 次、突发 5 次。Now 只用于测试注入。
type Options struct {
	SessionTTL         time.Duration
	BcryptCost         int
	LoginRatePerMinute float64
	LoginBurst         int
	Now                func() time.Time
}

// Service 是 IAM 的唯一入口。它不持有连接池所有权（db 由调用方管理），
// 只保存一个进程内登录节流器。
type Service struct {
	db       *sql.DB
	opts     Options
	throttle *loginThrottle

	dummyOnce sync.Once
	dummyHash []byte
}

// NewService 归一化 Options 后返回服务。db 为 nil 时只有不访问数据库的方法
// 可用（校验/节流单测）。
func NewService(db *sql.DB, opts Options) *Service {
	opts = normalizeOptions(opts)
	return &Service{
		db:       db,
		opts:     opts,
		throttle: newLoginThrottle(opts.LoginRatePerMinute, float64(opts.LoginBurst), opts.Now),
	}
}

func normalizeOptions(opts Options) Options {
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = defaultSessionTTL
	}
	if opts.BcryptCost < bcrypt.MinCost || opts.BcryptCost > bcrypt.MaxCost {
		opts.BcryptCost = defaultBcryptCost
	}
	if opts.LoginRatePerMinute <= 0 {
		opts.LoginRatePerMinute = defaultLoginRatePerMinute
	}
	if opts.LoginBurst <= 0 {
		opts.LoginBurst = defaultLoginBurst
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return opts
}

func (s *Service) now() time.Time { return s.opts.Now() }

// normalizeEmail 是 email 唯一的规范化入口：trim + 小写。
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validateEmail 只做形状校验：非空、单 @、无空白、不超过 254 字节。
// 不做 RFC 5322 全解析，也不发送邮件（ADR-0002 明确无邮件验证）。
func validateEmail(email string) error {
	if email == "" {
		return errEmailRequired
	}
	if len(email) > maxEmailBytes {
		return errEmailInvalid
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.IndexByte(email[at+1:], '@') >= 0 {
		return errEmailInvalid
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return errEmailInvalid
	}
	return nil
}

func validatePassword(password string) error {
	if password == "" {
		return errPasswordRequired
	}
	if len(password) > maxPasswordBytes {
		return errPasswordTooLong
	}
	return nil
}

func validateStatus(status string) error {
	if status != statusActive && status != statusDisabled {
		return fmt.Errorf("%w: %q", errStatusInvalid, status)
	}
	return nil
}

// scanUser 统一 users 视图的列顺序，供各处 QueryRowContext().Scan 复用。
func scanUser(scan func(dest ...any) error) (User, error) {
	var u User
	err := scan(&u.ID, &u.Email, &u.Status, &u.PlatformAdmin, &u.AuthVersion, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}
