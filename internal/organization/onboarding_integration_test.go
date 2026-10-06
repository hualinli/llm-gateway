package organization_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"
)

func TestActivationCodeStoresOnlyHashAndCreatesOwnerAccountAtomically(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	creator := createUser(t, db, "activation-creator@example.com")
	user := createUser(t, db, "activation-user@example.com")
	secondUser := createUser(t, db, "activation-second@example.com")
	personal, err := svc.EnsurePersonalOrganization(ctx, user.ID, "Personal")
	if err != nil {
		t.Fatal(err)
	}

	expiresAt := time.Now().UTC().Add(time.Hour)
	code, raw, err := svc.CreateActivationCode(ctx, creator.ID, " Acme Inc ", &expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	var storedHash string
	if err := db.QueryRow(`SELECT code_hash FROM enterprise_activation_codes WHERE id=$1`, code.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(raw))
	if storedHash != hex.EncodeToString(digest[:]) || storedHash == raw {
		t.Fatal("database did not store only the activation-code digest")
	}
	items, next, err := svc.ListActivationCodes(ctx, 1, 0)
	if err != nil || len(items) != 1 || next != "" || items[0].ID != code.ID {
		t.Fatalf("ListActivationCodes = (%+v, %q, %v)", items, next, err)
	}

	membership, err := svc.RedeemActivationCode(ctx, user.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if membership.Organization.Type != "enterprise" || membership.Organization.Name != "Acme Inc" ||
		membership.Role != "owner" || membership.Status != "active" || membership.MembershipID == 0 {
		t.Fatalf("redemption membership = %+v", membership)
	}
	assertZeroBalanceWallet(t, db, membership.Organization.ID)
	if membership.MonthlyLimitMicro != nil {
		t.Fatal("new enterprise owner unexpectedly has a monthly cap")
	}
	visible, _, err := svc.ListForUser(ctx, user.ID, 20, 0)
	if err != nil || len(visible) != 2 {
		t.Fatalf("personal account was not preserved with the redeemed enterprise: items=%+v err=%v", visible, err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM enterprise_activation_codes WHERE id=$1`, code.ID).Scan(&status); err != nil || status != "used" {
		t.Fatalf("activation status = %q, err=%v, want used", status, err)
	}
	if _, err := svc.RedeemActivationCode(ctx, secondUser.ID, raw); !errors.Is(err, organization.ErrActivationCodeUnavailable) {
		t.Fatalf("second redemption = %v, want unavailable", err)
	}
	var enterprises, wallets int
	if err := db.QueryRow(`SELECT count(*) FROM organizations WHERE type='enterprise' AND owner_user_id=$1`, user.ID).Scan(&enterprises); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM billing_accounts WHERE org_id=$1`, membership.Organization.ID).Scan(&wallets); err != nil {
		t.Fatal(err)
	}
	if enterprises != 1 || wallets != 1 || personal.ID == membership.Organization.ID {
		t.Fatalf("redemption left enterprises=%d wallets=%d personal=%d new=%d", enterprises, wallets, personal.ID, membership.Organization.ID)
	}
}

func TestConcurrentActivationRedemptionConsumesExactlyOnce(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	creator := createUser(t, db, "activation-race-creator@example.com")
	first := createUser(t, db, "activation-race-first@example.com")
	second := createUser(t, db, "activation-race-second@example.com")
	_, raw, err := svc.CreateActivationCode(ctx, creator.ID, "Race Enterprise", nil)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type result struct {
		userID int64
		orgID  int64
		err    error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, user := range []iam.User{first, second} {
		wg.Add(1)
		go func(user iam.User) {
			defer wg.Done()
			<-start
			membership, err := svc.RedeemActivationCode(ctx, user.ID, raw)
			results <- result{userID: user.ID, orgID: membership.Organization.ID, err: err}
		}(user)
	}
	close(start)
	wg.Wait()
	close(results)

	successes, unavailable := 0, 0
	var orgID int64
	for result := range results {
		if result.err == nil {
			successes++
			orgID = result.orgID
		} else if errors.Is(result.err, organization.ErrActivationCodeUnavailable) {
			unavailable++
		} else {
			t.Fatalf("redemption failed unexpectedly: %v", result.err)
		}
	}
	if successes != 1 || unavailable != 1 {
		t.Fatalf("redemption outcomes successes=%d unavailable=%d, want 1/1", successes, unavailable)
	}
	assertZeroBalanceWallet(t, db, orgID)
	var created int
	if err := db.QueryRow(`SELECT count(*) FROM organizations WHERE id=$1 AND type='enterprise'`, orgID).Scan(&created); err != nil || created != 1 {
		t.Fatalf("redeemed enterprise rows=%d err=%v", created, err)
	}
}

func TestJoinRequestScopeDisabledApplicantAndAtomicApproval(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	owner := createUser(t, db, "join-owner@example.com")
	admin := createUser(t, db, "join-admin@example.com")
	member := createUser(t, db, "join-member@example.com")
	applicant := createUser(t, db, "join-applicant@example.com")
	platformAdmin, err := iam.NewService(db, iam.Options{BcryptCost: bcrypt.MinCost}).CreateUser(ctx, "join-platform@example.com", "correct horse battery staple", true)
	if err != nil {
		t.Fatal(err)
	}
	org, err := svc.CreateEnterprise(ctx, owner.ID, "Join Request Enterprise")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(ctx, org.ID, admin.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(ctx, org.ID, member.ID, "member"); err != nil {
		t.Fatal(err)
	}
	personal, err := svc.EnsurePersonalOrganization(ctx, applicant.ID, "Applicant")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateJoinRequest(ctx, applicant.ID, personal.ID, "Applicant"); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("application to personal account = %v, want hidden not found", err)
	}
	if _, err := svc.CreateJoinRequest(ctx, applicant.ID, org.ID, "Applicant"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateJoinRequest(ctx, applicant.ID, org.ID, "Applicant again"); !errors.Is(err, organization.ErrApplicationPending) {
		t.Fatalf("duplicate pending application = %v", err)
	}
	if _, err := svc.CreateJoinRequest(ctx, owner.ID, org.ID, "Owner"); !errors.Is(err, organization.ErrApplicationAlreadyMember) {
		t.Fatalf("current member application = %v", err)
	}
	myItems, _, err := svc.ListMyJoinRequests(ctx, applicant.ID, 20, 0)
	if err != nil || len(myItems) != 1 || myItems[0].UserID != applicant.ID {
		t.Fatalf("applicant history = %+v err=%v", myItems, err)
	}
	if _, _, err := svc.ListMyJoinRequests(ctx, owner.ID, 20, 0); err != nil {
		t.Fatalf("own empty history: %v", err)
	}
	if _, _, err := svc.ListJoinRequests(ctx, owner.ID, org.ID, "pending", 20, 0); err != nil {
		t.Fatalf("owner list: %v", err)
	}
	if _, _, err := svc.ListJoinRequests(ctx, admin.ID, org.ID, "pending", 20, 0); err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if _, _, err := svc.ListJoinRequests(ctx, member.ID, org.ID, "pending", 20, 0); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("ordinary member list = %v, want forbidden", err)
	}
	if _, _, err := svc.ListJoinRequests(ctx, platformAdmin.ID, org.ID, "pending", 20, 0); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("platform admin without org membership = %v, want hidden not found", err)
	}
	if _, err := svc.DecideJoinRequest(ctx, owner.ID, org.ID, myItems[0].ID, "approved", nil, false); !errors.Is(err, organization.ErrLimitRequired) {
		t.Fatalf("approval without explicit cap = %v", err)
	}

	if _, err := iam.NewService(db, iam.Options{BcryptCost: bcrypt.MinCost}).SetUserStatus(ctx, applicant.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideJoinRequest(ctx, owner.ID, org.ID, myItems[0].ID, "approved", nil, true); !errors.Is(err, organization.ErrApplicantInactive) {
		t.Fatalf("disabled applicant approval = %v, want inactive applicant", err)
	}
	if _, err := iam.NewService(db, iam.Options{BcryptCost: bcrypt.MinCost}).SetUserStatus(ctx, applicant.ID, "active"); err != nil {
		t.Fatal(err)
	}

	beforeRevision := orgRevisionOf(t, db, org.ID)
	zero := int64(0)
	approved, err := svc.DecideJoinRequest(ctx, admin.ID, org.ID, myItems[0].ID, "approved", &zero, true)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "approved" || approved.ReviewedBy == nil || *approved.ReviewedBy != admin.ID || approved.MonthlyLimitMicro == nil || *approved.MonthlyLimitMicro != 0 {
		t.Fatalf("approved request = %+v", approved)
	}
	membership, err := svc.Membership(ctx, applicant.ID, org.ID)
	if err != nil || membership.Role != "member" || membership.MonthlyLimitMicro == nil || *membership.MonthlyLimitMicro != 0 {
		t.Fatalf("new membership = %+v err=%v", membership, err)
	}
	if orgRevisionOf(t, db, org.ID) != beforeRevision+1 {
		t.Fatalf("organization revision did not advance once on approval")
	}
	var publications int
	if err := db.QueryRow(`SELECT count(*) FROM policy_publications WHERE org_id=$1 AND org_revision=$2 AND scope='org' AND status='pending'`, org.ID, beforeRevision+1).Scan(&publications); err != nil || publications != 1 {
		t.Fatalf("approval publications=%d err=%v", publications, err)
	}
	if _, err := svc.DecideJoinRequest(ctx, owner.ID, org.ID, myItems[0].ID, "rejected", nil, true); !errors.Is(err, organization.ErrApplicationDecided) {
		t.Fatalf("second decision = %v, want already decided", err)
	}
}

func TestConcurrentJoinRequestsAndDecisionsAreSerialized(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	owner := createUser(t, db, "join-race-owner@example.com")
	admin := createUser(t, db, "join-race-admin@example.com")
	applicant := createUser(t, db, "join-race-applicant@example.com")
	org, err := svc.CreateEnterprise(ctx, owner.ID, "Join Race Enterprise")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(ctx, org.ID, admin.ID, "admin"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	requestResults := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.CreateJoinRequest(ctx, applicant.ID, org.ID, "Applicant")
			requestResults <- err
		}()
	}
	close(start)
	wg.Wait()
	close(requestResults)
	created, duplicate := 0, 0
	for err := range requestResults {
		if err == nil {
			created++
		} else if errors.Is(err, organization.ErrApplicationPending) {
			duplicate++
		} else {
			t.Fatalf("application creation: %v", err)
		}
	}
	if created != 1 || duplicate != 1 {
		t.Fatalf("concurrent application outcomes created=%d duplicate=%d", created, duplicate)
	}
	items, _, err := svc.ListJoinRequests(ctx, owner.ID, org.ID, "pending", 20, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("pending applications=%d err=%v", len(items), err)
	}

	beforeRevision := orgRevisionOf(t, db, org.ID)
	start = make(chan struct{})
	decisionResults := make(chan error, 2)
	for i, actor := range []int64{owner.ID, admin.ID} {
		wg.Add(1)
		go func(actor int64, cap int64) {
			defer wg.Done()
			<-start
			_, err := svc.DecideJoinRequest(ctx, actor, org.ID, items[0].ID, "approved", &cap, true)
			decisionResults <- err
		}(actor, int64(i+1))
	}
	close(start)
	wg.Wait()
	close(decisionResults)
	succeeded, alreadyDecided := 0, 0
	for err := range decisionResults {
		if err == nil {
			succeeded++
		} else if errors.Is(err, organization.ErrApplicationDecided) {
			alreadyDecided++
		} else {
			t.Fatalf("concurrent decision: %v", err)
		}
	}
	if succeeded != 1 || alreadyDecided != 1 {
		t.Fatalf("concurrent approval outcomes success=%d decided=%d", succeeded, alreadyDecided)
	}
	var currentMemberships, appCount, publicationCount int
	if err := db.QueryRow(`SELECT count(*) FROM memberships WHERE org_id=$1 AND user_id=$2 AND status IN ('active','suspended')`, org.ID, applicant.ID).Scan(&currentMemberships); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM membership_applications WHERE org_id=$1 AND user_id=$2 AND status='approved'`, org.ID, applicant.ID).Scan(&appCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM policy_publications WHERE org_id=$1 AND org_revision=$2 AND scope='org' AND status='pending'`, org.ID, beforeRevision+1).Scan(&publicationCount); err != nil {
		t.Fatal(err)
	}
	if currentMemberships != 1 || appCount != 1 || publicationCount != 1 || orgRevisionOf(t, db, org.ID) != beforeRevision+1 {
		t.Fatalf("atomic approval state memberships=%d applications=%d publications=%d", currentMemberships, appCount, publicationCount)
	}
}

func TestExpiredActivationCodeCannotCreatePartialAccount(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	creator := createUser(t, db, "expired-code-creator@example.com")
	user := createUser(t, db, "expired-code-user@example.com")
	_, raw, err := svc.CreateActivationCode(ctx, creator.ID, "Expired Enterprise", nil)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(digest[:])
	if _, err := db.Exec(`UPDATE enterprise_activation_codes SET expires_at=$2 WHERE code_hash=$1`, hash, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RedeemActivationCode(ctx, user.ID, raw); !errors.Is(err, organization.ErrActivationCodeUnavailable) {
		t.Fatalf("expired redemption = %v", err)
	}
	var orgs, wallets int
	if err := db.QueryRow(`SELECT count(*) FROM organizations WHERE type='enterprise' AND name='Expired Enterprise'`).Scan(&orgs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM billing_accounts WHERE org_id IN (SELECT id FROM organizations WHERE type='enterprise' AND name='Expired Enterprise')`).Scan(&wallets); err != nil {
		t.Fatal(err)
	}
	if orgs != 0 || wallets != 0 {
		t.Fatalf("expired code left partial state orgs=%d wallets=%d", orgs, wallets)
	}
}
