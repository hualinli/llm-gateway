package controlclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeAdminService is an in-process AdminService used by every test here. It
// records what arrived so the tests can assert on the exact wire contract
// (operation id, actor triple, tri-state allowlist) without a database.
type fakeAdminService struct {
	pb.UnimplementedAdminServiceServer

	mu       sync.Mutex
	requests []any

	createFn func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error)
	revokeFn func(*pb.RevokeApiKeyRequest) (*pb.RevokeApiKeyResponse, error)
	listFn   func(*pb.ListApiKeysRequest) (*pb.ListApiKeysResponse, error)
	getOpFn  func(*pb.GetOperationRequest) (*pb.GetOperationResponse, error)
	applyFn  func(*pb.ApplyOrgPolicyRequest) (*pb.ApplyOrgPolicyResponse, error)
}

func (f *fakeAdminService) note(request any) {
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
}

func (f *fakeAdminService) last(t *testing.T) any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request reached the fake AdminService")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeAdminService) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAdminService) CreateApiKey(_ context.Context, req *pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
	f.note(req)
	if f.createFn != nil {
		return f.createFn(req)
	}
	return nil, status.Error(codes.Unimplemented, "create not configured")
}

func (f *fakeAdminService) RevokeApiKey(_ context.Context, req *pb.RevokeApiKeyRequest) (*pb.RevokeApiKeyResponse, error) {
	f.note(req)
	if f.revokeFn != nil {
		return f.revokeFn(req)
	}
	return nil, status.Error(codes.Unimplemented, "revoke not configured")
}

func (f *fakeAdminService) ListApiKeys(_ context.Context, req *pb.ListApiKeysRequest) (*pb.ListApiKeysResponse, error) {
	f.note(req)
	if f.listFn != nil {
		return f.listFn(req)
	}
	return nil, status.Error(codes.Unimplemented, "list not configured")
}

func (f *fakeAdminService) GetOperation(_ context.Context, req *pb.GetOperationRequest) (*pb.GetOperationResponse, error) {
	f.note(req)
	if f.getOpFn != nil {
		return f.getOpFn(req)
	}
	return nil, status.Error(codes.Unimplemented, "get operation not configured")
}

func (f *fakeAdminService) ApplyOrgPolicy(_ context.Context, req *pb.ApplyOrgPolicyRequest) (*pb.ApplyOrgPolicyResponse, error) {
	f.note(req)
	if f.applyFn != nil {
		return f.applyFn(req)
	}
	return nil, status.Error(codes.Unimplemented, "apply not configured")
}

type fakeServer struct {
	service *fakeAdminService
	addr    string
	stop    func()
}

// startFakeServer runs the fake AdminService on a real local listener so the
// client exercises the real gRPC transport, codec and status handling.
func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	return startFakeServerWithOptions(t, grpc.NewServer(), &fakeAdminService{})
}

func startFakeServerWithOptions(t *testing.T, server *grpc.Server, service *fakeAdminService) *fakeServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	pb.RegisterAdminServiceServer(server, service)
	go func() { _ = server.Serve(listener) }()
	fs := &fakeServer{service: service, addr: listener.Addr().String(), stop: func() { server.Stop() }}
	t.Cleanup(fs.stop)
	return fs
}

func dialFake(t *testing.T, fs *fakeServer, opts Options) *Client {
	t.Helper()
	client, err := Dial(fs.addr, opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func int64Ptr(value int64) *int64 { return &value }

// --- happy paths ------------------------------------------------------------

func TestCreateApiKeyReturnsSecretOnceAndMapsMetadata(t *testing.T) {
	fs := startFakeServer(t)
	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	models := []string{"gpt-4o"}
	owner := int64(42)
	fs.service.createFn = func(req *pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
		return &pb.CreateApiKeyResponse{
			Key: &pb.ApiKeyMetadata{
				KeyId:          "key_1",
				Name:           "production",
				MembershipId:   77,
				KeyPrefix:      "sk-abc12...",
				OrgId:          7,
				OwnerUserId:    &owner,
				CreatedBy:      proto.Int64(9),
				ExpiresAt:      timestamppb.New(expires),
				ModelAllowlist: &pb.ModelAllowlist{Models: models},
				Status:         "active",
				PolicyVersion:  3,
				CreatedAt:      timestamppb.New(created),
				TenantId:       "org:7",
			},
			Secret:           "sk-super-secret-value",
			ResultingVersion: 12,
		}, nil
	}

	client := dialFake(t, fs, Options{})
	result, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{
		OperationID:    "key-create-1",
		ActorUserID:    42,
		ActorOrgID:     7,
		RequestID:      "req-1",
		OrgID:          7,
		MembershipID:   77,
		Name:           "production",
		OwnerUserID:    &owner,
		ExpiresAt:      &expires,
		ModelAllowlist: &models,
	})
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	if result.Secret != "sk-super-secret-value" || result.Replayed {
		t.Fatalf("secret=%q replayed=%t, want the fresh secret once", result.Secret, result.Replayed)
	}
	if result.ResultingVersion != 12 || result.Key.KeyID != "key_1" || result.Key.Status != "active" {
		t.Fatalf("unexpected result %+v", result)
	}
	if result.Key.Name != "production" || result.Key.MembershipID != 77 {
		t.Fatalf("name/membership metadata = %q/%d, want production/77", result.Key.Name, result.Key.MembershipID)
	}
	if result.Key.OwnerUserID == nil || *result.Key.OwnerUserID != 42 {
		t.Fatalf("owner=%v, want 42", result.Key.OwnerUserID)
	}
	if result.Key.ExpiresAt == nil || !result.Key.ExpiresAt.Equal(expires) {
		t.Fatalf("expires_at=%v, want %v", result.Key.ExpiresAt, expires)
	}
	if result.Key.ModelAllowlist == nil || len(*result.Key.ModelAllowlist) != 1 || (*result.Key.ModelAllowlist)[0] != "gpt-4o" {
		t.Fatalf("allowlist=%v, want [gpt-4o]", result.Key.ModelAllowlist)
	}
	if !result.Key.CreatedAt.Equal(created) || result.Key.TenantID != "org:7" {
		t.Fatalf("created_at/tenant projection wrong: %+v", result.Key)
	}

	// The wire request carries the operation id, the actor triple and the
	// request id Control records in its audit row.
	request, ok := fs.service.last(t).(*pb.CreateApiKeyRequest)
	if !ok {
		t.Fatalf("last request type %T", fs.service.last(t))
	}
	if request.GetOperationId() != "key-create-1" || request.GetActorUserId() != 42 ||
		request.GetActorOrgId() != 7 || request.GetRequestId() != "req-1" || request.GetOrgId() != 7 ||
		request.GetMembershipId() != 77 || request.GetName() != "production" {
		t.Fatalf("wire request lost the admin header: %+v", request)
	}
}

func TestCreateApiKeyReplayCarriesNoSecret(t *testing.T) {
	fs := startFakeServer(t)
	fs.service.createFn = func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
		return &pb.CreateApiKeyResponse{
			Key:              &pb.ApiKeyMetadata{KeyId: "key_1", OrgId: 7, Status: "active"},
			ResultingVersion: 4,
			Replayed:         true,
			// Control never re-sends a secret on replay.
		}, nil
	}
	client := dialFake(t, fs, Options{})
	result, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{OperationID: "op", OrgID: 7, ActorOrgID: 7})
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	if !result.Replayed || result.Secret != "" {
		t.Fatalf("replayed=%t secret=%q, want a replay without a secret", result.Replayed, result.Secret)
	}
	if result.Key.KeyID != "key_1" {
		t.Fatalf("replay lost the stored metadata: %+v", result.Key)
	}
}

func TestAllowlistTriStateSurvivesTheWireInBothDirections(t *testing.T) {
	fs := startFakeServer(t)
	fs.service.listFn = func(*pb.ListApiKeysRequest) (*pb.ListApiKeysResponse, error) {
		return &pb.ListApiKeysResponse{Keys: []*pb.ApiKeyMetadata{
			{KeyId: "inherit", ModelAllowlist: nil},
			{KeyId: "deny-all", ModelAllowlist: &pb.ModelAllowlist{}},
			{KeyId: "explicit", ModelAllowlist: &pb.ModelAllowlist{Models: []string{"a"}}},
		}}, nil
	}
	client := dialFake(t, fs, Options{})
	result, err := client.ListApiKeys(context.Background(), ListApiKeysRequest{OrgID: 7, PageSize: 100})
	if err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	if len(result.Keys) != 3 {
		t.Fatalf("keys=%d, want 3", len(result.Keys))
	}
	if result.Keys[0].ModelAllowlist != nil {
		t.Fatalf("NULL allowlist must stay nil (inherit), got %v", *result.Keys[0].ModelAllowlist)
	}
	if result.Keys[1].ModelAllowlist == nil || len(*result.Keys[1].ModelAllowlist) != 0 {
		t.Fatalf("[] allowlist must stay a present empty set (deny-all), got %v", result.Keys[1].ModelAllowlist)
	}
	if result.Keys[2].ModelAllowlist == nil || len(*result.Keys[2].ModelAllowlist) != 1 {
		t.Fatalf("explicit allowlist lost: %v", result.Keys[2].ModelAllowlist)
	}

	// Outbound: nil -> absent message, &[] -> present empty message.
	denyAll := []string{}
	if _, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{OperationID: "op-nil", OrgID: 7, ModelAllowlist: nil}); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CreateApiKey(nil): %v", err)
	}
	if _, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{OperationID: "op-empty", OrgID: 7, ModelAllowlist: &denyAll}); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CreateApiKey(empty): %v", err)
	}
	fs.service.mu.Lock()
	var creates []*pb.CreateApiKeyRequest
	for _, request := range fs.service.requests {
		if create, ok := request.(*pb.CreateApiKeyRequest); ok {
			creates = append(creates, create)
		}
	}
	fs.service.mu.Unlock()
	if len(creates) != 2 {
		t.Fatalf("create requests=%d, want 2", len(creates))
	}
	if creates[0].GetModelAllowlist() != nil {
		t.Fatal("nil allowlist must be sent as an absent message")
	}
	empty := creates[1].GetModelAllowlist()
	if empty == nil || len(empty.GetModels()) != 0 {
		t.Fatalf("empty allowlist must be sent as a present empty message, got %v", empty)
	}
}

func TestGetOperationRecoversMetadataWithoutSecret(t *testing.T) {
	fs := startFakeServer(t)
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fs.service.getOpFn = func(req *pb.GetOperationRequest) (*pb.GetOperationResponse, error) {
		if req.GetOperationId() != "lost-op" {
			return nil, status.Error(codes.InvalidArgument, "wrong operation")
		}
		return &pb.GetOperationResponse{
			OperationId: "lost-op",
			Kind:        "create_api_key",
			RequestId:   "req-9",
			CreatedAt:   timestamppb.New(created),
			Result: &pb.OperationResult{
				Kind:             "create_api_key",
				Key:              &pb.ApiKeyMetadata{KeyId: "key_9", Name: "production", MembershipId: 77, OrgId: 7, Status: "active"},
				ResultingVersion: 21,
			},
		}, nil
	}
	client := dialFake(t, fs, Options{})
	result, err := client.GetOperation(context.Background(), GetOperationRequest{OperationID: "lost-op", ActorUserID: 42, ActorOrgID: 7, MembershipID: 77})
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if result.Kind != "create_api_key" || result.Key == nil || result.Key.KeyID != "key_9" {
		t.Fatalf("unexpected operation result %+v", result)
	}
	if result.ResultingVersion != 21 || !result.CreatedAt.Equal(created) {
		t.Fatalf("unexpected versions/time %+v", result)
	}
	if result.Key.Name != "production" || result.Key.MembershipID != 77 {
		t.Fatalf("recovered name/membership metadata = %q/%d, want production/77", result.Key.Name, result.Key.MembershipID)
	}
	request := fs.service.last(t).(*pb.GetOperationRequest)
	if request.GetActorUserId() != 42 || request.GetActorOrgId() != 7 || request.GetMembershipId() != 77 {
		t.Fatalf("GetOperation request lost actor membership binding: %+v", request)
	}
}

func TestApplyOrgPolicySendsTheCompleteReplacement(t *testing.T) {
	fs := startFakeServer(t)
	rpm := int32(600)
	burst := int32(60)
	fs.service.applyFn = func(req *pb.ApplyOrgPolicyRequest) (*pb.ApplyOrgPolicyResponse, error) {
		return &pb.ApplyOrgPolicyResponse{
			Policy: &pb.OrgPolicy{
				OrgId:             req.GetOrgId(),
				ModelAllowlist:    req.GetModelAllowlist(),
				RequestsPerMinute: req.RequestsPerMinute,
				Burst:             req.Burst,
				PolicyVersion:     5,
			},
			ResultingVersion: 33,
		}, nil
	}
	client := dialFake(t, fs, Options{})
	result, err := client.ApplyOrgPolicy(context.Background(), ApplyOrgPolicyRequest{
		OperationID:       "policy:7:4:org:",
		ActorOrgID:        7,
		OrgID:             7,
		RequestsPerMinute: &rpm,
		Burst:             &burst,
	})
	if err != nil {
		t.Fatalf("ApplyOrgPolicy: %v", err)
	}
	if result.ResultingVersion != 33 || result.Policy.PolicyVersion != 5 {
		t.Fatalf("unexpected result %+v", result)
	}
	if result.Policy.RequestsPerMinute == nil || *result.Policy.RequestsPerMinute != 600 {
		t.Fatalf("rpm lost: %+v", result.Policy)
	}
	request := fs.service.last(t).(*pb.ApplyOrgPolicyRequest)
	if request.GetModelAllowlist() != nil {
		t.Fatal("a full replacement with no allowlist must send an absent message (NULL = inherit)")
	}
	if request.GetOperationId() != "policy:7:4:org:" || request.GetActorUserId() != 0 {
		t.Fatalf("publication must be a service-scope actor: %+v", request)
	}
}

// --- error mapping ----------------------------------------------------------

func TestErrorMappingFromGRPCCodes(t *testing.T) {
	fs := startFakeServer(t)
	client := dialFake(t, fs, Options{})

	cases := []struct {
		name string
		code codes.Code
		want error
	}{
		{"already exists", codes.AlreadyExists, ErrConflict},
		{"failed precondition", codes.FailedPrecondition, ErrPreconditionFailed},
		{"unauthenticated", codes.Unauthenticated, ErrUpstreamAuth},
		{"permission denied", codes.PermissionDenied, ErrUpstreamAuth},
		{"not found", codes.NotFound, ErrNotFound},
		{"invalid argument", codes.InvalidArgument, ErrInvalidArgument},
		{"unavailable", codes.Unavailable, ErrUnavailable},
		{"internal", codes.Internal, ErrUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fs.service.createFn = func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
				return nil, status.Error(testCase.code, "control said no")
			}
			_, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{OperationID: "op-" + testCase.name, OrgID: 7})
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error=%v, want errors.Is(%v)", err, testCase.want)
			}
			if !strings.Contains(err.Error(), "control said no") {
				t.Fatalf("error %q dropped the bounded server message", err)
			}
		})
	}
}

func TestFailedPreconditionCarriesTheCurrentVersion(t *testing.T) {
	fs := startFakeServer(t)
	client := dialFake(t, fs, Options{})
	expected := int64(11)
	fs.service.createFn = func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
		return nil, status.Errorf(codes.FailedPrecondition, "expected version %d, current 17; retry with expected_version=17", expected)
	}
	_, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{
		OperationID: "op", OrgID: 7, ExpectedVersion: &expected,
	})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("error=%v, want ErrPreconditionFailed", err)
	}
	version, ok := CurrentVersion(err)
	if !ok || version != 17 {
		t.Fatalf("current version=%d ok=%t, want 17 true", version, ok)
	}
	// A precondition failure without a version suffix stays recognizable.
	fs.service.createFn = func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, "version conflict")
	}
	_, err = client.CreateApiKey(context.Background(), CreateApiKeyRequest{OperationID: "op-2", OrgID: 7})
	if !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("error=%v, want ErrPreconditionFailed", err)
	}
	if _, ok := CurrentVersion(err); ok {
		t.Fatal("CurrentVersion reported a version for a message that carried none")
	}
}

// --- deadline and retry -----------------------------------------------------

func TestDeadlineBoundsOneAttempt(t *testing.T) {
	fs := startFakeServer(t)
	fs.service.createFn = func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
		time.Sleep(300 * time.Millisecond)
		return &pb.CreateApiKeyResponse{}, nil
	}
	client := dialFake(t, fs, Options{CallTimeout: 30 * time.Millisecond, MaxAttempts: 1})
	started := time.Now()
	_, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{OperationID: "slow", OrgID: 7})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error=%v, want ErrUnavailable after the deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("call took %s despite a 30ms deadline", elapsed)
	}
}

func TestRetryOnlyForTransportFailures(t *testing.T) {
	t.Run("transport failure is retried", func(t *testing.T) {
		fs := startFakeServer(t)
		var attempts int
		var mu sync.Mutex
		fs.service.createFn = func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
			mu.Lock()
			attempts++
			current := attempts
			mu.Unlock()
			if current == 1 {
				return nil, status.Error(codes.Unavailable, "control restarting")
			}
			return &pb.CreateApiKeyResponse{Key: &pb.ApiKeyMetadata{KeyId: "key_1"}, Secret: "sk-once"}, nil
		}
		client := dialFake(t, fs, Options{MaxAttempts: 2, RetryBackoff: time.Millisecond})
		result, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{OperationID: "op-retry", OrgID: 7})
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}
		if attempts != 2 {
			t.Fatalf("attempts=%d, want 2", attempts)
		}
		if result.Secret != "sk-once" {
			t.Fatalf("secret=%q, want the retried success", result.Secret)
		}
	})

	t.Run("business rejection is final", func(t *testing.T) {
		fs := startFakeServer(t)
		fs.service.createFn = func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
			return nil, status.Error(codes.AlreadyExists, "operation id already exists with a different payload")
		}
		client := dialFake(t, fs, Options{MaxAttempts: 3, RetryBackoff: time.Millisecond})
		if _, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{OperationID: "op-conflict", OrgID: 7}); !errors.Is(err, ErrConflict) {
			t.Fatalf("error=%v, want ErrConflict", err)
		}
		if calls := fs.service.calls(); calls != 1 {
			t.Fatalf("attempts=%d, want 1 (a conflict must not be retried)", calls)
		}
	})

	t.Run("reads are retried too", func(t *testing.T) {
		fs := startFakeServer(t)
		var attempts int
		var mu sync.Mutex
		fs.service.listFn = func(*pb.ListApiKeysRequest) (*pb.ListApiKeysResponse, error) {
			mu.Lock()
			attempts++
			current := attempts
			mu.Unlock()
			if current == 1 {
				return nil, status.Error(codes.Unavailable, "not ready")
			}
			return &pb.ListApiKeysResponse{Keys: []*pb.ApiKeyMetadata{{KeyId: "key_1"}}}, nil
		}
		client := dialFake(t, fs, Options{MaxAttempts: 2, RetryBackoff: time.Millisecond})
		result, err := client.ListApiKeys(context.Background(), ListApiKeysRequest{OrgID: 7})
		if err != nil {
			t.Fatalf("ListApiKeys: %v", err)
		}
		if attempts != 2 || len(result.Keys) != 1 {
			t.Fatalf("attempts=%d keys=%d, want 2 and 1", attempts, len(result.Keys))
		}
	})

	t.Run("a canceled caller is not retried", func(t *testing.T) {
		fs := startFakeServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := dialFake(t, fs, Options{MaxAttempts: 3, RetryBackoff: time.Millisecond})
		if _, err := client.CreateApiKey(ctx, CreateApiKeyRequest{OperationID: "op-canceled", OrgID: 7}); err == nil {
			t.Fatal("expected an error for a canceled caller")
		}
		if calls := fs.service.calls(); calls != 0 {
			t.Fatalf("attempts=%d, want 0 for an already-canceled context", calls)
		}
	})
}

func TestListApiKeysGeneratesAnOperationIDForReads(t *testing.T) {
	fs := startFakeServer(t)
	fs.service.listFn = func(*pb.ListApiKeysRequest) (*pb.ListApiKeysResponse, error) {
		return &pb.ListApiKeysResponse{}, nil
	}
	client := dialFake(t, fs, Options{})
	if _, err := client.ListApiKeys(context.Background(), ListApiKeysRequest{OrgID: 7}); err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	request := fs.service.last(t).(*pb.ListApiKeysRequest)
	if !strings.HasPrefix(request.GetOperationId(), "list-keys-") {
		t.Fatalf("operation_id=%q, want a generated read id", request.GetOperationId())
	}
	if _, err := client.ListApiKeys(context.Background(), ListApiKeysRequest{OrgID: 7, OperationID: "req-7"}); err != nil {
		t.Fatalf("ListApiKeys: %v", err)
	}
	if got := fs.service.last(t).(*pb.ListApiKeysRequest).GetOperationId(); got != "req-7" {
		t.Fatalf("operation_id=%q, want the caller supplied req-7", got)
	}
}

// --- secret hygiene ---------------------------------------------------------

func TestSecretNeverReachesTheLog(t *testing.T) {
	fs := startFakeServer(t)
	const secret = "sk-do-not-log-me-0123456789"
	fs.service.createFn = func(*pb.CreateApiKeyRequest) (*pb.CreateApiKeyResponse, error) {
		return &pb.CreateApiKeyResponse{
			Key:    &pb.ApiKeyMetadata{KeyId: "key_1", OrgId: 7, Status: "active"},
			Secret: secret,
		}, nil
	}
	var logs bytes.Buffer
	client := dialFake(t, fs, Options{Logger: log.New(&logs, "", 0)})
	result, err := client.CreateApiKey(context.Background(), CreateApiKeyRequest{
		OperationID: "op-secret", OrgID: 7, ActorUserID: 42, ActorOrgID: 7, RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	if result.Secret != secret {
		t.Fatalf("the fresh secret must be returned to the caller once")
	}
	if logs.Len() == 0 {
		t.Fatal("the client logged nothing; the log-scan assertion would be vacuous")
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("the raw secret reached the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "operation_id=op-secret") {
		t.Fatalf("the attempt log lost its correlation fields:\n%s", logs.String())
	}
}

// --- dialing and TLS --------------------------------------------------------

func TestDialRejectsAnEmptyAddress(t *testing.T) {
	if _, err := Dial("  ", Options{}); err == nil {
		t.Fatal("Dial accepted an empty control address")
	}
}

func TestTLSConfigFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantNil bool
		wantErr string
	}{
		{name: "nothing configured is the explicit insecure demo", env: nil, wantNil: true},
		{
			name:    "ca alone is allowed (server auth without a client identity)",
			env:     map[string]string{"CONTROL_TLS_CA_FILE": "ca.crt"},
			wantNil: false,
		},
		{
			name: "client certificate alone without a CA is rejected",
			env: map[string]string{
				"CONTROL_TLS_CLIENT_CERT_FILE": "c.crt",
				"CONTROL_TLS_CLIENT_KEY_FILE":  "c.key",
			},
			wantErr: "CONTROL_TLS_CA_FILE",
		},
		{
			name:    "half a client key pair is rejected",
			env:     map[string]string{"CONTROL_TLS_CA_FILE": "ca.crt", "CONTROL_TLS_CLIENT_CERT_FILE": "c.crt"},
			wantErr: "must be set together",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg, err := TLSConfigFromEnv(func(key string) string { return testCase.env[key] })
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("err=%v, want one mentioning %q", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("TLSConfigFromEnv: %v", err)
			}
			if testCase.wantNil && cfg != nil {
				t.Fatalf("cfg=%+v, want nil", cfg)
			}
			if !testCase.wantNil && cfg == nil {
				t.Fatal("cfg=nil, want a configuration")
			}
		})
	}
}

func TestClientIdentityMustMatchTheConfiguredConsoleService(t *testing.T) {
	material := newTestCertificates(t)
	cert, err := loadClientCertificate(&TLSConfig{
		ClientCertFile: material.clientCertFile,
		ClientKeyFile:  material.clientKeyFile,
	})
	if err != nil {
		t.Fatalf("loadClientCertificate: %v", err)
	}
	if err := verifyClientIdentity(cert, "console"); err != nil {
		t.Fatalf("the console identity must match its own certificate: %v", err)
	}
	if err := verifyClientIdentity(cert, "gateway-client"); err == nil {
		t.Fatal("a certificate for another service must be rejected before the first RPC")
	}
	if err := verifyClientIdentity(cert, ""); err != nil {
		t.Fatalf("an unset service name must not fail: %v", err)
	}
}

func TestMutualTLSRoundTrip(t *testing.T) {
	material := newTestCertificates(t)
	service := &fakeAdminService{}
	service.listFn = func(*pb.ListApiKeysRequest) (*pb.ListApiKeysResponse, error) {
		return &pb.ListApiKeysResponse{Keys: []*pb.ApiKeyMetadata{{KeyId: "key_mtls", OrgId: 7}}}, nil
	}

	serverTLS, err := tls.LoadX509KeyPair(material.serverCertFile, material.serverKeyFile)
	if err != nil {
		t.Fatalf("load server key pair: %v", err)
	}
	pool := x509.NewCertPool()
	caPEM, err := os.ReadFile(material.caFile)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA did not load")
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverTLS},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	})))
	fs := startFakeServerWithOptions(t, server, service)

	client := dialFake(t, fs, Options{
		TLS: &TLSConfig{
			CAFile:         material.caFile,
			ClientCertFile: material.clientCertFile,
			ClientKeyFile:  material.clientKeyFile,
			ServerName:     "control.test",
		},
		ConsoleServiceName: "console",
	})
	result, err := client.ListApiKeys(context.Background(), ListApiKeysRequest{OrgID: 7})
	if err != nil {
		t.Fatalf("mTLS ListApiKeys: %v", err)
	}
	if len(result.Keys) != 1 || result.Keys[0].KeyID != "key_mtls" {
		t.Fatalf("unexpected result %+v", result)
	}

	// A client without a certificate is refused by RequireAndVerifyClientCert.
	anonymous := dialFake(t, fs, Options{TLS: &TLSConfig{
		CAFile:     material.caFile,
		ServerName: "control.test",
	}})
	if _, err := anonymous.ListApiKeys(context.Background(), ListApiKeysRequest{OrgID: 7}); err == nil {
		t.Fatal("a client without a certificate must be refused by the mTLS server")
	}
}

// --- test certificate material ---------------------------------------------

type testCertificates struct {
	dir            string
	caFile         string
	serverCertFile string
	serverKeyFile  string
	clientCertFile string
	clientKeyFile  string
	otherCertFile  string
	otherKeyFile   string
}

// newTestCertificates writes a throwaway CA plus a control server certificate
// and two client certificates (CN=console and CN=gateway-client) into a temp
// directory. It is deterministic enough for a unit test and never touches the
// repository.
func newTestCertificates(t *testing.T) testCertificates {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "llm-gateway-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	material := testCertificates{dir: dir, caFile: filepath.Join(dir, "ca.crt")}
	writePEM(t, material.caFile, "CERTIFICATE", caDER)

	leaf := func(name string, serial int64, dns []string, client bool) (string, string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate %s key: %v", name, err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: name},
			DNSNames:     dns,
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if client {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		} else {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create %s certificate: %v", name, err)
		}
		certFile := filepath.Join(dir, name+".crt")
		keyFile := filepath.Join(dir, name+".key")
		writePEM(t, certFile, "CERTIFICATE", der)
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal %s key: %v", name, err)
		}
		writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
		return certFile, keyFile
	}

	material.serverCertFile, material.serverKeyFile = leaf("control", 2, []string{"control.test", "localhost"}, false)
	material.clientCertFile, material.clientKeyFile = leaf("console", 3, nil, true)
	material.otherCertFile, material.otherKeyFile = leaf("gateway-client", 4, nil, true)
	return material
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	buffer := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if buffer == nil {
		t.Fatalf("encode %s PEM", path)
	}
	if err := os.WriteFile(path, buffer, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
