package console

import (
	"context"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/billing"
)

// fakeStatements records the statement reads the handler makes.
type fakeStatements struct {
	mu    sync.Mutex
	page  billing.StatementPage
	err   error
	calls []billing.StatementParams
}

func (f *fakeStatements) MonthlyStatement(_ context.Context, params billing.StatementParams) (billing.StatementPage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, params)
	fn := f.page
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return billing.StatementPage{}, err
	}
	page := fn
	if page.Month.Label == "" {
		page.Month = params.Month
	}
	if page.Currency == "" {
		page.Currency = billing.CurrencyCNY
	}
	if page.GeneratedAt.IsZero() {
		page.GeneratedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	}
	page.Items = append([]billing.StatementLine(nil), page.Items...)
	return page, nil
}

func (f *fakeStatements) lastCall() (billing.StatementParams, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return billing.StatementParams{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// fakeExceptions records the durable money-exception calls the handler makes.
type fakeExceptions struct {
	mu          sync.Mutex
	items       []billing.Exception
	totals      billing.ExceptionBacklog
	nextCursor  *billing.ExceptionCursor
	events      map[int64][]billing.ExceptionEvent
	listErr     error
	getErr      error
	resolveErr  error
	resolved    billing.ResolveExceptionResult
	listCalls   []billing.ListExceptionsParams
	getCalls    [][2]int64
	resolveCall []billing.ResolveExceptionParams
}

func (f *fakeExceptions) ListExceptions(_ context.Context, params billing.ListExceptionsParams) (billing.ExceptionPage, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, params)
	items := append([]billing.Exception(nil), f.items...)
	totals := f.totals
	next := f.nextCursor
	err := f.listErr
	f.mu.Unlock()
	if err != nil {
		return billing.ExceptionPage{}, err
	}
	return billing.ExceptionPage{Items: items, Totals: totals, NextCursor: next}, nil
}

func (f *fakeExceptions) GetException(_ context.Context, orgID, exceptionID int64) (billing.Exception, error) {
	f.mu.Lock()
	f.getCalls = append(f.getCalls, [2]int64{orgID, exceptionID})
	err := f.getErr
	items := append([]billing.Exception(nil), f.items...)
	f.mu.Unlock()
	if err != nil {
		return billing.Exception{}, err
	}
	// The fake enforces the same scope the store does: an exception of another
	// organization is not found, never returned.
	for _, exception := range items {
		if exception.ID == exceptionID && exception.OrgID == orgID {
			return exception, nil
		}
	}
	return billing.Exception{}, billing.ErrExceptionNotFound
}

func (f *fakeExceptions) ListExceptionEvents(_ context.Context, orgID, exceptionID int64) ([]billing.ExceptionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]billing.ExceptionEvent(nil), f.events[exceptionID]...), nil
}

func (f *fakeExceptions) ResolveException(_ context.Context, params billing.ResolveExceptionParams) (billing.ResolveExceptionResult, error) {
	f.mu.Lock()
	f.resolveCall = append(f.resolveCall, params)
	err := f.resolveErr
	result := f.resolved
	items := append([]billing.Exception(nil), f.items...)
	f.mu.Unlock()
	if err != nil {
		return billing.ResolveExceptionResult{}, err
	}
	if result.Exception.ID == 0 {
		for _, exception := range items {
			if exception.ID == params.ExceptionID {
				result.Exception = exception
				break
			}
		}
	}
	return result, nil
}

// sampleException is the fixture the exception read tests share.
func sampleException(id, orgID int64, status string) billing.Exception {
	executionID := "exec_1"
	exception := billing.Exception{
		ID:          id,
		Kind:        billing.ExceptionKindAccountProjectionMismatch,
		OrgID:       orgID,
		ExecutionID: &executionID,
		AmountMicro: 500,
		Currency:    "CNY",
		Reason:      "projection disagrees with the ledger",
		Status:      status,
		DetectedAt:  time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		LastSeenAt:  time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		Occurrences: 2,
		DedupeKey:   "billing_exception:v1:accounts:account_projection_mismatch:org=7",
		Version:     2,
		CreatedAt:   time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
	}
	if status == billing.ExceptionStatusResolved {
		resolvedAt := time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
		resolvedBy := int64(1)
		resolution := billing.ExceptionResolutionWaived
		note := "waived: the drift was a bug in the old projection job"
		exception.ResolvedAt = &resolvedAt
		exception.ResolvedBy = &resolvedBy
		exception.Resolution = &resolution
		exception.ResolutionNote = &note
	}
	return exception
}

// statementHarness wires the fake statement/exception stores into the standard
// console harness with an owner membership in organization 7.
func statementHarness(t *testing.T, role string) (*harness, *fakeStatements, *fakeExceptions, *sessionFixture) {
	t.Helper()
	statements := &fakeStatements{}
	exceptions := &fakeExceptions{events: map[int64][]billing.ExceptionEvent{}}
	h := newHarness(t, func(opts *Options) {
		opts.Statements = statements
		opts.Exceptions = exceptions
	})
	fixture := defaultFixture()
	h.withSessionFixture(fixture)
	h.orgs.memberships = map[int64]map[int64]string{
		fixture.user.ID: {7: role},
	}
	return h, statements, exceptions, fixture
}
