package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"llm-gateway/internal/metering"

	"github.com/redis/go-redis/v9"
)

func main() {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})

	ledger := metering.NewLedger()
	consumer := metering.NewConsumer(rdb, ledger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go consumer.Run(ctx)

	// 对账/查询接口，演示 W3 验收
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ledger/count", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"count": ledger.Count(),
		})
	})
	mux.HandleFunc("/ledger/balance", func(w http.ResponseWriter, r *http.Request) {
		tenant := r.URL.Query().Get("tenant")
		if tenant == "" {
			tenant = "tenant-demo"
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"tenant":     tenant,
			"balance_cents": ledger.Balance(tenant),
		})
	})
	// 简单对账：比 Stream 长度和台账数
	mux.HandleFunc("/reconcile", func(w http.ResponseWriter, r *http.Request) {
		n, _ := rdb.XLen(ctx, metering.StreamName).Result()
		err := ledger.Reconcile(int(n))
		resp := map[string]interface{}{"stream_len": n, "ledger_count": ledger.Count()}
		if err != nil {
			resp["ok"] = false
			resp["error"] = err.Error()
			w.WriteHeader(http.StatusConflict)
		} else {
			resp["ok"] = true
		}
		json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{Addr: ":8091", Handler: mux}
	go func() {
		log.Printf("metering listening on :8091 ledger in-memory")
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
