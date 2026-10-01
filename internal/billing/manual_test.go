package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Pure (no PostgreSQL) tests of the manual disposition contract: the validation
// that runs before any transaction is opened, and the determinism of the debit
// digest that makes a manual settlement idempotent.

func manualTestParams() ManualDispositionParams {
	return ManualDispositionParams{
		ExecutionID:   "exec_manual",
		Action:        ManualActionSettle,
		Reason:        "operator: provider dashboard shows the full response was delivered",
		ActorUserID:   7,
		MinAge:        40 * time.Minute,
		LivenessLease: time.Minute,
		ObservedAt:    testTime(),
		RequestID:     "req-manual-1",
		Now:           testTime(),
	}
}

func TestValidateManualDispositionRefusesIncompleteDecisions(t *testing.T) {
	if err := ValidateManualDisposition(NormalizeManualDisposition(manualTestParams())); err != nil {
		t.Fatalf("the reference decision must validate: %v", err)
	}
	for name, mutate := range map[string]func(*ManualDispositionParams){
		"no execution":     func(p *ManualDispositionParams) { p.ExecutionID = "" },
		"padded execution": func(p *ManualDispositionParams) { p.ExecutionID = " exec_manual " },
		"unknown action":   func(p *ManualDispositionParams) { p.Action = "refund" },
		"empty reason":     func(p *ManualDispositionParams) { p.Reason = "" },
		"long reason":      func(p *ManualDispositionParams) { p.Reason = strings.Repeat("x", MaxReasonLength+1) },
		"no actor":         func(p *ManualDispositionParams) { p.ActorUserID = 0 },
		"negative actor":   func(p *ManualDispositionParams) { p.ActorUserID = -1 },
		"no window":        func(p *ManualDispositionParams) { p.MinAge = 0 },
		"no lease":         func(p *ManualDispositionParams) { p.LivenessLease = 0 },
		"no check instant": func(p *ManualDispositionParams) { p.ObservedAt = time.Time{} },
		"long request id":  func(p *ManualDispositionParams) { p.RequestID = strings.Repeat("r", MaxRecoveryRequestIDLength+1) },
	} {
		t.Run(name, func(t *testing.T) {
			params := manualTestParams()
			mutate(&params)
			if err := ValidateManualDisposition(NormalizeManualDisposition(params)); err == nil {
				t.Fatal("an incomplete manual decision must be refused before a transaction is opened")
			}
		})
	}

	// The missing check instant is the 检查留证 rule and has its own sentinel, so
	// the HTTP layer can map it without guessing.
	params := manualTestParams()
	params.ObservedAt = time.Time{}
	if err := ValidateManualDisposition(params); !errors.Is(err, ErrRecoveryCheckMissing) {
		t.Fatalf("missing check: %v, want ErrRecoveryCheckMissing", err)
	}
	unattributed := manualTestParams()
	unattributed.ActorUserID = 0
	if err := ValidateManualDisposition(unattributed); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing actor: %v, want ErrInvalidArgument", err)
	}
}

func TestNormalizeManualDispositionTrimsAndDefaults(t *testing.T) {
	normalized := NormalizeManualDisposition(ManualDispositionParams{
		ExecutionID: "exec_1",
		Action:      " SETTLE ",
		Reason:      "  reviewed  ",
		RequestID:   " req-1 ",
		ObservedAt:  testTime(),
	})
	if normalized.Action != ManualActionSettle || normalized.Reason != "reviewed" || normalized.RequestID != "req-1" {
		t.Fatalf("normalized=%+v", normalized)
	}
	if normalized.Now.IsZero() {
		t.Fatal("Now must default rather than stay zero")
	}
}

// TestManualSettlementDebitDigestIsDeterministic guards the property the ledger
// depends on: the same decision must produce the same digest (so a retry is a
// replay) and any other amount or price version must produce a different one (so
// a changed decision is a visible conflict, not a second charge).
func TestManualSettlementDebitDigestIsDeterministic(t *testing.T) {
	businessID := ReceiptBusinessID("exec_1", KindDebit)
	digest := ManualSettlementDebitDigest("exec_1", 3, 100, businessID)
	if digest != ManualSettlementDebitDigest("exec_1", 3, 100, businessID) {
		t.Fatal("the same decision must produce the same digest")
	}
	if len(digest) != 64 {
		t.Fatalf("digest=%q, want a sha256 hex string", digest)
	}
	for name, other := range map[string]string{
		"another amount":        ManualSettlementDebitDigest("exec_1", 3, 101, businessID),
		"another price version": ManualSettlementDebitDigest("exec_1", 4, 100, businessID),
		"another execution":     ManualSettlementDebitDigest("exec_2", 3, 100, businessID),
	} {
		if other == digest {
			t.Fatalf("%s must change the digest", name)
		}
	}
	// The manual digest is deliberately not the automatic settlement's digest:
	// the ledger row is the same, the evidence behind it is not.
	if digest == SettlementDebitDigest(Identity{ExecutionID: "exec_1", EventID: "evt", OrgID: 1, ChargeMicro: 100}, businessID) {
		t.Fatal("a manual settlement must not masquerade as a receipt-backed one")
	}
}

// TestManualDispositionRefusesWithoutADatabase: an unusable store is a wiring
// bug, not a degraded mode — the operator must be told that nothing was decided,
// never shown a success.
func TestManualDispositionRefusesWithoutADatabase(t *testing.T) {
	store := &PGAccountStore{DB: nil}
	if _, err := store.ReleaseExecutionManually(context.Background(), manualTestParams()); err == nil {
		t.Fatal("a store without a database must refuse rather than pretend")
	}
	if _, err := store.SettleExecutionManually(context.Background(), manualTestParams()); err == nil {
		t.Fatal("a store without a database must refuse rather than pretend")
	}
}
