package ready

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayReadyRequiresRoutesAndClosedRedis(t *testing.T) {
	redisDown := errors.New("dial tcp: connection refused")
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

func TestDependenciesFailClosed(t *testing.T) {
	st := Dependencies(nil, errors.New("ledger: closed"))
	if st.Ready || !strings.Contains(st.Messages[0], "ledger") {
		t.Fatalf("status=%+v", st)
	}
	rec := httptest.NewRecorder()
	Write(rec, st)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rec.Code)
	}
}
