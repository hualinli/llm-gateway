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
	srv := &control.Server{}
	// 初始版，和 gateway 的写死版保持一致
	srv.UpdateConfig(&pb.Config{
		Version: 1,
		Keys: map[string]string{
			"sk-demo-123": "tenant-demo",
			"sk-test-456": "tenant-test",
		},
		Routes: control.RoutesToProto(map[string][]string{
			"default": {"mock-a", "mock-b"},
		}),
	})

	// gRPC :50051
	go func() {
		if err := control.Run(":50051", srv); err != nil {
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
			newCfg := &pb.Config{
				Version: old.Version + 1,
				Keys:    req.Keys,
				Routes:  control.RoutesToProto(req.Routes),
			}
			// 兜底：空则保留旧值
			if newCfg.Keys == nil {
				newCfg.Keys = old.Keys
			}
			if newCfg.Routes == nil {
				newCfg.Routes = old.Routes
			}
			srv.UpdateConfig(newCfg)
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
