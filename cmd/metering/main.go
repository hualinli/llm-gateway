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
	"llm-gateway/internal/billing"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/ready"
	"llm-gateway/internal/testsupport/failpoint"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

const defaultMeteringAddr = ":8091"

// ledgerRequestTimeout bounds one /ledger/* read so a stalled database cannot
// hold the HTTP handler (and its goroutine) for as long as the database stalls.
const ledgerRequestTimeout = 5 * time.Second

func main() {
	profile, err := appenv.FromEnv()
	if err != nil {
		log.Fatalf("runtime profile: %v", err)
	}
	appenv.Log(profile)

	// T10 test-only fault injection. Attach is a no-op in a build without the
	// `failpoint` tag; in a `-tags failpoint` build it refuses to arm under the
	// production profile, and the preflight below refuses GATEWAY_FAILPOINTS in
	// every build.
	if err := failpoint.Attach(failpoint.Options{Lookup: appenv.EnvLookup, Profile: profile.String()}); err != nil {
		log.Fatalf("failpoints: %v", err)
	}
	if failpoint.Enabled() {
		log.Printf("WARNING: this process was built with -tags failpoint and has armed failpoints; it must not serve production traffic")
	}

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

	// T07C 对账：四层事实（execution / receipt / entry / account）在 PG 里分页比较，
	// 只有“差异行”会被取回内存；Redis 仅做传输检查，归档导致的 ledger-only 不是差异。
	// 对账需要一条到计费 schema 的直连：复用账本已经建好的连接池，不新开一个。
	reconcile := reconcileDeps{
		Ledger:   ledger,
		Redis:    rdb,
		Window:   reconcileEnvDuration(appenv.EnvLookup, "METERING_RECONCILE_WINDOW", defaultReconcileWindow),
		PageSize: reconcileEnvInt(appenv.EnvLookup, "METERING_RECONCILE_PAGE_SIZE", metering.DefaultReconcilePageSize),
	}
	if pg, ok := ledger.(*metering.PGLedger); ok {
		reconcile.DB = pg.DB()
		reconcile.Exceptions = billing.NewPGExceptionStore(pg.DB())
	} else {
		// 内存账本（demo 回退）没有计费事实层可读：报告会显式标记 degraded，
		// 而不是给一个看起来正常的绿色结果。
		log.Printf("WARNING: the ledger is in-memory; /reconcile will report the four durable layers as degraded")
	}
	if interval := reconcileEnvDuration(appenv.EnvLookup, "METERING_RECONCILE_INTERVAL", defaultReconcileInterval); interval > 0 {
		go runReconcileLoop(ctx, reconcile, interval)
	} else {
		log.Printf("metering reconciliation background loop disabled (METERING_RECONCILE_INTERVAL=0)")
	}

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
		if _, err := ledger.Count(ctx); err != nil {
			ledgerErr = fmt.Errorf("ledger: %w", err)
		}
		st := ready.Dependencies(redisErr, ledgerErr, settlementReadiness())
		// ready.Write 只渲染无敏感信息的摘要（依赖原文含内部地址/端口，
		// 而 /readyz 无需鉴权），所以详情必须在这里落日志，否则依赖故障
		// 在响应里被抹掉之后就再也查不到了。只在未就绪时记：健康探针
		// 每次被 nginx / docker healthcheck 打到，不该刷日志。
		if !st.Ready {
			for _, msg := range st.Messages {
				log.Printf("metering readyz: not ready: %s", msg)
			}
		}
		ready.Write(w, st)
	})
	mux.Handle("/ledger/count", requireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), ledgerRequestTimeout)
		defer cancel()
		count, err := ledger.Count(ctx)
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
		ctx, cancel := context.WithTimeout(r.Context(), ledgerRequestTimeout)
		defer cancel()
		balance, err := ledger.Balance(ctx, tenant)
		if err != nil {
			http.Error(w, "ledger unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tenant":        tenant,
			"balance_cents": balance,
		})
	}), adminToken))
	// 有界对账（T07C，报告 schema v2）：四层 PG 事实分页比较 + Redis 传输检查。
	// 保留 `ok` 与 `pending_messages`/`dlq_messages` 字段名以兼容 demo-accept.sh；
	// 旧的 stream_only/ledger_only/payload_mismatch 三个列表被层次化差异取代，
	// 其中 ledger_only 变成 report.transport.ledger_only_informational（永不是差异）。
	mux.Handle("/reconcile", requireBearer(newReconcileHandler(reconcile), adminToken))

	// T07A：receipt dispatcher（逻辑 outbox）与 settlement 消费者随进程启动。
	// 两者复用账本已经建好的计费 schema 连接池：dispatcher 的 claim/retry 都是
	// 短事务，投递本身在事务外做，Redis 只是传输，PG receipt 才是事实。
	settlementCfg, err := settlementConfigFromEnv(appenv.EnvLookup)
	if err != nil {
		log.Fatalf("settlement config: %v", err)
	}
	settlement, err := startSettlement(ctx, rdb, ledger, settlementCfg)
	if err != nil {
		log.Fatalf("settlement wiring: %v", err)
	}
	if settlement != nil {
		settlement.register(mux, adminToken)
	}

	// T07B：有界执行恢复（僵尸 reserved/executing 的审计释放）+ 24h 人工队列。
	// 恢复判断的是**别的进程**是否还活着，所以它跑在 metering（billing worker）
	// 而不是数据面：Gateway 只负责写心跳，metering 只负责读事实。
	recoveryCfg, err := recoveryConfigFromEnv(appenv.EnvLookup)
	if err != nil {
		log.Fatalf("recovery config: %v", err)
	}
	recovery, err := startRecovery(ctx, ledger, recoveryCfg)
	if err != nil {
		log.Fatalf("recovery wiring: %v", err)
	}
	if recovery != nil {
		recovery.register(mux, adminToken)
	} else {
		registerUnavailableRecoveryPolicy(mux, recoveryCfg.PolicyToken, adminToken)
	}

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
			Name:   failpoint.EnvVar,
			Detail: "must be empty: the T10 failpoints stop or kill this process at a settlement/ACK window and are only available to the isolated failure runner in demo/test (see internal/testsupport/failpoint)",
			OK:     strings.TrimSpace(lookup(failpoint.EnvVar)) == "",
		},
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
			Detail: "must be at least 16 characters and not a shipped demo placeholder so /ledger/count, /ledger/balance and /reconcile are not anonymous",
			OK:     len(strings.TrimSpace(lookup("METERING_ADMIN_TOKEN"))) >= 16 && !appenv.IsDemoSecret(lookup("METERING_ADMIN_TOKEN")),
		},
		{
			Name:   "METERING_POLICY_TOKEN",
			Detail: "when set, must be at least 16 characters, not a shipped demo placeholder, and differ from METERING_ADMIN_TOKEN; it is only for the read-only /recovery/policy endpoint",
			OK: func() bool {
				policyToken := strings.TrimSpace(lookup("METERING_POLICY_TOKEN"))
				adminToken := strings.TrimSpace(lookup("METERING_ADMIN_TOKEN"))
				return policyToken == "" || (len(policyToken) >= 16 && policyToken != adminToken && !appenv.IsDemoSecret(policyToken))
			}(),
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
