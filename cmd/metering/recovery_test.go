package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/metering"
)

// T07B metering-side tests: the recovery knobs, the "never pretend without
// durability" rule, and the two read-only operator endpoints (the 24h human
// queue and the policy/health status). They use a scripted store, so they need
// neither PostgreSQL nor Redis.

func recoveryLookup(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestRecoveryConfigFromEnvDefaultsAndRefusals(t *testing.T) {
	base, err := recoveryConfigFromEnv(recoveryLookup(nil))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if base.Mode != recoveryModeAuto ||
		base.BatchSize != billing.DefaultRecoveryBatchSize ||
		base.Interval != billing.DefaultRecoveryInterval ||
		base.MaxExecutionDuration != billing.DefaultRecoveryMaxExecutionDuration ||
		base.Grace != billing.DefaultRecoveryGrace ||
		base.LivenessLease != billing.DefaultLivenessLease ||
		base.ReviewSLA != billing.DefaultHumanReviewSLA ||
		base.Timeout != billing.DefaultRecoveryTimeout {
		t.Fatalf("defaults=%+v", base)
	}

	if off, err := recoveryConfigFromEnv(recoveryLookup(map[string]string{"METERING_RECOVERY": "off"})); err != nil || off.Mode != recoveryModeOff {
		t.Fatalf("off=(%+v, %v)", off, err)
	}
	if on, err := recoveryConfigFromEnv(recoveryLookup(map[string]string{"METERING_RECOVERY": "ON"})); err != nil || on.Mode != recoveryModeOn {
		t.Fatalf("on=(%+v, %v)", on, err)
	}
	if _, err := recoveryConfigFromEnv(recoveryLookup(map[string]string{"METERING_RECOVERY": "of"})); err == nil {
		t.Fatal("a typo in METERING_RECOVERY must be refused, not silently defaulted")
	}

	tuned, err := recoveryConfigFromEnv(recoveryLookup(map[string]string{
		"METERING_RECOVERY_BATCH":         "8",
		"METERING_RECOVERY_INTERVAL":      "15s",
		"METERING_RECOVERY_MAX_EXECUTION": "45m",
		"METERING_RECOVERY_GRACE":         "5m",
		"METERING_RECOVERY_LEASE":         "3m",
		"METERING_RECOVERY_SLA":           "12h",
		"METERING_RECOVERY_TIMEOUT":       "20s",
	}))
	if err != nil {
		t.Fatalf("tuned: %v", err)
	}
	if tuned.BatchSize != 8 || tuned.Interval != 15*time.Second || tuned.MaxExecutionDuration != 45*time.Minute ||
		tuned.Grace != 5*time.Minute || tuned.LivenessLease != 3*time.Minute ||
		tuned.ReviewSLA != 12*time.Hour || tuned.Timeout != 20*time.Second {
		t.Fatalf("tuned=%+v", tuned)
	}

	for name, values := range map[string]map[string]string{
		"zero batch":            {"METERING_RECOVERY_BATCH": "0"},
		"negative interval":     {"METERING_RECOVERY_INTERVAL": "-1s"},
		"lease below heartbeat": {"METERING_RECOVERY_LEASE": "5s"},
		"oversized sla":         {"METERING_RECOVERY_SLA": "1000h"},
		"non-duration":          {"METERING_RECOVERY_GRACE": "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := recoveryConfigFromEnv(recoveryLookup(values)); err == nil {
				t.Fatal("a bound that cannot be honored must be a startup error, not a silent fallback")
			}
		})
	}
}

// TestStartRecoveryRefusesToPretendWithoutDurability: without PostgreSQL the
// janitor cannot judge anything, so it is explicitly disabled (auto) or a startup
// error (on / production), never a silently missing recovery path.
func TestStartRecoveryRefusesToPretendWithoutDurability(t *testing.T) {
	ledger := metering.NewMemoryLedger()
	ctx := context.Background()

	off, err := startRecovery(ctx, ledger, recoveryConfig{Mode: recoveryModeOff, ReviewSLA: billing.DefaultHumanReviewSLA})
	if err != nil || off != nil {
		t.Fatalf("off=(%+v, %v), want a disabled runtime", off, err)
	}
	auto, err := startRecovery(ctx, ledger, recoveryConfig{Mode: recoveryModeAuto, ReviewSLA: billing.DefaultHumanReviewSLA})
	if err != nil || auto != nil {
		t.Fatalf("auto with an in-memory ledger=(%+v, %v), want disabled", auto, err)
	}
	if _, err := startRecovery(ctx, ledger, recoveryConfig{Mode: recoveryModeOn, ReviewSLA: billing.DefaultHumanReviewSLA}); err == nil {
		t.Fatal("METERING_RECOVERY=on without PostgreSQL must fail closed")
	}
	if _, err := startRecovery(ctx, ledger, recoveryConfig{Mode: recoveryModeAuto, RequirePostgres: true, ReviewSLA: billing.DefaultHumanReviewSLA}); err == nil {
		t.Fatal("production (METERING_REQUIRE_POSTGRES) without PostgreSQL must fail closed")
	}
}

// fakeRecoveryStore is a scripted recoveryStore for the HTTP handlers.
type fakeRecoveryStore struct {
	backlog    billing.ReviewBacklog
	page       billing.ReviewQueuePage
	gap        billing.RecoveryGap
	lastQueue  billing.ReviewQueueParams
	queueErr   error
	backlogErr error
	gapErr     error
	// T12: the scripted manual disposition.
	manual       billing.ManualDispositionResult
	manualErr    error
	lastManual   billing.ManualDispositionParams
	manualCalls  int
	lastReleased string
	lastSettled  string
}

func (s *fakeRecoveryStore) ListRecoveryCandidates(context.Context, billing.RecoveryCandidateScanParams) ([]billing.Execution, error) {
	return nil, nil
}

func (s *fakeRecoveryStore) RecoverExecution(context.Context, billing.RecoveryParams) (billing.RecoveryResult, error) {
	return billing.RecoveryResult{}, nil
}

func (s *fakeRecoveryStore) RecoveryGap(context.Context, billing.RecoveryGapParams) (billing.RecoveryGap, error) {
	return s.gap, s.gapErr
}

func (s *fakeRecoveryStore) ReviewRequiredBacklog(context.Context, billing.ReviewBacklogParams) (billing.ReviewBacklog, error) {
	return s.backlog, s.backlogErr
}

func (s *fakeRecoveryStore) ListReviewRequiredExecutions(_ context.Context, params billing.ReviewQueueParams) (billing.ReviewQueuePage, error) {
	s.lastQueue = params
	return s.page, s.queueErr
}

func (s *fakeRecoveryStore) ReleaseExecutionManually(_ context.Context, params billing.ManualDispositionParams) (billing.ManualDispositionResult, error) {
	s.manualCalls++
	s.lastManual = params
	s.lastReleased = params.ExecutionID
	result := s.manual
	result.Action = billing.ManualActionRelease
	return result, s.manualErr
}

func (s *fakeRecoveryStore) SettleExecutionManually(_ context.Context, params billing.ManualDispositionParams) (billing.ManualDispositionResult, error) {
	s.manualCalls++
	s.lastManual = params
	s.lastSettled = params.ExecutionID
	result := s.manual
	result.Action = billing.ManualActionSettle
	return result, s.manualErr
}

func newRecoveryTestRuntime(store recoveryStore, loop *billing.ExecutionRecovery) *recoveryRuntime {
	return &recoveryRuntime{store: store, loop: loop, reviewSLA: billing.DefaultHumanReviewSLA}
}

func decodeRecoveryJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %s: %v", recorder.Body.String(), err)
	}
	return payload
}

func TestRecoveryReviewQueueHandlerPagesWithAgeAndSLA(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-30 * time.Hour)
	younger := now.Add(-2 * time.Hour)
	reason := "usage missing"
	store := &fakeRecoveryStore{
		backlog: billing.ReviewBacklog{Open: 2, OldestAt: &oldest, Breaches: 1, SLA: billing.DefaultHumanReviewSLA},
		gap:     billing.RecoveryGap{OwnerUnknown: 4, OldestAt: &oldest},
		page: billing.ReviewQueuePage{
			Now: now,
			Items: []billing.ReviewQueueItem{
				{Execution: billing.Execution{
					ExecutionID: "exec_old", OrgID: 7, Model: "gpt-4o", Status: billing.StatusReviewRequired,
					ReservedMicro: 1500, PriceVersionID: 3, CreatedAt: oldest, UpdatedAt: oldest,
					RequestID: stringPtr("req-old"), TerminalReason: &reason,
				}, Age: 30 * time.Hour, Breached: true},
				{Execution: billing.Execution{
					ExecutionID: "exec_new", OrgID: 7, Model: "gpt-4o", Status: billing.StatusReviewRequired,
					ReservedMicro: 0, PriceVersionID: 3, CreatedAt: younger, UpdatedAt: younger,
				}, Age: 2 * time.Hour},
			},
			NextCursor: &billing.ReviewCursor{UpdatedAt: younger, ExecutionID: "exec_new"},
			Totals:     billing.ReviewBacklog{Open: 2, OldestAt: &oldest, Breaches: 1, SLA: billing.DefaultHumanReviewSLA},
		},
	}
	runtime := newRecoveryTestRuntime(store, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/recovery/review-required?limit=2&sla=24h", nil)
	runtime.handleReviewQueue(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	payload := decodeRecoveryJSON(t, recorder)
	if payload["ok"] != true {
		t.Fatalf("payload=%v", payload)
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items=%v", payload["items"])
	}
	first := items[0].(map[string]any)
	if first["execution_id"] != "exec_old" || first["sla_breached"] != true {
		t.Fatalf("first item=%v", first)
	}
	if first["reserved_micro"] != "1500" {
		t.Fatalf("reserved_micro=%v, want the decimal string 1500", first["reserved_micro"])
	}
	totals := payload["totals"].(map[string]any)
	if totals["open"] != float64(2) || totals["sla_breaches"] != float64(1) || totals["sla_seconds"] != float64(86400) {
		t.Fatalf("totals=%v", totals)
	}
	cursor, ok := payload["next_cursor"].(map[string]any)
	if !ok || cursor["execution_id"] != "exec_new" {
		t.Fatalf("next_cursor=%v", payload["next_cursor"])
	}
	if store.lastQueue.Limit != 2 || store.lastQueue.SLA != 24*time.Hour {
		t.Fatalf("handler passed %+v, want limit=2 sla=24h", store.lastQueue)
	}
}

func TestRecoveryReviewQueueHandlerRejectsBadInput(t *testing.T) {
	runtime := newRecoveryTestRuntime(&fakeRecoveryStore{}, nil)
	for name, target := range map[string]string{
		"limit out of range": "/recovery/review-required?limit=1000",
		"limit not a number": "/recovery/review-required?limit=many",
		"sla not a duration": "/recovery/review-required?sla=soon",
		"half cursor":        "/recovery/review-required?cursor_execution_id=exec_1",
		"bad cursor instant": "/recovery/review-required?cursor_updated_at=yesterday&cursor_execution_id=exec_1",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			runtime.handleReviewQueue(recorder, httptest.NewRequest(http.MethodGet, target, nil))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", recorder.Code, recorder.Body.String())
			}
		})
	}

	// A full cursor is accepted and parsed.
	recorder := httptest.NewRecorder()
	runtime.handleReviewQueue(recorder, httptest.NewRequest(http.MethodGet,
		"/recovery/review-required?cursor_updated_at=2026-10-01T00:00:00Z&cursor_execution_id=exec_1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRecoveryStatusReportsPolicyHealthAndGap(t *testing.T) {
	oldest := time.Now().UTC().Add(-3 * time.Hour)
	store := &fakeRecoveryStore{
		backlog: billing.ReviewBacklog{Open: 5, OldestAt: &oldest, Breaches: 2, SLA: billing.DefaultHumanReviewSLA},
		gap:     billing.RecoveryGap{OwnerUnknown: 4, OldestAt: &oldest},
	}
	loop, err := billing.NewExecutionRecovery(store, billing.RecoveryOptions{Logger: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newRecoveryTestRuntime(store, loop)
	recorder := httptest.NewRecorder()
	runtime.handleStatus(recorder, httptest.NewRequest(http.MethodGet, "/recovery/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	payload := decodeRecoveryJSON(t, recorder)
	policy := payload["policy"].(map[string]any)
	if policy["automatic_recovery_enabled"] != true || policy["platform_policy"] != billing.RecoveryPlatformPolicy {
		t.Fatalf("policy=%v", policy)
	}
	if policy["window_min_age_seconds"] != (billing.DefaultRecoveryMaxExecutionDuration + billing.DefaultRecoveryGrace).Seconds() {
		t.Fatalf("policy window=%v", policy["window_min_age_seconds"])
	}
	never := policy["never_released_statuses"].([]any)
	if len(never) != 2 || never[0] != billing.StatusReceiptRecorded || never[1] != billing.StatusSettled {
		t.Fatalf("never_released=%v", never)
	}
	review := payload["review_required"].(map[string]any)
	if review["open"] != float64(5) || review["sla_breaches"] != float64(2) {
		t.Fatalf("review=%v", review)
	}
	gap := payload["owner_unknown"].(map[string]any)
	if gap["open"] != float64(4) {
		t.Fatalf("owner_unknown=%v", gap)
	}
	health := payload["health"].(map[string]any)
	if health["running"] != false || health["ticks"] != float64(0) {
		t.Fatalf("health=%v", health)
	}
}

// TestRecoveryStatusWithoutALoopStillReportsTheQueue: an operator who disabled
// the automatic janitor needs the 24h queue more, not less.
func TestRecoveryStatusWithoutALoopStillReportsTheQueue(t *testing.T) {
	store := &fakeRecoveryStore{backlog: billing.ReviewBacklog{Open: 1, SLA: billing.DefaultHumanReviewSLA}}
	runtime := newRecoveryTestRuntime(store, nil)
	recorder := httptest.NewRecorder()
	runtime.handleStatus(recorder, httptest.NewRequest(http.MethodGet, "/recovery/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	policy := decodeRecoveryJSON(t, recorder)["policy"].(map[string]any)
	if policy["automatic_recovery_enabled"] != false {
		t.Fatalf("policy=%v, want the disabled janitor stated", policy)
	}
}

func TestRecoveryHandlersFailClosedWithoutPG(t *testing.T) {
	runtime := &recoveryRuntime{}
	for name, handler := range map[string]http.HandlerFunc{
		"status":       runtime.handleStatus,
		"review-queue": runtime.handleReviewQueue,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler(recorder, httptest.NewRequest(http.MethodGet, "/recovery", nil))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d, want 503 (a read that cannot be answered is not a green light)", recorder.Code)
			}
		})
	}
}

func TestRecoveryHandlersReportStoreErrors(t *testing.T) {
	store := &fakeRecoveryStore{
		backlogErr: errors.New("postgres is down"),
		queueErr:   errors.New("postgres is down"),
		gapErr:     errors.New("postgres is down"),
	}
	runtime := newRecoveryTestRuntime(store, nil)
	for name, handler := range map[string]http.HandlerFunc{
		"status":       runtime.handleStatus,
		"review-queue": runtime.handleReviewQueue,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler(recorder, httptest.NewRequest(http.MethodGet, "/recovery", nil))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s, want 503", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "postgres is down") {
				t.Fatalf("body=%s, want the reason", recorder.Body.String())
			}
		})
	}
}

// TestRecoveryObserverPublishesBoundedMetrics: the loop's closed vocabularies
// must exist as labelled series, with no organization/execution/instance label.
func TestRecoveryObserverPublishesBoundedMetrics(t *testing.T) {
	observer := recoveryObserver{}
	for _, result := range billing.RecoveryResults {
		observer.ObserveRecovery(result, 2, time.Millisecond)
	}
	for _, result := range billing.RecoveryTickResults {
		observer.ObserveRecoveryTick(result, 3, 1, 1, time.Millisecond)
	}
	observer.ObserveReviewQueue(4, 30*time.Hour, 2)
	observer.ObserveOwnerUnknown(1, time.Hour)
}

func stringPtr(value string) *string { return &value }

// TestRecoveryReviewQueueCursorIsForwardedAsAPosition guards the one detail the
// handler can get wrong without a visible symptom: the cursor must reach the
// store as the last row's (updated_at, execution_id).
func TestRecoveryReviewQueueCursorIsForwardedAsAPosition(t *testing.T) {
	store := &fakeRecoveryStore{}
	runtime := newRecoveryTestRuntime(store, nil)
	recorder := httptest.NewRecorder()
	runtime.handleReviewQueue(recorder, httptest.NewRequest(http.MethodGet,
		"/recovery/review-required?cursor_updated_at=2026-10-01T03:04:05Z&cursor_execution_id=exec_cursor", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if store.lastQueue.Cursor == nil {
		t.Fatal("cursor was not forwarded")
	}
	if store.lastQueue.Cursor.ExecutionID != "exec_cursor" ||
		!store.lastQueue.Cursor.UpdatedAt.Equal(time.Date(2026, 10, 1, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("cursor=%+v", store.lastQueue.Cursor)
	}
}
