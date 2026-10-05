package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/auth"
	"llm-gateway/internal/control"
	"llm-gateway/internal/observability"
	pb "llm-gateway/proto/control"
)

// maxConfigBody bounds the /config admin payload, mirroring the data-plane
// request body limit. Without it, an unauthenticated demo-mode client can
// force an unbounded allocation through JSON decoding.
const maxConfigBody = 4 << 20

const (
	defaultControlHTTPAddr = ":50052"
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

func redactProviderConfigs(configs map[string]*pb.ProviderConfig) map[string]map[string]interface{} {
	out := make(map[string]map[string]interface{}, len(configs))
	for id, cfg := range configs {
		if cfg == nil {
			out[id] = nil
			continue
		}
		// ProviderConfig intentionally has no raw secret field. Returning only
		// its opaque credential reference keeps the control API useful without
		// disclosing the locally resolved upstream key.
		entry := map[string]interface{}{
			"id":             cfg.Id,
			"protocol":       cfg.Protocol,
			"url":            cfg.Url,
			"credential_ref": cfg.CredentialRef,
			"model":          cfg.Model,
			"mock_delay_ms":  cfg.MockDelayMs,
			"model_mapping":  cfg.ModelMapping,
		}
		if cfg.Timeouts != nil {
			entry["timeouts"] = cfg.Timeouts
		}
		out[id] = entry
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

// flagEnv keeps the historical call sites but defers to appenv.BoolEnv, so the
// runtime switch and the production preflight (which uses the same parsing over
// an injected Lookup) can never disagree on spellings like " TRUE ".
func flagEnv(name string) bool {
	return appenv.BoolEnv(name)
}

func allowLegacyKeyInput(requireAdminAuth, requirePostgres bool) bool {
	return !requireAdminAuth && !requirePostgres
}

func main() {
	profile, err := appenv.FromEnv()
	if err != nil {
		log.Fatalf("runtime profile: %v", err)
	}
	appenv.Log(profile)

	// Read the gRPC TLS material once: the production preflight and the running
	// server must make the same decision.
	tlsConfig := control.TLSConfigFromEnv()
	checks := controlChecks(appenv.EnvLookup, tlsConfig != nil)
	if err := appenv.Validate(profile, checks); err != nil {
		log.Fatalf("control production preflight failed:\n%v", err)
	}
	if !profile.IsProduction() {
		warnFailedChecks(checks)
	}

	// Listener addresses are resolved before anything binds, so a conflicting
	// diagnostics address fails fast instead of after the gRPC port is taken.
	httpAddr := appenv.StringEnv("CONTROL_HTTP_ADDR", defaultControlHTTPAddr)
	diagnosticsAddr := strings.TrimSpace(os.Getenv("CONTROL_DIAGNOSTICS_ADDR"))
	if diagnosticsAddr != "" && diagnosticsAddr == httpAddr {
		log.Fatalf("CONTROL_DIAGNOSTICS_ADDR=%q must differ from CONTROL_HTTP_ADDR=%q", diagnosticsAddr, httpAddr)
	}

	store, err := control.NewStore()
	if err != nil {
		log.Fatalf("control store: %v", err)
	}
	srv, err := control.NewServer(store)
	if err != nil {
		log.Fatalf("control init: %v", err)
	}
	// An empty store stays empty. Providers and routes are business state and
	// must be submitted explicitly through the control plane; startup must not
	// silently point traffic at local mocks or any other endpoint.
	auditSink := control.AuditSink(control.LogAuditSink{})
	if pgStore, ok := store.(*control.PGStore); ok {
		var auditErr error
		auditSink, auditErr = control.NewPGAuditSink(pgStore.DB())
		if auditErr != nil {
			log.Fatalf("control audit store: %v", auditErr)
		}
	}

	if srv.CurrentConfig().Version == 0 {
		var bootstrapRecords []*pb.KeyRecord
		if rawKey := os.Getenv("LLM_GATEWAY_BOOTSTRAP_API_KEY"); rawKey != "" {
			tenantID := os.Getenv("LLM_GATEWAY_BOOTSTRAP_TENANT_ID")
			if tenantID == "" {
				tenantID = "tenant-demo"
			}
			record := auth.NewKeyRecord("bootstrap-"+auth.HashKey(rawKey), rawKey, tenantID)
			bootstrapRecords = []*pb.KeyRecord{{
				KeyId: record.ID, KeyHash: record.Hash, KeyPrefix: record.Prefix,
				TenantId: record.TenantID, Status: record.Status,
			}}
		}
		if len(bootstrapRecords) > 0 {
			if _, err := srv.ApplyKeyRecordsAndProviders(bootstrapRecords, nil, nil); err != nil {
				log.Fatalf("control bootstrap credential: %v", err)
			}
		}
	}

	// gRPC :50051；配置证书后启用 TLS/mTLS，否则仅用于本地 Demo。
	providerCredentials := configureProviderCredentials(srv, store, tlsConfig)
	srv.EnableProviderCredentialResolver(providerCredentials, auditSink, flagEnv("CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE"))
	// Console → Control 内部管理面：默认完全不注册；启用时要求 PG 事实表 +
	// mTLS 客户端证书 + 显式配置的 console 服务身份（ADR-0003 §2.2）。
	if err := configureAdminService(srv, store, auditSink, tlsConfig); err != nil {
		log.Fatalf("control admin service: %v", err)
	}
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
	providerCredentialService, err := control.NewProviderCredentialService(srv, providerCredentials, auditSink)
	if err != nil {
		log.Fatalf("provider credentials: %v", err)
	}
	providerCredentialHandler, err := control.NewProviderCredentialHTTPHandler(providerCredentialService)
	if err != nil {
		log.Fatalf("provider credential HTTP: %v", err)
	}
	configHandler := newConfigHandler(srv, auditSink, legacyKeyInputAllowed)
	mux.Handle("/config", control.AdminAuth(configHandler, adminToken, requireAdminAuth))
	mux.Handle("/gateways", control.AdminAuth(newGatewayStatusHandler(srv), adminToken, requireAdminAuth))
	keysAuth := control.IdentityAuth(credentialHandler, identityVerifier, true)
	mux.Handle("/keys", keysAuth)
	mux.Handle("/keys/", keysAuth)
	providerAuth := control.AdminAuth(providerCredentialHandler, adminToken, requireAdminAuth)
	mux.Handle("/provider-credentials", providerAuth)
	mux.Handle("/provider-credentials/", providerAuth)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	// 生产要求 CONTROL_DIAGNOSTICS_ADDR 与 HTTP 管理面分离；设置后在独立
	// listener 上同样提供 /healthz（本进程目前没有注册 metrics，不凭空发明）。
	var diagnosticsSrv *http.Server
	httpSrv := &http.Server{Addr: httpAddr, Handler: observability.Middleware(mux)}
	go func() {
		log.Printf("control http listening on %s", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("control http: %v", err)
		}
	}()
	if diagnosticsAddr != "" {
		diagnosticsMux := http.NewServeMux()
		diagnosticsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
		diagnosticsSrv = &http.Server{
			Addr:         diagnosticsAddr,
			Handler:      diagnosticsMux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		}
		go func() {
			log.Printf("control diagnostics listening on %s (/healthz)", diagnosticsAddr)
			if err := diagnosticsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("control diagnostics: %v", err)
			}
		}()
	}

	log.Printf("control ready gRPC :50051 http %s", httpAddr)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelShutdown()
	if diagnosticsSrv != nil {
		_ = diagnosticsSrv.Shutdown(shutdownCtx)
	}
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Println("control exit")
}

// configureAdminService registers the Console-facing AdminService when
// CONTROL_ADMIN_SERVICE_ENABLED is set. It fails fast instead of exposing an
// unauthenticated management surface: the console identity must be explicit and
// mTLS client certificates must already be required, unless the local demo
// escape hatch is set on purpose.
func configureAdminService(srv *control.Server, store control.Store, auditSink control.AuditSink, tlsConfig *control.TLSConfig) error {
	if !flagEnv("CONTROL_ADMIN_SERVICE_ENABLED") {
		return nil
	}
	consoleIdentity := strings.TrimSpace(os.Getenv("CONTROL_CONSOLE_SERVICE_NAME"))
	if consoleIdentity == "" {
		return errors.New("CONTROL_ADMIN_SERVICE_ENABLED requires CONTROL_CONSOLE_SERVICE_NAME")
	}
	allowInsecure := flagEnv("CONTROL_ALLOW_INSECURE_ADMIN_SERVICE")
	if !allowInsecure {
		if tlsConfig == nil || !tlsConfig.RequireClientCert || strings.TrimSpace(tlsConfig.CAFile) == "" {
			return errors.New("CONTROL_ADMIN_SERVICE_ENABLED requires mTLS client certificates (CONTROL_TLS_REQUIRE_CLIENT_CERT + CONTROL_TLS_CA_FILE)")
		}
	}
	pgStore, ok := store.(*control.PGStore)
	if !ok {
		return errors.New("CONTROL_ADMIN_SERVICE_ENABLED requires the PostgreSQL control store (CONTROL_REQUIRE_POSTGRES)")
	}
	keyStore, err := control.NewPGAPIKeyStore(pgStore.DB())
	if err != nil {
		return err
	}
	service, err := control.NewAPIKeyAdminService(srv, keyStore, auditSink, control.AdminServiceOptions{
		ConsoleServiceName: consoleIdentity,
		AllowInsecure:      allowInsecure,
	})
	if err != nil {
		return err
	}
	srv.EnableAdminService(service)
	log.Printf("control admin service enabled for console identity %q", consoleIdentity)
	return nil
}

// controlChecks is the pure production preflight list for the control plane. It
// never reads the process environment itself and has no side effects.
func controlChecks(lookup appenv.Lookup, controlTLSConfigured bool) []appenv.Check {
	httpAddr := strings.TrimSpace(lookup("CONTROL_HTTP_ADDR"))
	if httpAddr == "" {
		httpAddr = defaultControlHTTPAddr
	}
	diagnosticsAddr := strings.TrimSpace(lookup("CONTROL_DIAGNOSTICS_ADDR"))
	certFile := strings.TrimSpace(lookup("CONTROL_TLS_CERT_FILE"))
	keyFile := strings.TrimSpace(lookup("CONTROL_TLS_KEY_FILE"))
	caFile := strings.TrimSpace(lookup("CONTROL_TLS_CA_FILE"))
	// Mirror control.TLSConfigFromEnv() exactly for the client-cert switch, so a
	// truthy spelling it does not honour can never satisfy the check.
	requireClientCertValue := strings.TrimSpace(lookup("CONTROL_TLS_REQUIRE_CLIENT_CERT"))
	requireClientCert := requireClientCertValue == "1" || requireClientCertValue == "true"

	return []appenv.Check{
		{
			Name:   "CONTROL_REQUIRE_POSTGRES",
			Detail: "must be true: production control state is stored in PostgreSQL, not memory",
			OK:     truthy(lookup("CONTROL_REQUIRE_POSTGRES")),
		},
		{
			Name:   "POSTGRES_DSN",
			Detail: "must be set: CONTROL_REQUIRE_POSTGRES cannot be satisfied without a database URL",
			OK:     strings.TrimSpace(lookup("POSTGRES_DSN")) != "",
		},
		{
			Name:   "CONTROL_REQUIRE_ADMIN_AUTH",
			Detail: "must be true: the HTTP admin API cannot be anonymous in production",
			OK:     truthy(lookup("CONTROL_REQUIRE_ADMIN_AUTH")),
		},
		{
			Name:   "CONTROL_ADMIN_TOKEN",
			Detail: "must be at least 16 characters",
			OK:     len(strings.TrimSpace(lookup("CONTROL_ADMIN_TOKEN"))) >= 16,
		},
		{
			Name:   "CONTROL_IDENTITY_SIGNING_SECRET",
			Detail: "must be at least 16 characters",
			OK:     len(strings.TrimSpace(lookup("CONTROL_IDENTITY_SIGNING_SECRET"))) >= 16,
		},
		{
			Name:   "LLM_GATEWAY_BOOTSTRAP_API_KEY",
			Detail: "must be empty: production keys are issued through the credential service, not a raw bootstrap key",
			OK:     strings.TrimSpace(lookup("LLM_GATEWAY_BOOTSTRAP_API_KEY")) == "",
		},
		{
			Name:   "CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE",
			Detail: "must not be enabled: provider credentials may only be resolved over mTLS gRPC",
			OK:     !truthy(lookup("CONTROL_ALLOW_INSECURE_CREDENTIAL_RESOLVE")),
		},
		{
			Name:   "CONTROL_TLS_CERT_FILE/CONTROL_TLS_KEY_FILE",
			Detail: "must both be set: control gRPC must not serve plaintext in production",
			OK:     controlTLSConfigured && certFile != "" && keyFile != "",
		},
		{
			Name:   "CONTROL_TLS_REQUIRE_CLIENT_CERT/CONTROL_TLS_CA_FILE",
			Detail: "must require client certificates and set the client CA file so gateway connections are mutually authenticated",
			OK:     requireClientCert && caFile != "",
		},
		{
			Name:   "CONTROL_DIAGNOSTICS_ADDR",
			Detail: fmt.Sprintf("must be set to a listener address different from the HTTP admin address %q", httpAddr),
			OK:     diagnosticsAddr != "" && diagnosticsAddr != httpAddr,
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

// truthy mirrors the existing flagEnv()/store.go parsing for injected lookups:
// only 1/true/yes enable a switch.
func truthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func newConfigHandler(srv *control.Server, auditSink control.AuditSink, legacyKeyInputAllowed bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg, _ := srv.GetConfig(r.Context(), &pb.GetRequest{})
			// 转成便于 curl 看的 JSON 形态
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"result":          "persisted",
				"desired_version": cfg.Version,
				"version":         cfg.Version,
				"keys":            redactKeyRecords(cfg.KeyRecords),
				"routes":          control.RoutesFromProto(cfg.Routes),
				"providers":       redactProviderConfigs(cfg.Providers),
			})
		case http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, maxConfigBody)
			var req struct {
				Keys            map[string]string             `json:"keys"` // legacy input; converted to hashes before persistence
				KeyRecords      []*pb.KeyRecord               `json:"key_records"`
				Routes          map[string][]string           `json:"routes"`
				Providers       map[string]*pb.ProviderConfig `json:"providers"`
				ExpectedVersion *int64                        `json:"expected_version"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if req.Keys != nil && !legacyKeyInputAllowed {
				http.Error(w, "legacy keys input is disabled; use key_records or the credential service", http.StatusBadRequest)
				return
			}
			if req.Keys != nil && req.KeyRecords != nil {
				http.Error(w, "keys and key_records cannot be set together", http.StatusBadRequest)
				return
			}
			patch := control.ConfigPatch{KeyRecords: req.KeyRecords, LegacyKeys: req.Keys, ExpectedVersion: req.ExpectedVersion}
			if req.Routes != nil {
				patch.Routes = control.RoutesToProto(req.Routes)
			}
			if req.Providers != nil {
				patch.Providers = req.Providers
			}
			newCfg, applyErr := srv.PatchConfig(patch)
			if applyErr != nil {
				auditSink.Record(r.Context(), control.AuditEvent{
					Action: "config.apply", Outcome: "failure",
					Subject: principalSubject(r), Role: principalRole(r),
					RequestID: observability.RequestID(r.Context()), TraceID: observability.TraceID(r.Context()),
				})
				status := http.StatusInternalServerError
				switch {
				case errors.Is(applyErr, control.ErrConfigVersionConflict):
					status = http.StatusConflict
				case errors.Is(applyErr, control.ErrInvalidConfig):
					status = http.StatusBadRequest
				}
				http.Error(w, "persist config: "+applyErr.Error(), status)
				return
			}
			auditSink.Record(r.Context(), control.AuditEvent{
				Action: "config.apply", Outcome: "success",
				Subject: principalSubject(r), Role: principalRole(r),
				RequestID: observability.RequestID(r.Context()), TraceID: observability.TraceID(r.Context()),
				ConfigVersion: newCfg.Version, KeyCount: len(newCfg.KeyRecords),
			})
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"result":          "persisted",
				"desired_version": newCfg.Version,
				"version":         newCfg.Version,
				"keys":            redactKeyRecords(newCfg.KeyRecords),
				"routes":          control.RoutesFromProto(newCfg.Routes),
				"providers":       redactProviderConfigs(newCfg.Providers),
			})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
}

func newGatewayStatusHandler(srv *control.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"desired_version": srv.CurrentConfig().GetVersion(),
			"gateways":        srv.GatewayReports(),
		})
	})
}
