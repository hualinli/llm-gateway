package main

import (
	"context"
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

	"llm-gateway/internal/auth"
	"llm-gateway/internal/control"
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

func main() {
	// Routes and providers come from the control-plane snapshot. Local demo
	// seed points at cmd/upstream-mock; OPENAI_BASE_URL still registers a
	// bootstrap factory for a named provider.
	registry := provider.NewRegistry(nil)
	// Real Provider registration is explicit for bootstrap factories. A
	// control-plane snapshot may also construct the built-in provider protocols
	// dynamically; unknown route IDs are still rejected atomically.
	if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
		name := os.Getenv("OPENAI_PROVIDER_ID")
		if name == "" {
			name = "openai"
		}
		cfg := provider.DefaultOpenAIConfig(name, baseURL, os.Getenv("OPENAI_API_KEY"))
		cfg.Model = os.Getenv("OPENAI_MODEL")
		if err := registry.RegisterFactory(name, func() (provider.Provider, error) {
			return provider.NewOpenAIProvider(cfg)
		}); err != nil {
			log.Fatalf("register openai provider: %v", err)
		}
		log.Printf("provider registered: %s endpoint=%s", name, baseURL)
	}

	// Redis 复用：限流 + Stream 共享同一实例
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	lim := limiter.New(rdb, 20, 40, 500*time.Millisecond)
	limiterPolicy := os.Getenv("LIMITER_ON_REDIS_ERROR")
	if limiterPolicy == "" {
		limiterPolicy = "open"
	}
	if err := lim.SetOnRedisError(limiterPolicy); err != nil {
		log.Fatalf("limiter: %v", err)
	}
	log.Printf("limiter: redis=%s rate=20 burst=40 queue=500ms on-redis-error=%s", redisAddr, limiterPolicy)

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

	// 控制面同步：GetConfig 补拉、Watch 低延迟推送、周期校验和可选租约。
	// 版本跳跃由同步器接受，Gateway 只拒绝旧版本；租约到期时撤销本地 key。
	controlAddr := os.Getenv("CONTROL_ADDR")
	if controlAddr == "" {
		controlAddr = "localhost:50051"
	}
	controlCtx, controlCancel := context.WithCancel(context.Background())
	instanceID := gatewayInstanceID()
	log.Printf("gateway instance=%s", instanceID)
	controlSync := control.NewSynchronizer(controlAddr, control.SyncOptions{
		PollInterval:   durationEnv("CONTROL_CONFIG_POLL_INTERVAL", 30*time.Second),
		LeaseTimeout:   durationEnv("CONTROL_CONFIG_LEASE", 0),
		OnLeaseExpired: func() { authStore.UpdateRecords(nil) },
		TLS:            control.TLSConfigFromEnv(),
		InstanceID:     instanceID,
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
			authStore.UpdateRecords(control.KeyRecordsToAuth(cfg.KeyRecords))
			return nil
		})
		if err != nil && controlCtx.Err() == nil {
			log.Printf("control sync stopped: %v", err)
		}
	}()

	// 链路顺序：auth(提租户) → limiter(按租户限) → proxy(透传+计量)
	limitedProxy := limiter.Middleware(lim)(proxyHandler)
	authedLimitedProxy := auth.Middleware(authStore)(limitedProxy)

	mux := http.NewServeMux()
	// pprof 挂在同端口，压测时可直接 curl /debug/pprof/profile
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/metrics", promhttp.Handler())
	_ = metrics.ActiveStreams // 触发 metrics 包 init 注册
	mux.HandleFunc("/healthz", ready.Live)
	mux.HandleFunc("/livez", ready.Live)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()
		ready.Write(w, ready.Gateway(len(registry.Models()), rdb.Ping(ctx).Err(), lim.FailClosed()))
	})
	mux.Handle("/v1/chat/completions", authedLimitedProxy)
	mux.Handle("/v1/models", auth.Middleware(authStore)(limiter.Middleware(lim)(proxy.NewModelsHandler(registry))))

	requestsCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	drain := newRequestDrain()
	srv := &http.Server{
		Addr:         ":8090",
		Handler:      drain.middleware(observability.Middleware(mux)),
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
	if err := shutdownGateway(srv, drain, cancelRequests, meterProducer, shutdownOptions{
		GracePeriod:  10 * time.Second,
		CancelPeriod: 5 * time.Second,
		FlushPeriod:  5 * time.Second,
	}); err != nil {
		log.Printf("shutdown completed with errors: %v", err)
	}
	_ = rdb.Close()
	log.Println("exit")
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
