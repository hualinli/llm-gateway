package metering

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/observability"
	"llm-gateway/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

// T07A end-to-end: a durable receipt is dispatched to the settlement stream by
// the real dispatcher (Redis publisher), consumed by the real settlement
// consumer, and lands as exactly one debit and one release with a consistent
// account projection. PostgreSQL and Redis are both required.
//
// The suite is gated on METERING_TEST_POSTGRES_DSN + METERING_TEST_REDIS_ADDR and
// each test builds its own PostgreSQL schema; only the two receipt stream keys are
// deleted in Redis.

type settlementE2E struct {
	db        *sql.DB
	scoped    string
	accounts  *billing.PGAccountStore
	prices    *billing.PGPriceVersionStore
	rdb       *redis.Client
	orgID     int64
	price     billing.PriceVersion
	publisher *ReceiptStreamPublisher
}

func newSettlementE2E(t *testing.T) *settlementE2E {
	t.Helper()
	dsn := os.Getenv("METERING_TEST_POSTGRES_DSN")
	addr := os.Getenv("METERING_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("set METERING_TEST_POSTGRES_DSN and METERING_TEST_REDIS_ADDR for the settlement end-to-end test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("METERING_TEST_POSTGRES_DSN must be a PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "settlement_e2e_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped := parsed.String()
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })

	// One user and one enterprise organization own the money facts.
	now := time.Now().UTC().Add(-time.Hour)
	var userID int64
	if err := db.QueryRow(`INSERT INTO users
  (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ($1, 'hash', 'active', true, 1, $2, $2) RETURNING id`, "settlement-"+id+"@example.com", now).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var orgID int64
	if err := db.QueryRow(`INSERT INTO organizations
  (type, name, status, owner_user_id, created_at, updated_at)
VALUES ('enterprise', $1, 'active', $2, $3, $3) RETURNING id`, "settlement-"+id, userID, now).Scan(&orgID); err != nil {
		t.Fatalf("seed organization: %v", err)
	}

	accounts := billing.NewPGAccountStore(db)
	accounts.AdmissionTimeout = 30 * time.Second
	accounts.FinalizeTimeout = 10 * time.Second
	accounts.ReceiptVerifyTimeout = 5 * time.Second
	accounts.DispatchTimeout = 10 * time.Second
	accounts.SettlementTimeout = 10 * time.Second
	accounts.SettlementVerifyTimeout = 5 * time.Second
	prices := billing.NewPGPriceVersionStore(db)
	price, err := prices.CreatePriceVersion(context.Background(), billing.CreatePriceVersionParams{
		Model:                 "gpt-4o",
		InputMicroPerMillion:  billing.MicrosPerMillionTokens,
		OutputMicroPerMillion: billing.MicrosPerMillionTokens,
		EffectiveAt:           now,
	})
	if err != nil {
		t.Fatalf("create price version: %v", err)
	}
	// Fund the wallet: maintenance credits are adjustments, never reservations.
	funding := billing.AdjustParams{
		OrgID:          orgID,
		AmountMicro:    10_000_000,
		Direction:      billing.DirectionCredit,
		Kind:           billing.KindAdjustment,
		Reason:         "end-to-end funding",
		IdempotencyKey: "settlement-e2e-funding-" + id,
		Now:            now,
	}
	funding.PayloadDigest = billing.AdjustmentDigest(orgID, funding.AmountMicro, funding.Direction, funding.Kind, funding.Reason)
	if _, err := accounts.Adjust(context.Background(), funding); err != nil {
		t.Fatalf("fund the account: %v", err)
	}

	rdb := newRedisIntegrationClient(t)
	t.Cleanup(func() { _ = rdb.Close() })
	resetReceiptStreams(t, rdb)
	t.Cleanup(func() { resetReceiptStreams(t, rdb) })

	return &settlementE2E{
		db: db, scoped: scoped, accounts: accounts, prices: prices, rdb: rdb,
		orgID: orgID, price: price, publisher: NewReceiptStreamPublisher(rdb),
	}
}

func resetReceiptStreams(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Del(ctx, ReceiptStreamName, ReceiptDLQStreamName).Err(); err != nil {
		t.Fatal(err)
	}
}

// admitAndFinalize walks the T06 path: reserve (200 micro upper bound), mark
// executing and commit the durable receipt (15 micro charge for 10+5 tokens).
func (e *settlementE2E) admitAndFinalize(t *testing.T, executionID, eventID string, prompt, output int64) billing.Receipt {
	t.Helper()
	ctx := context.Background()
	params := billing.ReserveParams{
		ExecutionID:       executionID,
		OrgID:             e.orgID,
		KeyID:             "key-e2e",
		GatewayID:         "gateway-e2e",
		Model:             e.price.Model,
		PriceVersionID:    e.price.ID,
		InputTokensUpper:  100,
		OutputTokensUpper: 100,
		RequestID:         "req-" + executionID,
		Now:               time.Now().UTC().Add(-time.Minute),
	}
	if _, err := e.accounts.Reserve(ctx, params); err != nil {
		t.Fatalf("reserve %s: %v", executionID, err)
	}
	if _, err := e.accounts.MarkExecuting(ctx, executionID, "req-"+executionID); err != nil {
		t.Fatalf("mark executing %s: %v", executionID, err)
	}
	result, err := e.accounts.FinalizeReceipt(ctx, billing.FinalizeReceiptParams{
		ExecutionID:  executionID,
		EventID:      eventID,
		PromptTokens: prompt,
		OutputTokens: output,
		TotalTokens:  prompt + output,
		RequestID:    "req-" + executionID,
		TraceID:      "trace-" + executionID,
		OrgID:        e.orgID,
		Model:        e.price.Model,
	})
	if err != nil {
		t.Fatalf("finalize %s: %v", executionID, err)
	}
	return result.Receipt
}

func (e *settlementE2E) receipt(t *testing.T, executionID string) billing.Receipt {
	t.Helper()
	receipt, err := e.accounts.GetReceipt(context.Background(), executionID)
	if err != nil {
		t.Fatalf("get receipt %s: %v", executionID, err)
	}
	return receipt
}

func (e *settlementE2E) countEntries(t *testing.T, kind string) int64 {
	t.Helper()
	var count int64
	if err := e.db.QueryRow(`SELECT count(*) FROM billing_entries WHERE org_id = $1 AND kind = $2`, e.orgID, kind).Scan(&count); err != nil {
		t.Fatalf("count %s entries: %v", kind, err)
	}
	return count
}

func (e *settlementE2E) waitSettled(t *testing.T, executionID string) billing.Receipt {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := e.accounts.GetReceipt(context.Background(), executionID)
		if err == nil && receipt.Status == billing.ReceiptStatusSettled {
			return receipt
		}
		time.Sleep(20 * time.Millisecond)
	}
	receipt, err := e.accounts.GetReceipt(context.Background(), executionID)
	t.Fatalf("receipt %s did not settle in time: %+v (%v)", executionID, receipt, err)
	return billing.Receipt{}
}

func (e *settlementE2E) waitPending(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lag, err := StreamLag(context.Background(), e.rdb)
		if err == nil && lag.Pending == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	lag, err := StreamLag(context.Background(), e.rdb)
	t.Fatalf("settlement pending never reached %d: %+v (%v)", want, lag, err)
}

// startConsumer runs only the settlement consumer (the tests that inject their
// own envelopes must not also run the dispatcher, or the receipt's own event
// would be delivered alongside the injected one).
func (e *settlementE2E) startConsumer(ctx context.Context, t *testing.T, consumerOptions SettlementConsumerOptions) *SettlementConsumer {
	t.Helper()
	if consumerOptions.ReadBlock <= 0 {
		consumerOptions.ReadBlock = 100 * time.Millisecond
	}
	if consumerOptions.ReadCount <= 0 {
		consumerOptions.ReadCount = 10
	}
	if consumerOptions.ReclaimEvery <= 0 {
		consumerOptions.ReclaimEvery = 50 * time.Millisecond
	}
	if consumerOptions.PendingMinIdle <= 0 {
		consumerOptions.PendingMinIdle = 50 * time.Millisecond
	}
	consumerOptions.Logger = func(string, ...any) {}
	consumer := NewSettlementConsumer(e.rdb, e.accounts, consumerOptions)
	if err := consumer.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure settlement group: %v", err)
	}
	go consumer.Run(ctx)
	return consumer
}

// startPipeline runs the real dispatcher and settlement consumer until the test
// ends: the full receipt → transport → settlement loop.
func (e *settlementE2E) startPipeline(ctx context.Context, t *testing.T, consumerOptions SettlementConsumerOptions) (*billing.ReceiptDispatcher, *SettlementConsumer) {
	t.Helper()
	dispatcher, err := billing.NewReceiptDispatcher(e.accounts, e.publisher, billing.DispatcherOptions{
		BatchSize:    8,
		Concurrency:  2,
		PollInterval: 50 * time.Millisecond,
		Lease:        5 * time.Second,
		MaxAttempts:  5,
		Logger:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	go dispatcher.Run(ctx)
	return dispatcher, e.startConsumer(ctx, t, consumerOptions)
}

// TestSettlementEndToEndReceiptToAccountProjection is the full vertical loop:
// durable receipt → dispatcher (Redis) → settlement consumer → debit + release →
// execution settled → projection equals the ledger.
func TestSettlementEndToEndReceiptToAccountProjection(t *testing.T) {
	env := newSettlementE2E(t)
	receipt := env.admitAndFinalize(t, "exec_e2e", "evt_e2e", 10, 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher, consumer := env.startPipeline(ctx, t, SettlementConsumerOptions{Name: "e2e-consumer"})

	settled := env.waitSettled(t, "exec_e2e")
	if settled.SettledAt == nil || settled.Digest != receipt.Digest {
		t.Fatalf("settled receipt=%+v", settled)
	}
	env.waitPending(t, 0)

	execution, err := env.accounts.GetExecution(context.Background(), "exec_e2e")
	if err != nil || execution.Status != billing.StatusSettled {
		t.Fatalf("execution=(%+v, %v)", execution, err)
	}
	if got := env.countEntries(t, billing.KindDebit); got != 1 {
		t.Fatalf("debit entries=%d, want exactly 1", got)
	}
	if got := env.countEntries(t, billing.KindRelease); got != 1 {
		t.Fatalf("release entries=%d, want exactly 1", got)
	}
	account, err := env.accounts.GetAccount(context.Background(), env.orgID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if account.PostedBalanceMicro != 10_000_000-15 || account.ReservedMicro != 0 {
		t.Fatalf("account=%+v", account)
	}
	check, err := env.accounts.RecalculateProjection(context.Background(), env.orgID)
	if err != nil || !check.Matches {
		t.Fatalf("projection=(%+v, %v)", check, err)
	}
	health := consumer.Health()
	if health.Settled != 1 || health.Replayed != 0 {
		t.Fatalf("consumer health=%+v", health)
	}
	if dispatcher.Health().LastError != "" {
		t.Fatalf("dispatcher error: %+v", dispatcher.Health())
	}
}

// TestSettlementRedeliveryAndCrashedConsumerConvergeToOneDebit is the
// at-least-once acceptance case: two envelopes for one receipt, one of them left
// pending by a consumer that "crashed" before it could acknowledge, still settle
// to exactly one debit.
func TestSettlementRedeliveryAndCrashedConsumerConvergeToOneDebit(t *testing.T) {
	env := newSettlementE2E(t)
	receipt := env.admitAndFinalize(t, "exec_redeliver", "evt_redeliver", 10, 5)

	event, err := billing.NewReceiptEvent(receipt, time.Now().UTC())
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	payload, err := billing.MarshalReceiptEvent(event)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The group must exist before the "crashed" consumer can take the messages.
	group := NewSettlementConsumer(env.rdb, env.accounts, SettlementConsumerOptions{Name: "crashed-consumer"})
	if err := group.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	// Two deliveries of the same fact: the redelivery a lost ACK would produce.
	for i := 0; i < 2; i++ {
		if err := env.rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: ReceiptStreamName,
			Values: map[string]any{"kind": billing.ReceiptEventKind, "execution_id": receipt.ExecutionID, "data": string(payload)},
		}).Err(); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	// A consumer reads both and dies before acknowledging anything: the messages
	// stay pending for the whole MinIdle window.
	claimed, err := env.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    SettlementGroupName,
		Consumer: "crashed-consumer",
		Streams:  []string{ReceiptStreamName, ">"},
		Count:    10,
	}).Result()
	if err != nil {
		t.Fatalf("crashed consumer read: %v", err)
	}
	if len(claimed) != 1 || len(claimed[0].Messages) != 2 {
		t.Fatalf("crashed consumer claimed=%+v, want 2 pending messages", claimed)
	}

	_, consumer := env.startPipeline(ctx, t, SettlementConsumerOptions{Name: "recovering-consumer", PendingMinIdle: time.Millisecond})
	env.waitSettled(t, "exec_redeliver")
	env.waitPending(t, 0)

	if got := env.countEntries(t, billing.KindDebit); got != 1 {
		t.Fatalf("debit entries=%d, want exactly 1", got)
	}
	if got := env.countEntries(t, billing.KindRelease); got != 1 {
		t.Fatalf("release entries=%d, want exactly 1", got)
	}
	check, err := env.accounts.RecalculateProjection(context.Background(), env.orgID)
	if err != nil || !check.Matches {
		t.Fatalf("projection=(%+v, %v)", check, err)
	}
	health := consumer.Health()
	if health.Settled != 1 || health.Replayed < 1 {
		t.Fatalf("consumer health=%+v, want one settlement and at least one replay", health)
	}
}

// TestSettlementConflictingEnvelopeIsVisibleAndMovesNoMoney drives a payload
// that is self-consistent but is not the receipt's payload: the delivery must be
// refused, recorded and acknowledged, and no money may move.
func TestSettlementConflictingEnvelopeIsVisibleAndMovesNoMoney(t *testing.T) {
	env := newSettlementE2E(t)
	receipt := env.admitAndFinalize(t, "exec_conflict_e2e", "evt_conflict_e2e", 10, 5)

	// The same execution, a different usage: a *different* immutable payload.
	forged := receipt.Identity()
	forged.PromptTokens += 1
	forged.TotalTokens += 1
	forged.ChargeMicro += 1
	event := billing.ReceiptEvent{
		Kind:           billing.ReceiptEventKind,
		ExecutionID:    forged.ExecutionID,
		EventID:        forged.EventID,
		OrgID:          forged.OrgID,
		UserID:         forged.UserID,
		KeyID:          forged.KeyID,
		Model:          forged.Model,
		PromptTokens:   forged.PromptTokens,
		OutputTokens:   forged.OutputTokens,
		TotalTokens:    forged.TotalTokens,
		PriceVersionID: forged.PriceVersionID,
		ChargeMicro:    forged.ChargeMicro,
		Digest:         forged.Digest(),
		OccurredAt:     receipt.OccurredAt,
		PublishedAt:    time.Now().UTC(),
	}
	payload, err := billing.MarshalReceiptEvent(event)
	if err != nil {
		t.Fatalf("marshal forged envelope: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := env.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: ReceiptStreamName,
		Values: map[string]any{"kind": billing.ReceiptEventKind, "execution_id": event.ExecutionID, "data": string(payload)},
	}).Err(); err != nil {
		t.Fatalf("publish forged envelope: %v", err)
	}

	consumer := env.startConsumer(ctx, t, SettlementConsumerOptions{Name: "conflict-consumer"})

	deadline := time.Now().Add(10 * time.Second)
	var anomalies []billing.SettlementAnomaly
	for time.Now().Before(deadline) {
		anomalies, err = env.accounts.ListSettlementAnomalies(context.Background(), 10)
		if err == nil && len(anomalies) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(anomalies) != 1 || anomalies[0].Action != billing.ActionSettlementConflict {
		t.Fatalf("anomalies=%+v", anomalies)
	}
	if anomalies[0].ExecutionID != "exec_conflict_e2e" {
		t.Fatalf("anomaly=%+v", anomalies[0])
	}
	env.waitPending(t, 0)
	if got := env.countEntries(t, billing.KindDebit); got != 0 {
		t.Fatalf("a conflicting delivery moved money: %d debits", got)
	}
	if stored := env.receipt(t, "exec_conflict_e2e"); stored.Status != billing.ReceiptStatusPendingDispatch {
		t.Fatalf("receipt after a conflict=%+v", stored)
	}
	if health := consumer.Health(); health.Conflicts != 1 || health.Settled != 0 {
		t.Fatalf("consumer health=%+v", health)
	}
	// The evidence is also in the bounded DLQ.
	if dlq, err := env.rdb.XLen(context.Background(), ReceiptDLQStreamName).Result(); err != nil || dlq != 1 {
		t.Fatalf("receipt DLQ length=(%d, %v), want 1", dlq, err)
	}
}

// TestSettlementMissingReceiptIsRecordedAsUnmatchedAndCreatesNoMoney is the
// legacy-event path: no receipt means no chargeable fact.
func TestSettlementMissingReceiptIsRecordedAsUnmatchedAndCreatesNoMoney(t *testing.T) {
	env := newSettlementE2E(t)
	identity := billing.Identity{
		ExecutionID:    "exec_legacy_e2e",
		EventID:        "evt_legacy_e2e",
		OrgID:          env.orgID,
		Model:          env.price.Model,
		PromptTokens:   10,
		OutputTokens:   5,
		TotalTokens:    15,
		PriceVersionID: env.price.ID,
		ChargeMicro:    15,
	}
	event := billing.ReceiptEvent{
		Kind:           billing.ReceiptEventKind,
		ExecutionID:    identity.ExecutionID,
		EventID:        identity.EventID,
		OrgID:          identity.OrgID,
		Model:          identity.Model,
		PromptTokens:   identity.PromptTokens,
		OutputTokens:   identity.OutputTokens,
		TotalTokens:    identity.TotalTokens,
		PriceVersionID: identity.PriceVersionID,
		ChargeMicro:    identity.ChargeMicro,
		Digest:         identity.Digest(),
		OccurredAt:     time.Now().UTC().Add(-time.Minute),
		PublishedAt:    time.Now().UTC(),
	}
	payload, err := billing.MarshalReceiptEvent(event)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := env.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: ReceiptStreamName,
		Values: map[string]any{"kind": billing.ReceiptEventKind, "execution_id": event.ExecutionID, "data": string(payload)},
	}).Err(); err != nil {
		t.Fatalf("publish legacy envelope: %v", err)
	}

	consumer := env.startConsumer(ctx, t, SettlementConsumerOptions{Name: "legacy-consumer"})

	deadline := time.Now().Add(10 * time.Second)
	var anomalies []billing.SettlementAnomaly
	for time.Now().Before(deadline) {
		anomalies, err = env.accounts.ListSettlementAnomalies(context.Background(), 10)
		if err == nil && len(anomalies) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(anomalies) != 1 || anomalies[0].Action != billing.ActionSettlementUnmatched {
		t.Fatalf("anomalies=%+v", anomalies)
	}
	env.waitPending(t, 0)
	if got := env.countEntries(t, billing.KindDebit); got != 0 {
		t.Fatalf("a legacy event moved money: %d debits", got)
	}
	if got := env.countEntries(t, billing.KindRelease); got != 0 {
		t.Fatalf("a legacy event released money: %d releases", got)
	}
	if health := consumer.Health(); health.Unmatched != 1 || health.Settled != 0 {
		t.Fatalf("consumer health=%+v", health)
	}
	account, err := env.accounts.GetAccount(context.Background(), env.orgID)
	if err != nil || account.PostedBalanceMicro != 10_000_000 || account.ReservedMicro != 0 {
		t.Fatalf("account=(%+v, %v)", account, err)
	}
}

// blockedAnomalySettlementStore pauses the consumer after its refusal audit is
// committed. That creates the precise window where an operator can retry a
// transport-poisoned receipt before the stale consumer would have marked it as
// a settlement exception.
type blockedAnomalySettlementStore struct {
	base             SettlementStore
	anomalyRecorded  chan struct{}
	resumeAfterAudit chan struct{}
	markException    int
}

func (s *blockedAnomalySettlementStore) SettleReceipt(ctx context.Context, params billing.SettleReceiptParams) (billing.SettlementResult, error) {
	return s.base.SettleReceipt(ctx, params)
}

func (s *blockedAnomalySettlementStore) MarkReceiptException(ctx context.Context, params billing.ReceiptExceptionParams) (billing.ReceiptResult, error) {
	s.markException++
	return s.base.MarkReceiptException(ctx, params)
}

func (s *blockedAnomalySettlementStore) RecordSettlementAnomaly(ctx context.Context, params billing.SettlementAnomalyParams) error {
	if err := s.base.RecordSettlementAnomaly(ctx, params); err != nil {
		return err
	}
	close(s.anomalyRecorded)
	<-s.resumeAfterAudit
	return nil
}

// TestStaleSettlementRefusalCannotRePoisonOperatorRetriedReceipt fixes the
// ordering that used to lose a valid transport retry: a delivered copy is
// pending while the dispatcher marks the receipt poisoned; the stale consumer
// reads that exception; the operator retries it; then the old consumer resumes.
func TestStaleSettlementRefusalCannotRePoisonOperatorRetriedReceipt(t *testing.T) {
	env := newSettlementE2E(t)
	ctx := context.Background()
	receipt := env.admitAndFinalize(t, "exec_stale_refusal", "evt_stale_refusal", 10, 5)
	event, err := billing.NewReceiptEvent(receipt, time.Now().UTC())
	if err != nil {
		t.Fatalf("build receipt event: %v", err)
	}
	payload, err := billing.MarshalReceiptEvent(event)
	if err != nil {
		t.Fatalf("marshal receipt event: %v", err)
	}
	messageID, err := env.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: ReceiptStreamName,
		Values: map[string]any{"kind": billing.ReceiptEventKind, "execution_id": receipt.ExecutionID, "data": string(payload)},
	}).Result()
	if err != nil {
		t.Fatalf("publish stale delivery: %v", err)
	}

	// Model a publish whose Redis response was lost: the envelope exists, then
	// the exhausted dispatcher records a durable transport exception.
	now := time.Now().UTC()
	claimed, err := env.accounts.ClaimDispatchableReceipts(ctx, billing.ClaimReceiptsParams{Now: now, Limit: 10, Lease: time.Minute})
	if err != nil || len(claimed) != 1 || claimed[0].Receipt.ExecutionID != receipt.ExecutionID {
		t.Fatalf("claim for poison transition=(%+v, %v)", claimed, err)
	}
	poisoned, err := env.accounts.RecordDispatchFailure(ctx, billing.DispatchFailureParams{
		ExecutionID: receipt.ExecutionID,
		Lease:       claimed[0].Lease,
		Reason:      "publish: response timed out after append",
		Source:      messageID,
		Poison:      true,
		Now:         now,
	})
	if err != nil || poisoned.Status != billing.ReceiptStatusException {
		t.Fatalf("poison receipt=(%+v, %v)", poisoned, err)
	}

	store := &blockedAnomalySettlementStore{
		base:             env.accounts,
		anomalyRecorded:  make(chan struct{}),
		resumeAfterAudit: make(chan struct{}),
	}
	consumer := NewSettlementConsumer(env.rdb, store, SettlementConsumerOptions{Name: "stale-refusal-consumer"})
	if err := consumer.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure settlement group: %v", err)
	}
	streams, err := env.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: SettlementGroupName, Consumer: consumer.Name(), Streams: []string{ReceiptStreamName, ">"}, Count: 1,
	}).Result()
	if err != nil || len(streams) != 1 || len(streams[0].Messages) != 1 || streams[0].Messages[0].ID != messageID {
		t.Fatalf("read stale delivery=(%+v, %v)", streams, err)
	}

	done := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(store.resumeAfterAudit)
		}
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("settlement consumer did not finish")
		}
	}()
	go func() {
		defer close(done)
		consumer.handle(ctx, streams[0].Messages[0])
	}()
	select {
	case <-store.anomalyRecorded:
	case <-time.After(15 * time.Second):
		t.Fatal("consumer did not commit its settlement refusal anomaly")
	}

	var actorUserID int64
	if err := env.db.QueryRowContext(ctx, `SELECT owner_user_id FROM organizations WHERE id=$1`, env.orgID).Scan(&actorUserID); err != nil {
		t.Fatalf("find retry actor: %v", err)
	}
	retried, err := env.accounts.RetryDispatchException(ctx, billing.DispatchRetryParams{
		ExecutionID: receipt.ExecutionID,
		ActorUserID: actorUserID,
		Reason:      "verified Redis append after timeout",
		RequestID:   "retry-after-stale-refusal",
		Now:         time.Now().UTC(),
	})
	if err != nil || retried.Status != billing.ReceiptStatusPendingDispatch {
		t.Fatalf("operator retry=(%+v, %v)", retried, err)
	}
	close(store.resumeAfterAudit)
	released = true
	<-done

	stored := env.receipt(t, receipt.ExecutionID)
	if stored.Status != billing.ReceiptStatusPendingDispatch || stored.LastError != nil || stored.Digest != receipt.Digest {
		t.Fatalf("stale consumer changed retried receipt: %+v", stored)
	}
	if store.markException != 0 {
		t.Fatalf("stale consumer called MarkReceiptException %d times", store.markException)
	}
	if got := env.countEntries(t, billing.KindDebit); got != 0 {
		t.Fatalf("stale delivery moved money: %d debits", got)
	}
	if got := env.countEntries(t, billing.KindRelease); got != 0 {
		t.Fatalf("stale delivery released money: %d releases", got)
	}
	if got := countEntriesForAction(t, env.db, billing.ActionSettlementException, receipt.ExecutionID); got != 1 {
		t.Fatalf("settlement refusal audit rows=%d, want 1", got)
	}
	if got := countEntriesForAction(t, env.db, billing.ActionReceiptDispatchRetried, receipt.ExecutionID); got != 1 {
		t.Fatalf("operator retry audit rows=%d, want 1", got)
	}
	env.waitPending(t, 0)
}

func countEntriesForAction(t *testing.T, db *sql.DB, action, targetID string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(`SELECT count(*) FROM console_audit_events WHERE action=$1 AND target_id=$2`, action, targetID).Scan(&count); err != nil {
		t.Fatalf("count %s audit rows: %v", action, err)
	}
	return count
}
