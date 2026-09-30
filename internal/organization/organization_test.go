package organization

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeLimit(t *testing.T) {
	cases := map[int]int{
		-10:  defaultListLimit,
		0:    defaultListLimit,
		1:    1,
		20:   20,
		100:  maxListLimit,
		101:  maxListLimit,
		1000: maxListLimit,
	}
	for in, want := range cases {
		if got := normalizeLimit(in); got != want {
			t.Errorf("normalizeLimit(%d) = %d, want %d", in, got, want)
		}
	}
	if defaultListLimit != 20 || maxListLimit != 100 {
		t.Fatalf("list limits changed: default=%d max=%d, ADR-0002 §4 要求 20/100", defaultListLimit, maxListLimit)
	}
}

func TestNormalizeOrgName(t *testing.T) {
	if name, err := normalizeOrgName("  Acme  "); err != nil || name != "Acme" {
		t.Errorf("normalizeOrgName trimmed = (%q, %v), want (Acme, nil)", name, err)
	}
	for _, invalid := range []string{"", "   ", "\t\n"} {
		if _, err := normalizeOrgName(invalid); !errors.Is(err, ErrNameInvalid) {
			t.Errorf("normalizeOrgName(%q) = %v, want ErrNameInvalid", invalid, err)
		}
	}
	// 80 个多字节字符是合法的 80 个字符，不是 240 字节。
	if _, err := normalizeOrgName(strings.Repeat("组", maxNameRunes)); err != nil {
		t.Errorf("80-rune name rejected: %v", err)
	}
	if _, err := normalizeOrgName(strings.Repeat("组", maxNameRunes+1)); !errors.Is(err, ErrNameInvalid) {
		t.Errorf("81-rune name = %v, want ErrNameInvalid", err)
	}
}

func TestPersonalOrgNameAllowsBlank(t *testing.T) {
	if name, err := personalOrgName("   "); err != nil || name != "" {
		t.Errorf("personalOrgName(blank) = (%q, %v), want (\"\", nil): 空名由 SQL 用 email 兜底", name, err)
	}
	if name, err := personalOrgName(" Alice "); err != nil || name != "Alice" {
		t.Errorf("personalOrgName = (%q, %v), want (Alice, nil)", name, err)
	}
	if _, err := personalOrgName(strings.Repeat("x", maxNameRunes+1)); !errors.Is(err, ErrNameInvalid) {
		t.Errorf("oversized personal name = %v, want ErrNameInvalid", err)
	}
}

func TestValidRole(t *testing.T) {
	for _, role := range []string{roleOwner, roleAdmin, roleMember, roleBillingViewer} {
		if !validRole(role) {
			t.Errorf("validRole(%q) = false, want true", role)
		}
	}
	for _, role := range []string{"", "superuser", "OWNER", "owner "} {
		if validRole(role) {
			t.Errorf("validRole(%q) = true, want false", role)
		}
	}
}

func TestScanOrganizationColumnOrder(t *testing.T) {
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	org, err := scanOrganization(func(dest ...any) error {
		if len(dest) != 6 {
			t.Fatalf("scanOrganization asked for %d columns, want 6", len(dest))
		}
		*(dest[0].(*int64)) = 11
		*(dest[1].(*string)) = typeEnterprise
		*(dest[2].(*string)) = "Acme"
		*(dest[3].(*string)) = statusActive
		*(dest[4].(*sql.NullString)) = sql.NullString{}
		*(dest[5].(*time.Time)) = created
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if org.ID != 11 || org.Type != typeEnterprise || org.Name != "Acme" ||
		org.Status != statusActive || org.LegacyTenantID != nil || !org.CreatedAt.Equal(created) {
		t.Fatalf("unexpected organization: %+v", org)
	}
}

func TestScanOrganizationLegacyTenant(t *testing.T) {
	org, err := scanOrganization(func(dest ...any) error {
		*(dest[0].(*int64)) = 12
		*(dest[1].(*string)) = typePersonal
		*(dest[2].(*string)) = "Alice"
		*(dest[3].(*string)) = statusActive
		*(dest[4].(*sql.NullString)) = sql.NullString{String: "tenant-a", Valid: true}
		*(dest[5].(*time.Time)) = time.Now()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if org.LegacyTenantID == nil || *org.LegacyTenantID != "tenant-a" {
		t.Fatalf("LegacyTenantID = %v, want tenant-a", org.LegacyTenantID)
	}
}

func TestScanMemberOrgColumnOrder(t *testing.T) {
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	item, err := scanMemberOrg(func(dest ...any) error {
		if len(dest) != 7 {
			t.Fatalf("scanMemberOrg asked for %d columns, want 7", len(dest))
		}
		*(dest[0].(*int64)) = 3
		*(dest[1].(*string)) = typeEnterprise
		*(dest[2].(*string)) = "Acme"
		*(dest[3].(*string)) = statusActive
		*(dest[4].(*sql.NullString)) = sql.NullString{}
		*(dest[5].(*time.Time)) = created
		*(dest[6].(*string)) = roleAdmin
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.Organization.ID != 3 || item.Organization.Name != "Acme" || item.Role != roleAdmin {
		t.Fatalf("unexpected member org: %+v", item)
	}
}
