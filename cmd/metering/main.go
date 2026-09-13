package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"llm-gateway/internal/metering"
	"llm-gateway/internal/observability"

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
	mux.HandleFunc("/ledger/members", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		reader, ok := ledger.(metering.MemberBillingReader)
		if !ok {
			http.Error(w, "member billing unavailable", http.StatusServiceUnavailable)
			return
		}
		tenant := r.URL.Query().Get("tenant_id")
		if tenant == "" {
			http.Error(w, "tenant_id required", http.StatusBadRequest)
			return
		}
		if rawUser := r.URL.Query().Get("user_id"); rawUser != "" {
			userID, err := strconv.ParseInt(rawUser, 10, 64)
			if err != nil || userID <= 0 {
				http.Error(w, "valid user_id required", http.StatusBadRequest)
				return
			}
			cost, err := reader.MemberBalance(tenant, userID)
			if err != nil {
				http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tenant_id": tenant, "user_id": rawUser, "cost_cents": cost})
			return
		}
		bills, err := reader.MemberBills(tenant)
		if err != nil {
			http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
			return
		}
		tenantTotal, err := ledger.Balance(tenant)
		if err != nil {
			http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
			return
		}
		var attributed int64
		for _, bill := range bills {
			attributed += bill.CostCents
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tenant_id": tenant, "cost_cents": tenantTotal, "unattributed_cost_cents": tenantTotal - attributed, "members": bills})
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

	adminToken := os.Getenv("METERING_ADMIN_TOKEN")
	if meteringProductionMode() && adminToken == "" {
		log.Fatal("METERING_ADMIN_TOKEN required in production mode")
	}
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && (adminToken != "" || r.URL.Path == "/ledger/members") {
			provided := r.Header.Get("Authorization")
			if adminToken == "" || subtle.ConstantTimeCompare([]byte(provided), []byte("Bearer "+adminToken)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: ":8091", Handler: observability.Middleware(authed)}
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

func meteringProductionMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("METERING_REQUIRE_POSTGRES"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
