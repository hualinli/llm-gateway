package ready

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// render runs Write and returns the probe body plus the status code, so a test
// can assert on exactly what an unauthenticated caller would see.
func render(t *testing.T, st Status) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	Write(rec, st)
	return rec.Code, rec.Body.String()
}

func TestGatewayReadyRequiresRoutesAndClosedRedis(t *testing.T) {
	redisDown := errors.New("dial tcp redis:6379: connect: connection refused")
	if st := Gateway(0, nil, false); st.Ready || !strings.Contains(strings.Join(st.Messages, " "), "no routes") {
		t.Fatalf("empty routes: %+v", st)
	}
	if st := Gateway(2, redisDown, false); !st.Ready || !strings.Contains(strings.Join(st.Messages, " "), "degraded") {
		t.Fatalf("fail-open redis: %+v", st)
	}
	if st := Gateway(2, redisDown, true); st.Ready {
		t.Fatalf("fail-closed redis should not be ready: %+v", st)
	}
	if st := Gateway(1, nil, true); !st.Ready || st.Messages[0] != "ok" {
		t.Fatalf("healthy gateway: %+v", st)
	}
}

// The probe body is unauthenticated and served from the business listener, so
// it must name the dependency at most and never quote the driver error. The
// operator still needs the error, which is why Messages keeps it.
func TestGatewayProbeBodyHidesRedisErrorText(t *testing.T) {
	const errText = "dial tcp redis:6379: connect: connection refused"
	redisDown := errors.New(errText)

	cases := []struct {
		name       string
		st         Status
		wantCode   int
		wantBody   string
		wantDetail string
	}{
		{"fail-closed redis", Gateway(2, redisDown, true), http.StatusServiceUnavailable, "dependency unavailable: redis\n", errText},
		{"fail-open redis", Gateway(2, redisDown, false), http.StatusOK, "degraded: redis\n", errText},
		{"no routes", Gateway(0, nil, false), http.StatusServiceUnavailable, "no routes\n", "no routes"},
		{"healthy", Gateway(1, nil, true), http.StatusOK, "ok\n", "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := render(t, tc.st)
			if code != tc.wantCode {
				t.Fatalf("code=%d, want %d", code, tc.wantCode)
			}
			if body != tc.wantBody {
				t.Fatalf("body=%q, want %q", body, tc.wantBody)
			}
			if strings.Contains(body, errText) || strings.Contains(body, "6379") {
				t.Fatalf("body leaked dependency detail: %q", body)
			}
			if !strings.Contains(strings.Join(tc.st.Messages, "\n"), tc.wantDetail) {
				t.Fatalf("Messages lost the operator detail %q: %+v", tc.wantDetail, tc.st)
			}
		})
	}
}

func TestDependenciesFailClosed(t *testing.T) {
	st := Dependencies(nil, errors.New("ledger: closed"))
	if st.Ready || !strings.Contains(st.Messages[0], "ledger") {
		t.Fatalf("status=%+v", st)
	}
	code, body := render(t, st)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", code)
	}
	if strings.Contains(body, "ledger") || strings.Contains(body, "closed") {
		t.Fatalf("body leaked dependency detail: %q", body)
	}
	if body != "dependency unavailable\n" {
		t.Fatalf("body=%q", body)
	}
}

// Multiple failing dependencies must not be distinguishable from the outside:
// guessing which one broke is the same leak as quoting the error.
func TestDependenciesBodyIsIndistinguishableAcrossFailures(t *testing.T) {
	one := Dependencies(errors.New("redis: dial tcp 10.0.0.5:6379: i/o timeout"))
	many := Dependencies(errors.New("redis: dial tcp 10.0.0.5:6379: i/o timeout"), errors.New("ledger: sql: deadlock detected"))
	_, bodyOne := render(t, one)
	_, bodyMany := render(t, many)
	if bodyOne != bodyMany {
		t.Fatalf("body leaks which dependency failed: %q vs %q", bodyOne, bodyMany)
	}
	if len(many.Messages) != 2 {
		t.Fatalf("Messages must keep every error: %+v", many)
	}
}

// A hand-built Status with no Summary still has to render something a probe can
// key on, and must keep the 503 contract.
func TestWriteFallsBackWhenSummaryIsUnset(t *testing.T) {
	if _, body := render(t, Status{Ready: true}); body != "ok\n" {
		t.Fatalf("ready body=%q", body)
	}
	code, body := render(t, Status{Ready: false})
	if code != http.StatusServiceUnavailable || body != "not ready\n" {
		t.Fatalf("not-ready code=%d body=%q", code, body)
	}
}
