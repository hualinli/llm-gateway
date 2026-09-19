package main

import (
	"flag"
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
		listen        = flag.String("listen", ":18080", "HTTP listen address")
		name          = flag.String("name", "upstream-mock", "content prefix so failover targets are distinguishable")
		ttft          = flag.Duration("ttft", 20*time.Millisecond, "delay before the first SSE event")
		tokenInterval = flag.Duration("token-interval", 15*time.Millisecond, "delay between content events")
		tokens        = flag.Int("tokens", 8, "completion tokens / content events")
		promptTokens  = flag.Int("prompt-tokens", 12, "prompt_tokens reported in stream usage")
		status        = flag.Int("status", 200, "non-2xx returns a JSON error and no stream")
		hang          = flag.Bool("hang", false, "write SSE headers then block until the client cancels")
		failAfter     = flag.Int("fail-after", 0, "close after N content events without [DONE]; 0 completes")
		maxInflight   = flag.Int("max-inflight", 0, "concurrent stream limit; extra requests get 429; 0 unlimited")
		echo          = flag.Bool("echo", false, "stream body sha256 plus messages; default keeps the synthetic name loop")
	)
	flag.Parse()

	handler := upstreammock.NewHandler(upstreammock.Config{
		Name:             *name,
		TTFT:             *ttft,
		TokenInterval:    *tokenInterval,
		CompletionTokens: *tokens,
		PromptTokens:     *promptTokens,
		Status:           *status,
		Hang:             *hang,
		FailAfter:        *failAfter,
		MaxInflight:      *maxInflight,
		Echo:             *echo,
	})
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("upstream-mock %s listening on %s ttft=%s token-interval=%s tokens=%d status=%d hang=%v fail-after=%d max-inflight=%d echo=%v",
			*name, *listen, *ttft, *tokenInterval, *tokens, *status, *hang, *failAfter, *maxInflight, *echo)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("upstream-mock exit")
}
