package upstreammock

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// ValidateConfig also bounds process flags, not only request overrides.
func ValidateConfig(c Config) error {
	switch c.Mode {
	case "", "synthetic", "fixed", "echo", "info":
	default:
		return fmt.Errorf("mode must be synthetic, fixed, echo or info")
	}
	if !utf8.ValidString(c.Text) {
		return fmt.Errorf("fixed text must be UTF-8")
	}
	if c.Mode != "" && c.Mode != "synthetic" && c.EventLineBytes != 0 {
		return fmt.Errorf("event padding is only supported in synthetic mode")
	}
	if c.Echo && c.Mode != "" && c.Mode != "synthetic" {
		return fmt.Errorf("legacy echo cannot be combined with a text mode")
	}
	if math.IsNaN(c.TPS) || math.IsInf(c.TPS, 0) || c.TPS < 0 || (c.TPS > 0 && c.TPS < 0.001) || c.TPS > 1_000_000 {
		return fmt.Errorf("tps must be 0 or between 0.001 and 1000000")
	}
	if c.CompletionTokens < 0 || c.CompletionTokens > maxMockTokens || c.PromptTokens < 0 || c.PromptTokens > maxMockTokens {
		return fmt.Errorf("tokens out of range")
	}
	if c.Repeat < 0 || c.Repeat > maxMockTokens || c.TokensPerChunk < 0 || c.TokensPerChunk > 4096 || len(c.Text) > maxBodyBytes {
		return fmt.Errorf("text/repeat/chunk size out of range")
	}
	if c.Status < 200 || c.Status > 599 || (c.Status >= 200 && c.Status != 200 && c.Status < 300) {
		return fmt.Errorf("status must be 200 or 300..599")
	}
	if c.FaultStatus != 0 && (c.FaultStatus < 400 || c.FaultStatus > 599) {
		return fmt.Errorf("fault status must be 400..599")
	}
	if c.FaultEvery < 0 || c.MaxInflight < 0 || c.MaxInflight > 100000 || c.FailAfter < 0 || c.FailAfter > maxMockTokens || c.StallAfter < 0 || c.StallAfter > maxMockTokens {
		return fmt.Errorf("fault or concurrency setting out of range")
	}
	if c.TTFT < 0 || c.TTFT > 10*time.Minute || c.TokenInterval < 0 || c.TokenInterval > 10*time.Minute || c.StallDuration < 0 || c.StallDuration > 10*time.Minute {
		return fmt.Errorf("delay must be between 0 and 10 minutes")
	}
	if c.EventLineBytes < 0 || c.EventLineBytes > maxMockEventLineBytes || int64(c.EventLineBytes)*int64(c.CompletionTokens) > maxMockStreamBytes {
		return fmt.Errorf("requested stream size exceeds budget")
	}
	if _, ok := parseUsageMode(c.UsageMode); !ok {
		return fmt.Errorf("invalid usage mode")
	}
	return nil
}

func textSource(req chatRequest, cfg Config) string {
	text := cfg.Text
	switch cfg.Mode {
	case "echo":
		var b strings.Builder
		for _, msg := range req.Messages {
			b.WriteString(messageText(msg.Content))
		}
		text = b.String()
	case "info":
		text = fmt.Sprintf("Mock Provider: %s\nMode: info\nTarget TPS per stream: %g\nTTFT: %s\nSimulated tokenizer: one Unicode character = one token.\n", cfg.Name, cfg.TPS, cfg.TTFT)
	}
	return text
}

func textChunks(req chatRequest, cfg Config) []string {
	text := textSource(req, cfg)
	// Only materialize the bounded output, even for large input/repeat counts.
	runes := make([]rune, 0, cfg.CompletionTokens)
	for repeat := 0; repeat < cfg.Repeat && len(runes) < cfg.CompletionTokens && text != ""; repeat++ {
		for _, r := range text {
			if len(runes) == cfg.CompletionTokens {
				break
			}
			runes = append(runes, r)
		}
	}
	chunks := make([]string, 0, (len(runes)+cfg.TokensPerChunk-1)/cfg.TokensPerChunk)
	for i := 0; i < len(runes); i += cfg.TokensPerChunk {
		end := min(i+cfg.TokensPerChunk, len(runes))
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}
