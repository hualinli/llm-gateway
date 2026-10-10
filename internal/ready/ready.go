package ready

import (
	"net/http"
	"strings"
)

// Status is a process probe result. Live means the process can answer;
// Ready means it can serve the core request path.
type Status struct {
	Ready bool
	// Messages is the full detail for the operator log: raw driver errors,
	// internal DNS names, ports. It is deliberately never rendered into a
	// probe response — /readyz is unauthenticated and, for the gateway, is
	// mounted on the business listener behind nginx, so echoing "dial tcp
	// redis:6379: connect: connection refused" hands an anonymous caller a
	// map of the internal topology. Callers log this; the body gets Summary.
	Messages []string
	// Summary is the bounded, dependency-name-only body for the probe
	// endpoint. Never put err.Error() in here.
	Summary []string
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
		st.Summary = append(st.Summary, "no routes")
	}
	if redisErr != nil {
		msg := "redis: " + redisErr.Error()
		if failClosed {
			st.Ready = false
			st.Messages = append(st.Messages, msg)
			st.Summary = append(st.Summary, "dependency unavailable: redis")
		} else {
			st.Messages = append(st.Messages, "limiter degraded: "+msg)
			// Fail-open keeps the process in the load-balancer pool, so the
			// body has to stay 200 while still telling an operator that the
			// limiter is degraded; the reason lives in Messages.
			st.Summary = append(st.Summary, "degraded: redis")
		}
	}
	if st.Ready && len(st.Messages) == 0 {
		st.Messages = []string{"ok"}
	}
	return st
}

// Dependencies fails closed when any dependency error is non-nil. It takes the
// errors as opaque values and reports a single "dependency unavailable" line:
// with more than one dependency in flight (redis, ledger, settlement) there is
// no way to tell the caller which one broke without echoing whichever error
// happened to sort first, and naming a guess would be worse than admitting the
// aggregate. The per-dependency text stays in Messages for the log.
func Dependencies(errs ...error) Status {
	st := Status{Ready: true}
	for _, err := range errs {
		if err != nil {
			st.Ready = false
			st.Messages = append(st.Messages, err.Error())
		}
	}
	if !st.Ready {
		st.Summary = []string{"dependency unavailable"}
		return st
	}
	st.Messages = []string{"ok"}
	return st
}

// Write renders a probe body. Not-ready responses are HTTP 503; the status
// code is the contract every healthcheck in this repo (nginx, compose
// healthchecks, scripts/release-candidate.sh) depends on, so it must not move.
// Only Summary is rendered, so the body never carries dependency error text.
func Write(w http.ResponseWriter, st Status) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !st.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	body := strings.Join(st.Summary, "\n")
	if body == "" {
		// A caller may build a Status by hand and leave Summary unset; fall
		// back to the two words a probe actually needs.
		if st.Ready {
			body = "ok"
		} else {
			body = "not ready"
		}
	}
	_, _ = w.Write([]byte(body + "\n"))
}
