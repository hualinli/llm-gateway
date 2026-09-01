package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"sync"
	"time"

	"llm-gateway/internal/loadtest"
)

func main() {
	var (
		target      = flag.String("target", "http://localhost:8090/v1/chat/completions", "gateway target URL")
		concurrency = flag.Int("c", 100, "concurrent streams")
		total       = flag.Int("n", 1000, "total requests")
		rate        = flag.Int("rate", 0, "QPS limit, 0 = no limit")
		key         = flag.String("key", os.Getenv("GATEWAY_API_KEY"), "API key (prefer env GATEWAY_API_KEY)")
	)
	flag.Parse()
	if *key == "" {
		log.Fatal("API key is required; pass -key or set GATEWAY_API_KEY")
	}

	body, _ := json.Marshal(map[string]interface{}{
		"model": "default",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stream": true,
	})

	collector := loadtest.NewCollector(*total)
	var wg sync.WaitGroup
	sem := make(chan struct{}, *concurrency)

	var ticker <-chan time.Time
	if *rate > 0 {
		t := time.NewTicker(time.Second / time.Duration(*rate))
		defer t.Stop()
		ticker = t.C
	}

	start := time.Now()
	for i := 0; i < *total; i++ {
		if ticker != nil {
			<-ticker
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w := loadtest.NewWorker(*target, *key, body)
			// 每条流独立 10s 超时，避免慢连接拖住整个压测
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res := w.Do(ctx)
			collector.Add(res)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	rep := collector.Report()
	log.Printf("done in %s total=%d success=%d errors=%d status=%v", elapsed, rep.Total, rep.Success, rep.Errors, rep.Status)
	if rep.Success > 0 {
		log.Printf("TTFT  P50=%s P95=%s P99=%s min=%s max=%s avg=%s", rep.TTFT.P50, rep.TTFT.P95, rep.TTFT.P99, rep.TTFT.Min, rep.TTFT.Max, rep.TTFT.Avg)
		log.Printf("Total P50=%s P95=%s P99=%s avgChunks=%.1f", rep.TotalDur.P50, rep.TotalDur.P95, rep.TotalDur.P99, rep.AvgChunks)
		log.Printf("throughput=%.1f req/s", float64(rep.Success)/elapsed.Seconds())
	}
}
