package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/ready"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

const defaultMeteringAddr = ":8091"

func main() {
	profile, err := appenv.FromEnv()
	if err != nil {
		log.Fatalf("runtime profile: %v", err)
	}
	appenv.Log(profile)

	checks := meteringChecks(appenv.EnvLookup)
	if err := appenv.Validate(profile, checks); err != nil {
		log.Fatalf("metering production preflight failed:\n%v", err)
	}
	if !profile.IsProduction() {
		warnFailedChecks(checks)
	}

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

	// 对账/查询接口，演示 W3 验收；生产由 METERING_ADMIN_TOKEN 保护。
	adminToken := strings.TrimSpace(os.Getenv("METERING_ADMIN_TOKEN"))
	if adminToken == "" {
		log.Printf("WARNING: METERING_ADMIN_TOKEN is not set; /ledger/count, /ledger/balance and /reconcile are unauthenticated (demo only)")
	} else {
		log.Printf("metering admin token configured: /ledger/count, /ledger/balance and /reconcile require Authorization: Bearer")
	}
	mux := http.NewServeMux()
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
	mux.Handle("/ledger/count", requireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count, err := ledger.Count()
		if err != nil {
			http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"count": count})
	}), adminToken))
	mux.Handle("/ledger/balance", requireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}), adminToken))
	// 事件级对账：区分重复、pending、DLQ，以及 Stream/Ledger 两侧的具体差异。
	mux.Handle("/reconcile", requireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}), adminToken))

	// 生产把 pprof/metrics 与业务 listener 分离；demo/test 未设置诊断地址时
	// 保持原行为（仅 /metrics 挂在业务端口上，不额外暴露 pprof）。
	businessAddr := appenv.StringEnv("METERING_ADDR", defaultMeteringAddr)
	diagnosticsAddr := strings.TrimSpace(os.Getenv("METERING_DIAGNOSTICS_ADDR"))
	var diagnosticsSrv *http.Server
	switch {
	case diagnosticsAddr == "":
		mux.Handle("/metrics", promhttp.Handler())
	case diagnosticsAddr == businessAddr:
		log.Fatalf("METERING_DIAGNOSTICS_ADDR=%q must differ from METERING_ADDR=%q", diagnosticsAddr, businessAddr)
	default:
		diagnosticsMux := http.NewServeMux()
		diagnosticsMux.Handle("/metrics", promhttp.Handler())
		diagnosticsMux.HandleFunc("/debug/pprof/", pprof.Index)
		diagnosticsMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		diagnosticsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		diagnosticsMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		diagnosticsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		diagnosticsSrv = &http.Server{
			Addr:         diagnosticsAddr,
			Handler:      diagnosticsMux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 0, // pprof profile/trace 请求本身可能持续数十秒
			IdleTimeout:  60 * time.Second,
		}
		go func() {
			log.Printf("metering diagnostics listening on %s (/metrics, /debug/pprof/*)", diagnosticsAddr)
			if err := diagnosticsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("metering diagnostics: %v", err)
			}
		}()
	}

	srv := &http.Server{Addr: businessAddr, Handler: observability.Middleware(mux)}
	go func() {
		log.Printf("metering listening on %s", businessAddr)
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
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if diagnosticsSrv != nil {
		_ = diagnosticsSrv.Shutdown(shutdownCtx)
	}
	_ = srv.Shutdown(shutdownCtx)
}

// requireBearer guards the ledger/reconcile query endpoints. An empty token is
// the demo mode: the caller already logged a warning and the endpoints stay open.
// Comparison is constant time over SHA-256 digests, so neither the token nor its
// length leaks through response timing.
func requireBearer(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearerToken(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metering"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// meteringChecks is the pure production preflight list for the metering
// service. It never reads the process environment itself and has no side effects.
func meteringChecks(lookup appenv.Lookup) []appenv.Check {
	businessAddr := strings.TrimSpace(lookup("METERING_ADDR"))
	if businessAddr == "" {
		businessAddr = defaultMeteringAddr
	}
	diagnosticsAddr := strings.TrimSpace(lookup("METERING_DIAGNOSTICS_ADDR"))

	return []appenv.Check{
		{
			Name:   "METERING_REQUIRE_POSTGRES",
			Detail: "must be true: production must not silently fall back to the in-memory ledger",
			OK:     truthy(lookup("METERING_REQUIRE_POSTGRES")),
		},
		{
			Name:   "POSTGRES_DSN",
			Detail: "must be set: METERING_REQUIRE_POSTGRES cannot be satisfied without a database URL",
			OK:     strings.TrimSpace(lookup("POSTGRES_DSN")) != "",
		},
		{
			Name:   "METERING_ADMIN_TOKEN",
			Detail: "must be at least 16 characters so /ledger/count, /ledger/balance and /reconcile are not anonymous",
			OK:     len(strings.TrimSpace(lookup("METERING_ADMIN_TOKEN"))) >= 16,
		},
		{
			Name:   "METERING_DIAGNOSTICS_ADDR",
			Detail: fmt.Sprintf("must be set to a listener address different from the business address %q so metrics/pprof are not exposed on the query listener", businessAddr),
			OK:     diagnosticsAddr != "" && diagnosticsAddr != businessAddr,
		},
	}
}

// warnFailedChecks is the demo/test path: every production requirement that is
// not satisfied is logged, but the service still starts.
func warnFailedChecks(checks []appenv.Check) {
	for _, check := range checks {
		if !check.OK {
			log.Printf("WARNING: production check failed: %s: %s", check.Name, check.Detail)
		}
	}
}

// truthy mirrors the existing flagEnv()/requirePostgres() parsing for injected lookups.
func truthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
