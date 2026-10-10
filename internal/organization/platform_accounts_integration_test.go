package organization_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"llm-gateway/internal/organization"
)

func TestPlatformOrganizationListAndStatusPreserveMemberships(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	firstOwner := createUser(t, db, "platform-list-first@example.com")
	secondOwner := createUser(t, db, "platform-list-second@example.com")
	first, err := svc.CreateEnterprise(ctx, firstOwner.ID, "First account")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateEnterprise(ctx, secondOwner.ID, "Second account")
	if err != nil {
		t.Fatal(err)
	}
	personal, err := svc.EnsurePersonalOrganization(ctx, firstOwner.ID, "First personal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE organizations SET model_allowlist = '[]'::jsonb WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}

	page, next, err := svc.ListPlatformOrganizations(ctx, "enterprise", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != first.ID || page[0].OwnerUserID != firstOwner.ID || page[0].OwnerEmail != firstOwner.Email {
		t.Fatalf("first platform page = %+v, want first enterprise and its owner", page)
	}
	if page[0].ModelAllowlist == nil || len(*page[0].ModelAllowlist) != 0 || next == "" {
		t.Fatalf("explicit deny-all scope or next cursor lost: item=%+v next=%q", page[0], next)
	}

	cursor, err := strconv.ParseInt(next, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	page, next, err = svc.ListPlatformOrganizations(ctx, "enterprise", 1, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != second.ID || page[0].OwnerEmail != secondOwner.Email || page[0].ModelAllowlist != nil || next != "" {
		t.Fatalf("second platform page = %+v next=%q, want inherited null scope", page, next)
	}

	allTypes, _, err := svc.ListPlatformOrganizations(ctx, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawPersonal bool
	for _, item := range allTypes {
		if item.ID == personal.ID && item.Type == "personal" {
			sawPersonal = true
		}
	}
	if !sawPersonal {
		t.Fatalf("unfiltered platform account list omitted personal account %d: %+v", personal.ID, allTypes)
	}

	updated, err := svc.SetOrganizationStatus(ctx, first.ID, "suspended")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != first.ID || updated.Status != "suspended" || updated.OwnerUserID != firstOwner.ID || updated.OwnerEmail != firstOwner.Email || updated.ModelAllowlist == nil || len(*updated.ModelAllowlist) != 0 {
		t.Fatalf("status update = %+v", updated)
	}
	membership, err := svc.Membership(ctx, firstOwner.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if membership.Role != "owner" || membership.Status != "active" {
		t.Fatalf("organization suspension altered owner membership: %+v", membership)
	}
	visible, err := svc.GetForUser(ctx, firstOwner.ID, first.ID)
	if err != nil || visible.Organization.Status != "suspended" {
		t.Fatalf("owner cannot still see suspended account: item=%+v err=%v", visible, err)
	}

	// Repeating the current status is idempotent, while resume restores account
	// admission without recreating or changing the owner membership.
	if _, err := svc.SetOrganizationStatus(ctx, first.ID, "suspended"); err != nil {
		t.Fatal(err)
	}
	updated, err = svc.SetOrganizationStatus(ctx, first.ID, "active")
	if err != nil || updated.Status != "active" {
		t.Fatalf("resume status = %+v err=%v", updated, err)
	}
	membershipAfterResume, err := svc.Membership(ctx, firstOwner.ID, first.ID)
	if err != nil || membershipAfterResume.ID != membership.ID || membershipAfterResume.Status != membership.Status {
		t.Fatalf("resume changed owner membership: before=%+v after=%+v err=%v", membership, membershipAfterResume, err)
	}

	if _, err := svc.SetOrganizationStatus(ctx, first.ID, "deleted"); !errors.Is(err, organization.ErrOrganizationStatusInvalid) {
		t.Fatalf("invalid status error = %v", err)
	}
	if _, err := svc.SetOrganizationStatus(ctx, 999999999, "active"); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("unknown account error = %v", err)
	}
}
