package upstreammock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/provider"
)

// TestUsageModesShapeTheTerminalEvent pins the F04 injection surface: the three
// usage shapes the acceptance spec requires (missing, negative, and a total that
// contradicts its components) plus the normal one, both as a process flag and as
// the per-request x_mock_usage override the failure runner uses.
func TestUsageModesShapeTheTerminalEvent(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		wantUsage     bool
		wantPrompt    int
		wantTotal     int
		body          string
		wantBadStatus bool
	}{
		{mode: "", wantUsage: true, wantPrompt: 5, wantTotal: 8},
		{mode: UsageModeNormal, wantUsage: true, wantPrompt: 5, wantTotal: 8},
		{mode: UsageModeMissing, wantUsage: false},
		{mode: UsageModeNegative, wantUsage: true, wantPrompt: -1, wantTotal: 2},
		{mode: UsageModeMismatch, wantUsage: true, wantPrompt: 5, wantTotal: 15},
		{mode: "nonsense", wantUsage: true, wantPrompt: 5, wantTotal: 8}, // unknown falls back to normal
	} {
		t.Run("flag/"+tc.mode, func(t *testing.T) {
			cfg := Config{Name: "p", PromptTokens: 5, CompletionTokens: 3, UsageMode: tc.mode}
			assertUsageShape(t, cfg, "", tc.wantUsage, tc.wantPrompt, tc.wantTotal)
		})
	}

	// The per-request override must reach the terminal event even though it is
	// not part of the OpenAI protocol: the Gateway forwards unknown request
	// fields verbatim, which is what lets one mock process serve every shape.
	for _, tc := range []struct {
		override string
		want     string
	}{
		{override: UsageModeMissing, want: UsageModeMissing},
		{override: UsageModeNegative, want: UsageModeNegative},
		{override: UsageModeMismatch, want: UsageModeMismatch},
	} {
		t.Run("override/"+tc.override, func(t *testing.T) {
			cfg := Config{Name: "p", PromptTokens: 5, CompletionTokens: 3}
			req := `{"model":"m","stream":true,"x_mock_usage":"` + tc.override + `"}`
			resp := postStream(t, newServerURL(t, cfg), req)
			defer resp.Body.Close()
			events, _ := readSSE(t, resp.Body)
			last := events[len(events)-1]
			var payload struct {
				Usage *struct {
					PromptTokens int `json:"prompt_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(last, &payload); err != nil {
				t.Fatalf("terminal event=%s err=%v", last, err)
			}
			switch tc.want {
			case UsageModeMissing:
				if payload.Usage != nil {
					t.Fatalf("x_mock_usage=missing produced usage=%+v", payload.Usage)
				}
			case UsageModeNegative:
				if payload.Usage == nil || payload.Usage.PromptTokens != -1 {
					t.Fatalf("x_mock_usage=negative produced usage=%+v", payload.Usage)
				}
			case UsageModeMismatch:
				if payload.Usage == nil || payload.Usage.TotalTokens != 15 {
					t.Fatalf("x_mock_usage=mismatch produced usage=%+v", payload.Usage)
				}
			}
		})
	}

	// An unknown override is a client error, not a silent fallback: a scenario
	// that misspells the shape must not measure the wrong thing.
	srv := httptest.NewServer(NewHandler(Config{Name: "p"}))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"x_mock_usage":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown x_mock_usage status=%d, want 400", resp.StatusCode)
	}
}

func newServerURL(t *testing.T, cfg Config) string {
	t.Helper()
	srv := httptest.NewServer(NewHandler(cfg))
	t.Cleanup(srv.Close)
	return srv.URL
}

func assertUsageShape(t *testing.T, cfg Config, body string, wantUsage bool, wantPrompt, wantTotal int) {
	t.Helper()
	if body == "" {
		body = `{"model":"m","stream":true}`
	}
	resp := postStream(t, newServerURL(t, cfg), body)
	defer resp.Body.Close()
	events, done := readSSE(t, resp.Body)
	if !done {
		t.Fatal("stream missing [DONE]")
	}
	var payload struct {
		Usage *provider.Usage `json:"usage"`
	}
	if err := json.Unmarshal(events[len(events)-1], &payload); err != nil {
		t.Fatalf("terminal event=%s err=%v", events[len(events)-1], err)
	}
	if !wantUsage {
		if payload.Usage != nil {
			t.Fatalf("usage=%+v, want no usage object at all", payload.Usage)
		}
		return
	}
	if payload.Usage == nil {
		t.Fatalf("terminal event=%s carries no usage", events[len(events)-1])
	}
	if payload.Usage.PromptTokens != wantPrompt || payload.Usage.TotalTokens != wantTotal {
		t.Fatalf("usage=%+v, want prompt_tokens=%d total_tokens=%d", payload.Usage, wantPrompt, wantTotal)
	}
}
