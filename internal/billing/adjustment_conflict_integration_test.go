package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// F13, second half: the same adjustment business id with a different amount.
//
// The acceptance spec requires that this is a conflict: "same adjustment key,
// different amount" must not silently move money twice, and it must not be
// swallowed as a replay either. The ledger's uniqueness rule is
// (business_id, kind); a repeat with the SAME payload is a replay, and a repeat
// with a DIFFERENT payload is ErrIdempotencyConflict.
//
// This is a store-level test because the identity is an input here. The HTTP
// adjustment endpoint derives business_id from the Idempotency-Key header, so
// the conflict is reachable over the wire too, but only the store can be handed
// the exact same key with a different amount without inventing a second HTTP
// contract in a test.
func TestAdjustmentConflictSameBusinessIDDifferentAmount(t *testing.T) {
	db, _ := billingTestDB(t)
	accounts := NewPGAccountStore(db)
	ctx := context.Background()
	orgID := seedOrg(t, db, "t10-f13-adjustment")

	now := time.Now().UTC().Truncate(time.Microsecond)
	businessID := "adjustment:t10-f13-conflict"
	reason := "T10 F13: the first posting under this key"

	first := AdjustParams{
		OrgID:          orgID,
		AmountMicro:    2500,
		Direction:      DirectionCredit,
		Kind:           KindAdjustment,
		Reason:         reason,
		BusinessID:     businessID,
		IdempotencyKey: businessID,
		Now:            now,
	}
	first.PayloadDigest = AdjustmentDigest(orgID, first.AmountMicro, first.Direction, first.Kind, reason)

	applied, err := accounts.Adjust(ctx, first)
	if err != nil {
		t.Fatalf("first adjustment: %v", err)
	}
	if applied.Replayed {
		t.Fatal("the first posting must not report Replayed")
	}
	postedFirst, versionFirst := accountProjection(t, db, orgID)
	if postedFirst != 2500 {
		t.Fatalf("posted=%d after the first adjustment, want 2500", postedFirst)
	}

	// Same business id and kind, different amount, therefore a different payload
	// digest. This is the conflict the spec names.
	second := first
	second.AmountMicro = 9999
	second.Reason = "T10 F13: the same key, a different amount"
	second.Now = now.Add(time.Second)
	second.PayloadDigest = AdjustmentDigest(orgID, second.AmountMicro, second.Direction, second.Kind, second.Reason)

	if _, err := accounts.Adjust(ctx, second); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("second adjustment: err=%v, want ErrIdempotencyConflict", err)
	}

	postedSecond, versionSecond := accountProjection(t, db, orgID)
	if postedSecond != postedFirst {
		t.Fatalf("posted=%d after the conflict, want the unchanged %d: money moved for a refused posting",
			postedSecond, postedFirst)
	}
	if versionSecond != versionFirst {
		t.Fatalf("account version=%d after the conflict, want the unchanged %d", versionSecond, versionFirst)
	}
	amounts := adjustmentAmounts(t, db, orgID, businessID)
	if len(amounts) != 1 || amounts[0] != 2500 {
		t.Fatalf("entries under the business id=%v, want exactly one of 2500", amounts)
	}

	// The identical repeat is still a replay, not a conflict: the rule is about
	// the payload, not about repetition.
	replay, err := accounts.Adjust(ctx, first)
	if err != nil {
		t.Fatalf("identical repeat: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("an identical repeat must report Replayed")
	}
	if posted, _ := accountProjection(t, db, orgID); posted != 2500 {
		t.Fatalf("posted=%d after the replay, want 2500", posted)
	}
}

// TestReservationConflictSameExecutionIDDifferentPayload is F13's first half at
// the level where the identity is an input: the same execution id reserved twice
// with a different payload. The HTTP path mints a fresh execution id per request,
// so this cannot be produced over the wire; the rule is enforced by the store and
// by the constraints in migration 000009.
func TestReservationConflictSameExecutionIDDifferentPayload(t *testing.T) {
	prices, accounts, db := newExecutionStore(t)
	ctx := context.Background()
	orgID := seedOrg(t, db, "t10-f13-reservation")
	price := seedPriceVersion(t, prices, "t10-f13-model", microPerTokenPrice, microPerTokenPrice, testTime())
	creditOrg(t, accounts, orgID, 1_000_000, "t10-f13-funding")

	base := ReserveParams{
		ExecutionID:       "exec_t10_f13_conflict",
		OrgID:             orgID,
		KeyID:             "key-t10-f13",
		GatewayID:         "gw-t10-f13",
		Model:             "t10-f13-model",
		PriceVersionID:    price.ID,
		InputTokensUpper:  100,
		OutputTokensUpper: 100,
		Now:               testTime(),
	}
	if _, err := accounts.Reserve(ctx, base); err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	// Same execution id, a different pinned payload: a conflict, never a replay
	// and never a silent DO NOTHING.
	changed := base
	changed.OutputTokensUpper = 101
	changed.Now = testTime().Add(time.Second)
	if _, err := accounts.Reserve(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed reserve: err=%v, want ErrIdempotencyConflict", err)
	}

	// The identical repeat is a replay and moves nothing.
	replay, err := accounts.Reserve(ctx, base)
	if err != nil {
		t.Fatalf("identical reserve: %v", err)
	}
	if !replay.Replayed {
		t.Fatal("an identical repeat must report Replayed")
	}

	var executions, reservations int
	if err := db.QueryRow(`SELECT count(*) FROM billing_executions WHERE org_id = $1`, orgID).Scan(&executions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = 'reservation'`,
		orgID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if executions != 1 || reservations != 1 {
		t.Fatalf("executions=%d reservations=%d, want 1 and 1", executions, reservations)
	}
}

func accountProjection(t *testing.T, db *sql.DB, orgID int64) (posted int64, version int64) {
	t.Helper()
	if err := db.QueryRow(`SELECT posted_balance_micro, version FROM billing_accounts WHERE org_id = $1`,
		orgID).Scan(&posted, &version); err != nil {
		t.Fatalf("read account: %v", err)
	}
	return posted, version
}

func adjustmentAmounts(t *testing.T, db *sql.DB, orgID int64, businessID string) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT amount_micro FROM billing_entries
WHERE org_id = $1 AND business_id = $2 AND kind = 'adjustment' ORDER BY id`, orgID, businessID)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var amount int64
		if err := rows.Scan(&amount); err != nil {
			t.Fatalf("scan entry: %v", err)
		}
		out = append(out, amount)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("entries: %v", err)
	}
	return out
}
