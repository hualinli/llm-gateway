package limiter

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"llm-gateway/internal/auth"
)

// T09: the per-organization active-stream cap is an explicitly *per-instance*
// protection (docs/decisions.md「已知限制与决策索引」§ 组织并发上限是每实例
// 保护：不实施分布式并发租约).
// Two instances therefore add up. This test pins the semantics of one instance:
// the cap counts real in-flight streams, releases them when the handler returns,
// gives each organization its own budget, and passes through when disabled or
// when a request carries no organization attribution.
//
// cmd/gateway mounts this middleware around the chat handler only, so /v1/models
// never consumes a stream slot; that placement is asserted by the gateway's own
// route wiring, not here.

func orgRequest(orgID int64) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	record := auth.KeyRecord{ID: "key-1", TenantID: "tenant-1", OrgID: orgID}
	return req.WithContext(auth.WithKeyRecord(req.Context(), record))
}

// session is one in-flight request driven by a goroutine.
type session struct {
	recorder *httptest.ResponseRecorder
	finished chan struct{}
}

func start(handler http.Handler, orgID int64, entered chan string) *session {
	s := &session{recorder: httptest.NewRecorder(), finished: make(chan struct{})}
	go func() {
		defer close(s.finished)
		handler.ServeHTTP(s.recorder, orgRequest(orgID))
	}()
	return s
}

func TestStreamLimitCapsPerOrgOnOneInstance(t *testing.T) {
	lim := New(nil, 100000, 100000, 0)
	lim.SetMaxActiveStreamsPerOrg(2)

	release := make(chan struct{})
	entered := make(chan string, 8)
	handler := StreamLimit(lim)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- strconv.FormatInt(auth.OrgIDFromContext(r.Context()), 10)
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	first := start(handler, 1, entered)
	if got := <-entered; got != "1" {
		t.Fatalf("entered org=%s", got)
	}
	second := start(handler, 1, entered)
	if got := <-entered; got != "1" {
		t.Fatalf("entered org=%s", got)
	}

	// The third stream for the same organization is refused on this instance.
	third := httptest.NewRecorder()
	handler.ServeHTTP(third, orgRequest(1))
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("third stream status=%d, want 429", third.Code)
	}
	if got := lim.OrgStreamsInFlight(1); got != 2 {
		t.Fatalf("in-flight for org 1 = %d, want 2", got)
	}

	// A different organization has its own budget on this instance.
	other := start(handler, 2, entered)
	select {
	case got := <-entered:
		if got != "2" {
			t.Fatalf("entered org=%s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second organization was blocked by the first organization's cap")
	}
	if got := lim.OrgStreamsInFlight(2); got != 1 {
		t.Fatalf("in-flight for org 2 = %d, want 1 (instance caps are per organization)", got)
	}

	close(release)
	for _, s := range []*session{first, second, other} {
		select {
		case <-s.finished:
		case <-time.After(2 * time.Second):
			t.Fatal("a stream never finished after release")
		}
		if s.recorder.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200", s.recorder.Code)
		}
	}
	for _, orgID := range []int64{1, 2} {
		if got := lim.OrgStreamsInFlight(orgID); got != 0 {
			t.Fatalf("in-flight for org %d after release = %d, want 0", orgID, got)
		}
	}

	// With the slots released the organization can stream again.
	reuse := httptest.NewRecorder()
	reuseDone := make(chan struct{})
	releasing := make(chan struct{})
	close(releasing)
	reuseHandler := StreamLimit(lim)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	go func() { defer close(reuseDone); reuseHandler.ServeHTTP(reuse, orgRequest(1)) }()
	select {
	case <-reuseDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the released slot was not reusable")
	}
	_ = releasing
	if reuse.Code != http.StatusOK {
		t.Fatalf("post-release status=%d, want 200", reuse.Code)
	}
}

func TestStreamLimitDisabledOrUnattributedPassesThrough(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// Disabled cap: everything passes.
	recorder := httptest.NewRecorder()
	StreamLimit(New(nil, 1, 1, 0))(ok).ServeHTTP(recorder, orgRequest(7))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 when the cap is disabled", recorder.Code)
	}

	// Enabled cap, but a request without organization attribution must not be
	// counted against any organization's budget: it reaches the handler instead
	// of a 429.
	limited := New(nil, 1, 1, 0)
	limited.SetMaxActiveStreamsPerOrg(1)
	blocked := make(chan struct{})
	entered := make(chan int64, 2)
	counting := StreamLimit(limited)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgID := auth.OrgIDFromContext(r.Context())
		entered <- orgID
		if orgID > 0 { // the attributed stream stays in flight
			<-blocked
		}
		w.WriteHeader(http.StatusOK)
	}))
	occupied := httptest.NewRecorder()
	go func() { counting.ServeHTTP(occupied, orgRequest(7)) }()
	if got := <-entered; got != 7 {
		t.Fatalf("first entered org=%d, want 7", got)
	}
	if got := limited.OrgStreamsInFlight(7); got != 1 {
		t.Fatalf("in-flight for org 7 = %d, want 1", got)
	}

	// The same organization is now over its cap: the limiter must refuse before
	// the handler runs.
	overCap := httptest.NewRecorder()
	counting.ServeHTTP(overCap, orgRequest(7))
	if overCap.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap status=%d, want 429", overCap.Code)
	}

	anonymous := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	anonymousRecorder := httptest.NewRecorder()
	counting.ServeHTTP(anonymousRecorder, anonymous)
	if anonymousRecorder.Code != http.StatusOK {
		t.Fatalf("unattributed status=%d, want 200 (never counted against an organization)", anonymousRecorder.Code)
	}
	close(blocked)
}
