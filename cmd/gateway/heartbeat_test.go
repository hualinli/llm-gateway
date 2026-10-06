package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
)

// T07B wiring tests: the Gateway's instance heartbeat and the "late usage after
// the decision was made" anomaly. The heartbeat loop is unit-tested with a fake
// store; the two end-to-end properties (a real row in gateway_instances, a
// durable anomaly row) are PostgreSQL-gated and skip cleanly.

type recordingHeartbeatStore struct {
	mu    sync.Mutex
	beats []billing.InstanceHeartbeatParams
	// failFirst makes the first N calls fail, so the loop's resilience is
	// observable without a real outage.
	failFirst int
}

func (s *recordingHeartbeatStore) HeartbeatInstance(_ context.Context, params billing.InstanceHeartbeatParams) (billing.GatewayInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats = append(s.beats, params)
	if len(s.beats) <= s.failFirst {
		return billing.GatewayInstance{}, errors.New("postgres is down")
	}
	return billing.GatewayInstance{
		InstanceID: params.InstanceID,
		StartedAt:  params.StartedAt,
		LastSeenAt: params.Now,
	}, nil
}

func (s *recordingHeartbeatStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.beats)
}

func (s *recordingHeartbeatStore) snapshot() []billing.InstanceHeartbeatParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]billing.InstanceHeartbeatParams, len(s.beats))
	copy(out, s.beats)
	return out
}

func waitForCondition(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestRunInstanceHeartbeatWritesImmediatelyAndStopsOnCancel(t *testing.T) {
	store := &recordingHeartbeatStore{}
	started := time.Now().UTC().Add(-time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runInstanceHeartbeat(ctx, store, "gw-unit-heartbeat", started, 5*time.Millisecond)
		close(done)
	}()

	// The first write is immediate: an instance that has just started must be
	// visible before its first tick, otherwise an admission it makes could look
	// ownerless.
	waitForCondition(t, time.Second, "the first heartbeat", func() bool { return store.count() >= 1 })
	waitForCondition(t, time.Second, "a second heartbeat", func() bool { return store.count() >= 2 })

	first := store.snapshot()[0]
	if first.InstanceID != "gw-unit-heartbeat" || !first.StartedAt.Equal(started) {
		t.Fatalf("first beat=%+v", first)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runInstanceHeartbeat did not return after cancellation")
	}
	after := store.count()
	time.Sleep(20 * time.Millisecond)
	if got := store.count(); got != after {
		t.Fatalf("heartbeats continued after cancellation: %d -> %d", after, got)
	}
}

func TestRunInstanceHeartbeatKeepsGoingAfterAFailure(t *testing.T) {
	store := &recordingHeartbeatStore{failFirst: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runInstanceHeartbeat(ctx, store, "gw-unit-failing", time.Now().UTC().Add(-time.Minute), 5*time.Millisecond)
		close(done)
	}()
	// The failure must not stop the loop: a live process has to keep trying, or
	// it will be judged dead once its lease expires.
	waitForCondition(t, 2*time.Second, "a successful heartbeat after a failure", func() bool { return store.count() >= 3 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop")
	}
}

func TestRunInstanceHeartbeatWithoutAStoreIsANoop(t *testing.T) {
	done := make(chan struct{})
	go func() {
		runInstanceHeartbeat(context.Background(), nil, "gw", time.Now(), time.Second)
		runInstanceHeartbeat(context.Background(), &recordingHeartbeatStore{}, "", time.Now(), time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a heartbeat loop without a store or an instance id must return immediately")
	}
}

func TestBoundAnomalyReasonKeepsUTF8AndTheColumnBound(t *testing.T) {
	short := "late usage event"
	if got := boundAnomalyReason(short); got != short {
		t.Fatalf("short reason=%q, want it unchanged", got)
	}
	padded := "  late usage event  "
	if got := boundAnomalyReason(padded); got != short {
		t.Fatalf("padded reason=%q, want %q", got, short)
	}
	// A multi-byte reason truncated at the column bound must stay valid UTF-8:
	// PostgreSQL text rejects an invalid byte sequence, so a mid-rune cut would
	// turn a diagnosis into a failed insert.
	long := strings.Repeat("延迟用量事件", 200)
	got := boundAnomalyReason(long)
	if len(got) > billing.MaxReasonLength {
		t.Fatalf("reason is %d bytes, want <= %d", len(got), billing.MaxReasonLength)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("reason is not valid UTF-8: %q", got)
	}
	if strings.TrimSpace(got) != got {
		t.Fatalf("reason has surrounding whitespace: %q", got)
	}
}

// gatewayBillingConfig is the smallest real billing configuration: a real PG
// pool, the documented defaults, and a short heartbeat interval so the tests do
// not wait for the production one.
func gatewayBillingConfig(dsn string) billingConfig {
	return billingConfig{
		Mode:                 billingPG,
		DSN:                  dsn,
		MaxOpenConns:         4,
		MaxIdleConns:         4,
		ConnMaxLifetime:      time.Minute,
		PingTimeout:          5 * time.Second,
		AdmissionTimeout:     10 * time.Second,
		ReceiptTimeout:       time.Second,
		ReceiptVerifyTimeout: time.Second,
		DecisionTimeout:      time.Second,
		FinalizeTimeout:      2 * time.Second,
		HeartbeatInterval:    20 * time.Millisecond,
		Policy:               defaultAdmissionPolicy(),
	}
}

// seedGatewayBillingOrg creates one funded organization and one price version,
// returning everything the T07B tests need to admit an execution.
func seedGatewayBillingOrg(t *testing.T, db *sql.DB, name string) (int64, int64, int64, billing.PriceVersion) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var userID int64
	if err := db.QueryRow(`INSERT INTO users
  (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ($1, 'hash', 'active', false, 1, $2, $2) RETURNING id`, name+"@example.com", now).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var orgID int64
	if err := db.QueryRow(`INSERT INTO organizations
  (type, name, status, owner_user_id, created_at, updated_at)
VALUES ('enterprise', $1, 'active', $2, $3, $3) RETURNING id`, name, userID, now).Scan(&orgID); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	var membershipID int64
	if err := db.QueryRow(`INSERT INTO memberships
  (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
VALUES ($1, $2, 'owner', 'active', NULL, $3, $3) RETURNING id`, orgID, userID, now).Scan(&membershipID); err != nil {
		t.Fatalf("seed owner membership: %v", err)
	}
	prices := billing.NewPGPriceVersionStore(db)
	price, err := prices.CreatePriceVersion(ctx, billing.CreatePriceVersionParams{
		Model:                 "gpt-4o",
		InputMicroPerMillion:  billing.MicrosPerMillionTokens,
		OutputMicroPerMillion: billing.MicrosPerMillionTokens,
		EffectiveAt:           now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("seed price: %v", err)
	}
	accounts := billing.NewPGAccountStore(db)
	reason := "gateway heartbeat test funding"
	adjust := billing.AdjustParams{
		OrgID:          orgID,
		AmountMicro:    1_000_000,
		Direction:      billing.DirectionCredit,
		Kind:           billing.KindAdjustment,
		Reason:         reason,
		IdempotencyKey: name + "-funding",
		Now:            now,
	}
	adjust.PayloadDigest = billing.AdjustmentDigest(orgID, adjust.AmountMicro, adjust.Direction, adjust.Kind, reason)
	if _, err := accounts.Adjust(ctx, adjust); err != nil {
		t.Fatalf("fund org: %v", err)
	}
	return orgID, userID, membershipID, price
}

func TestBillingAdapterHeartbeatsItsInstance(t *testing.T) {
	db, scoped := billingHTTPSchema(t)
	const instanceID = "gw-heartbeat-pg"
	adapter, closeBilling, err := openBilling(gatewayBillingConfig(scoped), instanceID, func(string) bool { return true })
	if err != nil {
		t.Fatalf("open billing: %v", err)
	}
	if adapter == nil {
		t.Fatal("billing adapter was not built")
	}

	accounts := billing.NewPGAccountStore(db)
	var stored billing.GatewayInstance
	waitForCondition(t, 5*time.Second, "the gateway_instances row", func() bool {
		instance, err := accounts.GetInstanceLiveness(context.Background(), instanceID)
		if err != nil {
			return false
		}
		stored = instance
		return true
	})
	if stored.StartedAt.After(stored.LastSeenAt) || stored.LastSeenAt.IsZero() {
		t.Fatalf("stored heartbeat=%+v", stored)
	}

	// Closing the billing pool stops the loop: liveness must not keep claiming to
	// be refreshed by a process that has released the pool.
	if err := closeBilling(); err != nil {
		t.Fatalf("close billing: %v", err)
	}
	before, err := accounts.GetInstanceLiveness(context.Background(), instanceID)
	if err != nil {
		t.Fatalf("read after close: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	after, err := accounts.GetInstanceLiveness(context.Background(), instanceID)
	if err != nil {
		t.Fatalf("read after wait: %v", err)
	}
	if !after.LastSeenAt.Equal(before.LastSeenAt) {
		t.Fatalf("heartbeats continued after close: %s -> %s", before.LastSeenAt, after.LastSeenAt)
	}
}

// TestFinalizationOfAReleasedExecutionIsRefusedAndAudited is the T07B end-to-end
// wiring claim: after a recovery release, the finalize gate refuses to charge the
// execution (so no [DONE] is released for it) and the late usage becomes a durable
// billing.settlement.exception anomaly instead of a log line.
func TestFinalizationOfAReleasedExecutionIsRefusedAndAudited(t *testing.T) {
	db, scoped := billingHTTPSchema(t)
	config := gatewayBillingConfig(scoped)
	adapter, closeBilling, err := openBilling(config, "gw-late-usage", func(string) bool { return true })
	if err != nil {
		t.Fatalf("open billing: %v", err)
	}
	defer func() { _ = closeBilling() }()

	orgID, userID, membershipID, price := seedGatewayBillingOrg(t, db, "gateway-late-usage")
	ctx := context.Background()
	accounts := billing.NewPGAccountStore(db)
	accounts.AdmissionTimeout = 10 * time.Second
	executionID := "exec_gateway_late_usage"
	params := billing.ReserveParams{
		ExecutionID:      executionID,
		OrgID:            orgID,
		MembershipID:     membershipID,
		UserID:           userID,
		KeyID:            "key-late",
		GatewayID:        "gw-late-usage-owner",
		Model:            price.Model,
		PriceVersionID:   price.ID,
		InputTokensUpper: 100,
		RequestID:        "req-late",
	}
	if _, err := accounts.Reserve(ctx, params); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := accounts.MarkExecuting(ctx, executionID, "req-late"); err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	// Age the execution past the window and record a stale owner heartbeat, then
	// release it the way the reaper does.
	past := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := db.Exec(`UPDATE billing_executions SET created_at = $2, updated_at = $2 WHERE execution_id = $1`, executionID, past); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.HeartbeatInstance(ctx, billing.InstanceHeartbeatParams{
		InstanceID: "gw-late-usage-owner",
		StartedAt:  time.Now().UTC().Add(-2 * time.Hour),
		Now:        time.Now().UTC().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.RecoverExecution(ctx, billing.RecoveryParams{
		ExecutionID:   executionID,
		Reason:        "recovery: owner is dead",
		MinAge:        5 * time.Minute,
		LivenessLease: time.Minute,
		ObservedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// The late terminal event reaches the finalize gate.
	err = adapter.Finalize(ctx, proxy.FinalizeRequest{
		ExecutionID: executionID,
		EventID:     "evt-late-usage",
		OrgID:       orgID,
		Model:       price.Model,
		Usage:       provider.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
	})
	if !errors.Is(err, billing.ErrExecutionState) {
		t.Fatalf("late finalize err=%v, want ErrExecutionState (a refused [DONE])", err)
	}

	var (
		action, targetID string
		detail           []byte
	)
	if err := db.QueryRow(`SELECT action, target_id, detail FROM console_audit_events
WHERE action = $1 AND target_id = $2`, billing.ActionSettlementException, executionID).
		Scan(&action, &targetID, &detail); err != nil {
		t.Fatalf("late-usage anomaly row: %v", err)
	}
	if !strings.Contains(string(detail), "finalize_gate") {
		t.Fatalf("anomaly detail=%s, want the finalize_gate source", detail)
	}
	if !strings.Contains(string(detail), "late usage") {
		t.Fatalf("anomaly detail=%s, want it to name the late usage", detail)
	}
	// No receipt was written for the released execution.
	if _, err := accounts.GetReceipt(ctx, executionID); !errors.Is(err, billing.ErrReceiptNotFound) {
		t.Fatalf("receipt lookup err=%v, want ErrReceiptNotFound", err)
	}
}
