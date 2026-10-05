package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestAdminServiceNotRegisteredByDefault proves the management RPCs stay
// invisible unless the operator enables them explicitly.
func TestAdminServiceNotRegisteredByDefault(t *testing.T) {
	client, cleanup := serveAdminGRPC(t, newTestServer(t), nil)
	defer cleanup()
	_, err := client.ListApiKeys(context.Background(), &pb.ListApiKeysRequest{OperationId: "op"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unregistered admin service err = %v, want UNIMPLEMENTED", err)
	}
}

// TestAdminServiceRejectsUnverifiedTransport covers the app-layer half of the
// authorization: a caller that reached the RPC without a verified client
// certificate is rejected even when the transport accepted it.
func TestAdminServiceRejectsUnverifiedTransport(t *testing.T) {
	srv := newTestServer(t)
	service, err := NewAPIKeyAdminService(srv, &fakeAdminStore{}, nil, AdminServiceOptions{ConsoleServiceName: "console.test"})
	if err != nil {
		t.Fatal(err)
	}
	srv.EnableAdminService(service)
	client, cleanup := serveAdminGRPC(t, srv, nil)
	defer cleanup()
	_, err = client.ListApiKeys(context.Background(), &pb.ListApiKeysRequest{OperationId: "op"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("plaintext admin caller err = %v, want UNAUTHENTICATED", err)
	}
}

func serveAdminGRPC(t *testing.T, srv *Server, dialOptions []grpc.DialOption) (pb.AdminServiceClient, func()) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	gs := NewGRPCServer(srv)
	go func() { _ = gs.Serve(listener) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, dialOptions...)
	conn, err := grpc.DialContext(ctx, "bufnet", options...)
	if err != nil {
		gs.Stop()
		t.Fatal(err)
	}
	return pb.NewAdminServiceClient(conn), func() {
		_ = conn.Close()
		gs.Stop()
		_ = listener.Close()
	}
}

type adminCertificates struct {
	caCert                                            string
	serverCert, serverKey                             string
	consoleCert, consoleKey                           string
	foreignCert, foreignKey                           string
	consoleIdentity, foreignIdentity, serverNameValue string
}

// makeAdminCertificates issues a CA plus a server certificate, the console
// client certificate and a foreign client certificate (a Gateway identity) so
// the mTLS authorization can be tested end to end.
func makeAdminCertificates(t *testing.T) adminCertificates {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "t03 test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert := filepath.Join(dir, "ca.pem")
	writePEM(t, caCert, "CERTIFICATE", caDER)

	serverCert, serverKey := makeLeafCertificate(t, dir, "localhost", caTemplate, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	consoleCert, consoleKey := makeLeafCertificate(t, dir, "console.test", caTemplate, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	foreignCert, foreignKey := makeLeafCertificate(t, dir, "gateway.test", caTemplate, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	return adminCertificates{
		caCert: caCert, serverCert: serverCert, serverKey: serverKey,
		consoleCert: consoleCert, consoleKey: consoleKey,
		foreignCert: foreignCert, foreignKey: foreignKey,
		consoleIdentity: "console.test", foreignIdentity: "gateway.test", serverNameValue: "localhost",
	}
}

// TestAPIKeyAdminGRPCEndToEndPostgres exercises the whole Console → Control
// path over a real mTLS listener backed by the PostgreSQL fact store.
func TestAPIKeyAdminGRPCEndToEndPostgres(t *testing.T) {
	db := isolatedAdminDB(t)
	orgID, userID := seedAdminIdentity(t, db, "grpc@example.com", "tenant-grpc")
	keyStore := newAdminStore(t, db)
	certs := makeAdminCertificates(t)

	// The Control server keeps its own snapshot store; the AdminService writes
	// the api_keys fact table and publishes the derived snapshot in process.
	srv := newTestServer(t)
	service, err := NewAPIKeyAdminService(srv, keyStore, NewMemoryAuditSink(16), AdminServiceOptions{ConsoleServiceName: certs.consoleIdentity})
	if err != nil {
		t.Fatal(err)
	}
	srv.EnableAdminService(service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs, err := NewGRPCServerWithTLS(srv, &TLSConfig{
		CertFile: certs.serverCert, KeyFile: certs.serverKey, CAFile: certs.caCert, RequireClientCert: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(listener) }()
	defer func() {
		gs.Stop()
		_ = listener.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	consoleConn := dialAdminTLS(t, ctx, listener.Addr().String(), certs, certs.consoleCert, certs.consoleKey)
	defer consoleConn.Close()
	client := pb.NewAdminServiceClient(consoleConn)
	configClient := pb.NewConfigServiceClient(consoleConn)

	owner := userID
	created, err := client.CreateApiKey(ctx, &pb.CreateApiKeyRequest{
		OperationId: "op-e2e-create", OrgId: orgID, OwnerUserId: &owner,
		ActorUserId: userID, ActorOrgId: orgID, RequestId: "req-e2e-create",
		ModelAllowlist: &pb.ModelAllowlist{Models: []string{"gpt-b", "gpt-a"}},
	})
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	if !strings.HasPrefix(created.GetSecret(), "sk-") || len(created.GetSecret()) != 46 {
		t.Fatalf("secret = %q, want sk- plus 43 base64url characters", created.GetSecret())
	}
	if created.GetReplayed() {
		t.Fatal("first create reported a replay")
	}
	if created.GetKey().GetOrgId() != orgID || created.GetKey().GetStatus() != "active" {
		t.Fatalf("created key metadata = %+v", created.GetKey())
	}
	if created.GetKey().GetTenantId() != "tenant-grpc" {
		t.Fatalf("tenant projection = %q", created.GetKey().GetTenantId())
	}
	// Captured example for the task report: the secret is the only sensitive
	// field and is redacted here.
	redacted := strings.ReplaceAll(protojson.Format(created), created.GetSecret(), "<redacted>")
	t.Logf("captured CreateApiKey response (secret redacted): %s", redacted)
	if strings.Contains(redacted, created.GetSecret()) {
		t.Fatal("redaction failed")
	}

	// The same mTLS server still serves ConfigService unchanged, and the
	// in-memory snapshot published by the admin service is already visible.
	snapshot, err := configClient.GetConfig(ctx, &pb.GetRequest{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if snapshot.GetVersion() != 1 || len(snapshot.GetKeyRecords()) != 1 {
		t.Fatalf("published snapshot = %+v", snapshot)
	}
	if snapshot.GetKeyRecords()[0].GetKeyId() != created.GetKey().GetKeyId() {
		t.Fatalf("snapshot projection = %+v", snapshot.GetKeyRecords())
	}

	// The raw key resolves through the Gateway-facing mapping and never
	// appears in the snapshot payload.
	records := KeyRecordsToAuth(snapshot.GetKeyRecords())
	if len(records) != 1 || records[0].Hash != apiKeyHash(created.GetSecret()) {
		t.Fatalf("gateway records = %+v", records)
	}
	payload, err := protojson.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), created.GetSecret()) {
		t.Fatal("snapshot leaked the raw secret")
	}

	listed, err := client.ListApiKeys(ctx, &pb.ListApiKeysRequest{
		OperationId: "op-e2e-list", OrgId: orgID, PageSize: 1, ActorUserId: userID, ActorOrgId: orgID, RequestId: "req-e2e-list",
	})
	if err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	if len(listed.GetKeys()) != 1 || listed.GetKeys()[0].GetKeyId() != created.GetKey().GetKeyId() {
		t.Fatalf("list = %+v", listed.GetKeys())
	}
	if listed.GetNextPageToken() != "" {
		t.Fatalf("unexpected next page token %q", listed.GetNextPageToken())
	}
	if got := listed.GetKeys()[0].GetModelAllowlist().GetModels(); len(got) != 2 || got[0] != "gpt-a" || got[1] != "gpt-b" {
		t.Fatalf("list allowlist = %v", got)
	}
	listJSON := protojson.Format(listed)
	if strings.Contains(listJSON, created.GetSecret()) || strings.Contains(listJSON, apiKeyHash(created.GetSecret())) {
		t.Fatalf("ListApiKeys leaked credential material: %s", listJSON)
	}

	// Idempotent replay: same operation id, same payload, no second key.
	replayed, err := client.CreateApiKey(ctx, &pb.CreateApiKeyRequest{
		OperationId: "op-e2e-create", OrgId: orgID, OwnerUserId: &owner,
		ActorUserId: userID, ActorOrgId: orgID, RequestId: "req-e2e-create-retry",
		ModelAllowlist: &pb.ModelAllowlist{Models: []string{"gpt-a", "gpt-b"}},
	})
	if err != nil {
		t.Fatalf("replayed CreateApiKey: %v", err)
	}
	if !replayed.GetReplayed() || replayed.GetSecret() != "" {
		t.Fatalf("replay = %+v, want stored metadata without a secret", replayed)
	}
	if replayed.GetKey().GetKeyId() != created.GetKey().GetKeyId() {
		t.Fatalf("replay returned a different key: %s", replayed.GetKey().GetKeyId())
	}

	// ALREADY_EXISTS: same operation id, different business payload.
	_, err = client.CreateApiKey(ctx, &pb.CreateApiKeyRequest{
		OperationId: "op-e2e-create", OrgId: orgID, OwnerUserId: &owner,
		ActorUserId: userID, ActorOrgId: orgID, RequestId: "req-e2e-conflict",
		ModelAllowlist: &pb.ModelAllowlist{Models: []string{"gpt-z"}},
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("conflicting payload err = %v, want ALREADY_EXISTS", err)
	}
	t.Logf("captured ALREADY_EXISTS conflict: %v", err)

	// FAILED_PRECONDITION carries the current version.
	staleVersion := int64(0)
	_, err = client.RevokeApiKey(ctx, &pb.RevokeApiKeyRequest{
		OperationId: "op-e2e-stale", KeyId: created.GetKey().GetKeyId(), OrgId: orgID,
		ActorUserId: userID, ActorOrgId: orgID, ExpectedVersion: &staleVersion, RequestId: "req-e2e-stale",
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "current 1") {
		t.Fatalf("stale expected_version err = %v, want FAILED_PRECONDITION with current 1", err)
	}
	t.Logf("captured FAILED_PRECONDITION: %v", err)

	revoked, err := client.RevokeApiKey(ctx, &pb.RevokeApiKeyRequest{
		OperationId: "op-e2e-revoke", KeyId: created.GetKey().GetKeyId(), OrgId: orgID,
		ActorUserId: userID, ActorOrgId: orgID, RequestId: "req-e2e-revoke",
	})
	if err != nil {
		t.Fatalf("RevokeApiKey: %v", err)
	}
	if revoked.GetKey().GetStatus() != "revoked" || revoked.ResultingVersion != 2 {
		t.Fatalf("revoke = %+v", revoked)
	}
	snapshot, err = configClient.GetConfig(ctx, &pb.GetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetVersion() != 2 || snapshot.GetKeyRecords()[0].GetStatus() != "revoked" {
		t.Fatalf("revoked snapshot = %+v", snapshot)
	}

	// GetOperation recovers the stored result, never the secret.
	operation, err := client.GetOperation(ctx, &pb.GetOperationRequest{
		OperationId: "op-e2e-create", ActorUserId: userID, ActorOrgId: orgID, RequestId: "req-e2e-operation",
	})
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if operation.GetKind() != operationCreateAPIKey || operation.GetResult().GetKey().GetKeyId() != created.GetKey().GetKeyId() {
		t.Fatalf("operation = %+v", operation)
	}
	operationJSON := protojson.Format(operation)
	if strings.Contains(operationJSON, created.GetSecret()) {
		t.Fatalf("GetOperation leaked the secret: %s", operationJSON)
	}

	// A foreign client certificate (the Gateway identity) is rejected.
	foreignConn := dialAdminTLS(t, ctx, listener.Addr().String(), certs, certs.foreignCert, certs.foreignKey)
	defer foreignConn.Close()
	_, err = pb.NewAdminServiceClient(foreignConn).ListApiKeys(ctx, &pb.ListApiKeysRequest{OperationId: "op-e2e-foreign"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign identity err = %v, want PERMISSION_DENIED", err)
	}
	// ConfigService stays open to that same identity: only the management API
	// is identity-restricted.
	if _, err := pb.NewConfigServiceClient(foreignConn).GetConfig(ctx, &pb.GetRequest{}); err != nil {
		t.Fatalf("ConfigService rejected a Gateway identity: %v", err)
	}

	// Everything the Console created is durable in PostgreSQL.
	var storedHash, storedStatus string
	if err := db.QueryRow(`SELECT key_hash, status FROM api_keys WHERE key_id=$1`, created.GetKey().GetKeyId()).
		Scan(&storedHash, &storedStatus); err != nil {
		t.Fatal(err)
	}
	if storedHash != apiKeyHash(created.GetSecret()) || storedStatus != "revoked" {
		t.Fatalf("persisted key hash=%s status=%s", storedHash, storedStatus)
	}

	// ApplyOrgPolicy publishes the organization policy into the same snapshot
	// and bumps its policy version for keys created afterwards.
	rpm := int32(60)
	applied, err := client.ApplyOrgPolicy(ctx, &pb.ApplyOrgPolicyRequest{
		OperationId: "op-e2e-policy", OrgId: orgID, ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "req-e2e-policy", ModelAllowlist: &pb.ModelAllowlist{Models: []string{"gpt-a"}},
		RequestsPerMinute: &rpm,
	})
	if err != nil {
		t.Fatalf("ApplyOrgPolicy: %v", err)
	}
	if applied.GetPolicy().GetPolicyVersion() != 1 || applied.GetResultingVersion() != 3 {
		t.Fatalf("policy apply = %+v", applied)
	}
	snapshot, err = configClient.GetConfig(ctx, &pb.GetRequest{})
	if err != nil {
		t.Fatal(err)
	}
	policy := snapshot.GetOrgPolicies()[orgID]
	if policy == nil || policy.GetRequestsPerMinute() != 60 || policy.GetModelAllowlist() == nil {
		t.Fatalf("snapshot policy = %+v", policy)
	}
	later, err := client.CreateApiKey(ctx, &pb.CreateApiKeyRequest{
		OperationId: "op-e2e-create-2", OrgId: orgID, ActorUserId: userID, ActorOrgId: orgID,
		RequestId: "req-e2e-create-2",
	})
	if err != nil {
		t.Fatalf("CreateApiKey after policy: %v", err)
	}
	if later.GetKey().GetPolicyVersion() != 1 || later.GetKey().OwnerUserId != nil {
		t.Fatalf("key after policy = %+v", later.GetKey())
	}
}

func dialAdminTLS(t *testing.T, ctx context.Context, address string, certs adminCertificates, certFile, keyFile string) *grpc.ClientConn {
	t.Helper()
	transport, err := clientCredentials(&TLSConfig{
		CAFile: certs.caCert, ClientCertFile: certFile, ClientKeyFile: keyFile, ServerName: certs.serverNameValue,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.DialContext(ctx, address, grpc.WithTransportCredentials(transport), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	return conn
}
