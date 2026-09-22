package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-gateway/internal/metering"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/ready"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

func main() {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})

	ledger, err := metering.NewLedger()
	if err != nil {
		log.Fatalf("create ledger: %v", err)
	}
	consumerOpts := metering.ConsumerOptions{}
	if name := os.Getenv("METERING_CONSUMER_NAME"); name != "" {
		consumerOpts.Name = name
	}
	consumer := metering.NewConsumerWithOptions(rdb, ledger, consumerOpts)
	log.Printf("metering consumer name=%s", consumer.Name())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumer.Run(ctx)

	// 对账/查询接口，演示 W3 验收
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", ready.Live)
	mux.HandleFunc("/livez", ready.Live)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()
		var redisErr error
		if err := rdb.Ping(ctx).Err(); err != nil {
			redisErr = fmt.Errorf("redis: %w", err)
		}
		var ledgerErr error
		if _, err := ledger.Count(); err != nil {
			ledgerErr = fmt.Errorf("ledger: %w", err)
		}
		ready.Write(w, ready.Dependencies(redisErr, ledgerErr))
	})
	mux.HandleFunc("/ledger/count", func(w http.ResponseWriter, r *http.Request) {
		count, err := ledger.Count()
		if err != nil {
			http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": count})
	})
	mux.HandleFunc("/ledger/balance", func(w http.ResponseWriter, r *http.Request) {
		tenant := r.URL.Query().Get("tenant")
		if tenant == "" {
			tenant = "tenant-demo"
		}
		balance, err := ledger.Balance(tenant)
		if err != nil {
			http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tenant":        tenant,
			"balance_cents": balance,
		})
	})
	// 事件级对账：区分重复、pending、DLQ，以及 Stream/Ledger 两侧的具体差异。
	mux.HandleFunc("/reconcile", func(w http.ResponseWriter, r *http.Request) {
		report, err := metering.ReconcileStream(r.Context(), rdb, ledger)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !report.Consistent() {
			w.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     report.Consistent(),
			"report": report,
		})
	})

	srv := &http.Server{Addr: ":8091", Handler: observability.Middleware(mux)}
	go func() {
		log.Printf("metering listening on :8091")
		log.Printf("metering consumer group=%s stream=%s", metering.GroupName, metering.StreamName)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("metering http: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down metering...")
	cancel()
	_ = srv.Shutdown(context.Background())
}
