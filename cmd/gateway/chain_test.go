package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/billing"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/limiter"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
)

const (
	chainTestRawKey = "sk-chain-order-test"
	chainTestModel  = "gpt-chain-order"
)

// chainBillingStore counts reservations. It implements only the calls a request
// that is refused before admission could make; any other billingStore method
// panics if it is ever reached.
type chainBillingStore struct {
	billingStore
	mu       sync.Mutex
	reserved int
}

func (s *chainBillingStore) Reserve(_ context.Context, _ billing.ReserveParams) (billing.ReserveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved++
	return billing.ReserveResult{
		Execution:    billing.Execution{ReservedMicro: 1},
		PriceVersion: billing.PriceVersion{ID: 1, Model: chainTestModel},
	}, nil
}

func (s *chainBillingStore) MarkExecuting(_ context.Context, _, _ string) (billing.ExecutionResult, error) {
	return billing.ExecutionResult{}, nil
}

func (s *chainBillingStore) RecordFailure(_ context.Context, _, _ string) (billing.ExecutionResult, error) {
	return billing.ExecutionResult{}, nil
}

func (s *chainBillingStore) MarkAttemptAbandoned(_ context.Context, _, _ string) (billing.ExecutionResult, error) {
	return billing.ExecutionResult{}, nil
}

func (s *chainBillingStore) reservations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserved
}

type chainPrices struct{}

func (chainPrices) LatestPriceVersion(_ context.Context, model string, _ time.Time) (billing.PriceVersion, error) {
	return billing.PriceVersion{ID: 1, Model: model}, nil
}

// chainUpstream reports every request that reaches it on entered, then answers
// once release is closed.
type chainUpstream struct {
	entered chan struct{}
	release chan struct{}
}

func (u *chainUpstream) Name() string { return "chain-primary" }

func (u *chainUpstream) ChatStream(ctx context.Context, _ []byte, w io.Writer) (*provider.Usage, error) {
	select {
	case u.entered <- struct{}{}:
	default:
	}
	select {
	case <-u.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return nil, err
	}
	return &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}, nil
}

// chainFixture serves the production chat chain (chatChain) in front of an
// in-process upstream. Billing is the counting fake, so no PostgreSQL is needed.
type chainFixture struct {
	chain    http.Handler
	billing  *chainBillingStore
	upstream *chainUpstream
}

func newChainFixture(lim *limiter.Limiter, orgID int64) *chainFixture {
	upstream := &chainUpstream{entered: make(chan struct{}, 8), release: make(chan struct{})}
	registry := provider.NewRegistry(map[string][]provider.Provider{chainTestModel: {upstream}})
	store := &chainBillingStore{}
	adapter := &billingAdapter{
		store:           store,
		prices:          chainPrices{},
		policy:          defaultAdmissionPolicy(),
		gatewayID:       "gw-chain-order-test",
		decisionTimeout: time.Second,
		knownModel:      registry.HasModel,
	}
	record := auth.NewKeyRecord("key-chain-order", chainTestRawKey, "org:"+strconv.FormatInt(orgID, 10))
	record.OrgID = orgID
	ownerID := orgID
	record.OwnerUserID = &ownerID
	chain := chatChain(
		auth.NewMemoryStoreFromRecords([]auth.KeyRecord{record}),
		gatewayguard.SnapshotGuard{Policies: gatewayguard.NewPolicySet()},
		gatewayguard.Options{KnownModel: registry.HasModel},
		lim,
		adapter,
		proxy.NewHandler(registry, nil),
	)
	return &chainFixture{chain: chain, billing: store, upstream: upstream}
}

func (f *chainFixture) post() *httptest.ResponseRecorder {
	body := `{"model":"` + chainTestModel + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+chainTestRawKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.chain.ServeHTTP(rec, req)
	return rec
}

// A request refused by the organization stream cap was never admitted, so it
// must not reserve funds or write a billing row. Only the admitted request may.
func TestOrgStreamCapRefusesBeforeBillingReservesFunds(t *testing.T) {
	lim := limiter.New(nil, 1000, 1000, 0)
	lim.SetMaxActiveStreamsPerOrg(1)
	f := newChainFixture(lim, time.Now().UnixNano())
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(f.upstream.release) }) }
	defer releaseUpstream()

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- f.post() }()
	select {
	case <-f.upstream.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the upstream")
	}

	second := f.post()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d, want 429 from the organization stream cap", second.Code)
	}
	if got := f.billing.reservations(); got != 1 {
		t.Fatalf("the refused request reserved funds: reservations=%d, want 1 (the admitted request only)", got)
	}
	releaseUpstream()
	if rec := <-first; rec.Code != http.StatusOK {
		t.Fatalf("admitted request status=%d, want 200", rec.Code)
	}
}

// The rate limiter is the first gate. Against Redis it refuses the second
// request of a one-token burst, and that refusal must not reach billing. Gated
// on LIMITER_TEST_REDIS_ADDR like the other limiter integration tests.
func TestRateLimitedRequestsNeverReachBilling(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("LIMITER_TEST_REDIS_ADDR"))
	if addr == "" {
		t.Skip("set LIMITER_TEST_REDIS_ADDR to run the rate-limit ordering test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	// One token, refilled at one per second. The organization is unique to this
	// run so no stale bucket from an earlier run can change the outcome.
	f := newChainFixture(limiter.New(rdb, 1, 1, 0), time.Now().UnixNano())
	close(f.upstream.release)

	if first := f.post(); first.Code != http.StatusOK {
		t.Fatalf("first request status=%d, want 200 (the burst token)", first.Code)
	}
	second := f.post()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d, want 429 from the rate limiter", second.Code)
	}
	if got := f.billing.reservations(); got != 1 {
		t.Fatalf("the rate-limited request reserved funds: reservations=%d, want 1", got)
	}
}
