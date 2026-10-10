package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-gateway/internal/upstreammock"
)

func main() {
	var (
		mode          = flag.String("mode", "synthetic", "synthetic|fixed|echo|info; text modes count Unicode characters as tokens")
		text          = flag.String("text", "这是一个模拟 Provider 的固定回复。", "fixed response text")
		textFile      = flag.String("text-file", "", "UTF-8 fixed response file, up to 4 MiB")
		repeat        = flag.Int("repeat", 1, "repeat text/echo, bounded by -tokens")
		tps           = flag.Float64("tps", 0, "simulated tokens/sec per stream; 0 uses token-interval")
		chunkTokens   = flag.Int("chunk-tokens", 1, "Unicode characters per content event in text modes")
		faultEvery    = flag.Int("fault-every", 0, "every Nth valid request returns fault-status; 0 disabled")
		faultStatus   = flag.Int("fault-status", 503, "HTTP status for periodic failure")
		stallAfter    = flag.Int("stall-after", 0, "pause after N content events")
		stallDuration = flag.Duration("stall-duration", 0, "pause duration")
		listen        = flag.String("listen", ":18080", "HTTP listen address")
		name          = flag.String("name", "upstream-mock", "content prefix so failover targets are distinguishable")
		ttft          = flag.Duration("ttft", 20*time.Millisecond, "delay before the first SSE event")
		tokenInterval = flag.Duration("token-interval", 15*time.Millisecond, "delay between content events")
		tokens        = flag.Int("tokens", 8, "output cap: default 8 synthetic events, or 1024 text-mode Unicode characters")
		promptTokens  = flag.Int("prompt-tokens", 12, "prompt_tokens reported in stream usage")
		status        = flag.Int("status", 200, "non-2xx returns a JSON error and no stream")
		hang          = flag.Bool("hang", false, "write SSE headers then block until the client cancels")
		failAfter     = flag.Int("fail-after", 0, "close after N content events without [DONE]; 0 completes")
		maxInflight   = flag.Int("max-inflight", 0, "concurrent stream limit; extra requests get 429; 0 unlimited")
		echo          = flag.Bool("echo", false, "stream body sha256 plus messages; default keeps the synthetic name loop")
		eventLine     = flag.Int("event-line-bytes", 0, "pad each content event so its data: line reaches N bytes (>=1MiB is deliberately over the shared 1MiB line cap); 0 = historical small events")
		roleOnlyFirst = flag.Bool("role-only-first", false, "emit a role-only first event before content events")
		authorization = flag.String("authorization", os.Getenv("UPSTREAM_MOCK_AUTHORIZATION"), "required bearer token; empty disables the check")
		usageMode     = flag.String("usage", "normal", "F04 usage shape in the terminal event: normal, missing, negative or mismatch")
	)
	flag.Parse()
	tokensExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "tokens" {
			tokensExplicit = true
		}
	})
	if !tokensExplicit && *mode != "synthetic" {
		*tokens = 1024
	}
	if *tokens < 1 || *repeat < 1 || *chunkTokens < 1 {
		log.Fatal("tokens, repeat and chunk-tokens must be positive")
	}

	if *textFile != "" {
		file, err := os.Open(*textFile)
		if err != nil {
			log.Fatal(err)
		}
		data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
		file.Close()
		if err != nil {
			log.Fatal(err)
		}
		if len(data) > 4<<20 {
			log.Fatal("text file exceeds 4 MiB")
		}
		*text = string(data)
	}
	cfg := upstreammock.Config{
		Mode: *mode, Text: *text, Repeat: *repeat, TPS: *tps, TokensPerChunk: *chunkTokens,
		FaultEvery: *faultEvery, FaultStatus: *faultStatus, StallAfter: *stallAfter, StallDuration: *stallDuration,
		Name:                  *name,
		TTFT:                  *ttft,
		TokenInterval:         *tokenInterval,
		CompletionTokens:      *tokens,
		PromptTokens:          *promptTokens,
		Status:                *status,
		Hang:                  *hang,
		FailAfter:             *failAfter,
		MaxInflight:           *maxInflight,
		Echo:                  *echo,
		EventLineBytes:        *eventLine,
		RoleOnlyFirst:         *roleOnlyFirst,
		ExpectedAuthorization: *authorization,
		UsageMode:             *usageMode,
	}
	if err := upstreammock.ValidateConfig(cfg); err != nil {
		log.Fatal(err)
	}
	handler := upstreammock.NewHandler(cfg)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("upstream-mock %s listening on %s ttft=%s token-interval=%s tokens=%d status=%d hang=%v fail-after=%d max-inflight=%d echo=%v event-line-bytes=%d role-only-first=%v authorization=%s",
			*name, *listen, *ttft, *tokenInterval, *tokens, *status, *hang, *failAfter, *maxInflight, *echo,
			*eventLine, *roleOnlyFirst, authorizationState(*authorization))
		log.Printf("upstream-mock mode=%s target-tps-per-stream=%g chunk-tokens=%d usage=%s; statistics: /stats, /metrics", *mode, *tps, *chunkTokens, *usageMode)
		log.Printf("upstream-mock per-request overrides (mock-only, forwarded verbatim by the Gateway): x_mock_ttft_ms, x_mock_token_interval_ms, x_mock_tokens, x_mock_event_line_bytes, x_mock_role_only_first, x_mock_usage, x_mock_tps, x_mock_status, x_mock_hang, x_mock_fail_after, x_mock_stall_after, x_mock_stall_ms")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
	log.Println("upstream-mock exit")
}

func authorizationState(token string) string {
	if token == "" {
		return "off"
	}
	return "required"
}
