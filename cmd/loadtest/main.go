package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
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
		jsonPath    = flag.String("json", "", "write structured result to this file")
	)
	flag.Parse()
	if *concurrency <= 0 || *total <= 0 || *rate < 0 {
		log.Fatal("c and n must be positive; rate must be nonnegative")
	}

	body, _ := json.Marshal(map[string]interface{}{
		"model": "default",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stream": true,
	})

	collector := loadtest.NewCollector(*total)
	client := &http.Client{Timeout: 0, Transport: &http.Transport{MaxIdleConns: *concurrency + 16, MaxIdleConnsPerHost: *concurrency + 16}}
	defer client.CloseIdleConnections()
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
			w := loadtest.NewWorkerWithClient(client, *target, *key, body)
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
	if *jsonPath != "" {
		output := struct {
			Target         string          `json:"target"`
			Concurrency    int             `json:"concurrency"`
			Requests       int             `json:"requests"`
			Rate           int             `json:"rate_limit_qps"`
			ElapsedSeconds float64         `json:"elapsed_seconds"`
			Throughput     float64         `json:"success_per_second"`
			Report         loadtest.Report `json:"report"`
		}{*target, *concurrency, *total, *rate, elapsed.Seconds(), float64(rep.Success) / elapsed.Seconds(), rep}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*jsonPath, append(data, '\n'), 0644); err != nil {
			log.Fatal(err)
		}
	}
	log.Printf("done in %s total=%d success=%d errors=%d status=%v", elapsed, rep.Total, rep.Success, rep.Errors, rep.Status)
	if rep.Success > 0 {
		log.Printf("TTFT  P50=%s P95=%s P99=%s min=%s max=%s avg=%s", rep.TTFT.P50, rep.TTFT.P95, rep.TTFT.P99, rep.TTFT.Min, rep.TTFT.Max, rep.TTFT.Avg)
		log.Printf("Total P50=%s P95=%s P99=%s avgChunks=%.1f", rep.TotalDur.P50, rep.TotalDur.P95, rep.TotalDur.P99, rep.AvgChunks)
		log.Printf("throughput=%.1f req/s", float64(rep.Success)/elapsed.Seconds())
	}
	if rep.Success != rep.Total {
		fmt.Fprintf(os.Stderr, "loadtest: %d/%d requests succeeded\n", rep.Success, rep.Total)
		os.Exit(1)
	}
}
