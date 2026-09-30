package iam

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		" Alice@Example.COM ": "alice@example.com",
		"BOB@EXAMPLE.COM":     "bob@example.com",
		"carol@example.com":   "carol@example.com",
		"\tdave@example.com ": "dave@example.com",
	}
	for in, want := range cases {
		if got := normalizeEmail(in); got != want {
			t.Errorf("normalizeEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateEmail(t *testing.T) {
	valid := []string{"a@b.co", "alice@example.com", "a.b+c@sub.example.com"}
	for _, email := range valid {
		if err := validateEmail(email); err != nil {
			t.Errorf("validateEmail(%q) = %v, want nil", email, err)
		}
	}
	invalid := []string{"", "no-at", "@example.com", "alice@", "a@@b.com", "a b@example.com", "a@b\n.com"}
	for _, email := range invalid {
		if err := validateEmail(email); err == nil {
			t.Errorf("validateEmail(%q) = nil, want error", email)
		}
	}
	tooLong := strings.Repeat("a", maxEmailBytes) + "@example.com"
	if err := validateEmail(tooLong); !errors.Is(err, errEmailInvalid) {
		t.Errorf("long email = %v, want errEmailInvalid", err)
	}
}

func TestValidatePassword(t *testing.T) {
	if err := validatePassword(""); !errors.Is(err, errPasswordRequired) {
		t.Errorf("empty password = %v, want errPasswordRequired", err)
	}
	if err := validatePassword("short"); err != nil {
		t.Errorf("valid password = %v, want nil", err)
	}
	if err := validatePassword(strings.Repeat("x", maxPasswordBytes)); err != nil {
		t.Errorf("72-byte password = %v, want nil", err)
	}
	if err := validatePassword(strings.Repeat("x", maxPasswordBytes+1)); !errors.Is(err, errPasswordTooLong) {
		t.Errorf("73-byte password = %v, want errPasswordTooLong", err)
	}
}

func TestValidateStatus(t *testing.T) {
	for _, status := range []string{statusActive, statusDisabled} {
		if err := validateStatus(status); err != nil {
			t.Errorf("validateStatus(%q) = %v, want nil", status, err)
		}
	}
	if err := validateStatus("suspended"); err == nil {
		t.Error("validateStatus(suspended) = nil, want error")
	}
}

func TestNormalizeOptionsDefaults(t *testing.T) {
	svc := NewService(nil, Options{})
	if svc.opts.SessionTTL != defaultSessionTTL {
		t.Errorf("SessionTTL = %v, want %v", svc.opts.SessionTTL, defaultSessionTTL)
	}
	if svc.opts.BcryptCost != defaultBcryptCost {
		t.Errorf("BcryptCost = %d, want %d", svc.opts.BcryptCost, defaultBcryptCost)
	}
	if svc.opts.LoginRatePerMinute != defaultLoginRatePerMinute {
		t.Errorf("LoginRatePerMinute = %v, want %v", svc.opts.LoginRatePerMinute, defaultLoginRatePerMinute)
	}
	if svc.opts.LoginBurst != defaultLoginBurst {
		t.Errorf("LoginBurst = %d, want %d", svc.opts.LoginBurst, defaultLoginBurst)
	}
	if svc.opts.Now == nil {
		t.Error("Now = nil, want time.Now fallback")
	}
	if defaultBcryptCost != 12 {
		t.Errorf("bcrypt default cost = %d, want 12 (ADR-0002 §2.5)", defaultBcryptCost)
	}
}

func TestNormalizeOptionsOutOfRangeBcryptCost(t *testing.T) {
	for _, cost := range []int{-1, 0, bcrypt.MinCost - 1, bcrypt.MaxCost + 1} {
		svc := NewService(nil, Options{BcryptCost: cost})
		if svc.opts.BcryptCost != defaultBcryptCost {
			t.Errorf("BcryptCost %d normalized to %d, want %d", cost, svc.opts.BcryptCost, defaultBcryptCost)
		}
	}
	// 合法范围内的非默认 cost 必须原样保留，测试才能用 MinCost 加速。
	svc := NewService(nil, Options{BcryptCost: bcrypt.MinCost})
	if svc.opts.BcryptCost != bcrypt.MinCost {
		t.Errorf("BcryptCost = %d, want %d", svc.opts.BcryptCost, bcrypt.MinCost)
	}
}

func TestNewTokenIs32RandomBytes(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		token, err := newToken()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatalf("token %q is not base64url: %v", token, err)
		}
		if len(raw) != tokenBytes {
			t.Fatalf("token decoded to %d bytes, want %d", len(raw), tokenBytes)
		}
		if seen[token] {
			t.Fatalf("duplicate token generated: %q", token)
		}
		seen[token] = true
	}
}

func TestHashTokenMatchesSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("abc"))
	want := hex.EncodeToString(sum[:])
	if got := hashToken("abc"); got != want {
		t.Fatalf("hashToken(abc) = %q, want %q", got, want)
	}
	// 已知向量，防止实现被换成非 SHA-256。
	if got := hashToken("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hashToken(abc) = %q, want the SHA-256 test vector", got)
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("deadbeef", "deadbeef") {
		t.Error("equal digests reported unequal")
	}
	if constantTimeEqual("deadbeef", "deadbeee") {
		t.Error("different digests reported equal")
	}
	if constantTimeEqual("deadbeef", "deadbee") {
		t.Error("different length digests reported equal")
	}
}

// scanUser 的扫描顺序必须与 queries/*.sql 的 SELECT 列序一致：
// id, email, status, platform_admin, auth_version, created_at, updated_at。
func TestScanUserColumnOrder(t *testing.T) {
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	values := []any{int64(7), "alice@example.com", statusActive, true, int64(3), created, updated}
	user, err := scanUser(func(dest ...any) error {
		if len(dest) != len(values) {
			t.Fatalf("scanUser asked for %d columns, want %d", len(dest), len(values))
		}
		for i, value := range values {
			switch d := dest[i].(type) {
			case *int64:
				*d = value.(int64)
			case *string:
				*d = value.(string)
			case *bool:
				*d = value.(bool)
			case *time.Time:
				*d = value.(time.Time)
			default:
				t.Fatalf("unexpected scan destination type %T", dest[i])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != 7 || user.Email != "alice@example.com" || user.Status != statusActive ||
		!user.PlatformAdmin || user.AuthVersion != 3 ||
		!user.CreatedAt.Equal(created) || !user.UpdatedAt.Equal(updated) {
		t.Fatalf("unexpected user: %+v", user)
	}
}
