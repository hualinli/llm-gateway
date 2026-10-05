package console

import (
	"context"
	"net/http"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
)

// readyzProbeTimeout bounds the dependency check so a slow database cannot hold
// a probe open past the orchestrator's own timeout.
const readyzProbeTimeout = 2 * time.Second

// livez implements GET /livez: the process is answering. It never touches a
// dependency, so a database outage cannot make an otherwise healthy process
// look dead.
func (s *server) livez(_ context.Context, c *app.RequestContext) {
	writeText(c, http.StatusOK, "ok\n")
}

// readyz implements GET /readyz: the process can serve the request path, which
// for the Console means PostgreSQL is reachable. It is mounted on the business
// listener, never on the diagnostics listener.
func (s *server) readyz(ctx context.Context, c *app.RequestContext) {
	if s.ready == nil {
		writeText(c, http.StatusOK, "ok\n")
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, readyzProbeTimeout)
	defer cancel()
	if err := s.ready(probeCtx); err != nil {
		writeText(c, http.StatusServiceUnavailable, "postgres: "+err.Error()+"\n")
		return
	}
	writeText(c, http.StatusOK, "ok\n")
}

func writeText(c *app.RequestContext, status int, body string) {
	c.Response.Header.SetContentType("text/plain; charset=utf-8")
	c.SetStatusCode(status)
	c.Response.SetBodyString(body)
}
