package ready

import (
	"net/http"
	"strings"
)

// Status is a process probe result. Live means the process can answer;
// Ready means it can serve the core request path.
type Status struct {
	Ready    bool
	Messages []string
}

// Live reports that the process is answering probes.
func Live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Gateway is ready when at least one route is loaded. Redis is required only
// when the limiter is configured fail-closed; fail-open stays ready and says so.
func Gateway(routeCount int, redisErr error, failClosed bool) Status {
	st := Status{Ready: true}
	if routeCount == 0 {
		st.Ready = false
		st.Messages = append(st.Messages, "no routes")
	}
	if redisErr != nil {
		msg := "redis: " + redisErr.Error()
		if failClosed {
			st.Ready = false
			st.Messages = append(st.Messages, msg)
		} else {
			st.Messages = append(st.Messages, "limiter degraded: "+msg)
		}
	}
	if st.Ready && len(st.Messages) == 0 {
		st.Messages = []string{"ok"}
	}
	return st
}

// Dependencies fails closed when any dependency error is non-nil.
func Dependencies(errs ...error) Status {
	st := Status{Ready: true}
	for _, err := range errs {
		if err != nil {
			st.Ready = false
			st.Messages = append(st.Messages, err.Error())
		}
	}
	if st.Ready {
		st.Messages = []string{"ok"}
	}
	return st
}

// Write renders a probe body. Not-ready responses are HTTP 503.
func Write(w http.ResponseWriter, st Status) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !st.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = w.Write([]byte(strings.Join(st.Messages, "\n") + "\n"))
}
