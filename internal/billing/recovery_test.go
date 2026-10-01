package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pure (no PostgreSQL) tests of the T07B recovery policy: the liveness
// predicate, the option validation, the closed outcome vocabulary and the
// bounded loop's behaviour against a scripted store. The PostgreSQL facts
// (transaction guard, race with finalize, audit row) are proven in
// recovery_pg_integration_test.go.

func TestInstanceAliveForRequiresFreshHeartbeatForTheSameProcessLifetime(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	admitted := now.Add(-45 * time.Minute)
	lease := 60 * time.Second
	covering := GatewayInstance{
		InstanceID: "gw-1",
		StartedAt:  now.Add(-2 * time.Hour),
		LastSeenAt: now.Add(-5 * time.Second),
	}
	restarted := covering
	restarted.StartedAt = now.Add(-10 * time.Minute) // the process restarted after the admission
	stale := covering
	stale.LastSeenAt = now.Add(-30 * time.Second)

	tests := []struct {
		name     string
		instance GatewayInstance
		lease    time.Duration
		want     bool
	}{
		{"fresh heartbeat from the admitting process", covering, lease, true},
		{"exactly at the lease boundary is already dead", covering, 5 * time.Second, false},
		{"stale heartbeat", stale, 10 * time.Second, false},
		{"fresh heartbeat from a restarted process does not vouch for the old one", restarted, lease, false},
		{"no heartbeat at all is never alive", GatewayInstance{InstanceID: "gw-1"}, lease, false},
		{"zero lease falls back to the documented default", covering, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InstanceAliveFor(tt.instance, admitted, now, tt.lease); got != tt.want {
				t.Fatalf("InstanceAliveFor=%t, want %t", got, tt.want)
			}
		})
	}
	// A restart whose started_at equals the admission instant is still the
	// admitting process: the comparison is not strict.
	equal := covering
	equal.StartedAt = admitted
	if !InstanceAliveFor(equal, admitted, now, lease) {
		t.Fatal("started_at == admitted_at must count as the same process lifetime")
	}
}

func TestNormalizeHeartbeatAndValidate(t *testing.T) {
	now := testTime()
	params := NormalizeInstanceHeartbeat(InstanceHeartbeatParams{
		InstanceID: "  gw-1  ",
		StartedAt:  now.Add(-time.Minute),
		Now:        now,
	})
	if params.InstanceID != "gw-1" {
		t.Fatalf("instance_id=%q, want it trimmed", params.InstanceID)
	}
	if err := ValidateInstanceHeartbeat(params); err != nil {
		t.Fatalf("valid heartbeat refused: %v", err)
	}

	tests := []struct {
		name   string
		params InstanceHeartbeatParams
		want   error
	}{
		{"empty id", NormalizeInstanceHeartbeat(InstanceHeartbeatParams{StartedAt: now, Now: now}), ErrInvalidArgument},
		{
			"padded id",
			InstanceHeartbeatParams{InstanceID: " gw ", StartedAt: now, Now: now},
			ErrInvalidArgument,
		},
		{
			"oversized id",
			InstanceHeartbeatParams{InstanceID: strings.Repeat("g", MaxInstanceIDBytes+1), StartedAt: now, Now: now},
			ErrInvalidArgument,
		},
		{
			"missing started_at",
			NormalizeInstanceHeartbeat(InstanceHeartbeatParams{InstanceID: "gw", Now: now}),
			ErrInvalidArgument,
		},
		{
			"started_at in the future",
			NormalizeInstanceHeartbeat(InstanceHeartbeatParams{InstanceID: "gw", StartedAt: now.Add(time.Minute), Now: now}),
			ErrInvalidArgument,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateInstanceHeartbeat(tt.params); !errors.Is(err, tt.want) {
				t.Fatalf("err=%v, want %v", err, tt.want)
			}
		})
	}
}

func TestRecoveryOptionsDefaultsAndValidation(t *testing.T) {
	opts := NormalizeRecoveryOptions(RecoveryOptions{})
	if opts.BatchSize != DefaultRecoveryBatchSize || opts.Interval != DefaultRecoveryInterval ||
		opts.MaxExecutionDuration != DefaultRecoveryMaxExecutionDuration || opts.Grace != DefaultRecoveryGrace ||
		opts.LivenessLease != DefaultLivenessLease || opts.ReviewSLA != DefaultHumanReviewSLA ||
		opts.Timeout != DefaultRecoveryTimeout {
		t.Fatalf("unexpected defaults: %+v", opts)
	}
	if opts.MinAge() != DefaultRecoveryMaxExecutionDuration+DefaultRecoveryGrace {
		t.Fatalf("min age=%s, want max execution duration + grace", opts.MinAge())
	}
	if err := ValidateRecoveryOptions(opts); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}

	// A lease that cannot outlive even one heartbeat interval would declare a
	// healthy instance dead between two beats: the configuration is refused
	// rather than obeyed.
	broken := opts
	broken.LivenessLease = DefaultHeartbeatInterval
	if err := ValidateRecoveryOptions(broken); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want a refusal for a lease below the heartbeat interval", err)
	}

	for name, mutate := range map[string]func(*RecoveryOptions){
		"zero batch":             func(o *RecoveryOptions) { o.BatchSize = 0 },
		"oversized batch":        func(o *RecoveryOptions) { o.BatchSize = MaxRecoveryBatchSize + 1 },
		"zero grace":             func(o *RecoveryOptions) { o.Grace = 0 },
		"zero max execution":     func(o *RecoveryOptions) { o.MaxExecutionDuration = 0 },
		"oversized review sla":   func(o *RecoveryOptions) { o.ReviewSLA = MaxHumanReviewSLA + time.Second },
		"zero recovery timeout":  func(o *RecoveryOptions) { o.Timeout = 0 },
		"zero recovery interval": func(o *RecoveryOptions) { o.Interval = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := opts
			mutate(&candidate)
			candidate = NormalizeRecoveryOptions(candidate)
			// Normalize fills zero fields back in, so the refusal above only
			// triggers for values that survive normalization (out-of-range).
			if candidate.BatchSize <= MaxRecoveryBatchSize && candidate.ReviewSLA <= MaxHumanReviewSLA {
				if err := ValidateRecoveryOptions(candidate); err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err := ValidateRecoveryOptions(candidate); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err=%v, want a range refusal", err)
			}
		})
	}
}

func TestValidateRecoveryParamsRequiresARecordedCheck(t *testing.T) {
	base := RecoveryParams{
		ExecutionID:   "exec_1",
		Reason:        "recovery: owner not seen",
		MinAge:        time.Minute,
		LivenessLease: time.Minute,
		ObservedAt:    testTime(),
	}
	if err := ValidateRecoveryParams(NormalizeRecoveryParams(base)); err != nil {
		t.Fatalf("valid decision refused: %v", err)
	}

	noCheck := base
	noCheck.ObservedAt = time.Time{}
	if err := ValidateRecoveryParams(NormalizeRecoveryParams(noCheck)); !errors.Is(err, ErrRecoveryCheckMissing) {
		t.Fatalf("err=%v, want ErrRecoveryCheckMissing", err)
	}
	noLease := base
	noLease.LivenessLease = 0
	if err := ValidateRecoveryParams(NormalizeRecoveryParams(noLease)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want an argument refusal for a zero lease", err)
	}
	noReason := base
	noReason.Reason = "  "
	if err := ValidateRecoveryParams(NormalizeRecoveryParams(noReason)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want an argument refusal for a missing reason", err)
	}
}

// TestRecoveryOutcomeVocabularyIsClosed proves every refusal the transaction can
// return maps onto the observer's vocabulary, and that the vocabulary is the
// closed list the metrics label set is built from.
func TestRecoveryOutcomeVocabularyIsClosed(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, RecoveryResultReleased},
		{fmt.Errorf("wrap: %w", ErrRecoveryTooYoung), RecoveryResultTooYoung},
		{fmt.Errorf("wrap: %w", ErrRecoveryInstanceAlive), RecoveryResultInstanceAlive},
		{fmt.Errorf("wrap: %w", ErrRecoveryOwnerUnknown), RecoveryResultOwnerUnknown},
		{fmt.Errorf("wrap: %w", ErrInstanceNotFound), RecoveryResultOwnerUnknown},
		{fmt.Errorf("wrap: %w", ErrExecutionHasReceipt), RecoveryResultReceiptExists},
		{fmt.Errorf("wrap: %w", ErrExecutionState), RecoveryResultState},
		{fmt.Errorf("wrap: %w", ErrRecoveryCheckMissing), RecoveryResultCheckMissing},
		{errors.New("postgres is down"), RecoveryResultError},
	}
	for _, tt := range cases {
		got := RecoveryOutcome(tt.err)
		if got != tt.want {
			t.Fatalf("RecoveryOutcome(%v)=%q, want %q", tt.err, got, tt.want)
		}
		if !containsString(RecoveryResults, got) {
			t.Fatalf("%q is not part of the closed vocabulary", got)
		}
	}
	if len(RecoveryResults) != len(uniqueStrings(RecoveryResults)) {
		t.Fatal("the recovery vocabulary contains duplicates")
	}
}

func TestReviewBacklogAgeAndCursorValidation(t *testing.T) {
	now := testTime()
	empty := ReviewBacklog{SLA: DefaultHumanReviewSLA}
	if age := empty.OldestAge(now); age != 0 {
		t.Fatalf("empty queue age=%s, want 0", age)
	}
	oldest := now.Add(-30 * time.Hour)
	backlog := ReviewBacklog{Open: 3, OldestAt: &oldest, Breaches: 1, SLA: DefaultHumanReviewSLA}
	if age := backlog.OldestAge(now); age != 30*time.Hour {
		t.Fatalf("age=%s, want 30h", age)
	}
	future := now.Add(time.Hour)
	if age := (ReviewBacklog{OldestAt: &future}).OldestAge(now); age != 0 {
		t.Fatalf("future age=%s, want 0", age)
	}

	params := NormalizeReviewQueuePage(ReviewQueueParams{})
	if params.Limit != DefaultReviewQueuePageSize || params.SLA != DefaultHumanReviewSLA || params.Now.IsZero() {
		t.Fatalf("unexpected defaults: %+v", params)
	}
	if err := ValidateReviewQueuePage(params); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	bad := params
	bad.Limit = MaxReviewQueuePageSize + 1
	if err := ValidateReviewQueuePage(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want a page-size refusal", err)
	}
	halfCursor := params
	halfCursor.Cursor = &ReviewCursor{ExecutionID: "exec_1"}
	if err := ValidateReviewQueuePage(halfCursor); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want a cursor refusal", err)
	}
}

// scriptedRecoveryStore is a RecoveryStore whose answers are scripted per
// candidate. It records what the loop asked for, so the bounds can be asserted.
type scriptedRecoveryStore struct {
	candidates []Execution
	outcomes   map[string]error
	lastScan   RecoveryCandidateScanParams

	mu            sync.Mutex
	recovered     []string
	reviewBacklog ReviewBacklog
	reviewErr     error
	gap           RecoveryGap
	page          ReviewQueuePage
}

func (s *scriptedRecoveryStore) ListRecoveryCandidates(_ context.Context, params RecoveryCandidateScanParams) ([]Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastScan = params
	return s.candidates, nil
}

func (s *scriptedRecoveryStore) RecoverExecution(_ context.Context, params RecoveryParams) (RecoveryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recovered = append(s.recovered, params.ExecutionID)
	if err := s.outcomes[params.ExecutionID]; err != nil {
		return RecoveryResult{}, err
	}
	return RecoveryResult{Checks: RecoveryChecks{ReceiptChecked: true}}, nil
}

func (s *scriptedRecoveryStore) ReviewRequiredBacklog(context.Context, ReviewBacklogParams) (ReviewBacklog, error) {
	if s.reviewErr != nil {
		return ReviewBacklog{}, s.reviewErr
	}
	return s.reviewBacklog, nil
}

func (s *scriptedRecoveryStore) RecoveryGap(context.Context, RecoveryGapParams) (RecoveryGap, error) {
	return s.gap, nil
}

func (s *scriptedRecoveryStore) ListReviewRequiredExecutions(context.Context, ReviewQueueParams) (ReviewQueuePage, error) {
	return s.page, nil
}

type recordingObserver struct {
	mu       sync.Mutex
	outcomes map[string]int
	ticks    []string
	queue    []int64
	unknown  []int64
}

func (o *recordingObserver) ObserveRecovery(result string, count int, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.outcomes == nil {
		o.outcomes = map[string]int{}
	}
	o.outcomes[result] += count
}

func (o *recordingObserver) ObserveRecoveryTick(result string, _, _, _ int, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ticks = append(o.ticks, result)
}

func (o *recordingObserver) ObserveReviewQueue(open int64, _ time.Duration, _ int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.queue = append(o.queue, open)
}

func (o *recordingObserver) ObserveOwnerUnknown(open int64, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.unknown = append(o.unknown, open)
}

// TestRecoveryTickIsBoundedAndClassifiesEveryCandidate is the loop-level unit
// test: one tick scans at most BatchSize candidates, classifies every refusal,
// keeps going after a refusal, and reports the human queue.
func TestRecoveryTickIsBoundedAndClassifiesEveryCandidate(t *testing.T) {
	now := testTime()
	store := &scriptedRecoveryStore{
		candidates: []Execution{
			{ExecutionID: "exec_released", Status: StatusReserved, UpdatedAt: now.Add(-time.Hour)},
			{ExecutionID: "exec_alive", Status: StatusExecuting, UpdatedAt: now.Add(-time.Hour)},
			{ExecutionID: "exec_unknown", Status: StatusReserved, UpdatedAt: now.Add(-time.Hour)},
			{ExecutionID: "exec_young", Status: StatusExecuting, UpdatedAt: now.Add(-time.Hour)},
			{ExecutionID: "exec_receipt", Status: StatusReserved, UpdatedAt: now.Add(-time.Hour)},
			{ExecutionID: "exec_failed", Status: StatusReserved, UpdatedAt: now.Add(-time.Hour)},
		},
		outcomes: map[string]error{
			"exec_alive":   fmt.Errorf("%w: heartbeat is fresh", ErrRecoveryInstanceAlive),
			"exec_unknown": fmt.Errorf("%w: no heartbeat row", ErrRecoveryOwnerUnknown),
			"exec_young":   fmt.Errorf("%w: 5s old", ErrRecoveryTooYoung),
			"exec_receipt": fmt.Errorf("%w: pending_dispatch receipt", ErrExecutionHasReceipt),
			"exec_failed":  errors.New("postgres: connection reset"),
		},
		reviewBacklog: ReviewBacklog{Open: 2, Breaches: 1, SLA: DefaultHumanReviewSLA, OldestAt: timePtr(now.Add(-25 * time.Hour))},
		gap:           RecoveryGap{OwnerUnknown: 3, OldestAt: timePtr(now.Add(-3 * time.Hour))},
	}
	observer := &recordingObserver{}
	loop, err := NewExecutionRecovery(store, RecoveryOptions{
		BatchSize: 4,
		Now:       func() time.Time { return now },
		Logger:    func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.SetObserver(observer)

	result, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if store.lastScan.Limit != 4 {
		t.Fatalf("scan limit=%d, want the configured batch 4", store.lastScan.Limit)
	}
	if want := now.Add(-(DefaultRecoveryMaxExecutionDuration + DefaultRecoveryGrace)); !store.lastScan.Before.Equal(want) {
		t.Fatalf("scan before=%s, want now - (max execution + grace)=%s", store.lastScan.Before, want)
	}
	if want := now.Add(-DefaultLivenessLease); !store.lastScan.HeartbeatBefore.Equal(want) {
		t.Fatalf("heartbeat threshold=%s, want now - lease=%s", store.lastScan.HeartbeatBefore, want)
	}
	if result.Scanned != 6 {
		t.Fatalf("scanned=%d, want 6 (the scripted store returns its whole slice; the DB enforces LIMIT)", result.Scanned)
	}
	for outcome, want := range map[string]int{
		RecoveryResultReleased:      1,
		RecoveryResultInstanceAlive: 1,
		RecoveryResultOwnerUnknown:  1,
		RecoveryResultTooYoung:      1,
		RecoveryResultReceiptExists: 1,
		RecoveryResultError:         1,
	} {
		if got := result.Outcomes[outcome]; got != want {
			t.Fatalf("outcome %s=%d, want %d (all outcomes: %v)", outcome, got, want, result.Outcomes)
		}
	}
	if result.Released() != 1 || result.Refused() != 4 || result.Failed() != 1 {
		t.Fatalf("released=%d refused=%d failed=%d, want 1/4/1", result.Released(), result.Refused(), result.Failed())
	}
	if result.ReviewOpen != 2 || result.ReviewBreaches != 1 || result.ReviewOldest != 25*time.Hour {
		t.Fatalf("review snapshot: %+v", result)
	}
	if result.OwnerUnknown != 3 || result.OwnerUnknownOldest != 3*time.Hour {
		t.Fatalf("owner-unknown gap: %+v", result)
	}
	if len(observer.unknown) != 1 || observer.unknown[0] != 3 {
		t.Fatalf("owner-unknown observations=%v", observer.unknown)
	}
	if len(observer.ticks) != 1 || observer.ticks[0] != RecoveryTickOK {
		t.Fatalf("tick outcomes=%v, want one ok", observer.ticks)
	}
	health := loop.Health()
	if health.Ticks != 1 || health.LastScanned != 6 || health.LastReleased != 1 || health.LastRefused != 4 || health.LastError != "" {
		t.Fatalf("health=%+v", health)
	}
}

func TestRecoveryTickStopsOnCancellationAndReportsScanErrors(t *testing.T) {
	store := &scriptedRecoveryStore{}
	loop, err := NewExecutionRecovery(store, RecoveryOptions{Logger: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loop.Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}

	failing := &scriptedRecoveryStore{candidates: nil, reviewErr: errors.New("queue read failed")}
	loop2, err := NewExecutionRecovery(failing, RecoveryOptions{Logger: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop2.Tick(context.Background())
	if err == nil {
		t.Fatal("a queue read failure must be reported (and must not be silently green)")
	}
	if result.Outcomes[RecoveryResultEmpty] != 1 {
		t.Fatalf("outcomes=%v, want one empty scan", result.Outcomes)
	}
}

// TestRecoveryReasonFitsTheTerminalReasonColumn proves the generated reason
// always passes ValidateTerminalReason, including with a maximum-length
// instance id: an over-long reason would turn a release into an argument error.
func TestRecoveryReasonFitsTheTerminalReasonColumn(t *testing.T) {
	opts := NormalizeRecoveryOptions(RecoveryOptions{})
	owner := strings.Repeat("g", MaxInstanceIDBytes)
	candidate := Execution{
		ExecutionID: "exec_1",
		Status:      StatusReserved,
		GatewayID:   &owner,
		UpdatedAt:   testTime().Add(-72 * time.Hour),
	}
	reason := recoveryReason(candidate, testTime(), opts)
	if err := ValidateTerminalReason(reason); err != nil {
		t.Fatalf("generated reason is not writable: %v (reason=%q)", err, reason)
	}
	noOwner := candidate
	noOwner.GatewayID = nil
	if reason := recoveryReason(noOwner, testTime(), opts); !strings.Contains(reason, "owner instance unknown") {
		t.Fatalf("reason=%q, want it to name the unknown owner", reason)
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func timePtr(value time.Time) *time.Time { return &value }
