package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func setMemberLimit(t *testing.T, db *sql.DB, membershipID int64, limit *int64) {
	t.Helper()
	var value any
	if limit != nil {
		value = *limit
	}
	if _, err := db.Exec(`UPDATE memberships SET monthly_limit_micro = $2 WHERE id = $1`, membershipID, value); err != nil {
		t.Fatalf("set membership quota: %v", err)
	}
}

func memberUsageProjection(t *testing.T, db *sql.DB, orgID, userID int64, month string) (actual, reserved int64) {
	t.Helper()
	err := db.QueryRow(`SELECT actual_micro, reserved_micro
FROM billing_member_monthly_usage
WHERE org_id = $1 AND user_id = $2 AND quota_month = $3::date`, orgID, userID, month).Scan(&actual, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("read quota projection: %v", err)
	}
	return actual, reserved
}

func waitForAdmissionMembershipLock(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := db.QueryRow(`SELECT count(*)
FROM pg_stat_activity
WHERE wait_event_type = 'Lock' AND query LIKE '%FOR UPDATE OF m, u%'`).Scan(&waiting)
		if err != nil {
			t.Fatalf("inspect PostgreSQL lock wait: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Reserve never blocked on the membership row held by the concurrent update")
}

func waitForPostgresLock(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := db.QueryRow(query, args...).Scan(&waiting); err != nil {
			t.Fatalf("inspect PostgreSQL lock wait: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected PostgreSQL lock wait did not appear")
}

func TestMemberQuotaRacesReserveAndSettlesProjectionAtomically(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "quota-concurrent")
	setMemberLimit(t, db, orgID, micro(100))
	price := seedPriceVersion(t, prices, "quota-race-model", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 10_000, "quota-race-credit")

	const attempts = 40
	start := make(chan struct{})
	results := make(chan struct {
		id  string
		err error
	}, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		id := fmt.Sprintf("exec_quota_race_%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, id, price.Model, 10))
			results <- struct {
				id  string
				err error
			}{id: id, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var admitted []string
	refused := 0
	for result := range results {
		if result.err == nil {
			admitted = append(admitted, result.id)
		} else if errors.Is(result.err, ErrMemberMonthlyLimit) {
			refused++
		} else {
			t.Fatalf("Reserve(%s): %v", result.id, result.err)
		}
	}
	if len(admitted) != 10 || refused != attempts-10 {
		t.Fatalf("admitted=%d refused=%d, want 10 and %d", len(admitted), refused, attempts-10)
	}
	month := BillingMonthAt(executionTestBase())
	if actual, reserved := memberUsageProjection(t, db, orgID, orgID, month.StartDate); actual != 0 || reserved != 100 {
		t.Fatalf("after concurrent admission actual=%d reserved=%d, want 0/100", actual, reserved)
	}

	settledID := admitted[0]
	if _, err := accounts.MarkExecuting(ctx, settledID, "req-"+settledID); err != nil {
		t.Fatal(err)
	}
	occurredAt := time.Now().UTC()
	finalized, err := accounts.FinalizeReceipt(ctx, FinalizeReceiptParams{
		ExecutionID: settledID, EventID: "evt_" + settledID,
		PromptTokens: 4, TotalTokens: 4, OrgID: orgID, Model: price.Model,
		OccurredAt: occurredAt, Now: occurredAt,
	})
	if err != nil {
		t.Fatalf("finalize usage: %v", err)
	}
	if _, err := accounts.SettleReceipt(ctx, settleParams(finalized.Receipt, "quota-race-settle")); err != nil {
		t.Fatalf("settle usage: %v", err)
	}
	actual, reserved := memberUsageProjection(t, db, orgID, orgID, month.StartDate)
	if actual != 4 || reserved != 90 {
		t.Fatalf("after settlement actual=%d reserved=%d, want 4/90", actual, reserved)
	}
	for _, id := range admitted[1:] {
		if _, err := accounts.Release(ctx, id, "unused test reservation"); err != nil {
			t.Fatalf("release %s: %v", id, err)
		}
	}
	actual, reserved = memberUsageProjection(t, db, orgID, orgID, month.StartDate)
	if actual != 4 || reserved != 0 {
		t.Fatalf("after releases actual=%d reserved=%d, want 4/0", actual, reserved)
	}
	quota, err := accounts.GetMemberQuota(ctx, orgID, orgID, orgID, month)
	if err != nil || quota.UsedMicro != 4 || quota.ReservedMicro != 0 || quota.RemainingMicro == nil || *quota.RemainingMicro != 96 {
		t.Fatalf("quota read=%+v err=%v", quota, err)
	}
}

func TestMemberQuotaZeroPriceZeroCapAndLoweredCapBlockAdmission(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "quota-zero-cap")
	setMemberLimit(t, db, orgID, micro(0))
	free := seedPriceVersion(t, prices, "quota-free-model", 0, 0, testTime())
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, free.ID, "exec_quota_free_zero", free.Model, 100)); !errors.Is(err, ErrMemberMonthlyLimit) {
		t.Fatalf("zero cap admitted a free request: %v", err)
	}
	if got := countRows(t, db, `SELECT count(*) FROM billing_executions WHERE org_id=$1`, orgID); got != 0 {
		t.Fatalf("zero-cap execution rows=%d, want 0", got)
	}

	setMemberLimit(t, db, orgID, micro(100))
	paid := seedPriceVersion(t, prices, "quota-lower-model", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 10_000, "quota-lower-credit")
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, paid.ID, "exec_quota_lower_held", paid.Model, 80)); err != nil {
		t.Fatalf("reserve before quota adjustment: %v", err)
	}
	setMemberLimit(t, db, orgID, micro(50))
	if _, err := accounts.Reserve(ctx, reserveFor(orgID, free.ID, "exec_quota_lower_free", free.Model, 100)); !errors.Is(err, ErrMemberMonthlyLimit) {
		t.Fatalf("lowered cap forgot an existing reservation: %v", err)
	}
	if _, err := accounts.Release(ctx, "exec_quota_lower_held", "quota adjustment test"); err != nil {
		t.Fatal(err)
	}
}

func TestMemberQuotaPersistsAcrossRejoinAndLateSettlementUsesAdmissionMonth(t *testing.T) {
	db, _ := billingTestDB(t)
	db.SetMaxOpenConns(16)
	accounts := NewPGAccountStore(db)
	prices := NewPGPriceVersionStore(db)
	orgID := seedOrg(t, db, "quota-rejoin")
	now := time.Now().UTC().Truncate(time.Millisecond)
	var userID, membershipID int64
	if err := db.QueryRow(`INSERT INTO users
  (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ('quota-member@example.test', 'hash', 'active', false, 1, $1, $1) RETURNING id`, now).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO memberships
  (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
VALUES ($1, $2, 'member', 'active', 100, $3, $3) RETURNING id`, orgID, userID, now).Scan(&membershipID); err != nil {
		t.Fatal(err)
	}
	creditOrg(t, accounts, orgID, 10_000, "quota-rejoin-credit")
	price := seedPriceVersion(t, prices, "quota-rejoin-model", microPerTokenPrice, 0, now)
	params := reserveFor(orgID, price.ID, "exec_quota_rejoin", price.Model, 50)
	params.UserID, params.MembershipID, params.Now = userID, membershipID, now
	if _, err := accounts.Reserve(context.Background(), params); err != nil {
		t.Fatalf("reserve before month boundary: %v", err)
	}
	if _, err := accounts.MarkExecuting(context.Background(), params.ExecutionID, "req-quota-rejoin"); err != nil {
		t.Fatal(err)
	}
	month := BillingMonthAt(now)
	settleAt := month.End.Add(time.Minute)
	finalized, err := accounts.FinalizeReceipt(context.Background(), FinalizeReceiptParams{
		ExecutionID: params.ExecutionID, EventID: "evt_quota_rejoin",
		PromptTokens: 20, TotalTokens: 20, OrgID: orgID, Model: price.Model,
		OccurredAt: settleAt, Now: settleAt,
	})
	if err != nil {
		t.Fatalf("finalize after month boundary: %v", err)
	}
	settlement := settleParams(finalized.Receipt, "quota-rejoin-settle")
	settlement.Now = settleAt.Add(time.Minute)
	if _, err := accounts.SettleReceipt(context.Background(), settlement); err != nil {
		t.Fatalf("settle after month boundary: %v", err)
	}
	if actual, reserved := memberUsageProjection(t, db, orgID, userID, month.StartDate); actual != 20 || reserved != 0 {
		t.Fatalf("original month quota actual=%d reserved=%d, want 20/0", actual, reserved)
	}
	nextMonth := BillingMonthAt(settleAt)
	if actual, reserved := memberUsageProjection(t, db, orgID, userID, nextMonth.StartDate); actual != 0 || reserved != 0 {
		t.Fatalf("settlement incorrectly moved to next month: actual=%d reserved=%d", actual, reserved)
	}

	if _, err := db.Exec(`UPDATE memberships SET status='removed' WHERE id=$1`, membershipID); err != nil {
		t.Fatal(err)
	}
	var rejoinedMembershipID int64
	if err := db.QueryRow(`INSERT INTO memberships
  (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
VALUES ($1, $2, 'member', 'active', 100, $3, $3) RETURNING id`, orgID, userID, now).Scan(&rejoinedMembershipID); err != nil {
		t.Fatal(err)
	}
	quota, err := accounts.GetMemberQuota(context.Background(), orgID, userID, rejoinedMembershipID, month)
	if err != nil || quota.UsedMicro != 20 || quota.RemainingMicro == nil || *quota.RemainingMicro != 80 {
		t.Fatalf("rejoined quota=%+v err=%v", quota, err)
	}
	second := reserveFor(orgID, price.ID, "exec_quota_rejoin_second", price.Model, 81)
	second.UserID, second.MembershipID, second.Now = userID, rejoinedMembershipID, now
	if _, err := accounts.Reserve(context.Background(), second); !errors.Is(err, ErrMemberMonthlyLimit) {
		t.Fatalf("rejoin reset monthly usage: %v", err)
	}
}

func TestReserveWaitsForAuthoritativeQuotaAndMembershipState(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "quota-authoritative-race")
	price := seedPriceVersion(t, prices, "quota-authoritative-model", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 10_000, "quota-authoritative-credit")

	mutation, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutation.Exec(`UPDATE memberships SET monthly_limit_micro=50 WHERE id=$1`, orgID); err != nil {
		_ = mutation.Rollback()
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_quota_cap_race", price.Model, 60))
		result <- err
	}()
	waitForAdmissionMembershipLock(t, db)
	if err := mutation.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrMemberMonthlyLimit) {
		t.Fatalf("Reserve used a stale quota cap: %v", err)
	}

	mutation, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutation.Exec(`UPDATE memberships SET status='removed' WHERE id=$1`, orgID); err != nil {
		_ = mutation.Rollback()
		t.Fatal(err)
	}
	result = make(chan error, 1)
	go func() {
		_, err := accounts.Reserve(ctx, reserveFor(orgID, price.ID, "exec_quota_status_race", price.Model, 1))
		result <- err
	}()
	waitForAdmissionMembershipLock(t, db)
	if err := mutation.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrMembershipInactive) {
		t.Fatalf("Reserve used stale membership state: %v", err)
	}
}

func TestReserveRejectsSuspendedMembershipAndDisabledUser(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	orgID := seedOrg(t, db, "quota-disabled-user")
	price := seedPriceVersion(t, prices, "quota-disabled-model", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 10_000, "quota-disabled-credit")
	if _, err := db.Exec(`UPDATE memberships SET status='suspended' WHERE id=$1`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Reserve(context.Background(), reserveFor(orgID, price.ID, "exec_suspended_member", price.Model, 1)); !errors.Is(err, ErrMembershipInactive) {
		t.Fatalf("suspended membership admission=%v", err)
	}
	if _, err := db.Exec(`UPDATE memberships SET status='active' WHERE id=$1`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Reserve(context.Background(), reserveFor(orgID, price.ID, "exec_disabled_user", price.Model, 1)); !errors.Is(err, ErrMembershipInactive) {
		t.Fatalf("disabled user admission=%v", err)
	}
}

func TestReserveAndOrganizationMembershipUpdateDoNotDeadlockOnOrgForeignKey(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	orgID := seedOrg(t, db, "quota-org-lock-order")
	price := seedPriceVersion(t, prices, "quota-org-lock-model", microPerTokenPrice, 0, testTime())
	creditOrg(t, accounts, orgID, 10_000, "quota-org-lock-credit")
	accounts.AdmissionTimeout = 10 * time.Second

	const advisoryClass, advisoryObject = 74661, 15433
	if _, err := db.Exec(`CREATE FUNCTION billing_test_pause_quota_insert() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
  PERFORM pg_advisory_xact_lock(TG_ARGV[0]::integer, TG_ARGV[1]::integer);
  RETURN NEW;
END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER billing_test_pause_quota_insert
BEFORE INSERT ON billing_member_monthly_usage FOR EACH ROW
EXECUTE FUNCTION billing_test_pause_quota_insert('74661', '15433')`); err != nil {
		t.Fatal(err)
	}

	pause, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pause.Exec(`SELECT pg_advisory_xact_lock($1, $2)`, advisoryClass, advisoryObject); err != nil {
		_ = pause.Rollback()
		t.Fatal(err)
	}
	reserveResult := make(chan error, 1)
	go func() {
		_, err := accounts.Reserve(context.Background(), reserveFor(orgID, price.ID, "exec_org_lock_order", price.Model, 10))
		reserveResult <- err
	}()
	waitForPostgresLock(t, db, `SELECT count(*) FROM pg_locks
WHERE locktype='advisory' AND NOT granted AND classid=$1::oid AND objid=$2::oid`, advisoryClass, advisoryObject)

	mutation, err := db.Begin()
	if err != nil {
		_ = pause.Rollback()
		t.Fatal(err)
	}
	if _, err := mutation.Exec(`SELECT id FROM organizations WHERE id=$1 FOR NO KEY UPDATE`, orgID); err != nil {
		_ = mutation.Rollback()
		_ = pause.Rollback()
		t.Fatal(err)
	}
	mutationResult := make(chan error, 1)
	go func() {
		_, err := mutation.Exec(`UPDATE memberships SET status='suspended' WHERE id=$1`, orgID)
		mutationResult <- err
	}()
	waitForPostgresLock(t, db, `SELECT count(*) FROM pg_stat_activity
WHERE wait_event_type='Lock' AND query LIKE '%UPDATE memberships SET status%'`)

	if err := pause.Commit(); err != nil {
		_ = mutation.Rollback()
		t.Fatal(err)
	}
	select {
	case err := <-reserveResult:
		if err != nil {
			t.Fatalf("Reserve under organization mutation lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = mutation.Rollback()
		t.Fatal("Reserve deadlocked with organization membership update")
	}
	select {
	case err := <-mutationResult:
		if err != nil {
			t.Fatalf("membership update: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = mutation.Rollback()
		t.Fatal("organization membership update remained blocked after Reserve committed")
	}
	if err := mutation.Commit(); err != nil {
		t.Fatal(err)
	}
}
