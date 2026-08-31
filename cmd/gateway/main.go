package main

import (
	"context"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/control"
	"llm-gateway/internal/limiter"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/proxy"
	pb "llm-gateway/proto/control"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

func main() {
	// 本地演示用两个 mock，上线后由控制面通过 gRPC 热替这张表
	registry := provider.NewRegistry(map[string][]provider.Provider{
		"default": {
			provider.NewMockProvider("mock-a", 80*time.Millisecond),
			provider.NewMockProvider("mock-b", 80*time.Millisecond),
		},
	})

	// Redis 复用：限流 + Stream 共享同一实例
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	lim := limiter.New(rdb, 20, 40, 500*time.Millisecond)
	log.Printf("limiter: redis=%s rate=20 burst=40 queue=500ms", redisAddr)

	// 计量生产：批量 XADD，不阻塞热路径
	meterProducer := metering.NewProducer(rdb, 1000, 50, 200*time.Millisecond)
	proxyHandler := proxy.NewHandler(registry, meterProducer)

	// 演示用固定 Key，生产由控制面下发；明文只在这里出现一次，落 Store 即哈希
	authStore := auth.NewMemoryStore(map[string]string{
		"sk-demo-123": "tenant-demo",
		"sk-test-456": "tenant-test",
	})

	// 控制面同步：重连后先 GetConfig 补拉断连期间的版本，再 WatchConfig 长订即时热更；
	// 版本单调，旧版本一律拒绝；流断开则 3s 后重连。
	controlAddr := os.Getenv("CONTROL_ADDR")
	if controlAddr == "" {
		controlAddr = "localhost:50051"
	}
	go func() {
		var applied int64
		apply := func(cfg *pb.Config) {
			if cfg == nil || cfg.Version <= applied {
				return
			}
			log.Printf("control: hot reload %d -> %d", applied, cfg.Version)
			authStore.Update(cfg.Keys)
			registry.UpdateRoutes(control.RoutesFromProto(cfg.Routes))
			applied = cfg.Version
		}
		for {
			client, err := control.Dial(controlAddr)
			if err != nil {
				log.Printf("control dial failed %s: %v", controlAddr, err)
				time.Sleep(3 * time.Second)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if cfg, err := client.GetConfig(ctx); err == nil {
				apply(cfg)
			} else {
				log.Printf("control getconfig failed: %v", err)
			}
			cancel()

			watchCtx, wcancel := context.WithCancel(context.Background())
			if wc, err := client.WatchConfig(watchCtx, applied); err == nil {
				for {
					cfg, err := wc.Recv()
					if err != nil {
						log.Printf("control watch ended: %v", err)
						break
					}
					apply(cfg)
				}
			} else {
				log.Printf("control watch open failed: %v", err)
			}
			wcancel()
			client.Close()
			time.Sleep(3 * time.Second)
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/v1/chat/completions", authedLimitedProxy)

	srv := &http.Server{
		Addr:         ":8090",
		Handler:      mux,
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
	log.Println("exit")
}
