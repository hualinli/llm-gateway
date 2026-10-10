package control

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/migrations"
	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestProviderAdminGRPCPostgresConcurrentKeyAndProviderWrites(t *testing.T) {
	dsn, db := isolatedCredentialDB(t)
	if err := migrations.Up(dsn); err != nil {
		t.Fatal(err)
	}
	orgID, userID := seedAdminIdentity(t, db, "provider-admin@example.com", "tenant-provider-admin")
	_, membershipID := ownerMembership(t, db, orgID)
	keyStore := newAdminStore(t, db)
	configStore, err := NewPGStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = configStore.DB().Close() })

	masterKeyPath := t.TempDir() + "/provider-master-key"
	masterKey, _, err := EnsureMasterKeyFile(masterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(masterKeyFileEnv, masterKeyPath)
	t.Setenv(masterKeyValueEnv, "")
	credentialStore, err := NewPGProviderCredentialStore(db, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := credentialStore.VerifyMasterKey(context.Background()); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer(configStore)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetProviderCredentialStore(credentialStore)
	audit, err := NewPGAuditSink(db)
	if err != nil {
		t.Fatal(err)
	}
	certs := makeAdminCertificates(t)
	service, err := NewAPIKeyAdminService(srv, keyStore, audit, AdminServiceOptions{ConsoleServiceName: certs.consoleIdentity})
	if err != nil {
		t.Fatal(err)
	}
	srv.EnableAdminService(service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer, err := NewGRPCServerWithTLS(srv, &TLSConfig{
		CertFile: certs.serverCert, KeyFile: certs.serverKey, CAFile: certs.caCert, RequireClientCert: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := dialAdminTLS(t, ctx, listener.Addr().String(), certs, certs.consoleCert, certs.consoleKey)
	defer conn.Close()
	client := pb.NewAdminServiceClient(conn)
	configClient := pb.NewConfigServiceClient(conn)

	const count = 4
	keyIDs := make([]string, count)
	for i := 0; i < count; i++ {
		created, err := client.CreateApiKey(ctx, &pb.CreateApiKeyRequest{
			OperationId: "provider-race-create-" + itoa(int64(i)), Name: "provider race key " + itoa(int64(i)), MembershipId: membershipID, OrgId: orgID,
			ActorUserId: userID, ActorOrgId: orgID, RequestId: "provider-race-create-request-" + itoa(int64(i)),
		})
		if err != nil {
			t.Fatalf("create key %d: %v", i, err)
		}
		keyIDs[i] = created.GetKey().GetKeyId()
	}

	start := make(chan struct{})
	errCh := make(chan error, count*2)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := client.UpsertProvider(ctx, &pb.UpsertProviderRequest{
				OperationId: "provider-race-upsert-" + itoa(int64(i)),
				ActorUserId: userID, ActorOrgId: orgID, RequestId: "provider-race-upsert-request-" + itoa(int64(i)),
				CreateOnly: true,
				Provider:   &pb.ProviderConfig{Id: "provider-" + itoa(int64(i)), Protocol: "openai-compatible", Url: "https://upstream.example/v1"},
			})
			errCh <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := client.RevokeApiKey(ctx, &pb.RevokeApiKeyRequest{
				OperationId: "provider-race-revoke-" + itoa(int64(i)), KeyId: keyIDs[i], OrgId: orgID,
				MembershipId: membershipID,
				ActorUserId:  userID, ActorOrgId: orgID, RequestId: "provider-race-revoke-request-" + itoa(int64(i)),
			})
			errCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent provider/key mutation: %v", err)
		}
	}

	config, err := configClient.GetConfig(ctx, &pb.GetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if config.GetVersion() != count*3 || len(config.GetProviders()) != count || len(config.GetKeyRecords()) != count {
		t.Fatalf("latest config lost a concurrent write: version=%d providers=%d keys=%d", config.GetVersion(), len(config.GetProviders()), len(config.GetKeyRecords()))
	}
	for id, provider := range config.GetProviders() {
		if provider.GetId() != id {
			t.Fatalf("provider map key %q has payload id %q", id, provider.GetId())
		}
	}
	for _, key := range config.GetKeyRecords() {
		if key.GetStatus() != "revoked" {
			t.Fatalf("provider patch resurrected API key %q: status=%q", key.GetKeyId(), key.GetStatus())
		}
	}
	for _, keyID := range keyIDs {
		var state string
		if err := db.QueryRow(`SELECT status FROM api_keys WHERE key_id=$1`, keyID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "revoked" {
			t.Fatalf("key %s persisted with status %q", keyID, state)
		}
	}

	listed, err := client.ListProviders(ctx, &pb.ListProvidersRequest{
		OperationId: "provider-race-list", ActorUserId: userID, ActorOrgId: orgID, RequestId: "provider-race-list-request",
	})
	if err != nil || len(listed.GetProviders()) != count {
		t.Fatalf("ListProviders returned %d providers, err=%v", len(listed.GetProviders()), err)
	}

	// A non-empty route target list selects providers; an explicit empty list
	// disables that model and must survive the Control RPC round trip as [].
	version := config.GetVersion()
	routes, err := client.ReplaceRoutes(ctx, &pb.ReplaceRoutesRequest{
		OperationId: "provider-race-routes", ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "provider-race-routes-request", ExpectedVersion: version,
		Routes: map[string]*pb.ProviderList{"disabled-model": {Providers: []string{}}},
	})
	if err != nil {
		t.Fatalf("ReplaceRoutes with disabled model: %v", err)
	}
	if route := routes.GetRoutes()["disabled-model"]; route == nil || len(route.GetProviders()) != 0 {
		t.Fatalf("disabled route response = %+v", route)
	}

	const upstreamSecret = "this-upstream-secret-must-never-return"
	createdCredential, err := client.CreateProviderCredential(ctx, &pb.CreateProviderCredentialRequest{
		OperationId: "provider-admin-credential-create", ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "provider-admin-credential-create-request", ProviderId: "provider-0", Secret: []byte(upstreamSecret),
	})
	if err != nil {
		t.Fatalf("CreateProviderCredential: %v", err)
	}
	if createdCredential.GetCredential().GetFingerprint() == "" || strings.Contains(protojson.Format(createdCredential), upstreamSecret) {
		t.Fatalf("credential creation returned secret or omitted fingerprint: %s", protojson.Format(createdCredential))
	}
	updatedProvider, err := client.UpsertProvider(ctx, &pb.UpsertProviderRequest{
		OperationId: "provider-admin-bind-credential", ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "provider-admin-bind-credential-request",
		Provider:  &pb.ProviderConfig{Id: "provider-0", Protocol: "openai-compatible", Url: "https://upstream.example/v1", CredentialRef: createdCredential.GetCredential().GetCredentialRef()},
	})
	if err != nil || updatedProvider.GetProvider().GetCredentialRef() != createdCredential.GetCredential().GetCredentialRef() {
		t.Fatalf("bind credential: provider=%+v err=%v", updatedProvider.GetProvider(), err)
	}
	if _, err := client.RevokeProviderCredential(ctx, &pb.RevokeProviderCredentialRequest{
		OperationId: "provider-admin-revoke-in-use", ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "provider-admin-revoke-in-use-request", CredentialId: createdCredential.GetCredential().GetCredentialId(),
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("revoke in-use credential err=%v, want FAILED_PRECONDITION", err)
	}
	if _, err := client.UpsertProvider(ctx, &pb.UpsertProviderRequest{
		OperationId: "provider-admin-unbind-credential", ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "provider-admin-unbind-credential-request",
		Provider:  &pb.ProviderConfig{Id: "provider-0", Protocol: "openai-compatible", Url: "https://upstream.example/v1"},
	}); err != nil {
		t.Fatalf("unbind credential: %v", err)
	}
	revokedCredential, err := client.RevokeProviderCredential(ctx, &pb.RevokeProviderCredentialRequest{
		OperationId: "provider-admin-revoke-unused", ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "provider-admin-revoke-unused-request", CredentialId: createdCredential.GetCredential().GetCredentialId(),
	})
	if err != nil || revokedCredential.GetCredential().GetStatus() != "revoked" {
		t.Fatalf("revoke unused credential: credential=%+v err=%v", revokedCredential.GetCredential(), err)
	}
	credentialList, err := client.ListProviderCredentials(ctx, &pb.ListProviderCredentialsRequest{
		OperationId: "provider-admin-credential-list", ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "provider-admin-credential-list-request",
	})
	if err != nil || len(credentialList.GetItems()) != 1 || strings.Contains(protojson.Format(credentialList), upstreamSecret) {
		t.Fatalf("credential list leaked secret or returned unexpected items: items=%d err=%v", len(credentialList.GetItems()), err)
	}

	// Simulate an old persisted configuration that contains URL credentials and
	// query parameters. The management read RPC strips these legacy values.
	legacy := srv.CurrentConfig()
	legacy.Providers["legacy"] = &pb.ProviderConfig{
		Id: "legacy", Protocol: "openai-compatible",
		Url: "https://user:" + upstreamSecret + "@legacy.example/v1?api_key=" + upstreamSecret + "#" + upstreamSecret,
	}
	srv.cfg.Store(legacy)
	legacyList, err := client.ListProviders(ctx, &pb.ListProvidersRequest{
		OperationId: "provider-admin-legacy-list", ActorUserId: userID, ActorOrgId: orgID, RequestId: "provider-admin-legacy-list-request",
	})
	if err != nil {
		t.Fatalf("ListProviders with legacy config: %v", err)
	}
	legacyJSON := protojson.Format(legacyList)
	if strings.Contains(legacyJSON, upstreamSecret) || !strings.Contains(legacyJSON, "https://legacy.example/v1") {
		t.Fatalf("legacy provider URL was not safely redacted: %s", legacyJSON)
	}
}
