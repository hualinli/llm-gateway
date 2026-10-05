package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/auth"
	"llm-gateway/internal/control"
	"llm-gateway/internal/gatewayguard"
	"llm-gateway/internal/limiter"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
	"llm-gateway/internal/ready"
	pb "llm-gateway/proto/control"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

const (
	defaultGatewayAddr = ":8090"
)

func main() {
	profile, err := appenv.FromEnv()
	if err != nil {
		log.Fatalf("runtime profile: %v", err)
	}
	appenv.Log(profile)

	// The control channel config is computed once: the same value drives the
	// production preflight check and the synchronizer, so a check can never
	// disagree with what the process actually uses.
	controlTLS := control.TLSConfigFromEnv()
	checks := gatewayChecks(appenv.EnvLookup, controlTLS != nil)
	if err := appenv.Validate(profile, checks); err != nil {
		log.Fatalf("gateway production preflight failed:\n%v", err)
	}
	if !profile.IsProduction() {
		warnFailedChecks(checks)
	}

	// Routes and provider instances come exclusively from the versioned
	// control-plane snapshot. The Gateway deliberately starts empty: it must
	// never invent a provider or silently fall back to an in-process mock.
	instanceID := gatewayInstanceID()
	credentials := &controlCredentialResolver{instanceID: instanceID}
	registry := provider.NewRegistryWithOptions(nil, provider.RegistryOptions{
		Credentials:    credentials,
		ResolveTimeout: durationEnv("GATEWAY_CREDENTIAL_RESOLVE_TIMEOUT", 2*time.Second),
	})

	// Redis 复用：限流 + Stream 共享同一实例
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	limiterRate := intEnv("GATEWAY_LIMITER_RATE", 20)
	limiterBurst := intEnv("GATEWAY_LIMITER_BURST", 40)
	lim := limiter.New(rdb, limiterRate, limiterBurst, 500*time.Millisecond)
	limiterPolicy := os.Getenv("LIMITER_ON_REDIS_ERROR")
	if limiterPolicy == "" {
		limiterPolicy = "open"
	}
	if err := lim.SetOnRedisError(limiterPolicy); err != nil {
		log.Fatalf("limiter: %v", err)
	}
	log.Printf("limiter: redis=%s rate=%d burst=%d queue=500ms on-redis-error=%s", redisAddr, limiterRate, limiterBurst, limiterPolicy)

	// 计量生产：批量 XADD，不阻塞热路径；Redis 故障时落本地 JSONL spool。
	spoolDir := os.Getenv("METERING_SPOOL_DIR")
	if spoolDir == "" {
		spoolDir = "./data/metering-spool"
	}
	meterProducer := metering.NewProducerWithOptions(rdb, 1000, 50, 200*time.Millisecond, metering.ProducerOptions{
		SpoolDir: spoolDir,
	})
	proxyHandler := proxy.NewHandler(registry, meterProducer)
	maxStreams := intEnv("GATEWAY_MAX_ACTIVE_STREAMS", 0)
	proxyHandler.SetMaxActiveStreams(maxStreams)
	log.Printf("gateway max active streams=%d (0 means unlimited)", maxStreams)

	// 本地 bootstrap key 仅通过环境变量注入，生产应由控制面下发；内存 Store 只保存 hash。
	var bootstrapRecords []auth.KeyRecord
	if rawKey := os.Getenv("LLM_GATEWAY_BOOTSTRAP_API_KEY"); rawKey != "" {
		tenantID := os.Getenv("LLM_GATEWAY_BOOTSTRAP_TENANT_ID")
		if tenantID == "" {
			tenantID = "tenant-demo"
		}
		record := auth.NewKeyRecord("bootstrap-"+auth.HashKey(rawKey), rawKey, tenantID)
		bootstrapRecords = []auth.KeyRecord{record}
	}
	authStore := auth.NewMemoryStoreFromRecords(bootstrapRecords)

	// 数据面 PG access guard（ADR-0003 §2.8）：在快照解析 Key 之后、进入上游
	// 之前，用一条短只读查询确认 org/user/member/key 的当前有效性。生产 profile
	// 必须 GATEWAY_ACCESS_GUARD=pg；demo/test 默认关闭并打印 WARNING，关闭时
	// 退化为只执行快照可见的检查，且完全不建立 PG 连接。
	guardConfig, err := accessGuardConfigFromEnv(appenv.EnvLookup)
	if err != nil {
		log.Fatalf("access guard: %v", err)
	}
	orgPolicies := gatewayguard.NewPolicySet()
	accessGuard, closeAccessGuard, err := openAccessGuard(guardConfig, orgPolicies)
	if err != nil {
		log.Fatalf("access guard: %v", err)
	}
	log.Printf("access guard mode=%s", guardConfig.Mode)

	// 计费准入与 durable receipt（T06C）：独立的、有界的 PG 池；准入位于 access
	// guard 之后、上游之前，finalizer 决定 [DONE] 能否释放。GATEWAY_BILLING=off
	// 时不建立连接并打印 WARNING（demo profile 只在没有 DSN 的语义下使用）。
	billingSettings, err := billingConfigFromEnv(appenv.EnvLookup)
	if err != nil {
		log.Fatalf("billing: %v", err)
	}
	billingAdmission, closeBilling, err := openBilling(billingSettings, instanceID, registry.HasModel)
	if err != nil {
		log.Fatalf("billing: %v", err)
	}
	if billingAdmission != nil {
		// 只有安装 finalizer 才会在完整 SSE 终态事件边界阻塞 [DONE]；没有它
		// 就是 T06A 之前的透传语义（demo 显式选择）。
		proxyHandler.SetFinalizer(billingAdmission)
		proxyHandler.SetFinalizeTimeout(billingSettings.FinalizeTimeout)
	}

	// 控制面同步：GetConfig 补拉、Watch 低延迟推送、周期校验和策略租约。
	// 版本跳跃由同步器接受，Gateway 只拒绝旧版本；租约到期时撤销本地 key。
	//
	// T09：租约默认 30s（CONTROL_CONFIG_LEASE）——失联超过租约的实例必须对新请求
	// fail-closed，同时让在途流自然排水。心跳（ReportGatewayStatus）默认每 10s 一次，
	// 即使配置没有变化也发，控制面才能区分「空闲但健康」和「失联」。
	controlAddr := os.Getenv("CONTROL_ADDR")
	if controlAddr == "" {
		controlAddr = "localhost:50051"
	}
	controlCtx, controlCancel := context.WithCancel(context.Background())
	startedAt := time.Now().UTC()
	log.Printf("gateway instance=%s started_at=%s", instanceID, startedAt.Format(time.RFC3339Nano))
	controlSync := control.NewSynchronizer(controlAddr, control.SyncOptions{
		PollInterval:   durationEnv("CONTROL_CONFIG_POLL_INTERVAL", 30*time.Second),
		LeaseTimeout:   durationEnv("CONTROL_CONFIG_LEASE", 30*time.Second),
		ReportInterval: durationEnv("CONTROL_STATUS_REPORT_INTERVAL", control.DefaultReportInterval),
		StartedAt:      startedAt,
		OnLeaseExpired: func() {
			authStore.UpdateRecords(nil)
			// 控制面失联：本地 key 全部撤销，同时清掉组织策略，避免用过期
			// 策略继续放行（allowlist/RPM 都来自同一个快照）。
			orgPolicies.Replace(nil)
		},
		TLS:        controlTLS,
		InstanceID: instanceID,
		OnClient: func(client *control.Client) {
			credentials.client.Store(client)
		},
	})
	go func() {
		err := controlSync.Run(controlCtx, func(cfg *pb.Config) error {
			log.Printf("control: hot reload to v%d", cfg.Version)
			if err := registry.ApplyConfig(control.RoutesFromProto(cfg.Routes), control.ProviderConfigsFromProto(cfg.Providers)); err != nil {
				// 路由配置失败时仍优先撤销已删除凭据；新增 key 要等完整配置修复后生效。
				authStore.RevokeMissingRecords(control.KeyRecordsToAuth(cfg.KeyRecords))
				log.Printf("control: reject config v%d: %v", cfg.Version, err)
				return err
			}
			// 策略先于 key 生效：新 key 的第一个请求就能看到它所属组织的策略。
			orgPolicies.Replace(orgPoliciesFromProto(cfg.OrgPolicies))
			authStore.UpdateRecords(control.KeyRecordsToAuth(cfg.KeyRecords))
			return nil
		})
		if err != nil && controlCtx.Err() == nil {
			log.Printf("control sync stopped: %v", err)
		}
	}()

	// 组织级 RPM 直接消费快照里的 org_policies；没有发布策略的组织保持原来的
	// 租户默认桶（单层令牌桶，不叠加）。
	lim.SetOrgRates(orgPolicies)

	// 每实例的活跃流上限：全局（proxy 内部，进入上游前 503）与组织级（这里，
	// 429）。两者都是**单实例**保护，不做分布式并发租约：两个实例时一个组织
	// 实际可达到的上限是 2x 配置值。跨实例共享的只有 Redis 里的 org RPM 令牌桶。
	maxOrgStreams := intEnv("GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG", 0)
	lim.SetMaxActiveStreamsPerOrg(maxOrgStreams)
	if maxOrgStreams > 0 {
		log.Printf("gateway per-org active streams=%d (per instance, not a cross-instance quota; global per-instance limit=%d)", maxOrgStreams, maxStreams)
	} else {
		log.Printf("WARNING: GATEWAY_MAX_ACTIVE_STREAMS_PER_ORG is 0 (unlimited): per-organization concurrency is not bounded on this instance")
	}

	// 链路顺序：auth(提租户) → guard(权威校验+归属) → limiter(按 org/租户限) →
	// billing(资金预占+fault-closed) → proxy(透传+计量)。计费必须在 guard 之后
	// （需要已验证的 org/user/key/execution 归属）且在任何上游之前。
	guarded := gatewayguard.Middleware(accessGuard, gatewayguard.Options{KnownModel: registry.HasModel})
	// StreamLimit 只包住 SSE 出口：/v1/models 这类非流请求不占组织并发预算。
	admittedProxy := limiter.Middleware(lim)(limiter.StreamLimit(lim)(proxyHandler))
	if billingAdmission != nil {
		admittedProxy = billingAdmission.Middleware(admittedProxy)
	}
	authedLimitedProxy := auth.Middleware(authStore)(guarded(admittedProxy))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", ready.Live)
	mux.HandleFunc("/livez", ready.Live)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()
		ready.Write(w, ready.Gateway(len(registry.Models()), rdb.Ping(ctx).Err(), lim.FailClosed()))
	})
	mux.Handle("/v1/chat/completions", authedLimitedProxy)
	mux.Handle("/v1/models", auth.Middleware(authStore)(guarded(limiter.Middleware(lim)(proxy.NewModelsHandler(registry)))))

	// 生产把 pprof/metrics 与业务 listener 分离；demo/test 未设置诊断地址时保持
	// 原行为（诊断端点挂在业务端口上）。
	businessAddr := appenv.StringEnv("GATEWAY_ADDR", defaultGatewayAddr)
	diagnosticsAddr := strings.TrimSpace(os.Getenv("GATEWAY_DIAGNOSTICS_ADDR"))
	var diagnosticsSrv *http.Server
	switch {
	case diagnosticsAddr == "":
		registerGatewayDiagnostics(mux)
	case diagnosticsAddr == businessAddr:
		log.Fatalf("GATEWAY_DIAGNOSTICS_ADDR=%q must differ from GATEWAY_ADDR=%q", diagnosticsAddr, businessAddr)
	default:
		diagnosticsMux := http.NewServeMux()
		registerGatewayDiagnostics(diagnosticsMux)
		diagnosticsSrv = &http.Server{
			Addr:         diagnosticsAddr,
			Handler:      diagnosticsMux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 0, // pprof profile/trace 请求本身可能持续数十秒
			IdleTimeout:  60 * time.Second,
		}
		go func() {
			log.Printf("gateway diagnostics listening on %s (/metrics, /debug/pprof/*)", diagnosticsAddr)
			if err := diagnosticsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("diagnostics listen: %v", err)
			}
		}()
	}

	requestsCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	drain := newRequestDrain()
	srv := &http.Server{
		Addr: businessAddr,
		// EntryMetrics 必须在最外层：它按 route/result_class 统计每一个到达进程的
		// 请求，包括 drain 503、observability 的 400、auth 401 和 limiter 429；
		// 放进 auth/limiter 链内部就看不到提前拒绝。它只包一层状态捕获，
		// 保留 Flush/Unwrap，SSE 仍然流式。
		Handler:      observability.EntryMetrics(drain.middleware(observability.Middleware(mux))),
		BaseContext:  func(net.Listener) context.Context { return requestsCtx },
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 0, // SSE 长连接不能设写超时，由 handler 的 context 控制
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("gateway listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// 优雅退出：在途的流有机会自然结束，而不是被粗暴切断（为后续热更新排水做准备）
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	controlCancel()
	shutdownErr := shutdownGateway(srv, drain, cancelRequests, meterProducer, shutdownOptions{
		GracePeriod:  10 * time.Second,
		CancelPeriod: 5 * time.Second,
		FlushPeriod:  5 * time.Second,
	})
	if diagnosticsSrv != nil {
		diagnosticsCtx, cancelDiagnostics := context.WithTimeout(context.Background(), 5*time.Second)
		if err := diagnosticsSrv.Shutdown(diagnosticsCtx); err != nil {
			log.Printf("diagnostics shutdown: %v", err)
		}
		cancelDiagnostics()
	}
	if shutdownErr != nil {
		log.Printf("shutdown completed with errors: %v", shutdownErr)
	}
	if err := closeAccessGuard(); err != nil {
		log.Printf("access guard shutdown: %v", err)
	}
	if err := closeBilling(); err != nil {
		log.Printf("billing shutdown: %v", err)
	}
	_ = rdb.Close()
	log.Println("exit")
}

// registerGatewayDiagnostics mounts pprof and Prometheus metrics on exactly one
// listener: the dedicated diagnostics server in production, or the business
// server in demo/test when GATEWAY_DIAGNOSTICS_ADDR is unset.
func registerGatewayDiagnostics(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/metrics", promhttp.Handler())
	_ = metrics.ActiveStreams // 触发 metrics 包 init 注册
}

// gatewayChecks is the pure production preflight list for the data plane. It
// never reads the process environment itself and has no side effects.
func gatewayChecks(lookup appenv.Lookup, controlTLSConfigured bool) []appenv.Check {
	businessAddr := strings.TrimSpace(lookup("GATEWAY_ADDR"))
	if businessAddr == "" {
		businessAddr = defaultGatewayAddr
	}
	diagnosticsAddr := strings.TrimSpace(lookup("GATEWAY_DIAGNOSTICS_ADDR"))
	clientCertFile := strings.TrimSpace(lookup("CONTROL_TLS_CLIENT_CERT_FILE"))
	clientKeyFile := strings.TrimSpace(lookup("CONTROL_TLS_CLIENT_KEY_FILE"))

	return []appenv.Check{
		{
			Name:   "LLM_GATEWAY_BOOTSTRAP_API_KEY",
			Detail: "must be empty: production keys are issued by the control plane, and a raw bootstrap key bypasses it",
			OK:     strings.TrimSpace(lookup("LLM_GATEWAY_BOOTSTRAP_API_KEY")) == "",
		},
		{
			Name:   "GATEWAY_ACCESS_GUARD",
			Detail: `must be "pg": production must verify the key's organization, owner/member and key row against PostgreSQL before every upstream call (demo/test may set "off")`,
			OK:     accessGuardModeOf(lookup) == accessGuardPG,
		},
		{
			Name:   "GATEWAY_BILLING",
			Detail: `must be "pg": production must reserve funds in PostgreSQL before every upstream call and commit a durable usage receipt before "[DONE]" (demo/test may set "off", which logs a WARNING)`,
			OK:     billingModeOf(lookup) == billingPG,
		},
		{
			Name:   "POSTGRES_DSN",
			Detail: `must be set when GATEWAY_ACCESS_GUARD=pg or GATEWAY_BILLING=pg: the access guard and the billing admission read authoritative PostgreSQL state`,
			OK: (accessGuardModeOf(lookup) != accessGuardPG && billingModeOf(lookup) != billingPG) ||
				strings.TrimSpace(lookup("POSTGRES_DSN")) != "",
		},
		{
			Name:   "LIMITER_ON_REDIS_ERROR",
			Detail: `must be "closed" so a Redis outage rejects traffic instead of silently failing open`,
			OK:     strings.EqualFold(strings.TrimSpace(lookup("LIMITER_ON_REDIS_ERROR")), "closed"),
		},
		{
			Name:   "GATEWAY_LIMITER_RATE",
			Detail: "must be explicitly set to a positive integer; production must not inherit the demo default",
			OK:     positiveInt(lookup, "GATEWAY_LIMITER_RATE"),
		},
		{
			Name:   "GATEWAY_LIMITER_BURST",
			Detail: "must be explicitly set to a positive integer; production must not inherit the demo default",
			OK:     positiveInt(lookup, "GATEWAY_LIMITER_BURST"),
		},
		{
			Name:   "GATEWAY_MAX_ACTIVE_STREAMS",
			Detail: "must be explicitly set to a positive integer; 0 means unlimited and is not allowed in production",
			OK:     positiveInt(lookup, "GATEWAY_MAX_ACTIVE_STREAMS"),
		},
		{
			Name:   "GATEWAY_DIAGNOSTICS_ADDR",
			Detail: fmt.Sprintf("must be set to a listener address different from the business address %q so pprof/metrics are not exposed on the API listener", businessAddr),
			OK:     diagnosticsAddr != "" && diagnosticsAddr != businessAddr,
		},
		{
			Name:   "CONTROL_TLS_CA_FILE",
			Detail: "must be set so the gateway verifies the control-plane server certificate over TLS",
			OK:     controlTLSConfigured && strings.TrimSpace(lookup("CONTROL_TLS_CA_FILE")) != "",
		},
		{
			Name:   "CONTROL_TLS_CLIENT_CERT_FILE/CONTROL_TLS_CLIENT_KEY_FILE",
			Detail: "must both be set so the gateway authenticates to the control plane with mTLS",
			OK:     clientCertFile != "" && clientKeyFile != "",
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

// billingModeOf mirrors the runtime parser for the preflight: an invalid value
// fails the check instead of silently disabling fund reservation.
func billingModeOf(lookup appenv.Lookup) billingMode {
	mode, err := parseBillingMode(lookup("GATEWAY_BILLING"))
	if err != nil {
		return ""
	}
	return mode
}

// accessGuardModeOf mirrors the runtime parser for the preflight: an invalid
// value fails the check instead of silently disabling the guard.
func accessGuardModeOf(lookup appenv.Lookup) accessGuardMode {
	mode, err := parseAccessGuardMode(lookup("GATEWAY_ACCESS_GUARD"))
	if err != nil {
		return ""
	}
	return mode
}

// positiveInt mirrors appenv.PositiveIntEnv over an injected Lookup.
func positiveInt(lookup appenv.Lookup, name string) bool {
	raw := strings.TrimSpace(lookup(name))
	if raw == "" {
		return false
	}
	value, err := strconv.Atoi(raw)
	return err == nil && value > 0
}

func gatewayInstanceID() string {
	if id := strings.TrimSpace(os.Getenv("GATEWAY_INSTANCE_ID")); id != "" {
		return id
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "gateway"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}

func intEnv(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		log.Printf("invalid %s=%q, using %d", name, value, fallback)
		return fallback
	}
	return n
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("invalid %s=%q, using %s: %v", name, value, fallback, err)
		return fallback
	}
	return duration
}
