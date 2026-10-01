package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Pure policy tests for T07A. They need no PostgreSQL and no Redis: the retry
// curve, the wire envelope and the digest rules are decisions that must be
// provable on their own, before any integration test exercises them.

func TestDispatchBackoffIsBoundedAndMonotonic(t *testing.T) {
	base, max := time.Second, 5*time.Second
	previous := time.Duration(0)
	for attempt := 1; attempt <= 8; attempt++ {
		delay := DispatchBackoff(attempt, base, max)
		if delay < base || delay > max {
			t.Fatalf("attempt %d: delay=%s, want %s..%s", attempt, delay, base, max)
		}
		if delay < previous {
			t.Fatalf("attempt %d: delay=%s decreased from %s", attempt, delay, previous)
		}
		previous = delay
	}
	if got := DispatchBackoff(1, base, max); got != base {
		t.Fatalf("first retry=%s, want %s", got, base)
	}
	if got := DispatchBackoff(2, base, max); got != 2*base {
		t.Fatalf("second retry=%s, want %s", got, 2*base)
	}
	if got := DispatchBackoff(99, base, max); got != max {
		t.Fatalf("saturated retry=%s, want %s", got, max)
	}
	// A zero/negative attempt is the first attempt, never "no delay at all".
	if got := DispatchBackoff(0, base, max); got != base {
		t.Fatalf("attempt 0=%s, want %s", got, base)
	}
}

func TestRedispatchDelayGrowsThenCaps(t *testing.T) {
	base, max := 60*time.Second, 15*time.Minute
	if got := RedispatchDelay(1, base, max); got != base {
		t.Fatalf("first re-dispatch=%s, want %s", got, base)
	}
	if got := RedispatchDelay(3, base, max); got != 4*base {
		t.Fatalf("third re-dispatch=%s, want %s", got, 4*base)
	}
	if got := RedispatchDelay(20, base, max); got != max {
		t.Fatalf("re-dispatch must cap at %s, got %s", max, got)
	}
}

// TestReceiptEventRoundTripPreservesTheReceiptDigest is the transport contract:
// whatever a dispatcher publishes must recompute to the stored receipt digest,
// or settlement would (correctly) refuse it.
func TestReceiptEventRoundTripPreservesTheReceiptDigest(t *testing.T) {
	keyID := "key-1"
	userID := int64(7)
	receipt := Receipt{
		ExecutionID:    "exec_1",
		EventID:        "evt_1",
		OrgID:          42,
		UserID:         &userID,
		KeyID:          &keyID,
		Model:          "gpt-4o",
		PromptTokens:   10,
		OutputTokens:   20,
		TotalTokens:    30,
		PriceVersionID: 3,
		ChargeMicro:    1_500_000,
		Status:         ReceiptStatusPendingDispatch,
	}
	receipt.Digest = receipt.Identity().Digest()

	publishedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	event, err := NewReceiptEvent(receipt, publishedAt)
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	if event.Kind != ReceiptEventKind || event.Digest != receipt.Digest {
		t.Fatalf("event=%+v", event)
	}
	payload, err := MarshalReceiptEvent(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseReceiptEvent(payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := parsed.Identity().Digest(); got != receipt.Digest {
		t.Fatalf("round-tripped digest=%s, want %s", got, receipt.Digest)
	}
	if !parsed.PublishedAt.Equal(publishedAt) || !parsed.OccurredAt.Equal(receipt.OccurredAt) {
		t.Fatalf("timestamps did not survive the round trip: %+v", parsed)
	}

	// Absent optional attribution projects exactly the way the digest reads it.
	anonymous := receipt
	anonymous.UserID, anonymous.KeyID = nil, nil
	anonymous.Digest = anonymous.Identity().Digest()
	anonymousEvent, err := NewReceiptEvent(anonymous, publishedAt)
	if err != nil {
		t.Fatalf("anonymous event: %v", err)
	}
	if anonymousEvent.UserID != 0 || anonymousEvent.KeyID != "" {
		t.Fatalf("absent attribution must stay absent: %+v", anonymousEvent)
	}
	if anonymousEvent.Identity().Digest() != anonymous.Digest {
		t.Fatal("the absent-attribution rendering must match the digest")
	}
}

func TestReceiptEventRejectsMalformedPayloads(t *testing.T) {
	base := Receipt{
		ExecutionID: "exec_1", EventID: "evt_1", OrgID: 1, Model: "gpt-4o",
		PromptTokens: 1, OutputTokens: 1, TotalTokens: 2, PriceVersionID: 1,
		ChargeMicro: 5, Status: ReceiptStatusPendingDispatch,
	}
	base.Digest = base.Identity().Digest()
	valid, err := NewReceiptEvent(base, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	// An exception receipt has no chargeable usage: it must never be published.
	exception := base
	exception.Status = ReceiptStatusException
	if _, err := NewReceiptEvent(exception, time.Now().UTC()); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("exception receipt: %v, want ErrReceiptConflict", err)
	}

	cases := map[string]func(*ReceiptEvent){
		"kind":             func(e *ReceiptEvent) { e.Kind = "other" },
		"execution_id":     func(e *ReceiptEvent) { e.ExecutionID = "" },
		"event_id":         func(e *ReceiptEvent) { e.EventID = "" },
		"org_id":           func(e *ReceiptEvent) { e.OrgID = 0 },
		"model":            func(e *ReceiptEvent) { e.Model = "" },
		"negative tokens":  func(e *ReceiptEvent) { e.PromptTokens = -1 },
		"price_version_id": func(e *ReceiptEvent) { e.PriceVersionID = 0 },
		"negative charge":  func(e *ReceiptEvent) { e.ChargeMicro = -1 },
		"digest":           func(e *ReceiptEvent) { e.Digest = "short" },
	}
	for name, mutate := range cases {
		broken := valid
		mutate(&broken)
		payload, err := json.Marshal(broken)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseReceiptEvent(payload); err == nil {
			t.Fatalf("%s: malformed event was accepted", name)
		}
	}
	if _, err := ParseReceiptEvent([]byte("{not json")); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	// Trailing/leading whitespace in an identity field is a different id, not a
	// padded one: the digest would silently change.
	padded := valid
	padded.ExecutionID = " exec_1 "
	payload, _ := json.Marshal(padded)
	if _, err := ParseReceiptEvent(payload); err == nil {
		t.Fatal("a padded execution id must be refused")
	}
}

func TestValidateSettleReceiptRefusesUnverifiableDeliveries(t *testing.T) {
	base := SettleReceiptParams{
		Identity: Identity{
			ExecutionID: "exec_1", EventID: "evt_1", OrgID: 1, Model: "gpt-4o",
			PromptTokens: 1, OutputTokens: 1, TotalTokens: 2, PriceVersionID: 1, ChargeMicro: 5,
		},
		Digest: strings.Repeat("a", 64),
		Source: "1-1",
	}
	base.Digest = base.Identity.Digest()
	if err := ValidateSettleReceipt(NormalizeSettleReceipt(base)); err != nil {
		t.Fatalf("valid params refused: %v", err)
	}

	broken := base
	broken.Digest = ""
	if err := ValidateSettleReceipt(broken); err == nil {
		t.Fatal("a settlement without a digest must be refused before any transaction")
	}
	broken = base
	broken.Identity.ChargeMicro = -1
	if err := ValidateSettleReceipt(broken); !errors.Is(err, ErrUsageInvalid) {
		t.Fatalf("negative charge: %v, want ErrUsageInvalid", err)
	}
	broken = base
	broken.Identity.OrgID = 0
	if err := ValidateSettleReceipt(broken); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing org: %v, want ErrInvalidArgument", err)
	}
	long := base
	long.Source = strings.Repeat("s", MaxSettlementSourceLength+1)
	if err := ValidateSettleReceipt(long); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("oversized source: %v, want ErrInvalidArgument", err)
	}
}

func TestSettlementDebitDigestDependsOnTheReceiptIdentity(t *testing.T) {
	identity := Identity{
		ExecutionID: "exec_1", EventID: "evt_1", OrgID: 1, Model: "gpt-4o",
		PromptTokens: 1, OutputTokens: 2, TotalTokens: 3, PriceVersionID: 1, ChargeMicro: 5,
	}
	businessID := ReceiptBusinessID("exec_1", KindDebit)
	first := SettlementDebitDigest(identity, businessID)
	if len(first) != 64 || first != SettlementDebitDigest(identity, businessID) {
		t.Fatal("the debit digest must be a stable sha256")
	}
	changed := identity
	changed.ChargeMicro = 6
	if SettlementDebitDigest(changed, businessID) == first {
		t.Fatal("a changed charge must change the posting digest")
	}
	if ReceiptBusinessID("exec_1", KindDebit) == ExecutionBusinessID("exec_1", KindRelease) {
		t.Fatal("receipt and execution business ids must not collide")
	}
	if ReceiptBusinessID("exec_1", KindDebit) == ReceiptBusinessID("exec_1", KindRelease) {
		t.Fatal("the debit and release business ids must be namespaced by kind")
	}
}

func TestValidateClaimAndDispatchPolicy(t *testing.T) {
	if err := ValidateClaimReceipts(NormalizeClaimReceipts(ClaimReceiptsParams{})); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	if err := ValidateClaimReceipts(ClaimReceiptsParams{Limit: MaxDispatchBatchSize + 1, Lease: time.Second}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("oversized batch: %v, want ErrInvalidArgument", err)
	}
	if err := ValidateClaimReceipts(ClaimReceiptsParams{Limit: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing lease: %v, want ErrInvalidArgument", err)
	}
	// The claim's default batch is bounded, not unbounded.
	if got := NormalizeClaimReceipts(ClaimReceiptsParams{}).Limit; got != DefaultDispatchBatchSize {
		t.Fatalf("default batch=%d", got)
	}
	if _, ok := interface{}(DispatchResults).([]string); !ok {
		t.Fatal("DispatchResults must stay a closed vocabulary")
	}
	for _, result := range DispatchResults {
		if strings.TrimSpace(result) != result || result == "" {
			t.Fatalf("dispatch result %q is not a bounded label", result)
		}
	}
	for _, result := range SettlementResults {
		if strings.TrimSpace(result) != result || result == "" {
			t.Fatalf("settlement result %q is not a bounded label", result)
		}
	}
}

func TestValidateSettlementAnomalyVocabulary(t *testing.T) {
	valid := NormalizeSettlementAnomaly(SettlementAnomalyParams{
		Action:      ActionSettlementUnmatched,
		ExecutionID: "exec_1",
		EventID:     "evt_1",
		Reason:      "no receipt",
	})
	if err := ValidateSettlementAnomaly(valid); err != nil {
		t.Fatalf("valid anomaly refused: %v", err)
	}
	unknown := valid
	unknown.Action = "billing.settlement.something-else"
	if err := ValidateSettlementAnomaly(unknown); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unknown action: %v, want ErrInvalidArgument", err)
	}
	unidentified := valid
	unidentified.ExecutionID, unidentified.EventID = "", ""
	if err := ValidateSettlementAnomaly(unidentified); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("anonymous anomaly: %v, want ErrInvalidArgument", err)
	}
	reasonless := valid
	reasonless.Reason = "  "
	if err := ValidateSettlementAnomaly(reasonless); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("reasonless anomaly: %v, want ErrInvalidArgument", err)
	}
}

func TestReceiptBacklogAges(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	due := now.Add(-90 * time.Second)
	unsettled := now.Add(-10 * time.Minute)
	backlog := ReceiptBacklog{
		PendingDispatch:   2,
		Dispatched:        1,
		Settled:           5,
		Exception:         1,
		Due:               2,
		OldestDueAt:       &due,
		OldestUnsettledAt: &unsettled,
	}
	if backlog.Unsettled() != 4 {
		t.Fatalf("unsettled=%d, want 4", backlog.Unsettled())
	}
	if got := backlog.OldestDueAge(now); got != 90*time.Second {
		t.Fatalf("oldest due age=%s", got)
	}
	if got := backlog.OldestUnsettledAge(now); got != 10*time.Minute {
		t.Fatalf("oldest unsettled age=%s", got)
	}
	if got := (ReceiptBacklog{}).OldestDueAge(now); got != 0 {
		t.Fatalf("an empty backlog must not report an age, got %s", got)
	}
	// A clock skew (dispatch_after in the future but claimed as due) must clamp
	// to zero instead of reporting a negative age.
	future := now.Add(time.Hour)
	if got := (ReceiptBacklog{OldestDueAt: &future}).OldestDueAge(now); got != 0 {
		t.Fatalf("negative age clamped to %s, want 0", got)
	}
}

func TestTruncateReasonBoundsLastError(t *testing.T) {
	raw := strings.Repeat("x", MaxDispatchReasonLength+50)
	got := truncateReason(raw, MaxDispatchReasonLength)
	if len(got) != MaxDispatchReasonLength {
		t.Fatalf("len=%d, want %d", len(got), MaxDispatchReasonLength)
	}
	if !bytes.HasPrefix([]byte(raw), []byte(got)) {
		t.Fatal("truncation must drop the tail, not invent content")
	}
	if got := truncateReason("  spaced  ", 20); got != "spaced" {
		t.Fatalf("trim=%q", got)
	}
}
