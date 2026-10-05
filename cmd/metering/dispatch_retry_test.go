package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/billing"
)

type fakeDispatchRetryStore struct {
	*fakeRecoveryStore
	params billing.DispatchRetryParams
	calls  int
	err    error
}

func (s *fakeDispatchRetryStore) RetryDispatchException(_ context.Context, p billing.DispatchRetryParams) (billing.Receipt, error) {
	s.calls++
	s.params = p
	return billing.Receipt{ExecutionID: p.ExecutionID, Status: billing.ReceiptStatusPendingDispatch}, s.err
}
func TestDispatchRetryOperatorBoundary(t *testing.T) {
	s := &fakeDispatchRetryStore{fakeRecoveryStore: &fakeRecoveryStore{}}
	mux, _ := manualTestRuntime(t, s)
	path := "/receipts/exec_retry/retry"
	body := `{"actor_user_id":42,"reason":" reviewed transport outage "}`
	for _, token := range []string{"", "wrong"} {
		if r := postManual(t, mux, path, token, body); r.Code != 401 {
			t.Fatalf("unauthorized=%d", r.Code)
		}
	}
	if s.calls != 0 {
		t.Fatal("unauthorized request reached store")
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+manualTestToken)
	r := httptest.NewRecorder()
	mux.ServeHTTP(r, req)
	if r.Code != 405 {
		t.Fatalf("GET=%d", r.Code)
	}
	if r := postManual(t, mux, path, manualTestToken, `{"reason":"reviewed"}`); r.Code != 403 {
		t.Fatalf("missing actor=%d", r.Code)
	}
	if r := postManual(t, mux, path, manualTestToken, body); r.Code != 200 || !strings.Contains(r.Body.String(), `"money_moved":false`) {
		t.Fatalf("retry=%d %s", r.Code, r.Body.String())
	}
	if s.params.ExecutionID != "exec_retry" || s.params.ActorUserID != 42 || s.params.Reason != "reviewed transport outage" || s.params.Now.IsZero() {
		t.Fatalf("params=%+v", s.params)
	}
	for _, tc := range []struct {
		err    error
		status int
	}{
		{billing.ErrInvalidArgument, 400}, {billing.ErrExecutionNotFound, 404}, {billing.ErrReceiptNotFound, 404},
		{billing.ErrExecutionState, 409}, {billing.ErrSettlementState, 409}, {billing.ErrReceiptConflict, 409},
		{errors.New("private database error"), 503},
	} {
		s.err = tc.err
		r := postManual(t, mux, path, manualTestToken, body)
		if r.Code != tc.status || strings.Contains(r.Body.String(), "private database") {
			t.Fatalf("error %v: %d %s", tc.err, r.Code, r.Body.String())
		}
	}
	unsupported, _ := manualTestRuntime(t, &fakeRecoveryStore{})
	if r := postManual(t, unsupported, path, manualTestToken, body); r.Code != 503 {
		t.Fatalf("unsupported=%d", r.Code)
	}
}
