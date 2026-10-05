package organization_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"llm-gateway/internal/organization"
)

func TestInvitationCannotChangeActiveMembership(t *testing.T) {
	for _, targetRole := range []string{"owner", "admin", "member", "billing_viewer"} {
		t.Run(targetRole, func(t *testing.T) {
			db, _ := isolatedDSN(t)
			svc := organization.NewService(db)
			ctx := context.Background()
			owner := createUser(t, db, "owner@example.com")
			admin := createUser(t, db, "admin@example.com")
			target := owner
			ids, roles := []int64{owner.ID, admin.ID}, []string{"owner", "admin"}
			if targetRole != "owner" {
				target = createUser(t, db, "target@example.com")
				ids, roles = append(ids, target.ID), append(roles, targetRole)
			}
			org := seedEnterprise(t, db, svc, "Invitation safety", ids, roles)
			invite, token, err := svc.CreateInvitation(ctx, admin.ID, org.ID, target.Email, "member", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			revision := orgRevisionOf(t, db, org.ID)
			if _, err := svc.AcceptInvitation(ctx, token, target.ID, target.Email); !errors.Is(err, organization.ErrInvitationConflict) {
				t.Fatalf("accept error=%v, want membership conflict", err)
			}
			role, status := membershipRole(t, db, org.ID, target.ID)
			if role != targetRole || status != "active" || activeOwnerCount(t, db, org.ID) != 1 || orgRevisionOf(t, db, org.ID) != revision {
				t.Fatal("rejected invitation changed role, owner count, status or revision")
			}
			var inviteStatus string
			if err := db.QueryRow(`SELECT status FROM invitations WHERE id=$1`, invite.ID).Scan(&inviteStatus); err != nil {
				t.Fatal(err)
			}
			if inviteStatus != "pending" {
				t.Fatalf("status=%s, want pending", inviteStatus)
			}
		})
	}
}

func TestOldInvitationCannotRestorePrivilegesOrRemovedMembership(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	owner := createUser(t, db, "owner@example.com")
	target := createUser(t, db, "target@example.com")
	org := seedEnterprise(t, db, svc, "Old invitations", []int64{owner.ID}, []string{"owner"})
	_, oldToken, err := svc.CreateInvitation(ctx, owner.ID, org.ID, target.Email, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, newToken, err := svc.CreateInvitation(ctx, owner.ID, org.ID, target.Email, "member", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvitation(ctx, newToken, target.ID, target.Email); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvitation(ctx, oldToken, target.ID, target.Email); !errors.Is(err, organization.ErrInvitationConflict) {
		t.Fatalf("old grant overwrote active member: %v", err)
	}
	if err := svc.RemoveMember(ctx, owner.ID, org.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	revision := orgRevisionOf(t, db, org.ID)
	if _, err := svc.AcceptInvitation(ctx, oldToken, target.ID, target.Email); !errors.Is(err, organization.ErrInvitationConflict) {
		t.Fatalf("pre-removal invitation restored removed member: %v", err)
	}
	role, status := membershipRole(t, db, org.ID, target.ID)
	if role != "member" || status != "removed" || orgRevisionOf(t, db, org.ID) != revision {
		t.Fatal("stale invitation changed membership")
	}
	_, freshToken, err := svc.CreateInvitation(ctx, owner.ID, org.ID, target.Email, "billing_viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	member, err := svc.AcceptInvitation(ctx, freshToken, target.ID, target.Email)
	if err != nil || member.Role != "billing_viewer" || member.Status != "active" {
		t.Fatalf("fresh rejoin=%+v err=%v", member, err)
	}
}

func TestInvitationRequiresInviterCurrentGrantPermission(t *testing.T) {
	for _, change := range []string{"demote", "remove", "lose owner grant"} {
		t.Run(change, func(t *testing.T) {
			db, _ := isolatedDSN(t)
			svc := organization.NewService(db)
			ctx := context.Background()
			owner := createUser(t, db, "owner@example.com")
			inviter := createUser(t, db, "inviter@example.com")
			target := createUser(t, db, "target@example.com")
			inviterRole, grantRole := "admin", "member"
			if change == "lose owner grant" {
				inviterRole, grantRole = "owner", "owner"
			}
			org := seedEnterprise(t, db, svc, "Inviter safety", []int64{owner.ID, inviter.ID}, []string{"owner", inviterRole})
			_, token, err := svc.CreateInvitation(ctx, inviter.ID, org.ID, target.Email, grantRole, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if change == "remove" {
				err = svc.RemoveMember(ctx, owner.ID, org.ID, inviter.ID)
			} else {
				role := "member"
				if change == "lose owner grant" {
					role = "admin"
				}
				err = svc.ChangeMemberRole(ctx, owner.ID, org.ID, inviter.ID, role)
			}
			if err != nil {
				t.Fatal(err)
			}
			revision := orgRevisionOf(t, db, org.ID)
			if _, err := svc.AcceptInvitation(ctx, token, target.ID, target.Email); !errors.Is(err, organization.ErrInvitationRevoked) {
				t.Fatalf("obsolete inviter granted membership: %v", err)
			}
			if _, err := svc.Membership(ctx, target.ID, org.ID); !errors.Is(err, organization.ErrNotFound) || orgRevisionOf(t, db, org.ID) != revision {
				t.Fatal("obsolete invitation changed organization state")
			}
		})
	}
}

func TestConcurrentInvitationsCreateOnlyOneMembership(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	owner := createUser(t, db, "owner@example.com")
	target := createUser(t, db, "target@example.com")
	org := seedEnterprise(t, db, svc, "Concurrent invitations", []int64{owner.ID}, []string{"owner"})
	tokens := make([]string, 2)
	for i, role := range []string{"admin", "member"} {
		_, token, err := svc.CreateInvitation(ctx, owner.ID, org.ID, target.Email, role, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = token
	}
	revision := orgRevisionOf(t, db, org.ID)
	start := make(chan struct{})
	results := make(chan error, len(tokens))
	for _, token := range tokens {
		go func(token string) {
			<-start
			_, err := svc.AcceptInvitation(ctx, token, target.ID, target.Email)
			results <- err
		}(token)
	}
	close(start)
	succeeded, conflicted := 0, 0
	for range tokens {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, organization.ErrInvitationConflict):
			conflicted++
		default:
			t.Fatalf("unexpected accept error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 || orgRevisionOf(t, db, org.ID) != revision+1 {
		t.Fatalf("successes=%d conflicts=%d revision=%d", succeeded, conflicted, orgRevisionOf(t, db, org.ID))
	}
}
