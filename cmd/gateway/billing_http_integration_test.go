package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/billing"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/httperr"
	"llm-gateway/internal/limiter"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
	"llm-gateway/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgreSQL + real HTTP acceptance tests for the T06C admission/finalization
// path. They are gated on GATEWAY_TEST_POSTGRES_DSN (or IAM_TEST_POSTGRES_DSN,
// which the billing/IAM suites use) and every test builds its own schema, so a
// business database is never touched.
//
// The chain under test is the production one:
//
//	observability → auth → access guard → limiter → billing → proxy → upstream
//
// with a real OpenAI-compatible SSE upstream and a real PostgreSQL for prices,
// reservations and receipts.

const (
	billingTestRawKey = "sk-billing-http-test"
	billingTestModel  = "gpt-billing"
	// billingUnpricedModel is registered in the registry but has no price
	// version: a priced-less model must fail closed.
	billingUnpricedModel = "gpt-unpriced"

	billingContentEvent = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	billingUsageEvent   = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"
	// billingInvalidUsageEvent is impossible usage: a negative count can never
	// be billed and must become a PG exception, not a zero-cost success.
	billingInvalidUsageEvent = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":-1,\"completion_tokens\":2,\"total_tokens\":1}}\n\n"
	billingDoneEvent         = "data: [DONE]\n\n"
)

// sseResponder serves a fixed list of SSE events, flushing after each one.
//
// It closes the upstream response ("Connection: close") on purpose. The fake
// upstream is not what T06C tests, and the provider's upstream keep-alive path
// has an unrelated failure mode under a rapid sequential loop: with an idle
// keep-alive connection, the handler can block in resp.Body.Close() until the
// client gives up (reproducible with GATEWAY_BILLING=off, so it is not a
// billing regression; reported separately). Closing the response keeps this
// suite about admission and the DONE boundary.
func sseResponder(events ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "close")
		flusher, _ := w.(http.Flusher)
		for _, event := range events {
			_, _ = io.WriteString(w, event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

type billingHarnessConfig struct {
	// respond is the upstream SSE behaviour.
	respond func(http.ResponseWriter, *http.Request)
	// creditMicro is the organization's opening balance. Ignored when
	// disableBilling is set.
	creditMicro int64
	// seedPrice publishes a price version for billingTestModel.
	seedPrice bool
	// disableBilling builds the chain with GATEWAY_BILLING=off (no adapter, no
	// finalizer): the legacy passthrough used as the latency baseline.
	disableBilling bool
	// registerUnpricedModel adds a second registry entry with no price.
	registerUnpricedModel bool

	admissionTimeout time.Duration
	receiptTimeout   time.Duration
	verifyTimeout    time.Duration
	decisionTimeout  time.Duration
	finalizeTimeout  time.Duration

	observer BillingObserver
}

type billingHarness struct {
	t             *testing.T
	server        *httptest.Server
	db            *sql.DB
	accounts      *billing.PGAccountStore
	prices        *billing.PGPriceVersionStore
	adapter       *billingAdapter
	closeBilling  func() error
	orgID         int64
	userID        int64
	membershipID  int64
	price         billing.PriceVersion
	upstreamCalls *atomic.Int32
	producer      *recordingEventProducer
	rawKey        string
}

type billingHarnessOption func(*billingHarnessConfig)

func withCredit(micro int64) billingHarnessOption {
	return func(c *billingHarnessConfig) { c.creditMicro = micro }
}

func withReceiptBounds(receipt, verify, finalize time.Duration) billingHarnessOption {
	return func(c *billingHarnessConfig) {
		c.receiptTimeout, c.verifyTimeout, c.finalizeTimeout = receipt, verify, finalize
	}
}

func withBillingDisabled() billingHarnessOption {
	return func(c *billingHarnessConfig) { c.disableBilling = true }
}

func withObserver(observer BillingObserver) billingHarnessOption {
	return func(c *billingHarnessConfig) { c.observer = observer }
}

func withUnpricedModel() billingHarnessOption {
	return func(c *billingHarnessConfig) { c.registerUnpricedModel = true }
}

func withUpstream(respond func(http.ResponseWriter, *http.Request)) billingHarnessOption {
	return func(c *billingHarnessConfig) { c.respond = respond }
}

// newBillingHarness builds one throwaway schema, one real HTTP chain and one
// fake upstream.
func newBillingHarness(t *testing.T, options ...billingHarnessOption) *billingHarness {
	t.Helper()
	config := billingHarnessConfig{
		respond:          sseResponder(billingContentEvent, billingUsageEvent, billingDoneEvent),
		creditMicro:      1_000_000,
		seedPrice:        true,
		admissionTimeout: 5 * time.Second,
		receiptTimeout:   3 * time.Second,
		verifyTimeout:    2 * time.Second,
		decisionTimeout:  2 * time.Second,
		finalizeTimeout:  5 * time.Second,
	}
	for _, option := range options {
		option(&config)
	}

	db, scoped := billingHTTPSchema(t)
	accounts := billing.NewPGAccountStore(db)
	accounts.AdmissionTimeout = 10 * time.Second
	prices := billing.NewPGPriceVersionStore(db)

	h := &billingHarness{
		t:             t,
		db:            db,
		accounts:      accounts,
		prices:        prices,
		upstreamCalls: &atomic.Int32{},
		producer:      &recordingEventProducer{},
		rawKey:        billingTestRawKey,
	}
	h.orgID, h.userID = h.seedOrg("billing-http")
	if !config.disableBilling {
		h.fund(config.creditMicro)
	}
	if config.seedPrice && !config.disableBilling {
		// 1 micro per token in both directions: a receipt's charge equals its
		// token count, and the reservation's upper bound equals its token bound.
		version, err := prices.CreatePriceVersion(context.Background(), billing.CreatePriceVersionParams{
			Model:                 billingTestModel,
			InputMicroPerMillion:  billing.MicrosPerMillionTokens,
			OutputMicroPerMillion: billing.MicrosPerMillionTokens,
			EffectiveAt:           time.Now().UTC().Add(-time.Hour),
		})
		if err != nil {
			t.Fatalf("seed price: %v", err)
		}
		h.price = version
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.upstreamCalls.Add(1)
		config.respond(w, r)
	}))
	t.Cleanup(upstream.Close)

	providerConfig := provider.DefaultOpenAIConfig("upstream", upstream.URL, "")
	providerConfig.AttemptTimeout = 30 * time.Second
	providerConfig.IdleTimeout = 5 * time.Second
	providerConfig.FirstByteTimeout = 5 * time.Second
	openaiProvider, err := provider.NewOpenAIProvider(providerConfig)
	if err != nil {
		t.Fatal(err)
	}
	registryModels := map[string][]provider.Provider{billingTestModel: {openaiProvider}}
	if config.registerUnpricedModel {
		registryModels[billingUnpricedModel] = []provider.Provider{provider.NewMockProvider("unpriced", 0)}
	}
	registry := provider.NewRegistry(registryModels)

	proxyHandler := proxy.NewHandler(registry, h.producer)
	proxyHandler.SetFinalizeTimeout(config.finalizeTimeout)

	if !config.disableBilling {
		adapter, closer, err := openBilling(billingConfig{
			Mode:                 billingPG,
			DSN:                  scoped,
			MaxOpenConns:         8,
			ConnMaxLifetime:      time.Minute,
			PingTimeout:          5 * time.Second,
			AdmissionTimeout:     config.admissionTimeout,
			ReceiptTimeout:       config.receiptTimeout,
			ReceiptVerifyTimeout: config.verifyTimeout,
			DecisionTimeout:      config.decisionTimeout,
			FinalizeTimeout:      config.finalizeTimeout,
			Policy:               defaultAdmissionPolicy(),
		}, "gw-billing-test", registry.HasModel)
		if err != nil {
			t.Fatalf("open billing: %v", err)
		}
		if adapter == nil {
			t.Fatal("GATEWAY_BILLING=pg must return an adapter")
		}
		adapter.SetObserver(config.observer)
		h.adapter = adapter
		h.closeBilling = closer
		t.Cleanup(func() { _ = closer() })
		proxyHandler.SetFinalizer(adapter)
	}

	// Snapshot guard: the chain still resolves the verified attribution
	// (org/user/key/execution) that admission requires, without a second
	// PostgreSQL guard (covered by internal/gatewayguard's own suite).
	guard := gatewayguard.SnapshotGuard{Policies: gatewayguard.NewPolicySet()}
	keyRecord := auth.NewKeyRecord("key-billing", h.rawKey, fmt.Sprintf("org:%d", h.orgID))
	keyRecord.OrgID = h.orgID
	keyRecord.OwnerUserID = &h.userID
	keyRecord.MembershipID = h.membershipID
	authStore := auth.NewMemoryStoreFromRecords([]auth.KeyRecord{keyRecord})

	rotator := limiter.New(nil, 1000, 1000, 0)
	var chat http.Handler = proxyHandler
	if h.adapter != nil {
		chat = h.adapter.Middleware(chat)
	}
	chain := observability.Middleware(
		auth.Middleware(authStore)(
			gatewayguard.Middleware(guard, gatewayguard.Options{KnownModel: registry.HasModel})(
				limiter.Middleware(rotator)(chat))))
	h.server = httptest.NewServer(chain)
	t.Cleanup(h.server.Close)
	return h
}

// billingHTTPSchema creates a throwaway schema with the full migration set and
// returns a pool scoped to it.
func billingHTTPSchema(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("GATEWAY_TEST_POSTGRES_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("IAM_TEST_POSTGRES_DSN"))
	}
	if dsn == "" {
		t.Skip("set GATEWAY_TEST_POSTGRES_DSN (or IAM_TEST_POSTGRES_DSN) to run the billing HTTP integration tests")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("GATEWAY_TEST_POSTGRES_DSN must be a PostgreSQL URL")
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
	schema := "gateway_billing_http_" + id
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
	return db, scoped
}

func (h *billingHarness) seedOrg(name string) (int64, int64) {
	h.t.Helper()
	now := time.Now().UTC()
	var userID int64
	if err := h.db.QueryRow(`INSERT INTO users
  (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ($1, 'hash', 'active', false, 1, $2, $2) RETURNING id`, name+"@example.com", now).Scan(&userID); err != nil {
		h.t.Fatalf("seed user: %v", err)
	}
	var orgID int64
	if err := h.db.QueryRow(`INSERT INTO organizations
  (type, name, status, owner_user_id, created_at, updated_at)
VALUES ('enterprise', $1, 'active', $2, $3, $3) RETURNING id`, name, userID, now).Scan(&orgID); err != nil {
		h.t.Fatalf("seed organization: %v", err)
	}
	if err := h.db.QueryRow(`INSERT INTO memberships
  (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
VALUES ($1, $2, 'owner', 'active', NULL, $3, $3) RETURNING id`, orgID, userID, now).Scan(&h.membershipID); err != nil {
		h.t.Fatalf("seed owner membership: %v", err)
	}
	return orgID, userID
}

func (h *billingHarness) fund(micro int64) {
	h.t.Helper()
	if micro <= 0 {
		// A zero balance is created by the first admission's EnsureAccount; an
		// explicit row keeps assertions simpler.
		if _, _, err := h.accounts.EnsureAccount(context.Background(), h.orgID); err != nil {
			h.t.Fatalf("ensure account: %v", err)
		}
		return
	}
	reason := "billing http test funding"
	params := billing.AdjustParams{
		OrgID:          h.orgID,
		AmountMicro:    micro,
		Direction:      billing.DirectionCredit,
		Kind:           billing.KindAdjustment,
		Reason:         reason,
		IdempotencyKey: fmt.Sprintf("fund-%d-%d", h.orgID, micro),
	}
	params.PayloadDigest = billing.AdjustmentDigest(h.orgID, micro, params.Direction, params.Kind, reason)
	if _, err := h.accounts.Adjust(context.Background(), params); err != nil {
		h.t.Fatalf("fund: %v", err)
	}
}

// defaultBody is one billable text request with a declared output limit.
func (h *billingHarness) defaultBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hello"}],"max_tokens":64}`, model)
}

// post sends one authenticated chat completion with a bounded request context.
// The returned cancel must be called by the test (it is also safe to defer).
func (h *billingHarness) post(model, body string) (*http.Response, context.CancelFunc) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.rawKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		cancel()
		h.t.Fatalf("request failed: %v", err)
	}
	return resp, cancel
}

func (h *billingHarness) executionIDs() []string {
	h.t.Helper()
	rows, err := h.db.Query(`SELECT execution_id FROM billing_executions ORDER BY created_at`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return ids
}

func (h *billingHarness) singleExecutionID() string {
	h.t.Helper()
	ids := h.executionIDs()
	if len(ids) != 1 {
		h.t.Fatalf("executions=%v, want exactly one admission", ids)
	}
	return ids[0]
}

func (h *billingHarness) execution(executionID string) billing.Execution {
	h.t.Helper()
	execution, err := h.accounts.GetExecution(context.Background(), executionID)
	if err != nil {
		h.t.Fatalf("get execution: %v", err)
	}
	return execution
}

func (h *billingHarness) receipt(executionID string) billing.Receipt {
	h.t.Helper()
	receipt, err := h.accounts.GetReceipt(context.Background(), executionID)
	if err != nil {
		h.t.Fatalf("get receipt: %v", err)
	}
	return receipt
}

func (h *billingHarness) account() billing.Account {
	h.t.Helper()
	account, err := h.accounts.GetAccount(context.Background(), h.orgID)
	if err != nil {
		h.t.Fatalf("get account: %v", err)
	}
	return account
}

func (h *billingHarness) count(query string, args ...any) int {
	h.t.Helper()
	var count int
	if err := h.db.QueryRow(query, args...).Scan(&count); err != nil {
		h.t.Fatalf("count %q: %v", query, err)
	}
	return count
}

// lockReceiptsTable holds ACCESS EXCLUSIVE on usage_receipts, so the next
// receipt INSERT blocks until the returned release function is called. That is
// how the tests observe "the finalization is in flight".
func (h *billingHarness) lockReceiptsTable() func() {
	h.t.Helper()
	ctx := context.Background()
	conn, err := h.db.Conn(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `LOCK TABLE usage_receipts IN ACCESS EXCLUSIVE MODE`); err != nil {
		h.t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback()
			_ = conn.Close()
		})
	}
	h.t.Cleanup(release)
	return release
}

// waitForBlockedReceiptWrite waits until a backend is visibly waiting on the
// usage_receipts table lock. Depending on the replay check, the blocked
// statement can be the digest SELECT or the INSERT; either proves that the
// finalization is in flight.
func (h *billingHarness) waitForBlockedReceiptWrite(timeout time.Duration) {
	h.t.Helper()
	waitFor(h.t, timeout, "the receipt write to block on the table lock", func() bool {
		var count int
		err := h.db.QueryRow(`SELECT count(*) FROM pg_stat_activity
 WHERE datname = current_database() AND wait_event_type = 'Lock'
   AND query LIKE '%usage_receipts%'`).Scan(&count)
		return err == nil && count > 0
	})
}

// dumpBackends prints what every backend is doing; it is only called on a
// failure so a stuck finalization is diagnosable from the test output.
func (h *billingHarness) dumpBackends() {
	rows, err := h.db.Query(`SELECT pid, coalesce(state,''), coalesce(wait_event_type,''), coalesce(wait_event,''),
       coalesce(left(query, 160),''), coalesce(extract(epoch from now()-query_start)::text,'')
FROM pg_stat_activity WHERE datname = current_database() ORDER BY pid`)
	if err != nil {
		h.t.Logf("pg_stat_activity: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var pid int
		var state, waitType, waitEvent, query, age string
		if err := rows.Scan(&pid, &state, &waitType, &waitEvent, &query, &age); err != nil {
			h.t.Logf("scan: %v", err)
			return
		}
		h.t.Logf("backend pid=%d state=%q wait=%s/%s age=%ss query=%q", pid, state, waitType, waitEvent, age, query)
	}
	locks, err := h.db.Query(`SELECT l.pid, l.locktype, l.mode, l.granted, coalesce(c.relname,'')
FROM pg_locks l LEFT JOIN pg_class c ON c.oid = l.relation
WHERE l.pid <> pg_backend_pid() ORDER BY l.pid`)
	if err != nil {
		h.t.Logf("pg_locks: %v", err)
		return
	}
	defer locks.Close()
	for locks.Next() {
		var pid int
		var lockType, mode, relation string
		var granted bool
		if err := locks.Scan(&pid, &lockType, &mode, &granted, &relation); err != nil {
			h.t.Logf("scan locks: %v", err)
			return
		}
		h.t.Logf("lock pid=%d type=%s mode=%s granted=%t relation=%s", pid, lockType, mode, granted, relation)
	}
	h.dumpGoroutines()
}

// dumpGoroutines prints the stacks of goroutines that belong to this module, so
// a stuck handler is diagnosable instead of merely observed.
func (h *billingHarness) dumpGoroutines() {
	var buffer bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buffer, 1); err != nil {
		h.t.Logf("goroutine dump: %v", err)
		return
	}
	blocks := strings.Split(buffer.String(), "\n\n")
	for _, block := range blocks {
		switch {
		case strings.Contains(block, "dumpGoroutines"), strings.Contains(block, "testing.(*T)."):
			continue
		case strings.Contains(block, "llm-gateway/"),
			strings.Contains(block, "sseResponder"),
			strings.Contains(block, "net/http.(*conn).serve"):
			h.t.Logf("goroutine:\n%s", block)
		}
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// sseReader reads one SSE response incrementally so a test can observe the wire
// between events.
type sseReader struct {
	t     *testing.T
	lines chan streamLine
	body  strings.Builder
	// doneAt is when the protocol end marker reached the client.
	doneAt time.Time
}

type streamLine struct {
	line string
	err  error
}

func newSSEReader(t *testing.T, resp *http.Response) *sseReader {
	t.Helper()
	reader := &sseReader{t: t, lines: make(chan streamLine, 64)}
	go func() {
		defer close(reader.lines)
		buffered := bufio.NewReader(resp.Body)
		for {
			line, err := buffered.ReadString('\n')
			reader.lines <- streamLine{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()
	return reader
}

// next returns the next line, or an error when the timeout expires.
func (r *sseReader) next(timeout time.Duration) (string, error) {
	select {
	case result, ok := <-r.lines:
		if !ok {
			return "", io.EOF
		}
		r.body.WriteString(result.line)
		if strings.Contains(result.line, "[DONE]") {
			r.doneAt = time.Now()
		}
		return result.line, result.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for the next SSE line (body=%q)", r.body.String())
	}
}

// expectQuiet fails if any byte arrives within the window.
func (r *sseReader) expectQuiet(window time.Duration) {
	r.t.Helper()
	select {
	case result, ok := <-r.lines:
		if !ok {
			return
		}
		r.body.WriteString(result.line)
		r.t.Fatalf("the gateway released %q while finalization was still in flight (body=%q)", result.line, r.body.String())
	case <-time.After(window):
	}
}

// drain reads until EOF or the timeout.
func (r *sseReader) drain(timeout time.Duration) string {
	r.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			r.t.Fatalf("timed out draining the stream (body=%q)", r.body.String())
		}
		if _, err := r.next(remaining); err != nil {
			return r.body.String()
		}
	}
}

func (r *sseReader) stream() string { return r.body.String() }

// --- tests --------------------------------------------------------------------

// TestBillingHTTPCompletedStreamCommitsReceiptBeforeDone is the core T06C
// claim: a completed stream with valid usage has a durable receipt committed
// before the client sees "[DONE]", and the same committed instant is recorded
// on the execution.
func TestBillingHTTPCompletedStreamCommitsReceiptBeforeDone(t *testing.T) {
	h := newBillingHarness(t)
	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	reader := newSSEReader(t, resp)
	stream := reader.drain(15 * time.Second)
	if !strings.HasSuffix(stream, "[DONE]\n\n") {
		t.Fatalf("stream did not end with [DONE]: %q", stream)
	}
	if reader.doneAt.IsZero() {
		t.Fatal("the client never observed the end marker")
	}

	executionID := h.singleExecutionID()
	receipt := h.receipt(executionID)
	if receipt.Status != billing.ReceiptStatusPendingDispatch {
		t.Fatalf("receipt status=%q, want %q", receipt.Status, billing.ReceiptStatusPendingDispatch)
	}
	if receipt.PromptTokens != 10 || receipt.OutputTokens != 2 || receipt.TotalTokens != 12 {
		t.Fatalf("receipt usage=%+v", receipt)
	}
	// The price is 1 micro per token, so the charge is the weighted sum rounded
	// once: 10 + 2.
	if receipt.ChargeMicro != 12 {
		t.Fatalf("charge=%d, want 12", receipt.ChargeMicro)
	}
	if receipt.PriceVersionID != h.price.ID {
		t.Fatalf("price version=%d, want the pinned %d", receipt.PriceVersionID, h.price.ID)
	}
	if receipt.DispatchAfter == nil || receipt.SettledAt != nil || receipt.Attempts != 0 {
		t.Fatalf("outbox fields=%+v", receipt)
	}
	if receipt.CreatedAt.After(reader.doneAt) {
		t.Fatalf("the receipt was committed at %s, after the client saw [DONE] at %s",
			receipt.CreatedAt, reader.doneAt)
	}

	execution := h.execution(executionID)
	if execution.Status != billing.StatusReceiptRecorded {
		t.Fatalf("execution status=%q, want %q", execution.Status, billing.StatusReceiptRecorded)
	}
	if execution.ReceiptRecordedAt == nil || !execution.ReceiptRecordedAt.Equal(receipt.CreatedAt) {
		t.Fatalf("execution receipt_recorded_at=%v, receipt created_at=%v; the two must be one transaction",
			execution.ReceiptRecordedAt, receipt.CreatedAt)
	}
	// The reservation is still held: T06C records consumption evidence, T07
	// settles it.
	if account := h.account(); account.ReservedMicro <= 0 {
		t.Fatalf("reserved=%d, want the admission's reservation still held", account.ReservedMicro)
	}
	if got := h.count(`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind IN ('debit','release')`, h.orgID); got != 0 {
		t.Fatalf("settlement-like ledger rows=%d, want 0", got)
	}
	if len(h.producer.events) != 1 {
		t.Fatalf("metering events=%d, want 1", len(h.producer.events))
	}
}

// TestBillingHTTPBlockingReceiptWithholdsDone observes the wire while the
// receipt INSERT is blocked by a table lock: the client must see the content
// event, then nothing, and "[DONE]" only after the receipt committed.
func TestBillingHTTPBlockingReceiptWithholdsDone(t *testing.T) {
	h := newBillingHarness(t, withReceiptBounds(15*time.Second, 5*time.Second, 20*time.Second))
	release := h.lockReceiptsTable()

	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	reader := newSSEReader(t, resp)

	// The content event is not a terminal boundary: it must arrive while the
	// receipt write is still blocked. Read the whole event (data line plus its
	// terminating blank line).
	sawContent := false
	for {
		line, err := reader.next(10 * time.Second)
		if err != nil {
			t.Fatalf("reading the content event: %v", err)
		}
		if strings.Contains(line, `"content":"hi"`) {
			sawContent = true
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	if !sawContent {
		t.Fatalf("the content event never arrived: %q", reader.stream())
	}
	// The usage event is the terminal boundary: it (and [DONE]) must not be
	// released while the receipt is uncommitted.
	reader.expectQuiet(300 * time.Millisecond)
	h.waitForBlockedReceiptWrite(10 * time.Second)
	if got := reader.stream(); strings.Contains(got, "[DONE]") {
		t.Fatalf("[DONE] reached the client before the receipt committed: %q", got)
	}
	executionID := h.singleExecutionID()
	if status := h.execution(executionID).Status; status == billing.StatusReceiptRecorded {
		t.Fatal("the execution must not be receipt_recorded while the receipt insert is blocked")
	}

	release()
	stream := reader.drain(20 * time.Second)
	if !strings.HasSuffix(stream, "[DONE]\n\n") {
		t.Fatalf("stream did not end with [DONE] after the receipt committed: %q", stream)
	}
	receipt := h.receipt(executionID)
	if receipt.CreatedAt.After(reader.doneAt) {
		t.Fatalf("receipt committed at %s, after [DONE] at %s", receipt.CreatedAt, reader.doneAt)
	}
}

// TestBillingHTTPReceiptFailureWithholdsDoneAndKeepsReservation is the
// failpoint acceptance: with the receipt INSERT forced to fail, the client must
// NOT receive "[DONE]" and the execution must keep its reservation.
func TestBillingHTTPReceiptFailureWithholdsDoneAndKeepsReservation(t *testing.T) {
	h := newBillingHarness(t)
	if _, err := h.db.Exec(`ALTER TABLE usage_receipts RENAME TO usage_receipts_failpoint`); err != nil {
		t.Fatalf("install failpoint: %v", err)
	}
	executionID := ""
	reservedBefore := int64(0)

	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	reader := newSSEReader(t, resp)
	stream := reader.drain(15 * time.Second)
	if strings.Contains(stream, "[DONE]") {
		t.Fatalf("a failed receipt must not release [DONE]: %q", stream)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d (the SSE status is committed before the terminal event)", resp.StatusCode)
	}
	executionID = h.singleExecutionID()
	// The reservation is still held and the execution is still an anchor: a
	// failed finalization must not silently release money.
	execution := h.execution(executionID)
	if execution.Status == billing.StatusReleased || execution.Status == billing.StatusFailedNoCharge {
		t.Fatalf("execution status=%q: a failed receipt must not release the reservation", execution.Status)
	}
	if execution.Status != billing.StatusExecuting {
		t.Fatalf("execution status=%q, want it left as %q for T07 recovery", execution.Status, billing.StatusExecuting)
	}
	if account := h.account(); account.ReservedMicro <= 0 {
		t.Fatalf("reserved=%d, want the reservation held", account.ReservedMicro)
	}
	reservedBefore = h.account().ReservedMicro
	if got := h.count(`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='release'`, h.orgID); got != 0 {
		t.Fatalf("release entries=%d, want 0", got)
	}
	if reservedBefore != execution.ReservedMicro {
		t.Fatalf("reserved=%d, execution reserved=%d", reservedBefore, execution.ReservedMicro)
	}
	if got := h.count(`SELECT count(*) FROM usage_receipts_failpoint`); got != 0 {
		t.Fatalf("receipt rows=%d, want 0", got)
	}
}

// TestBillingHTTPClientCancellationAfterFinalizeStartsStillCommitsReceipt
// proves the detached, bounded finalization context: the client disconnects
// while the receipt write is in flight, and the durable decision still commits.
func TestBillingHTTPClientCancellationAfterFinalizeStartsStillCommitsReceipt(t *testing.T) {
	h := newBillingHarness(t, withReceiptBounds(15*time.Second, 5*time.Second, 20*time.Second))
	release := h.lockReceiptsTable()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions",
		strings.NewReader(h.defaultBody(billingTestModel)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.rawKey)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := newSSEReader(t, resp)
	sawContent := false
	for {
		line, err := reader.next(10 * time.Second)
		if err != nil {
			t.Fatalf("reading the content event: %v", err)
		}
		if strings.Contains(line, `"content":"hi"`) {
			sawContent = true
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	if !sawContent {
		t.Fatalf("the content event never arrived: %q", reader.stream())
	}
	// The finalization is running (and blocked on the lock) when the client
	// goes away.
	h.waitForBlockedReceiptWrite(10 * time.Second)
	executionID := h.singleExecutionID()
	cancel()
	// Give the server a moment to observe the disconnect, then let the blocked
	// write finish.
	time.Sleep(100 * time.Millisecond)
	release()

	waitFor(t, 15*time.Second, "the durable receipt of the disconnected client", func() bool {
		var status string
		if err := h.db.QueryRow(`SELECT status FROM usage_receipts WHERE execution_id=$1`, executionID).Scan(&status); err != nil {
			return false
		}
		return status == billing.ReceiptStatusPendingDispatch
	})
	receipt := h.receipt(executionID)
	if receipt.PromptTokens != 10 || receipt.OutputTokens != 2 || receipt.ChargeMicro != 12 {
		t.Fatalf("receipt=%+v, want the full committed usage", receipt)
	}
	if status := h.execution(executionID).Status; status != billing.StatusReceiptRecorded {
		t.Fatalf("execution status=%q, want %q", status, billing.StatusReceiptRecorded)
	}
}

// TestBillingHTTPMissingOrInvalidUsageLeavesPGException covers both halves of
// "usage unknown is not zero": the client gets no "[DONE]", PostgreSQL keeps an
// exception receipt, the execution needs review, and the reservation stays
// held.
func TestBillingHTTPMissingOrInvalidUsageLeavesPGException(t *testing.T) {
	cases := []struct {
		name    string
		respond func(http.ResponseWriter, *http.Request)
		reason  string
	}{
		{
			name:    "missing usage",
			respond: sseResponder(billingContentEvent, billingDoneEvent),
			reason:  "usage",
		},
		{
			name:    "invalid usage",
			respond: sseResponder(billingContentEvent, billingInvalidUsageEvent, billingDoneEvent),
			reason:  "usage",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newBillingHarness(t, withUpstream(tc.respond))
			resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
			defer cancel()
			reader := newSSEReader(t, resp)
			stream := reader.drain(15 * time.Second)
			if strings.Contains(stream, "[DONE]") {
				t.Fatalf("unknown usage must not end cleanly: %q", stream)
			}

			executionID := h.singleExecutionID()
			receipt := h.receipt(executionID)
			if receipt.Status != billing.ReceiptStatusException {
				t.Fatalf("receipt status=%q, want %q", receipt.Status, billing.ReceiptStatusException)
			}
			if receipt.ChargeMicro != 0 || receipt.PromptTokens != 0 || receipt.OutputTokens != 0 || receipt.TotalTokens != 0 {
				t.Fatalf("an exception must not carry a charge or usage: %+v", receipt)
			}
			if receipt.LastError == nil || !strings.Contains(*receipt.LastError, tc.reason) {
				t.Fatalf("last_error=%v, want a %q explanation", receipt.LastError, tc.reason)
			}
			if receipt.DispatchAfter != nil {
				t.Fatalf("an exception must not be dispatchable: %v", receipt.DispatchAfter)
			}
			if execution := h.execution(executionID); execution.Status != billing.StatusReviewRequired {
				t.Fatalf("execution status=%q, want %q", execution.Status, billing.StatusReviewRequired)
			}
			// The reservation is evidence: it stays held for the review.
			if account := h.account(); account.ReservedMicro <= 0 {
				t.Fatalf("reserved=%d, want the reservation held for review", account.ReservedMicro)
			}
			if got := h.count(`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='release'`, h.orgID); got != 0 {
				t.Fatalf("release entries=%d, want 0", got)
			}
			// The PG state exists even though the terminal event carried nothing
			// usable: not only a Redis DLQ row.
			if got := h.count(`SELECT count(*) FROM console_audit_events WHERE action=$1`, billing.ActionReceiptException); got != 1 {
				t.Fatalf("exception audit rows=%d, want 1", got)
			}
		})
	}
}

// TestBillingHTTPInsufficientFundsNeverReachesUpstream is the money acceptance:
// a wallet that cannot cover the request's maximum cost refuses with 402 and
// the fake upstream is never called.
func TestBillingHTTPInsufficientFundsNeverReachesUpstream(t *testing.T) {
	h := newBillingHarness(t, withCredit(5))
	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status=%d, want 402", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if envelope.Error.Code != httperr.CodeInsufficientBalance {
		t.Fatalf("code=%q, want %q", envelope.Error.Code, httperr.CodeInsufficientBalance)
	}
	if got := h.upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls=%d, want 0: an unaffordable request must never reach an upstream", got)
	}
	if got := h.count(`SELECT count(*) FROM billing_executions`); got != 0 {
		t.Fatalf("execution rows=%d, want 0: a refused admission writes no anchor", got)
	}
}

// TestBillingHTTPUnpricedModelFailsClosed: a model the registry serves but the
// price table does not know is a 503, not a free request.
func TestBillingHTTPUnpricedModelFailsClosed(t *testing.T) {
	h := newBillingHarness(t, withUnpricedModel())
	resp, cancel := h.post(billingUnpricedModel, h.defaultBody(billingUnpricedModel))
	defer cancel()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != httperr.CodeBillingUnavailable || !strings.Contains(envelope.Error.Message, "price") {
		t.Fatalf("refusal=%+v, want billing_unavailable with a price message", envelope.Error)
	}
	if got := h.upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls=%d, want 0", got)
	}
}

// TestBillingHTTPPostgresUnavailableAtAdmissionFailsClosed closes the adapter's
// own pool: admission must refuse with 503 instead of quietly letting the
// request through unbilled.
func TestBillingHTTPPostgresUnavailableAtAdmissionFailsClosed(t *testing.T) {
	h := newBillingHarness(t)
	if h.closeBilling == nil {
		t.Fatal("the harness must expose the billing pool closer")
	}
	if err := h.closeBilling(); err != nil {
		t.Fatalf("close billing pool: %v", err)
	}
	resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
	defer cancel()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != httperr.CodeBillingUnavailable {
		t.Fatalf("code=%q, want %q", envelope.Error.Code, httperr.CodeBillingUnavailable)
	}
	if got := h.upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls=%d, want 0: billing unavailable must fail closed", got)
	}
}

// TestBillingHTTPUnboundableRequestIsRefusedExplicitly: a request the policy
// cannot bound never reaches an upstream and never holds funds.
func TestBillingHTTPUnboundableRequestIsRefusedExplicitly(t *testing.T) {
	h := newBillingHarness(t)
	cases := map[string]string{
		"tool definitions": `{"model":"` + billingTestModel + `","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`,
		"no messages":      `{"model":"` + billingTestModel + `","stream":true,"messages":[]}`,
		"output above cap": `{"model":"` + billingTestModel + `","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":999999}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, cancel := h.post(billingTestModel, body)
			defer cancel()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", resp.StatusCode)
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != httperr.CodeInvalidRequest {
				t.Fatalf("code=%q, want %q", envelope.Error.Code, httperr.CodeInvalidRequest)
			}
		})
	}
	if got := h.upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls=%d, want 0", got)
	}
	if got := h.count(`SELECT count(*) FROM billing_executions`); got != 0 {
		t.Fatalf("execution rows=%d, want 0: an unbounded request must not hold funds", got)
	}
	if got := h.count(`SELECT count(*) FROM usage_receipts`); got != 0 {
		t.Fatalf("receipt rows=%d, want 0", got)
	}
}

// --- latency ------------------------------------------------------------------

// billingLatencyRecorder collects the two phases the admission adapter adds.
type billingLatencyRecorder struct {
	mu         sync.Mutex
	admission  []time.Duration
	finalize   []time.Duration
	admissionR map[string]int
	finalizeR  map[string]int
}

func newBillingLatencyRecorder() *billingLatencyRecorder {
	return &billingLatencyRecorder{admissionR: map[string]int{}, finalizeR: map[string]int{}}
}

func (r *billingLatencyRecorder) ObserveAdmission(model, result string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.admission = append(r.admission, d)
	r.admissionR[result]++
}

func (r *billingLatencyRecorder) ObserveFinalization(model, result string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalize = append(r.finalize, d)
	r.finalizeR[result]++
}

func percentile(durations []time.Duration, fraction float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	index := int(float64(len(sorted)-1) * fraction)
	return sorted[index]
}

// TestBillingHTTPAddedLatency measures the admission and finalization latency
// the billing path adds, and compares the end-to-end request duration against
// the same chain with GATEWAY_BILLING=off (no finalizer, legacy passthrough).
func TestBillingHTTPAddedLatency(t *testing.T) {
	const rounds = 40
	recorder := newBillingLatencyRecorder()
	billed := newBillingHarness(t, withObserver(recorder))

	measure := func(h *billingHarness) []time.Duration {
		h.t.Helper()
		durations := make([]time.Duration, 0, rounds)
		for i := 0; i < rounds; i++ {
			start := time.Now()
			resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
			reader := newSSEReader(t, resp)
			stream := reader.drain(15 * time.Second)
			cancel()
			_ = resp.Body.Close()
			if !strings.Contains(stream, "[DONE]") {
				h.dumpBackends()
				t.Fatalf("round %d did not complete: %q", i, stream)
			}
			durations = append(durations, time.Since(start))
		}
		return durations
	}

	billedDurations := measure(billed)
	unbilled := newBillingHarness(t, withBillingDisabled())
	unbilledDurations := measure(unbilled)

	recorder.mu.Lock()
	admissions := append([]time.Duration(nil), recorder.admission...)
	finalizations := append([]time.Duration(nil), recorder.finalize...)
	admissionResults := fmt.Sprint(recorder.admissionR)
	finalizationResults := fmt.Sprint(recorder.finalizeR)
	recorder.mu.Unlock()

	if len(admissions) != rounds || len(finalizations) != rounds {
		t.Fatalf("observations: admissions=%d finalizations=%d, want %d each", len(admissions), len(finalizations), rounds)
	}
	t.Logf("billing admission  p50=%s p95=%s (n=%d, results=%s)",
		percentile(admissions, 0.50), percentile(admissions, 0.95), len(admissions), admissionResults)
	t.Logf("billing finalize   p50=%s p95=%s (n=%d, results=%s)",
		percentile(finalizations, 0.50), percentile(finalizations, 0.95), len(finalizations), finalizationResults)
	t.Logf("end-to-end request p50=%s p95=%s with billing; p50=%s p95=%s without billing",
		percentile(billedDurations, 0.50), percentile(billedDurations, 0.95),
		percentile(unbilledDurations, 0.50), percentile(unbilledDurations, 0.95))

	// The budgets are generous on purpose (a shared CI PostgreSQL is not a
	// latency benchmark); the point is that neither phase can grow unbounded.
	if got := percentile(admissions, 0.95); got > time.Second {
		t.Fatalf("admission p95=%s, above the 1s budget", got)
	}
	if got := percentile(finalizations, 0.95); got > time.Second {
		t.Fatalf("finalization p95=%s, above the 1s budget", got)
	}
}

// TestBillingHTTPSequentialRequestsProduceOneReceiptEach guards against an
// accidental "one receipt per request" assumption: two different executions
// produce two receipts, each with its own event identity.
func TestBillingHTTPSequentialRequestsProduceOneReceiptEach(t *testing.T) {
	h := newBillingHarness(t)
	for i := 0; i < 2; i++ {
		resp, cancel := h.post(billingTestModel, h.defaultBody(billingTestModel))
		reader := newSSEReader(t, resp)
		stream := reader.drain(15 * time.Second)
		cancel()
		_ = resp.Body.Close()
		if !strings.Contains(stream, "[DONE]") {
			t.Fatalf("round %d: %q", i, stream)
		}
	}
	ids := h.executionIDs()
	if len(ids) != 2 {
		t.Fatalf("executions=%v, want 2", ids)
	}
	if got := h.count(`SELECT count(*) FROM usage_receipts`); got != 2 {
		t.Fatalf("receipts=%d, want 2", got)
	}
	if got := h.count(`SELECT count(DISTINCT event_id) FROM usage_receipts`); got != 2 {
		t.Fatalf("distinct event ids=%d, want 2", got)
	}
	for _, id := range ids {
		if receipt := h.receipt(id); receipt.ChargeMicro != 12 {
			t.Fatalf("receipt %s charge=%d, want 12", id, receipt.ChargeMicro)
		}
	}
	// Two admissions, two reservations, no settlement: the projection equals
	// the ledger.
	check, err := h.accounts.RecalculateProjection(context.Background(), h.orgID)
	if err != nil {
		t.Fatal(err)
	}
	if !check.Matches {
		t.Fatalf("projection mismatch: %+v", check)
	}
}

// Unsupported request shapes must fail before funds are reserved or upstream
// work is performed, even when their misleading byte/choice bound would fit.
func TestBillingHTTPRejectsUnboundedChoicesAndImages(t *testing.T) {
	for name, body := range map[string]string{
		"multiple choices": `{"model":"gpt-billing","stream":true,"n":8,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`,
		"remote image":     `{"model":"gpt-billing","stream":true,"max_tokens":1,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x.co/i","detail":"high"}}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newBillingHarness(t, withCredit(300))
			resp, cancel := h.post(billingTestModel, body)
			defer cancel()
			defer resp.Body.Close()
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadRequest || envelope.Error.Code != httperr.CodeInvalidRequest {
				t.Fatalf("status=%d envelope=%+v", resp.StatusCode, envelope)
			}
			if h.upstreamCalls.Load() != 0 || len(h.executionIDs()) != 0 || h.account().ReservedMicro != 0 {
				t.Fatal("unsupported request reached upstream or reserved funds")
			}
		})
	}
}
