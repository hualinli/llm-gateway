// Package controlclient is the Console's typed client for the internal Control
// AdminService (ADR-0003 §2.2).
//
// It exists so the HTTP layer never sees a gRPC status, a proto message or a
// raw secret it did not ask for:
//
//   - every method is typed (no *pb.* in signatures) and carries the actor
//     triple Control records in its audit row;
//   - CreateApiKey returns the raw secret exactly once, in
//     CreateApiKeyResult.Secret, and only when Control did not replay the
//     operation. Nothing in this package writes a request or response payload
//     to a log: the optional Logger receives method, operation id, gRPC code and
//     duration only;
//   - gRPC failures are mapped onto the sentinel errors in errors.go
//     (ALREADY_EXISTS -> ErrConflict, FAILED_PRECONDITION ->
//     ErrPreconditionFailed with the current version, UNAUTHENTICATED /
//     PERMISSION_DENIED -> ErrUpstreamAuth, ...) so the Console can answer the
//     frozen HTTP status codes;
//   - every call has a deadline, and a bounded retry happens only for
//     transport-level failures of calls that carry a deterministic
//     operation_id: Control replays the stored result for the same
//     (operation_id, payload digest), so a retry cannot apply an effect twice.
//
// mTLS uses the same CONTROL_TLS_* variables as the Gateway client (see
// tls.go); the Console presents CONTROL_TLS_CLIENT_CERT_FILE, whose identity
// must match the Control-side CONTROL_CONSOLE_SERVICE_NAME.
package controlclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Defaults for the client deadline and retry policy.
const (
	DefaultCallTimeout  = 5 * time.Second
	DefaultMaxAttempts  = 2
	DefaultRetryBackoff = 150 * time.Millisecond
)

// Options configures one Client.
type Options struct {
	// TLS is the Console client half of the Control transport. nil selects the
	// explicit local-demo insecure transport.
	TLS *TLSConfig
	// CallTimeout bounds one attempt. <= 0 means DefaultCallTimeout.
	CallTimeout time.Duration
	// MaxAttempts is the total number of attempts per call (1 disables
	// retries). <= 0 means DefaultMaxAttempts.
	MaxAttempts int
	// RetryBackoff is the base delay between attempts; attempt N waits
	// N*RetryBackoff. <= 0 means DefaultRetryBackoff.
	RetryBackoff time.Duration
	// ConsoleServiceName is the identity Control trusts
	// (CONTROL_CONSOLE_SERVICE_NAME). When TLS client certificates are
	// configured the certificate is checked against it before the first RPC.
	ConsoleServiceName string
	// Logger receives one line per RPC attempt. It is never given request or
	// response payloads, so it can never contain an API key secret. nil
	// disables logging.
	Logger *log.Logger
}

func (o Options) withDefaults() Options {
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = DefaultRetryBackoff
	}
	return o
}

// Client wraps one AdminService connection. It is safe for concurrent use and
// reconnects transparently after Control restarts.
type Client struct {
	conn   *grpc.ClientConn
	admin  pb.AdminServiceClient
	opts   Options
	logger *log.Logger
}

// Dial creates the client without blocking on connectivity: the Console keeps
// serving login/organization traffic while Control is down, and the publication
// worker retries with backoff. Per-call deadlines bound every RPC.
func Dial(addr string, opts Options) (*Client, error) {
	opts = opts.withDefaults()
	target := strings.TrimSpace(addr)
	if target == "" {
		return nil, errors.New("controlclient: control address is required")
	}
	creds, err := transportCredentials(opts.TLS)
	if err != nil {
		return nil, err
	}
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	if err := verifyClientCertificate(opts); err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                15 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("controlclient: dial %s: %w", target, err)
	}
	return &Client{conn: conn, admin: pb.NewAdminServiceClient(conn), opts: opts, logger: opts.Logger}, nil
}

func verifyClientCertificate(opts Options) error {
	cert, err := loadClientCertificate(opts.TLS)
	if err != nil {
		return err
	}
	return verifyClientIdentity(cert, opts.ConsoleServiceName)
}

// Close releases the connection. It is nil-safe.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// --- typed request/result types ---------------------------------------------

// KeyMetadata is the redacted projection of one api_keys row. It never contains
// the key hash or the raw secret.
type KeyMetadata struct {
	KeyID     string
	KeyPrefix string
	OrgID     int64
	// OwnerUserID is nil for a legacy/unknown owner.
	OwnerUserID *int64
	CreatedBy   *int64
	// ExpiresAt is nil when the key never expires.
	ExpiresAt *time.Time
	// ModelAllowlist is tri-state: nil inherits the parent scope (SQL NULL),
	// a non-nil pointer to an empty slice is an explicit deny-all (JSONB []),
	// and a non-empty slice is an explicit allowlist. The distinction is part
	// of the contract (ADR-0003 §2.7) and is preserved on the wire.
	ModelAllowlist *[]string
	// Status is "active" or "revoked".
	Status string
	// PolicyVersion is the policy version the key was last written with.
	PolicyVersion int64
	CreatedAt     time.Time
	// RevokedAt is nil while the key is active.
	RevokedAt *time.Time
	// TenantID is the legacy tenant projection the Gateway still resolves.
	TenantID string
}

// CreateApiKeyRequest creates one key. OwnerUserID nil stores NULL
// (legacy/unknown owner); ModelAllowlist follows the tri-state rule above.
type CreateApiKeyRequest struct {
	OperationID     string
	ExpectedVersion *int64
	ActorUserID     int64
	ActorOrgID      int64
	RequestID       string
	OrgID           int64
	OwnerUserID     *int64
	ExpiresAt       *time.Time
	ModelAllowlist  *[]string
}

// CreateApiKeyResult carries the raw secret exactly once. Replayed is true when
// Control returned a stored result for an operation_id it had already applied:
// Secret is empty then, and no API can recover it (use GetOperation).
type CreateApiKeyResult struct {
	Key              KeyMetadata
	Secret           string
	ResultingVersion int64
	Replayed         bool
}

// RevokeApiKeyRequest revokes one key. A non-zero OrgID is a scope assertion:
// a key that belongs to another organization answers NOT_FOUND.
type RevokeApiKeyRequest struct {
	OperationID     string
	ExpectedVersion *int64
	ActorUserID     int64
	ActorOrgID      int64
	RequestID       string
	KeyID           string
	OrgID           int64
}

// RevokeApiKeyResult is the redacted result of a revoke (or of its replay).
type RevokeApiKeyResult struct {
	Key              KeyMetadata
	ResultingVersion int64
	Replayed         bool
}

// ListApiKeysRequest lists redacted metadata. OrgID = 0 lists every
// organization; OwnerUserID nil lists every owner. PageSize defaults to 20 and
// is capped at 100 by Control; PageToken is the opaque cursor of the previous
// page.
type ListApiKeysRequest struct {
	OperationID     string
	ExpectedVersion *int64
	ActorUserID     int64
	ActorOrgID      int64
	RequestID       string
	OrgID           int64
	OwnerUserID     *int64
	PageSize        int
	PageToken       string
}

// ListApiKeysResult is one page of key metadata, sorted by key_id ascending.
type ListApiKeysResult struct {
	Keys          []KeyMetadata
	NextPageToken string
}

// GetOperationRequest reads back the stored result of one mutation so a Console
// that lost the response can recover metadata. The secret is never part of it.
type GetOperationRequest struct {
	OperationID     string
	ExpectedVersion *int64
	ActorUserID     int64
	ActorOrgID      int64
	RequestID       string
}

// OperationResult is the redacted, replayable result of one mutation.
type OperationResult struct {
	OperationID   string
	Kind          string
	PayloadDigest string
	ActorScope    string
	RequestID     string
	// Key is set by CreateApiKey and RevokeApiKey.
	Key              *KeyMetadata
	ResultingVersion int64
	// PolicyVersion / Policy are set by ApplyOrgPolicy.
	PolicyVersion int64
	Policy        *OrgPolicy
	CreatedAt     time.Time
}

// OrgPolicy is the complete data-plane policy of one organization.
type OrgPolicy struct {
	OrgID             int64
	ModelAllowlist    *[]string
	RequestsPerMinute *int32
	Burst             *int32
	PolicyVersion     int64
	UpdatedAt         time.Time
	UpdatedBy         int64
}

// ApplyOrgPolicyRequest replaces the organization's complete policy. Control
// treats it as a full replacement: a nil ModelAllowlist clears the allowlist to
// NULL (inherit), a non-nil empty slice is deny-all, and nil
// RequestsPerMinute/Burst clear the corresponding limit.
type ApplyOrgPolicyRequest struct {
	OperationID       string
	ExpectedVersion   *int64
	ActorUserID       int64
	ActorOrgID        int64
	RequestID         string
	OrgID             int64
	ModelAllowlist    *[]string
	RequestsPerMinute *int32
	Burst             *int32
}

// ApplyOrgPolicyResult is the effective policy after the write (or its replay).
type ApplyOrgPolicyResult struct {
	Policy           OrgPolicy
	ResultingVersion int64
	Replayed         bool
}

// ProviderTimeouts and ProviderConfig are the redacted, non-secret provider
// settings managed through Control's mTLS AdminService.
type ProviderTimeouts struct {
	ConnectTimeoutMS        int64
	TLSHandshakeTimeoutMS   int64
	ResponseHeaderTimeoutMS int64
	FirstByteTimeoutMS      int64
	IdleTimeoutMS           int64
	AttemptTimeoutMS        int64
	MaxEventBytes           int64
}

type ProviderConfig struct {
	ID            string
	Protocol      string
	URL           string
	CredentialRef string
	Model         string
	Timeouts      *ProviderTimeouts
	ModelMapping  map[string]string
	MockDelayMS   int64
}

type ProviderListResult struct {
	Items   []ProviderConfig
	Version int64
}

type ProviderMutationResult struct {
	Provider ProviderConfig
	Version  int64
}

type ProviderRouteMap map[string][]string

type ProviderRoutesResult struct {
	Routes  ProviderRouteMap
	Version int64
}

type ProviderCredentialMetadata struct {
	CredentialID  string
	CredentialRef string
	ProviderID    string
	Fingerprint   string
	Status        string
	CreatedAt     time.Time
	RevokedAt     *time.Time
}

type ProviderCredentialPage struct {
	Items []ProviderCredentialMetadata
}

// --- methods ----------------------------------------------------------------

// CreateApiKey issues one API key. The raw secret is returned exactly once; a
// replay of the same operation_id returns the stored metadata with an empty
// Secret and Replayed = true.
func (c *Client) CreateApiKey(ctx context.Context, req CreateApiKeyRequest) (CreateApiKeyResult, error) {
	request := &pb.CreateApiKeyRequest{
		OperationId:     req.OperationID,
		ExpectedVersion: req.ExpectedVersion,
		ActorUserId:     req.ActorUserID,
		ActorOrgId:      req.ActorOrgID,
		RequestId:       req.RequestID,
		OrgId:           req.OrgID,
		OwnerUserId:     req.OwnerUserID,
		ExpiresAt:       timestampOrNil(req.ExpiresAt),
		ModelAllowlist:  allowlistToProto(req.ModelAllowlist),
	}
	var response *pb.CreateApiKeyResponse
	if err := c.invoke(ctx, "CreateApiKey", req.OperationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.CreateApiKey(callCtx, request)
		return callErr
	}); err != nil {
		return CreateApiKeyResult{}, err
	}
	return CreateApiKeyResult{
		Key:              keyMetadataFromProto(response.GetKey()),
		Secret:           response.GetSecret(),
		ResultingVersion: response.GetResultingVersion(),
		Replayed:         response.GetReplayed(),
	}, nil
}

// RevokeApiKey revokes one key inside the asserted organization scope.
func (c *Client) RevokeApiKey(ctx context.Context, req RevokeApiKeyRequest) (RevokeApiKeyResult, error) {
	request := &pb.RevokeApiKeyRequest{
		OperationId:     req.OperationID,
		ExpectedVersion: req.ExpectedVersion,
		ActorUserId:     req.ActorUserID,
		ActorOrgId:      req.ActorOrgID,
		RequestId:       req.RequestID,
		KeyId:           req.KeyID,
		OrgId:           req.OrgID,
	}
	var response *pb.RevokeApiKeyResponse
	if err := c.invoke(ctx, "RevokeApiKey", req.OperationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.RevokeApiKey(callCtx, request)
		return callErr
	}); err != nil {
		return RevokeApiKeyResult{}, err
	}
	return RevokeApiKeyResult{
		Key:              keyMetadataFromProto(response.GetKey()),
		ResultingVersion: response.GetResultingVersion(),
		Replayed:         response.GetReplayed(),
	}, nil
}

// ListApiKeys lists redacted key metadata. It is a read: Control records no
// operation row, but the RPC still requires a non-empty operation_id, which is
// generated when the caller does not supply one.
func (c *Client) ListApiKeys(ctx context.Context, req ListApiKeysRequest) (ListApiKeysResult, error) {
	operationID := strings.TrimSpace(req.OperationID)
	if operationID == "" {
		generated, err := newOperationID("list-keys")
		if err != nil {
			return ListApiKeysResult{}, err
		}
		operationID = generated
	}
	request := &pb.ListApiKeysRequest{
		OperationId:     operationID,
		ExpectedVersion: req.ExpectedVersion,
		ActorUserId:     req.ActorUserID,
		ActorOrgId:      req.ActorOrgID,
		RequestId:       req.RequestID,
		OrgId:           req.OrgID,
		OwnerUserId:     req.OwnerUserID,
		PageSize:        int32(req.PageSize),
		PageToken:       req.PageToken,
	}
	var response *pb.ListApiKeysResponse
	if err := c.invoke(ctx, "ListApiKeys", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.ListApiKeys(callCtx, request)
		return callErr
	}); err != nil {
		return ListApiKeysResult{}, err
	}
	keys := make([]KeyMetadata, 0, len(response.GetKeys()))
	for _, key := range response.GetKeys() {
		keys = append(keys, keyMetadataFromProto(key))
	}
	return ListApiKeysResult{Keys: keys, NextPageToken: response.GetNextPageToken()}, nil
}

// GetOperation reads back the stored result of one mutation. The raw secret is
// never part of the stored result.
func (c *Client) GetOperation(ctx context.Context, req GetOperationRequest) (OperationResult, error) {
	request := &pb.GetOperationRequest{
		OperationId:     req.OperationID,
		ExpectedVersion: req.ExpectedVersion,
		ActorUserId:     req.ActorUserID,
		ActorOrgId:      req.ActorOrgID,
		RequestId:       req.RequestID,
	}
	var response *pb.GetOperationResponse
	if err := c.invoke(ctx, "GetOperation", req.OperationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.GetOperation(callCtx, request)
		return callErr
	}); err != nil {
		return OperationResult{}, err
	}
	return operationResultFromProto(response), nil
}

// ApplyOrgPolicy replaces the complete organization policy.
func (c *Client) ApplyOrgPolicy(ctx context.Context, req ApplyOrgPolicyRequest) (ApplyOrgPolicyResult, error) {
	request := &pb.ApplyOrgPolicyRequest{
		OperationId:       req.OperationID,
		ExpectedVersion:   req.ExpectedVersion,
		ActorUserId:       req.ActorUserID,
		ActorOrgId:        req.ActorOrgID,
		RequestId:         req.RequestID,
		OrgId:             req.OrgID,
		ModelAllowlist:    allowlistToProto(req.ModelAllowlist),
		RequestsPerMinute: req.RequestsPerMinute,
		Burst:             req.Burst,
	}
	var response *pb.ApplyOrgPolicyResponse
	if err := c.invoke(ctx, "ApplyOrgPolicy", req.OperationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.ApplyOrgPolicy(callCtx, request)
		return callErr
	}); err != nil {
		return ApplyOrgPolicyResult{}, err
	}
	return ApplyOrgPolicyResult{
		Policy:           orgPolicyFromProto(response.GetPolicy()),
		ResultingVersion: response.GetResultingVersion(),
		Replayed:         response.GetReplayed(),
	}, nil
}

// ListProviders reads the current redacted provider projection.
func (c *Client) ListProviders(ctx context.Context, actorUserID, actorOrgID int64, requestID string) (ProviderListResult, error) {
	operationID, err := generatedOperationID("list-providers")
	if err != nil {
		return ProviderListResult{}, err
	}
	request := &pb.ListProvidersRequest{OperationId: operationID, ActorUserId: actorUserID, ActorOrgId: actorOrgID, RequestId: requestID}
	var response *pb.ListProvidersResponse
	if err := c.invoke(ctx, "ListProviders", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.ListProviders(callCtx, request)
		return callErr
	}); err != nil {
		return ProviderListResult{}, err
	}
	items := make([]ProviderConfig, 0, len(response.GetProviders()))
	for _, provider := range response.GetProviders() {
		items = append(items, providerConfigFromProto(provider))
	}
	return ProviderListResult{Items: items, Version: response.GetVersion()}, nil
}

// UpsertProvider sends one provider patch. New provider mutations deliberately
// use one RPC attempt: unlike API-key operations, Control does not persist an
// operation result for these commands, so retrying after a lost reply could
// apply a second version increment or create a duplicate credential.
func (c *Client) UpsertProvider(ctx context.Context, actorUserID, actorOrgID int64, requestID string, provider ProviderConfig, createOnly bool) (ProviderMutationResult, error) {
	operationID, err := generatedOperationID("provider-upsert")
	if err != nil {
		return ProviderMutationResult{}, err
	}
	providerProto, err := providerConfigToProto(provider)
	if err != nil {
		return ProviderMutationResult{}, err
	}
	request := &pb.UpsertProviderRequest{OperationId: operationID, ActorUserId: actorUserID, ActorOrgId: actorOrgID, RequestId: requestID, Provider: providerProto, CreateOnly: createOnly}
	var response *pb.UpsertProviderResponse
	if err := c.invokeOnce(ctx, "UpsertProvider", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.UpsertProvider(callCtx, request)
		return callErr
	}); err != nil {
		return ProviderMutationResult{}, err
	}
	return ProviderMutationResult{Provider: providerConfigFromProto(response.GetProvider()), Version: response.GetResultingVersion()}, nil
}

func (c *Client) DeleteProvider(ctx context.Context, actorUserID, actorOrgID int64, requestID, providerID string) (int64, error) {
	operationID, err := generatedOperationID("provider-delete")
	if err != nil {
		return 0, err
	}
	request := &pb.DeleteProviderRequest{OperationId: operationID, ActorUserId: actorUserID, ActorOrgId: actorOrgID, RequestId: requestID, ProviderId: providerID}
	var response *pb.DeleteProviderResponse
	if err := c.invokeOnce(ctx, "DeleteProvider", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.DeleteProvider(callCtx, request)
		return callErr
	}); err != nil {
		return 0, err
	}
	return response.GetResultingVersion(), nil
}

func (c *Client) GetRoutes(ctx context.Context, actorUserID, actorOrgID int64, requestID string) (ProviderRoutesResult, error) {
	operationID, err := generatedOperationID("get-routes")
	if err != nil {
		return ProviderRoutesResult{}, err
	}
	request := &pb.GetRoutesRequest{OperationId: operationID, ActorUserId: actorUserID, ActorOrgId: actorOrgID, RequestId: requestID}
	var response *pb.GetRoutesResponse
	if err := c.invoke(ctx, "GetRoutes", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.GetRoutes(callCtx, request)
		return callErr
	}); err != nil {
		return ProviderRoutesResult{}, err
	}
	return ProviderRoutesResult{Routes: providerRoutesFromProto(response.GetRoutes()), Version: response.GetVersion()}, nil
}

func (c *Client) ReplaceRoutes(ctx context.Context, actorUserID, actorOrgID int64, requestID string, routes ProviderRouteMap, expectedVersion int64) (ProviderRoutesResult, error) {
	operationID, err := generatedOperationID("replace-routes")
	if err != nil {
		return ProviderRoutesResult{}, err
	}
	request := &pb.ReplaceRoutesRequest{OperationId: operationID, ActorUserId: actorUserID, ActorOrgId: actorOrgID, RequestId: requestID, Routes: providerRoutesToProto(routes), ExpectedVersion: expectedVersion}
	var response *pb.ReplaceRoutesResponse
	if err := c.invokeOnce(ctx, "ReplaceRoutes", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.ReplaceRoutes(callCtx, request)
		return callErr
	}); err != nil {
		return ProviderRoutesResult{}, err
	}
	return ProviderRoutesResult{Routes: providerRoutesFromProto(response.GetRoutes()), Version: response.GetResultingVersion()}, nil
}

func (c *Client) ListProviderCredentials(ctx context.Context, actorUserID, actorOrgID int64, requestID, providerID string) (ProviderCredentialPage, error) {
	operationID, err := generatedOperationID("list-provider-credentials")
	if err != nil {
		return ProviderCredentialPage{}, err
	}
	request := &pb.ListProviderCredentialsRequest{OperationId: operationID, ActorUserId: actorUserID, ActorOrgId: actorOrgID, RequestId: requestID, ProviderId: providerID}
	var response *pb.ListProviderCredentialsResponse
	if err := c.invoke(ctx, "ListProviderCredentials", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.ListProviderCredentials(callCtx, request)
		return callErr
	}); err != nil {
		return ProviderCredentialPage{}, err
	}
	items := make([]ProviderCredentialMetadata, 0, len(response.GetItems()))
	for _, item := range response.GetItems() {
		items = append(items, providerCredentialMetadataFromProto(item))
	}
	return ProviderCredentialPage{Items: items}, nil
}

func (c *Client) CreateProviderCredential(ctx context.Context, actorUserID, actorOrgID int64, requestID, providerID string, secret []byte) (ProviderCredentialMetadata, error) {
	operationID, err := generatedOperationID("provider-credential-create")
	if err != nil {
		return ProviderCredentialMetadata{}, err
	}
	secretCopy := append([]byte(nil), secret...)
	request := &pb.CreateProviderCredentialRequest{OperationId: operationID, ActorUserId: actorUserID, ActorOrgId: actorOrgID, RequestId: requestID, ProviderId: providerID, Secret: secretCopy}
	defer wipeBytes(request.Secret)
	var response *pb.CreateProviderCredentialResponse
	if err := c.invokeOnce(ctx, "CreateProviderCredential", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.CreateProviderCredential(callCtx, request)
		return callErr
	}); err != nil {
		return ProviderCredentialMetadata{}, err
	}
	return providerCredentialMetadataFromProto(response.GetCredential()), nil
}

func (c *Client) RevokeProviderCredential(ctx context.Context, actorUserID, actorOrgID int64, requestID, credentialID string) (ProviderCredentialMetadata, error) {
	operationID, err := generatedOperationID("provider-credential-revoke")
	if err != nil {
		return ProviderCredentialMetadata{}, err
	}
	request := &pb.RevokeProviderCredentialRequest{OperationId: operationID, ActorUserId: actorUserID, ActorOrgId: actorOrgID, RequestId: requestID, CredentialId: credentialID}
	var response *pb.RevokeProviderCredentialResponse
	if err := c.invokeOnce(ctx, "RevokeProviderCredential", operationID, func(callCtx context.Context) error {
		var callErr error
		response, callErr = c.admin.RevokeProviderCredential(callCtx, request)
		return callErr
	}); err != nil {
		return ProviderCredentialMetadata{}, err
	}
	return providerCredentialMetadataFromProto(response.GetCredential()), nil
}

// --- call plumbing ----------------------------------------------------------

func generatedOperationID(prefix string) (string, error) { return newOperationID(prefix) }

func (c *Client) invokeOnce(ctx context.Context, method, operationID string, call func(context.Context) error) error {
	if c == nil || c.admin == nil {
		return fmt.Errorf("%w: control client is not configured", ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := c.opts.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	err := call(callCtx)
	code := status.Code(err)
	c.logAttempt(method, operationID, 1, code, time.Since(started))
	return mapError(err)
}

// invoke runs one RPC under a deadline. It retries only transport-level
// failures, and only while the caller's context is still live.
func (c *Client) invoke(ctx context.Context, method, operationID string, call func(context.Context) error) error {
	if c == nil || c.admin == nil {
		return fmt.Errorf("%w: control client is not configured", ErrUnavailable)
	}
	attempts := c.opts.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return mapError(lastErr)
			}
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, c.opts.CallTimeout)
		started := time.Now()
		callErr := call(callCtx)
		cancel()
		if callErr == nil {
			c.logAttempt(method, operationID, attempt, codes.OK, time.Since(started))
			return nil
		}
		code := status.Code(callErr)
		c.logAttempt(method, operationID, attempt, code, time.Since(started))
		lastErr = callErr
		if !retryableCode(code) || attempt == attempts || ctx.Err() != nil {
			break
		}
		wait := time.Duration(attempt) * c.opts.RetryBackoff
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return mapError(lastErr)
		case <-timer.C:
		}
	}
	return mapError(lastErr)
}

// logAttempt writes one bounded line per attempt. It deliberately has no
// access to the request or the response: method, operation id, attempt, code
// and duration are the whole record, so an API key secret can never reach a log
// through this package.
func (c *Client) logAttempt(method, operationID string, attempt int, code codes.Code, elapsed time.Duration) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.Printf("controlclient method=%s operation_id=%s attempt=%d code=%s duration=%s",
		method, operationID, attempt, code.String(), elapsed.Round(time.Millisecond))
}

// newOperationID returns a random, length-bounded idempotency key. Control
// caps operation_id at 200 bytes and rejects control characters.
func newOperationID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("controlclient: generate operation id: %w", err)
	}
	return prefix + "-" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// --- proto conversion -------------------------------------------------------

func keyMetadataFromProto(key *pb.ApiKeyMetadata) KeyMetadata {
	if key == nil {
		return KeyMetadata{}
	}
	metadata := KeyMetadata{
		KeyID:          key.GetKeyId(),
		KeyPrefix:      key.GetKeyPrefix(),
		OrgID:          key.GetOrgId(),
		OwnerUserID:    key.OwnerUserId,
		CreatedBy:      key.CreatedBy,
		ModelAllowlist: allowlistFromProto(key.GetModelAllowlist()),
		Status:         key.GetStatus(),
		PolicyVersion:  key.GetPolicyVersion(),
		TenantID:       key.GetTenantId(),
	}
	if ts := key.GetCreatedAt(); ts != nil && ts.IsValid() {
		metadata.CreatedAt = ts.AsTime().UTC()
	}
	if ts := key.GetExpiresAt(); ts != nil && ts.IsValid() {
		value := ts.AsTime().UTC()
		metadata.ExpiresAt = &value
	}
	if ts := key.GetRevokedAt(); ts != nil && ts.IsValid() {
		value := ts.AsTime().UTC()
		metadata.RevokedAt = &value
	}
	return metadata
}

func operationResultFromProto(response *pb.GetOperationResponse) OperationResult {
	if response == nil {
		return OperationResult{}
	}
	result := OperationResult{
		OperationID:   response.GetOperationId(),
		Kind:          response.GetKind(),
		PayloadDigest: response.GetPayloadDigest(),
		ActorScope:    response.GetActorScope(),
		RequestID:     response.GetRequestId(),
	}
	if ts := response.GetCreatedAt(); ts != nil && ts.IsValid() {
		result.CreatedAt = ts.AsTime().UTC()
	}
	stored := response.GetResult()
	if stored == nil {
		return result
	}
	result.ResultingVersion = stored.GetResultingVersion()
	result.PolicyVersion = stored.GetPolicyVersion()
	if stored.GetKey() != nil {
		key := keyMetadataFromProto(stored.GetKey())
		result.Key = &key
	}
	if stored.GetPolicy() != nil {
		policy := orgPolicyFromProto(stored.GetPolicy())
		result.Policy = &policy
	}
	return result
}

func orgPolicyFromProto(policy *pb.OrgPolicy) OrgPolicy {
	if policy == nil {
		return OrgPolicy{}
	}
	out := OrgPolicy{
		OrgID:             policy.GetOrgId(),
		ModelAllowlist:    allowlistFromProto(policy.GetModelAllowlist()),
		RequestsPerMinute: policy.RequestsPerMinute,
		Burst:             policy.Burst,
		PolicyVersion:     policy.GetPolicyVersion(),
		UpdatedBy:         policy.GetUpdatedBy(),
	}
	if ts := policy.GetUpdatedAt(); ts != nil && ts.IsValid() {
		out.UpdatedAt = ts.AsTime().UTC()
	}
	return out
}

func providerConfigToProto(provider ProviderConfig) (*pb.ProviderConfig, error) {
	if provider.Timeouts != nil && (provider.Timeouts.MaxEventBytes < 0 || provider.Timeouts.MaxEventBytes > int64(1<<31-1)) {
		return nil, fmt.Errorf("%w: max_event_bytes is outside the supported integer range", ErrInvalidArgument)
	}
	result := &pb.ProviderConfig{
		Id:            provider.ID,
		Protocol:      provider.Protocol,
		Url:           provider.URL,
		CredentialRef: provider.CredentialRef,
		Model:         provider.Model,
		MockDelayMs:   provider.MockDelayMS,
		ModelMapping:  cloneStringMap(provider.ModelMapping),
	}
	if provider.Timeouts != nil {
		result.Timeouts = &pb.ProviderTimeouts{
			ConnectTimeoutMs:        provider.Timeouts.ConnectTimeoutMS,
			TlsHandshakeTimeoutMs:   provider.Timeouts.TLSHandshakeTimeoutMS,
			ResponseHeaderTimeoutMs: provider.Timeouts.ResponseHeaderTimeoutMS,
			FirstByteTimeoutMs:      provider.Timeouts.FirstByteTimeoutMS,
			IdleTimeoutMs:           provider.Timeouts.IdleTimeoutMS,
			AttemptTimeoutMs:        provider.Timeouts.AttemptTimeoutMS,
			MaxEventBytes:           int32(provider.Timeouts.MaxEventBytes),
		}
	}
	return result, nil
}

func providerConfigFromProto(provider *pb.ProviderConfig) ProviderConfig {
	if provider == nil {
		return ProviderConfig{}
	}
	result := ProviderConfig{
		ID:            provider.GetId(),
		Protocol:      provider.GetProtocol(),
		URL:           provider.GetUrl(),
		CredentialRef: provider.GetCredentialRef(),
		Model:         provider.GetModel(),
		MockDelayMS:   provider.GetMockDelayMs(),
		ModelMapping:  cloneStringMap(provider.GetModelMapping()),
	}
	if timeouts := provider.GetTimeouts(); timeouts != nil {
		result.Timeouts = &ProviderTimeouts{
			ConnectTimeoutMS:        timeouts.GetConnectTimeoutMs(),
			TLSHandshakeTimeoutMS:   timeouts.GetTlsHandshakeTimeoutMs(),
			ResponseHeaderTimeoutMS: timeouts.GetResponseHeaderTimeoutMs(),
			FirstByteTimeoutMS:      timeouts.GetFirstByteTimeoutMs(),
			IdleTimeoutMS:           timeouts.GetIdleTimeoutMs(),
			AttemptTimeoutMS:        timeouts.GetAttemptTimeoutMs(),
			MaxEventBytes:           int64(timeouts.GetMaxEventBytes()),
		}
	}
	return result
}

func providerRoutesToProto(routes ProviderRouteMap) map[string]*pb.ProviderList {
	result := make(map[string]*pb.ProviderList, len(routes))
	for model, providers := range routes {
		copyProviders := make([]string, len(providers))
		copy(copyProviders, providers)
		result[model] = &pb.ProviderList{Providers: copyProviders}
	}
	return result
}

func providerRoutesFromProto(routes map[string]*pb.ProviderList) ProviderRouteMap {
	result := make(ProviderRouteMap, len(routes))
	for model, list := range routes {
		if list == nil {
			result[model] = []string{}
			continue
		}
		providers := list.GetProviders()
		copyProviders := make([]string, len(providers))
		copy(copyProviders, providers)
		result[model] = copyProviders
	}
	return result
}

func providerCredentialMetadataFromProto(metadata *pb.ProviderCredentialMetadata) ProviderCredentialMetadata {
	if metadata == nil {
		return ProviderCredentialMetadata{}
	}
	result := ProviderCredentialMetadata{
		CredentialID:  metadata.GetCredentialId(),
		CredentialRef: metadata.GetCredentialRef(),
		ProviderID:    metadata.GetProviderId(),
		Fingerprint:   metadata.GetFingerprint(),
		Status:        metadata.GetStatus(),
	}
	if ts := metadata.GetCreatedAt(); ts != nil && ts.IsValid() {
		result.CreatedAt = ts.AsTime().UTC()
	}
	if ts := metadata.GetRevokedAt(); ts != nil && ts.IsValid() {
		value := ts.AsTime().UTC()
		result.RevokedAt = &value
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func wipeBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

// allowlistToProto preserves message presence: nil stays absent (NULL, inherit)
// while a non-nil pointer always produces a present message ([] is deny-all).
func allowlistToProto(list *[]string) *pb.ModelAllowlist {
	if list == nil {
		return nil
	}
	models := append([]string(nil), (*list)...)
	if models == nil {
		models = []string{}
	}
	return &pb.ModelAllowlist{Models: models}
}

func allowlistFromProto(list *pb.ModelAllowlist) *[]string {
	if list == nil {
		return nil
	}
	models := append([]string(nil), list.GetModels()...)
	if models == nil {
		models = []string{}
	}
	return &models
}

func timestampOrNil(value *time.Time) *timestamppb.Timestamp {
	if value == nil {
		return nil
	}
	return timestamppb.New(value.UTC())
}
