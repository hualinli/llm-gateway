package provider

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveOpenAI is opt-in because it makes a real provider request and may
// consume quota. Set OPENAI_BASE_URL, OPENAI_API_KEY and OPENAI_MODEL before
// running it; it is skipped during normal CI.
func TestLiveOpenAI(t *testing.T) {
	baseURL := os.Getenv("OPENAI_BASE_URL")
	apiKey := os.Getenv("OPENAI_API_KEY")
	model := os.Getenv("OPENAI_MODEL")
	if baseURL == "" || apiKey == "" || model == "" {
		t.Skip("set OPENAI_BASE_URL, OPENAI_API_KEY and OPENAI_MODEL to run live")
	}

	cfg := DefaultOpenAIConfig("openai-live", baseURL, apiKey)
	cfg.Model = model
	cfg.FirstByteTimeout = 20 * time.Second
	cfg.IdleTimeout = 30 * time.Second
	cfg.AttemptTimeout = 2 * time.Minute
	p, err := NewOpenAIProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	usage, err := p.ChatStream(context.Background(), []byte(`{
		"model":"client-model",
		"messages":[{"role":"user","content":"Reply with exactly: gateway-smoke-ok"}],
		"stream":true
	}`), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "data: [DONE]") {
		t.Fatalf("live response did not contain [DONE]: %q", out.String())
	}
	t.Logf("provider=%s bytes=%d usage=%+v", p.Name(), out.Len(), usage)
}
