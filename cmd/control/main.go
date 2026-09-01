package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"llm-gateway/internal/control"
	pb "llm-gateway/proto/control"
)

func main() {
	store := control.NewStore()
	srv, err := control.NewServer(store)
	if err != nil {
		log.Fatalf("control init: %v", err)
	}
	// 空库启动时种入初始版，和 gateway 的写死版保持一致；有存档则直接恢复
	if srv.CurrentConfig().Version == 0 {
		if _, err := srv.ApplyConfig(map[string]string{
			"sk-demo-123": "tenant-demo",
			"sk-test-456": "tenant-test",
		}, control.RoutesToProto(map[string][]string{
			"default": {"mock-a", "mock-b"},
		})); err != nil {
			log.Fatalf("control seed: %v", err)
		}
	}

	// gRPC :50051；配置证书后启用 TLS/mTLS，否则仅用于本地 Demo。
	tlsConfig := control.TLSConfigFromEnv()
	go func() {
		if err := control.RunWithTLS(":50051", srv, tlsConfig); err != nil {
			log.Fatalf("control grpc: %v", err)
		}
	}()

	// 临时 HTTP 管理面 :50052
	mux := http.NewServeMux()
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg, _ := srv.GetConfig(r.Context(), &pb.GetRequest{})
			// 转成便于 curl 看的 JSON 形态
			json.NewEncoder(w).Encode(map[string]interface{}{
				"version": cfg.Version,
				"keys":    cfg.Keys,
				"routes":  control.RoutesFromProto(cfg.Routes),
			})
		case http.MethodPost:
			var req struct {
				Keys   map[string]string   `json:"keys"`
				Routes map[string][]string `json:"routes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			old, _ := srv.GetConfig(r.Context(), &pb.GetRequest{})
			// 兜底：空则保留旧值
			keys, routes := req.Keys, control.RoutesToProto(req.Routes)
			if keys == nil {
				keys = old.Keys
			}
			if req.Routes == nil {
				routes = old.Routes
			}
			// 版本推进/持久化/广播全部收敛到 ApplyConfig，持久化失败则拒绝本次写入
			newCfg, err := srv.ApplyConfig(keys, routes)
			if err != nil {
				http.Error(w, "persist config: "+err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"version": newCfg.Version,
				"keys":    newCfg.Keys,
				"routes":  req.Routes,
			})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	go func() {
		log.Println("control http listening on :50052")
		log.Fatal(http.ListenAndServe(":50052", mux))
	}()

	log.Println("control ready gRPC :50051 http :50052")
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("control exit")
}
