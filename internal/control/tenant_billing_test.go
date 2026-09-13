package control

import (
	"context"
	"errors"
	"testing"

	"llm-gateway/internal/auth"
)

func TestTenantMemberKeyOwnership(t *testing.T) {
	ctx := context.Background()
	directory := NewMemoryDirectory()
	owner, personal, err := directory.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member, memberPersonal, err := directory.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.AddMember(ctx, personal, member, MembershipMember); err == nil {
		t.Fatal("personal tenant accepted another member")
	}
	enterprise, err := directory.CreateEnterprise(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.AddMember(ctx, enterprise, member, MembershipMember); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCredentialService(srv)
	if err != nil {
		t.Fatal(err)
	}
	service.SetDirectory(directory)
	admin := Principal{Subject: "admin", Role: RoleAdmin}
	memberPrincipal := Principal{Subject: "member", Role: RoleTenantUser, TenantID: enterprise, UserID: member}
	ownerPrincipal := Principal{Subject: "owner", Role: RoleTenantUser, TenantID: enterprise, UserID: owner}
	memberKey, err := service.CreateAs(ctx, memberPrincipal, enterprise)
	if err != nil {
		t.Fatal(err)
	}
	forcedSelfKey, err := service.CreateForUserAs(ctx, memberPrincipal, enterprise, owner)
	if err != nil || forcedSelfKey.Metadata.UserID != member {
		t.Fatalf("member changed key owner: %+v %v", forcedSelfKey.Metadata, err)
	}
	ownerKey, err := service.CreateAs(ctx, ownerPrincipal, enterprise)
	if err != nil {
		t.Fatal(err)
	}
	personalKey, err := service.CreateForUserAs(ctx, admin, memberPersonal, member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateForUserAs(ctx, admin, enterprise, 99999); !errors.Is(err, ErrTenantForbidden) {
		t.Fatalf("nonmember key create: %v", err)
	}
	memberKeys, err := service.ListAs(ctx, memberPrincipal, enterprise)
	if err != nil || len(memberKeys) != 2 || memberKeys[0].KeyID != memberKey.Metadata.KeyID {
		t.Fatalf("member list: %+v %v", memberKeys, err)
	}
	if err := service.RevokeAs(ctx, memberPrincipal, ownerKey.Metadata.KeyID, enterprise); !errors.Is(err, ErrTenantForbidden) {
		t.Fatalf("cross-member revoke: %v", err)
	}
	if err := service.RevokeAs(ctx, memberPrincipal, personalKey.Metadata.KeyID, enterprise); !errors.Is(err, ErrTenantForbidden) {
		t.Fatalf("cross-tenant revoke: %v", err)
	}
	ownerKeys, err := service.ListAs(ctx, ownerPrincipal, enterprise)
	if err != nil || len(ownerKeys) != 3 {
		t.Fatalf("owner list: %+v %v", ownerKeys, err)
	}
	identityStore := auth.NewMemoryStoreFromRecords(KeyRecordsToAuth(srv.CurrentConfig().KeyRecords))
	identity, ok := identityStore.Resolve(memberKey.RawKey)
	if !ok || identity.TenantID != enterprise || identity.UserID != member || identity.KeyID != memberKey.Metadata.KeyID {
		t.Fatalf("wrong execution identity: %+v %v", identity, ok)
	}
	identity, ok = identityStore.Resolve(personalKey.RawKey)
	if !ok || identity.TenantID != memberPersonal || identity.UserID != member {
		t.Fatalf("wrong personal identity: %+v %v", identity, ok)
	}
	directory.mu.Lock()
	delete(directory.members[enterprise], member)
	directory.mu.Unlock()
	if _, err := service.ListAs(ctx, memberPrincipal, enterprise); !errors.Is(err, ErrTenantForbidden) {
		t.Fatalf("removed member retained key access: %v", err)
	}
	if _, err := service.CreateAs(ctx, memberPrincipal, enterprise); !errors.Is(err, ErrTenantForbidden) {
		t.Fatalf("removed member created key: %v", err)
	}
}
