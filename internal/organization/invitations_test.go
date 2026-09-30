package organization

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeInvitationEmail(t *testing.T) {
	cases := map[string]string{
		"  Alice@Example.COM ": "alice@example.com",
		"bob@example.com":      "bob@example.com",
		"":                     "",
	}
	for in, want := range cases {
		if got := normalizeInvitationEmail(in); got != want {
			t.Errorf("normalizeInvitationEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateInvitationEmail(t *testing.T) {
	valid := []string{"a@b.c", "alice@example.com", strings.Repeat("a", 200) + "@example.com"}
	for _, email := range valid {
		if err := validateInvitationEmail(email); err != nil {
			t.Errorf("validateInvitationEmail(%q) = %v, want nil", email, err)
		}
	}
	invalid := []string{
		"",
		"alice",
		"alice@",
		"@example.com",
		"a@@b.c",
		"a b@example.com",
		"alice@exa mple.com",
		"alice@example.com\n",
		strings.Repeat("a", 250) + "@example.com", // > 254 bytes
	}
	for _, email := range invalid {
		if err := validateInvitationEmail(email); !errors.Is(err, ErrInvitationEmailInvalid) {
			t.Errorf("validateInvitationEmail(%q) = %v, want ErrInvitationEmailInvalid", email, err)
		}
	}
}

func TestNewInvitationTokenIsRandomAndHashedOnly(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		raw, hash, err := newInvitationToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != 43 {
			t.Fatalf("raw token length = %d, want 43 base64url characters", len(raw))
		}
		if seen[raw] {
			t.Fatal("duplicate raw token generated")
		}
		seen[raw] = true
		sum := sha256.Sum256([]byte(raw))
		if want := hex.EncodeToString(sum[:]); hash != want {
			t.Fatalf("token hash = %q, want the SHA-256 hex digest %q", hash, want)
		}
		if hash == raw || strings.Contains(hash, raw) {
			t.Fatal("stored hash must not contain the raw token")
		}
		if len(hash) != 64 {
			t.Fatalf("token hash length = %d, want 64 hex characters", len(hash))
		}
	}
}

// TestCheckInvitationUsable 覆盖接受邀请的全部拒绝分支：过期、错 email、
// 已接受（重放）、已撤销、未知状态。纯函数，不需要数据库。
func TestCheckInvitationUsable(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	base := Invitation{
		ID:        7,
		OrgID:     2,
		Email:     "invitee@example.com",
		Role:      roleMember,
		Status:    statusInvitationPending,
		ExpiresAt: now.Add(time.Hour),
	}
	cases := []struct {
		name      string
		mutate    func(*Invitation)
		userEmail string
		want      error
	}{
		{name: "pending and matching", mutate: func(i *Invitation) {}, userEmail: "invitee@example.com"},
		{name: "email comparison is case-insensitive", mutate: func(i *Invitation) {}, userEmail: "  INVITEE@Example.com "},
		{name: "wrong email", mutate: func(i *Invitation) {}, userEmail: "someone@example.com", want: ErrInvitationEmailMismatch},
		{name: "empty email", mutate: func(i *Invitation) {}, userEmail: "", want: ErrInvitationEmailMismatch},
		{name: "expired", mutate: func(i *Invitation) { i.ExpiresAt = now.Add(-time.Second) }, userEmail: "invitee@example.com", want: ErrInvitationExpired},
		{name: "expires exactly now is expired", mutate: func(i *Invitation) { i.ExpiresAt = now }, userEmail: "invitee@example.com", want: ErrInvitationExpired},
		{name: "already accepted", mutate: func(i *Invitation) { i.Status = statusInvitationAccepted }, userEmail: "invitee@example.com", want: ErrInvitationUsed},
		{name: "revoked", mutate: func(i *Invitation) { i.Status = statusInvitationRevoked }, userEmail: "invitee@example.com", want: ErrInvitationRevoked},
		{name: "unknown status", mutate: func(i *Invitation) { i.Status = "expired" }, userEmail: "invitee@example.com", want: ErrInvitationNotFound},
		// 顺序：状态先于过期/email，重放不能被“换个 email”绕过。
		{name: "accepted wins over wrong email", mutate: func(i *Invitation) { i.Status = statusInvitationAccepted }, userEmail: "other@example.com", want: ErrInvitationUsed},
		{name: "expired wins over wrong email", mutate: func(i *Invitation) { i.ExpiresAt = now.Add(-time.Hour) }, userEmail: "other@example.com", want: ErrInvitationExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invitation := base
			tc.mutate(&invitation)
			if err := checkInvitationUsable(invitation, tc.userEmail, now); !errors.Is(err, tc.want) {
				t.Fatalf("checkInvitationUsable = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestCreateInvitationValidatesBeforeTouchingTheDatabase：nil 数据库也会在输入
// 校验阶段就返回类型化错误（证明校验先于任何 SQL，HTTP 层可以安全地映射 400）。
func TestCreateInvitationValidatesBeforeTouchingTheDatabase(t *testing.T) {
	svc := NewService(nil)
	ctx := t.Context()
	cases := []struct {
		name  string
		email string
		role  string
		ttl   time.Duration
		want  error
	}{
		{name: "invalid email", email: "not-an-email", role: roleMember, want: ErrInvitationEmailInvalid},
		{name: "empty email", email: "", role: roleMember, want: ErrInvitationEmailInvalid},
		{name: "platform role", email: "a@b.c", role: RolePlatformAdmin, want: ErrPlatformRole},
		{name: "invalid role", email: "a@b.c", role: "superuser", want: ErrRoleInvalid},
		{name: "empty role", email: "a@b.c", role: "", want: ErrRoleInvalid},
		{name: "negative ttl", email: "a@b.c", role: roleMember, ttl: -time.Minute, want: nil}, // 见下方单独断言
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.CreateInvitation(ctx, 1, 1, tc.email, tc.role, tc.ttl)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("CreateInvitation = %v, want %v", err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatal("CreateInvitation = nil, want a validation error before any SQL")
			}
			if errors.Is(err, ErrInvitationEmailInvalid) || errors.Is(err, ErrRoleInvalid) || errors.Is(err, ErrPlatformRole) {
				t.Fatalf("CreateInvitation = %v, want the ttl validation error", err)
			}
		})
	}
}

// TestAcceptInvitationRejectsBlankTokenWithoutDatabase：空 token 是“邀请不存在”，
// 不能因为没有数据库或 token 形状不对而 panic 或返回 500。
func TestAcceptInvitationRejectsBlankTokenWithoutDatabase(t *testing.T) {
	svc := NewService(nil)
	for _, token := range []string{"", "   ", "\t\n"} {
		if _, err := svc.AcceptInvitation(t.Context(), token, 1, "a@b.c"); !errors.Is(err, ErrInvitationNotFound) {
			t.Fatalf("AcceptInvitation(%q) = %v, want ErrInvitationNotFound", token, err)
		}
	}
}

// TestCheckInvitationGrantKeepsEscalationClosed：邀请路径与成员角色变更共用授予
// 规则，唯一区别是 owner 可以邀请 owner。
func TestCheckInvitationGrantKeepsEscalationClosed(t *testing.T) {
	cases := []struct {
		actor  string
		invite string
		want   error
	}{
		{roleOwner, roleOwner, nil},
		{roleAdmin, roleOwner, ErrOwnerGrant},
		{roleOwner, roleAdmin, nil},
		{roleAdmin, roleAdmin, ErrForbidden},
		{roleAdmin, roleMember, nil},
		{roleOwner, RolePlatformAdmin, ErrPlatformRole},
		{roleAdmin, RolePlatformAdmin, ErrPlatformRole},
		{roleMember, roleMember, ErrForbidden},
	}
	for _, tc := range cases {
		if err := checkInvitationGrant(tc.actor, tc.invite); !errors.Is(err, tc.want) {
			t.Errorf("checkInvitationGrant(%q, %q) = %v, want %v", tc.actor, tc.invite, err, tc.want)
		}
	}
}
