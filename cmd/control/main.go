package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"llm-gateway/internal/control"
	"llm-gateway/internal/observability"
	pb "llm-gateway/proto/control"
)

func redactKeyRecords(records []*pb.KeyRecord) []map[string]string {
	out := make([]map[string]string, 0, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		out = append(out, map[string]string{
			"key_id":     record.KeyId,
			"key_prefix": record.KeyPrefix,
			"tenant_id":  record.TenantId,
			"status":     record.Status,
		})
	}
	return out
}

func principalSubject(r *http.Request) string {
	principal, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		return "unknown"
	}
	return principal.Subject
}

func principalRole(r *http.Request) control.Role {
	principal, ok := control.PrincipalFromContext(r.Context())
	if !ok {
		return "unknown"
	}
	return principal.Role
}

func flagEnv(name string) bool {
	value := os.Getenv(name)
	return value == "1" || value == "true" || value == "yes"
}

func allowLegacyKeyInput(requireAdminAuth, requirePostgres bool) bool {
	return !requireAdminAuth && !requirePostgres
}

func main() {
	store, err := control.NewStore()
	if err != nil {
		log.Fatalf("control store: %v", err)
	}
	srv, err := control.NewServer(store)
	if err != nil {
		log.Fatalf("control init: %v", err)
	}
	// 空库启动时种入初始版，和 gateway 的写死版保持一致；有存档则直接恢复
	auditSink := control.AuditSink(control.LogAuditSink{})
	if pgStore, ok := store.(*control.PGStore); ok {
		var auditErr error
		auditSink, auditErr = control.NewPGAuditSink(pgStore.DB())
		if auditErr != nil {
			log.Fatalf("control audit store: %v", auditErr)
		}
	}

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

	// HTTP 管理面 :50052；本地 Demo 可不配置 token，生产必须开启鉴权。
	mux := http.NewServeMux()
	adminToken := os.Getenv("CONTROL_ADMIN_TOKEN")
	requireAdminAuth := flagEnv("CONTROL_REQUIRE_ADMIN_AUTH")
	requirePostgres := flagEnv("CONTROL_REQUIRE_POSTGRES")
	legacyKeyInputAllowed := allowLegacyKeyInput(requireAdminAuth, requirePostgres)
	if requireAdminAuth && adminToken == "" {
		log.Fatal("CONTROL_REQUIRE_ADMIN_AUTH requires CONTROL_ADMIN_TOKEN")
	}
	identityVerifier, err := control.NewIdentityVerifier(adminToken, os.Getenv("CONTROL_IDENTITY_SIGNING_SECRET"))
	if err != nil {
		log.Fatalf("control identity: %v", err)
	}
	credentialService, err := control.NewCredentialServiceWithAudit(srv, auditSink)
	if err != nil {
		log.Fatalf("control credentials: %v", err)
	}
	credentialHandler, err := control.NewCredentialHTTPHandler(credentialService)
	if err != nil {
		log.Fatalf("control credential HTTP: %v", err)
	}
	configHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg, _ := srv.GetConfig(r.Context(), &pb.GetRequest{})
			// 转成便于 curl 看的 JSON 形态
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"version": cfg.Version,
				"keys":    redactKeyRecords(cfg.KeyRecords),
				"routes":  control.RoutesFromProto(cfg.Routes),
			})
		case http.MethodPost:
			var req struct {
				Keys       map[string]string   `json:"keys"` // legacy input; converted to hashes before persistence
				KeyRecords []*pb.KeyRecord     `json:"key_records"`
				Routes     map[string][]string `json:"routes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if req.Keys != nil && !legacyKeyInputAllowed {
				http.Error(w, "legacy keys input is disabled; use key_records or the credential service", http.StatusBadRequest)
				return
			}
			old, _ := srv.GetConfig(r.Context(), &pb.GetRequest{})
			// 兜底：空则保留旧值。Legacy keys 只在请求边界转换，新的
			// Config/Store/Watch 路径只保存 KeyRecords。
			records, routes := req.KeyRecords, control.RoutesToProto(req.Routes)
			if records == nil && req.Keys == nil {
				records = old.KeyRecords
			}
			if req.Routes == nil {
				routes = old.Routes
			}
			var (
				newCfg   *pb.Config
				applyErr error
			)
			if records != nil {
				newCfg, applyErr = srv.ApplyKeyRecords(records, routes)
			} else {
				// Compatibility for the local demo; ApplyConfig hashes immediately.
				newCfg, applyErr = srv.ApplyConfig(req.Keys, routes)
			}
			if applyErr != nil {
				auditSink.Record(r.Context(), control.AuditEvent{
					Action: "config.apply", Outcome: "failure",
					Subject: principalSubject(r), Role: principalRole(r),
					RequestID: observability.RequestID(r.Context()), TraceID: observability.TraceID(r.Context()),
				})
				http.Error(w, "persist config: "+applyErr.Error(), http.StatusInternalServerError)
				return
			}
			auditSink.Record(r.Context(), control.AuditEvent{
				Action: "config.apply", Outcome: "success",
				Subject: principalSubject(r), Role: principalRole(r),
				RequestID: observability.RequestID(r.Context()), TraceID: observability.TraceID(r.Context()),
				ConfigVersion: newCfg.Version, KeyCount: len(newCfg.KeyRecords),
			})
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"version": newCfg.Version,
				"keys":    redactKeyRecords(newCfg.KeyRecords),
				"routes":  control.RoutesFromProto(newCfg.Routes),
			})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
	mux.Handle("/config", control.AdminAuth(configHandler, adminToken, requireAdminAuth))
	keysAuth := control.IdentityAuth(credentialHandler, identityVerifier, true)
	mux.Handle("/keys", keysAuth)
	mux.Handle("/keys/", keysAuth)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	go func() {
		log.Println("control http listening on :50052")
		log.Fatal(http.ListenAndServe(":50052", observability.Middleware(mux)))
	}()

	log.Println("control ready gRPC :50051 http :50052")
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("control exit")
}
