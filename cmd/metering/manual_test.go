package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/observability"
)

// T12 metering-side tests: the manual disposition HTTP surface. They use a
// scripted store, so they need neither PostgreSQL nor Redis; the money movement
// itself is proven by the PG-gated billing suite.

const manualTestToken = "admin-token-secret"

// manualTestRuntime mounts the operator surface on a mux, exactly as the process
// does, so path values, the method guard and the bearer are all exercised.
func manualTestRuntime(t *testing.T, store recoveryStore) (*http.ServeMux, *recoveryRuntime) {
	t.Helper()
	runtime := newRecoveryTestRuntime(store, nil)
	runtime.window = 40 * time.Minute
	runtime.lease = time.Minute
	mux := http.NewServeMux()
	runtime.register(mux, manualTestToken)
	return mux, runtime
}

func manualTestResult() billing.ManualDispositionResult {
	reason := "operator: reviewed the stuck execution"
	return billing.ManualDispositionResult{
		Execution: billing.Execution{
			ExecutionID:    "exec_1",
			OrgID:          7,
			Model:          "gpt-4o",
			Status:         billing.StatusReleased,
			ReservedMicro:  1500,
			PriceVersionID: 3,
			TerminalReason: &reason,
			UpdatedAt:      time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		},
		Account:        billing.Account{OrgID: 7, PostedBalanceMicro: 10_000, ReservedMicro: 0, Version: 4},
		ReleaseEntry:   &billing.Entry{ID: 11, Kind: billing.KindRelease, BusinessID: billing.ExecutionBusinessID("exec_1", billing.KindRelease), AmountMicro: 1500},
		DebitEntry:     &billing.Entry{ID: 12, Kind: billing.KindDebit, BusinessID: billing.ReceiptBusinessID("exec_1", billing.KindDebit), AmountMicro: 1500},
		ChargeMicro:    1500,
		ReceiptStatus:  billing.ReceiptStatusException,
		ReceiptChecked: true,
		Policy:         billing.ManualSettlementPolicy,
	}
}

func postManual(t *testing.T, mux *http.ServeMux, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

// TestManualDispositionRequiresTheAdminBearer: the two money-moving actions use
// exactly the same bearer as /recovery/status, and they are POST-only.
func TestManualDispositionRequiresTheAdminBearer(t *testing.T) {
	store := &fakeRecoveryStore{manual: manualTestResult()}
	mux, _ := manualTestRuntime(t, store)
	body := `{"actor_user_id": 42, "reason": "reviewed"}`

	for name, request := range map[string]*http.Request{
		"no bearer":    httptest.NewRequest(http.MethodPost, "/recovery/review-required/exec_1/release", strings.NewReader(body)),
		"wrong bearer": httptest.NewRequest(http.MethodPost, "/recovery/review-required/exec_1/release", strings.NewReader(body)),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "wrong bearer" {
				request.Header.Set("Authorization", "Bearer not-the-token")
			}
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s, want 401", recorder.Code, recorder.Body.String())
			}
		})
	}
	if store.manualCalls != 0 {
		t.Fatalf("manual calls=%d, want 0: an unauthenticated request must never reach the store", store.manualCalls)
	}

	// The documented token works, and the method is part of the contract: a GET
	// is a 405, not a read that happens to move money.
	if recorder := postManual(t, mux, "/recovery/review-required/exec_1/release", manualTestToken, body); recorder.Code != http.StatusOK {
		t.Fatalf("authorized release status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	getRequest := httptest.NewRequest(http.MethodGet, "/recovery/review-required/exec_1/release", nil)
	getRequest.Header.Set("Authorization", "Bearer "+manualTestToken)
	getRecorder := httptest.NewRecorder()
	mux.ServeHTTP(getRecorder, getRequest)
	if getRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405 (the action is POST-only)", getRecorder.Code)
	}
}

// TestManualDispositionRequiresActorAndReason: an unattributed or unexplained
// decision is refused before the store is called.
func TestManualDispositionRequiresActorAndReason(t *testing.T) {
	for name, testCase := range map[string]struct {
		body string
		want int
	}{
		"no actor":              {`{"reason": "reviewed"}`, http.StatusForbidden},
		"zero actor":            {`{"actor_user_id": 0, "reason": "reviewed"}`, http.StatusForbidden},
		"negative actor":        {`{"actor_user_id": -3, "reason": "reviewed"}`, http.StatusForbidden},
		"no reason":             {`{"actor_user_id": 42}`, http.StatusBadRequest},
		"blank reason":          {`{"actor_user_id": 42, "reason": "   "}`, http.StatusBadRequest},
		"not json":              {`actor=42`, http.StatusBadRequest},
		"unknown field":         {`{"actor_user_id": 42, "reason": "x", "charge_micro": 1}`, http.StatusBadRequest},
		"two objects":           {`{"actor_user_id": 42, "reason": "x"}{"actor_user_id": 43}`, http.StatusBadRequest},
		"reason too long":       {`{"actor_user_id": 42, "reason": "` + strings.Repeat("x", billing.MaxRecoveryReasonLength+1) + `"}`, http.StatusBadRequest},
		"actor is not a number": {`{"actor_user_id": "42", "reason": "x"}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeRecoveryStore{manual: manualTestResult()}
			mux, _ := manualTestRuntime(t, store)
			recorder := postManual(t, mux, "/recovery/review-required/exec_1/settle", manualTestToken, testCase.body)
			if recorder.Code != testCase.want {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), testCase.want)
			}
			if store.manualCalls != 0 {
				t.Fatalf("manual calls=%d, want 0: the request must be refused before the store", store.manualCalls)
			}
		})
	}
}

// TestManualDispositionMapsRefusalsToDocumentedStatuses: every store refusal has
// one documented code, and a refusal is never a 5xx that reads like a crash.
func TestManualDispositionMapsRefusalsToDocumentedStatuses(t *testing.T) {
	wrap := func(messages ...error) error {
		var joined error
		for _, message := range messages {
			if joined == nil {
				joined = message
				continue
			}
			joined = errors.Join(joined, message)
		}
		return joined
	}
	for name, testCase := range map[string]struct {
		err  error
		want int
	}{
		"unknown execution":    {billing.ErrExecutionNotFound, http.StatusNotFound},
		"wrong state":          {wrap(billing.ErrExecutionState, errors.New("execution exec_1 is settled")), http.StatusConflict},
		"chargeable receipt":   {wrap(billing.ErrExecutionHasReceipt, errors.New("receipt status=dispatched")), http.StatusConflict},
		"inside the window":    {billing.ErrRecoveryTooYoung, http.StatusConflict},
		"ledger conflict":      {billing.ErrIdempotencyConflict, http.StatusConflict},
		"invalid argument":     {billing.ErrInvalidArgument, http.StatusBadRequest},
		"missing check":        {billing.ErrRecoveryCheckMissing, http.StatusBadRequest},
		"postgres unavailable": {errors.New("postgres is down"), http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeRecoveryStore{manualErr: testCase.err}
			mux, _ := manualTestRuntime(t, store)
			recorder := postManual(t, mux, "/recovery/review-required/exec_1/release", manualTestToken,
				`{"actor_user_id": 42, "reason": "reviewed"}`)
			if recorder.Code != testCase.want {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), testCase.want)
			}
			if payload := decodeRecoveryJSON(t, recorder); payload["ok"] != false {
				t.Fatalf("payload=%v, want ok=false", payload)
			}
		})
	}
}

// TestManualDispositionReturnsTheResultingExecutionState checks the success
// contract: the new state, what moved, and the audit identity — plus the bounds
// the handler forwards to the store.
func TestManualDispositionReturnsTheResultingExecutionState(t *testing.T) {
	store := &fakeRecoveryStore{manual: manualTestResult()}
	mux, runtime := manualTestRuntime(t, store)

	request := httptest.NewRequest(http.MethodPost, "/recovery/review-required/exec_9/settle",
		strings.NewReader(`{"actor_user_id": 42, "reason": "  reviewed against the provider dashboard  "}`))
	request = request.WithContext(observability.WithRequestID(request.Context(), "req-http-1"))
	request.Header.Set("Authorization", "Bearer "+manualTestToken)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	payload := decodeRecoveryJSON(t, recorder)
	if payload["ok"] != true || payload["action"] != billing.ManualActionSettle ||
		payload["status"] != billing.StatusReleased || payload["execution_id"] != "exec_1" {
		t.Fatalf("payload=%v", payload)
	}
	money := payload["money"].(map[string]any)
	if money["charge_micro"] != "1500" || money["released_micro"] != "1500" ||
		money["debit_entry_id"] != float64(12) || money["release_entry_id"] != float64(11) {
		t.Fatalf("money=%v", money)
	}
	audit := payload["audit"].(map[string]any)
	if audit["manual"] != true || audit["action"] != billing.ActionExecutionManuallySettled ||
		audit["policy"] != billing.ManualSettlementPolicy {
		t.Fatalf("audit=%v", audit)
	}
	if receipt := audit["receipt"].(map[string]any); receipt["status"] != billing.ReceiptStatusException || receipt["checked"] != true {
		t.Fatalf("audit receipt=%v", receipt)
	}

	// The handler forwarded the path execution id, the trimmed reason, the actor
	// and the configured window/lease/sla bounds.
	if store.lastSettled != "exec_9" || store.lastReleased != "" {
		t.Fatalf("settled=%q released=%q, want the settle action only", store.lastSettled, store.lastReleased)
	}
	got := store.lastManual
	if got.Action != billing.ManualActionSettle || got.ExecutionID != "exec_9" ||
		got.ActorUserID != 42 || got.Reason != "reviewed against the provider dashboard" ||
		got.MinAge != runtime.windowValue() || got.LivenessLease != runtime.leaseValue() ||
		got.RequestID != "req-http-1" || got.ObservedAt.IsZero() {
		t.Fatalf("forwarded params=%+v", got)
	}

	// The release action goes through its own store method and reports the
	// release audit identity.
	store.lastSettled = ""
	releaseRecorder := postManual(t, mux, "/recovery/review-required/exec_8/release", manualTestToken,
		`{"actor_user_id": 42, "reason": "reviewed"}`)
	if releaseRecorder.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", releaseRecorder.Code, releaseRecorder.Body.String())
	}
	releasePayload := decodeRecoveryJSON(t, releaseRecorder)
	if store.lastReleased != "exec_8" || store.lastSettled != "" {
		t.Fatalf("released=%q settled=%q, want the release action only", store.lastReleased, store.lastSettled)
	}
	releaseAudit := releasePayload["audit"].(map[string]any)
	if releaseAudit["action"] != billing.ActionExecutionRecovered {
		t.Fatalf("release audit=%v, want the recovery audit action", releaseAudit)
	}
}

// TestManualDispositionFailsClosedWithoutPostgres: an operator surface that
// cannot reach the ledger says so; it never reports a decision it did not make.
func TestManualDispositionFailsClosedWithoutPostgres(t *testing.T) {
	runtime := &recoveryRuntime{}
	for name, handler := range map[string]http.HandlerFunc{
		"release": runtime.handleManualRelease,
		"settle":  runtime.handleManualSettle,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/recovery/review-required/exec_1/release",
				strings.NewReader(`{"actor_user_id": 42, "reason": "reviewed"}`))
			request.SetPathValue("execution", "exec_1")
			recorder := httptest.NewRecorder()
			handler(recorder, request)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d, want 503", recorder.Code)
			}
		})
	}
	_ = context.Background()
}
