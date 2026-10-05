package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/url"
	"strings"
	"testing"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// fakeAdminStore records the params it receives and returns canned results so
// AdminService semantics can be tested without PostgreSQL.
type fakeAdminStore struct {
	createResult *APIKeyMutation
	createErr    error
	createParams *CreateAPIKeyParams

	revokeResult *APIKeyMutation
	revokeErr    error
	revokeParams *RevokeAPIKeyParams

	listResult []*pb.ApiKeyMetadata
	listNext   string
	listErr    error
	listParams *ListAPIKeysParams

	operationResult *pb.GetOperationResponse
	operationErr    error

	policyResult *OrgPolicyMutation
	policyErr    error
	policyParams *ApplyOrgPolicyParams
}

func (f *fakeAdminStore) CreateApiKey(_ context.Context, params CreateAPIKeyParams) (*APIKeyMutation, error) {
	f.createParams = &params
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResult != nil {
		return f.createResult, nil
	}
	return &APIKeyMutation{Key: &pb.ApiKeyMetadata{KeyId: params.KeyID, OrgId: params.OrgID, Status: "active"}, ResultingVersion: 1}, nil
}

func (f *fakeAdminStore) RevokeApiKey(_ context.Context, params RevokeAPIKeyParams) (*APIKeyMutation, error) {
	f.revokeParams = &params
	if f.revokeErr != nil {
		return nil, f.revokeErr
	}
	if f.revokeResult != nil {
		return f.revokeResult, nil
	}
	return &APIKeyMutation{Key: &pb.ApiKeyMetadata{KeyId: params.KeyID, Status: "revoked"}, ResultingVersion: 2}, nil
}

func (f *fakeAdminStore) ListApiKeys(_ context.Context, params ListAPIKeysParams) ([]*pb.ApiKeyMetadata, string, error) {
	f.listParams = &params
	return f.listResult, f.listNext, f.listErr
}

func (f *fakeAdminStore) GetOperation(_ context.Context, operationID string) (*pb.GetOperationResponse, error) {
	if f.operationErr != nil {
		return nil, f.operationErr
	}
	if f.operationResult != nil {
		return f.operationResult, nil
	}
	return &pb.GetOperationResponse{OperationId: operationID, Kind: operationCreateAPIKey}, nil
}

func (f *fakeAdminStore) ApplyOrgPolicy(_ context.Context, params ApplyOrgPolicyParams) (*OrgPolicyMutation, error) {
	f.policyParams = &params
	if f.policyErr != nil {
		return nil, f.policyErr
	}
	if f.policyResult != nil {
		return f.policyResult, nil
	}
	return &OrgPolicyMutation{Policy: &pb.OrgPolicy{OrgId: params.OrgID, PolicyVersion: 1}, ResultingVersion: 3}, nil
}

func newTestAdminService(t *testing.T, store APIKeyStore, opts AdminServiceOptions) pb.AdminServiceServer {
	t.Helper()
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	service, err := NewAPIKeyAdminService(newTestServer(t), store, NewMemoryAuditSink(8), opts)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func insecureAdminOptions() AdminServiceOptions {
	return AdminServiceOptions{AllowInsecure: true, ConsoleServiceName: "console.test"}
}

func TestAdminServiceReturnsSecretOnlyOnce(t *testing.T) {
	store := &fakeAdminStore{}
	service := newTestAdminService(t, store, insecureAdminOptions())
	request := &pb.CreateApiKeyRequest{OperationId: "op-1", OrgId: 1, ActorUserId: 9, ActorOrgId: 1, RequestId: "req-1"}

	created, err := service.CreateApiKey(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.GetSecret() == "" || !strings.HasPrefix(created.GetSecret(), "sk-") {
		t.Fatalf("first create did not return a raw secret: %q", created.GetSecret())
	}
	if created.GetReplayed() {
		t.Fatal("first create reported a replay")
	}
	if store.createParams == nil || len(store.createParams.RawKey) != len(created.GetSecret()) {
		t.Fatal("store did not receive the generated raw key")
	}
	if store.createParams.OwnerUserID != nil {
		t.Fatal("absent owner_user_id must stay NULL")
	}

	// Same operation replayed: the stored metadata comes back without the
	// secret, because the secret is never persisted.
	store.createResult = &APIKeyMutation{Key: created.GetKey(), ResultingVersion: created.GetResultingVersion(), Replayed: true}
	replayed, err := service.CreateApiKey(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.GetSecret() != "" {
		t.Fatalf("replay leaked a secret: %q", replayed.GetSecret())
	}
	if !replayed.GetReplayed() || replayed.GetKey().GetKeyId() != created.GetKey().GetKeyId() {
		t.Fatalf("replay did not return the stored result: %+v", replayed)
	}
}

func TestAdminServicePayloadDigestIgnoresRequestIDAndActor(t *testing.T) {
	store := &fakeAdminStore{}
	service := newTestAdminService(t, store, insecureAdminOptions())
	first := &pb.CreateApiKeyRequest{OperationId: "op-1", OrgId: 5, RequestId: "req-1", ActorUserId: 1, ActorOrgId: 5}
	if _, err := service.CreateApiKey(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	digest := store.createParams.PayloadDigest
	second := &pb.CreateApiKeyRequest{OperationId: "op-1", OrgId: 5, RequestId: "req-2", ActorUserId: 42, ActorOrgId: 9}
	if _, err := service.CreateApiKey(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if store.createParams.PayloadDigest != digest {
		t.Fatalf("request_id/actor fields changed the payload digest: %s vs %s", digest, store.createParams.PayloadDigest)
	}
	if store.createParams.ActorScope == "" || !strings.Contains(store.createParams.ActorScope, "user=42") {
		t.Fatalf("actor scope not recorded: %q", store.createParams.ActorScope)
	}
}

func TestAdminServiceRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		call func(pb.AdminServiceServer) error
	}{
		{"missing operation id", func(s pb.AdminServiceServer) error {
			_, err := s.CreateApiKey(context.Background(), &pb.CreateApiKeyRequest{OrgId: 1})
			return err
		}},
		{"missing org", func(s pb.AdminServiceServer) error {
			_, err := s.CreateApiKey(context.Background(), &pb.CreateApiKeyRequest{OperationId: "op"})
			return err
		}},
		{"negative actor", func(s pb.AdminServiceServer) error {
			_, err := s.CreateApiKey(context.Background(), &pb.CreateApiKeyRequest{OperationId: "op", OrgId: 1, ActorUserId: -1})
			return err
		}},
		{"negative expected version", func(s pb.AdminServiceServer) error {
			expected := int64(-2)
			_, err := s.CreateApiKey(context.Background(), &pb.CreateApiKeyRequest{OperationId: "op", OrgId: 1, ExpectedVersion: &expected})
			return err
		}},
		{"zero owner", func(s pb.AdminServiceServer) error {
			owner := int64(0)
			_, err := s.CreateApiKey(context.Background(), &pb.CreateApiKeyRequest{OperationId: "op", OrgId: 1, OwnerUserId: &owner})
			return err
		}},
		{"empty allowlist entry", func(s pb.AdminServiceServer) error {
			_, err := s.CreateApiKey(context.Background(), &pb.CreateApiKeyRequest{OperationId: "op", OrgId: 1, ModelAllowlist: &pb.ModelAllowlist{Models: []string{" "}}})
			return err
		}},
		{"missing key id", func(s pb.AdminServiceServer) error {
			_, err := s.RevokeApiKey(context.Background(), &pb.RevokeApiKeyRequest{OperationId: "op"})
			return err
		}},
		{"bad cursor", func(s pb.AdminServiceServer) error {
			_, err := s.ListApiKeys(context.Background(), &pb.ListApiKeysRequest{OperationId: "op", PageToken: "not-a-cursor!"})
			return err
		}},
		{"missing policy org", func(s pb.AdminServiceServer) error {
			_, err := s.ApplyOrgPolicy(context.Background(), &pb.ApplyOrgPolicyRequest{OperationId: "op"})
			return err
		}},
		{"negative rpm", func(s pb.AdminServiceServer) error {
			rpm := int32(-1)
			_, err := s.ApplyOrgPolicy(context.Background(), &pb.ApplyOrgPolicyRequest{OperationId: "op", OrgId: 1, RequestsPerMinute: &rpm})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAdminStore{}
			err := tc.call(newTestAdminService(t, store, insecureAdminOptions()))
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
			}
			if store.createParams != nil || store.revokeParams != nil || store.policyParams != nil {
				t.Fatal("invalid request reached the store")
			}
		})
	}
}

func TestAdminServiceMapsStoreErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code codes.Code
		want string
	}{
		{"operation conflict", ErrOperationConflict, codes.AlreadyExists, "operation"},
		{"version conflict", &versionConflictError{Expected: 3, Current: 9}, codes.FailedPrecondition, "current 9"},
		{"missing key", ErrAPIKeyNotFound, codes.NotFound, "not found"},
		{"missing organization", ErrOrganizationNotFound, codes.NotFound, "not found"},
		{"missing operation", ErrOperationNotFound, codes.NotFound, "not found"},
		{"invalid argument", ErrAdminInvalidArgument, codes.InvalidArgument, "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAdminStore{createErr: tc.err}
			service := newTestAdminService(t, store, insecureAdminOptions())
			_, err := service.CreateApiKey(context.Background(), &pb.CreateApiKeyRequest{OperationId: "op", OrgId: 1})
			if status.Code(err) != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if !strings.Contains(status.Convert(err).Message(), tc.want) {
				t.Fatalf("message %q does not contain %q", status.Convert(err).Message(), tc.want)
			}
			if tc.code == codes.AlreadyExists && !strings.Contains(status.Convert(err).Message(), "op") {
				t.Fatalf("ALREADY_EXISTS message should name the operation: %q", status.Convert(err).Message())
			}
		})
	}
}

func TestAdminServiceBoundsListPage(t *testing.T) {
	store := &fakeAdminStore{}
	service := newTestAdminService(t, store, insecureAdminOptions())
	if _, err := service.ListApiKeys(context.Background(), &pb.ListApiKeysRequest{OperationId: "op"}); err != nil {
		t.Fatal(err)
	}
	if store.listParams.PageSize != defaultListPageSize {
		t.Fatalf("default page size = %d, want %d", store.listParams.PageSize, defaultListPageSize)
	}
	if _, err := service.ListApiKeys(context.Background(), &pb.ListApiKeysRequest{OperationId: "op", PageSize: 5000}); err != nil {
		t.Fatal(err)
	}
	if store.listParams.PageSize != maxListPageSize {
		t.Fatalf("oversized page size = %d, want %d", store.listParams.PageSize, maxListPageSize)
	}
	if _, err := service.ListApiKeys(context.Background(), &pb.ListApiKeysRequest{OperationId: "op", PageSize: 7}); err != nil {
		t.Fatal(err)
	}
	if store.listParams.PageSize != 7 {
		t.Fatalf("explicit page size = %d, want 7", store.listParams.PageSize)
	}
	owner := int64(4)
	if _, err := service.ListApiKeys(context.Background(), &pb.ListApiKeysRequest{OperationId: "op", OrgId: 2, OwnerUserId: &owner}); err != nil {
		t.Fatal(err)
	}
	if store.listParams.OrgID != 2 || store.listParams.OwnerUserID == nil || *store.listParams.OwnerUserID != 4 {
		t.Fatalf("list filters = %+v", store.listParams)
	}
}

func TestPageTokenRoundTrip(t *testing.T) {
	if cursor, err := decodePageToken(""); err != nil || cursor != "" {
		t.Fatalf("empty cursor = %q,%v", cursor, err)
	}
	cursor, err := decodePageToken(encodePageToken("key_abc"))
	if err != nil || cursor != "key_abc" {
		t.Fatalf("cursor round trip = %q,%v", cursor, err)
	}
	if _, err := decodePageToken("v1:key_abc"); err == nil {
		t.Fatal("a non-base64 cursor was accepted")
	}
}

func TestAdminAuthorizerRequiresConsoleIdentity(t *testing.T) {
	consoleCert := &x509.Certificate{Subject: pkix.Name{CommonName: "console.test"}, DNSNames: []string{"console.test"}}
	spiffe, err := url.Parse("spiffe://internal/console")
	if err != nil {
		t.Fatal(err)
	}
	spiffeCert := &x509.Certificate{Subject: pkix.Name{CommonName: "other"}, URIs: []*url.URL{spiffe}}
	gatewayCert := &x509.Certificate{Subject: pkix.Name{CommonName: "gateway.test"}, DNSNames: []string{"gateway.test"}}

	authorizer := &adminAuthorizer{consoleServiceName: "console.test"}
	if _, err := authorizer.identify(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("plaintext caller err = %v, want UNAUTHENTICATED", err)
	}
	if _, err := authorizer.identify(peerContext(gatewayCert)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign identity err = %v, want PERMISSION_DENIED", err)
	}
	identity, err := authorizer.identify(peerContext(consoleCert))
	if err != nil || identity.Service != "console.test" {
		t.Fatalf("console identity rejected: %v %q", err, identity.Service)
	}
	spiffeAuthorizer := &adminAuthorizer{consoleServiceName: "spiffe://internal/console"}
	if _, err := spiffeAuthorizer.identify(peerContext(spiffeCert)); err != nil {
		t.Fatalf("URI SAN identity rejected: %v", err)
	}
	insecure := &adminAuthorizer{allowInsecure: true}
	if identity, err := insecure.identify(context.Background()); err != nil || identity.Service != "insecure-local" {
		t.Fatalf("insecure demo identity = %+v err=%v", identity, err)
	}
}

func TestNewAPIKeyAdminServiceRequiresConfiguration(t *testing.T) {
	if _, err := NewAPIKeyAdminService(nil, &fakeAdminStore{}, nil, insecureAdminOptions()); err == nil {
		t.Fatal("nil server was accepted")
	}
	if _, err := NewAPIKeyAdminService(newTestServer(t), nil, nil, insecureAdminOptions()); err == nil {
		t.Fatal("nil store was accepted")
	}
	if _, err := NewAPIKeyAdminService(newTestServer(t), &fakeAdminStore{}, nil, AdminServiceOptions{}); err == nil {
		t.Fatal("missing console identity was accepted without the insecure override")
	}
	if _, err := NewAPIKeyAdminService(newTestServer(t), &fakeAdminStore{}, nil, AdminServiceOptions{ConsoleServiceName: "console.test"}); err != nil {
		t.Fatalf("mTLS console identity rejected: %v", err)
	}
}

func peerContext(certificate *x509.Certificate) context.Context {
	state := tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certificate},
		VerifiedChains:   [][]*x509.Certificate{{certificate}},
	}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
}
