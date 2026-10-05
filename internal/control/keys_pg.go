package control

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "llm-gateway/proto/control"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// controlVersionAdvisoryLock serializes snapshot version allocation across
// control writers. It is a PostgreSQL advisory lock taken with
// pg_advisory_xact_lock inside the mutation transaction; no row lock is ever
// held across RPC or network I/O.
const controlVersionAdvisoryLock int64 = 0x636F6E74726F6C // "control"

var (
	// ErrOperationConflict means the same operation_id was already used with a
	// different payload digest (or a different RPC kind).
	ErrOperationConflict = errors.New("operation id already exists with a different payload")
	// ErrAPIKeyNotFound means no api_keys row matches the request scope.
	ErrAPIKeyNotFound = errors.New("api key not found")
	// ErrOrganizationNotFound means api_keys.org_id would violate its FK.
	ErrOrganizationNotFound = errors.New("organization not found")
	// ErrOperationNotFound means control_operations has no such operation id.
	ErrOperationNotFound = errors.New("operation not found")
)

// versionConflictError reports a failed expected_version precondition. It
// carries the current version so the gRPC layer can return it to the caller
// inside FAILED_PRECONDITION.
type versionConflictError struct {
	Expected int64
	Current  int64
}

func (e *versionConflictError) Error() string {
	return fmt.Sprintf("config version conflict: expected %d, current %d", e.Expected, e.Current)
}

// APIKeyMutation is the redacted result of one key mutation. Snapshot is the
// committed versioned projection; the caller publishes it to watchers after
// the transaction commits. It never contains a raw secret.
type APIKeyMutation struct {
	Key              *pb.ApiKeyMetadata
	ResultingVersion int64
	Replayed         bool
	Snapshot         *pb.Config
	Operation        *pb.GetOperationResponse
}

// OrgPolicyMutation is the redacted result of one ApplyOrgPolicy call.
type OrgPolicyMutation struct {
	Policy           *pb.OrgPolicy
	ResultingVersion int64
	Replayed         bool
	Snapshot         *pb.Config
	Operation        *pb.GetOperationResponse
}

// CreateAPIKeyParams carries one idempotent key creation. ModelAllowlist uses
// nil for NULL (inherit) and an empty slice for deny-all.
type CreateAPIKeyParams struct {
	OperationID     string
	PayloadDigest   string
	ActorScope      string
	RequestID       string
	ExpectedVersion *int64
	OrgID           int64
	OwnerUserID     *int64
	CreatedBy       *int64
	ExpiresAt       *time.Time
	ModelAllowlist  []string
	KeyID           string
	RawKey          string
	Now             time.Time
}

// RevokeAPIKeyParams carries one idempotent revocation. OrgID is an optional
// scope assertion: 0 means "any organization".
type RevokeAPIKeyParams struct {
	OperationID     string
	PayloadDigest   string
	ActorScope      string
	RequestID       string
	ExpectedVersion *int64
	KeyID           string
	OrgID           int64
	Now             time.Time
}

// ListAPIKeysParams is the bounded metadata query. OwnerUserID nil means "any
// owner"; OrgID 0 means "every organization"; Cursor is the decoded last
// key_id of the previous page.
type ListAPIKeysParams struct {
	OrgID       int64
	OwnerUserID *int64
	PageSize    int
	Cursor      string
}

// ApplyOrgPolicyParams replaces the complete policy of one organization.
type ApplyOrgPolicyParams struct {
	OperationID       string
	PayloadDigest     string
	ActorScope        string
	RequestID         string
	ExpectedVersion   *int64
	OrgID             int64
	ModelAllowlist    []string // nil = NULL/inherit, empty = deny-all
	RequestsPerMinute *int32
	Burst             *int32
	ActorUserID       *int64
	Now               time.Time
}

// PGAPIKeyStore is the PostgreSQL fact store for api_keys. Every mutation runs
// in one transaction that writes api_keys, a new derived control_config
// snapshot, control_audit_events and control_operations.
type PGAPIKeyStore struct {
	db *sql.DB
}

func NewPGAPIKeyStore(db *sql.DB) (*PGAPIKeyStore, error) {
	if db == nil {
		return nil, errors.New("api key store requires a database handle")
	}
	// Fail fast on a schema that has not been migrated to 000006.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var apiKeys, operations sql.NullString
	if err := db.QueryRowContext(ctx, queryAdminSchemaReady).Scan(&apiKeys, &operations); err != nil {
		return nil, fmt.Errorf("verify control key schema: %w", err)
	}
	if !apiKeys.Valid || !operations.Valid {
		return nil, fmt.Errorf("%w: api_keys/control_operations are missing, run migrate up", ErrSchemaMissing)
	}
	return &PGAPIKeyStore{db: db}, nil
}

// ErrSchemaMissing marks a database that has not applied migration 000006.
var ErrSchemaMissing = errors.New("control api key schema is missing")

func (s *PGAPIKeyStore) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// CreateApiKey inserts one active key and its derived snapshot projection.
func (s *PGAPIKeyStore) CreateApiKey(ctx context.Context, params CreateAPIKeyParams) (*APIKeyMutation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("api key store is not configured")
	}
	if err := validateMutationParams(params.OperationID, params.PayloadDigest); err != nil {
		return nil, err
	}
	if params.OrgID <= 0 {
		return nil, fmt.Errorf("%w: org_id is required", ErrAdminInvalidArgument)
	}
	if strings.TrimSpace(params.KeyID) == "" || params.RawKey == "" {
		return nil, fmt.Errorf("%w: key_id and raw key are required", ErrAdminInvalidArgument)
	}
	mutation := &APIKeyMutation{}
	err := s.withMutationTx(ctx, func(tx *sql.Tx) error {
		replayed, err := replayOperationTx(ctx, tx, params.OperationID, operationCreateAPIKey, params.PayloadDigest)
		if err != nil {
			return err
		}
		if replayed != nil {
			mutation.Key = replayed.Result.GetKey()
			mutation.ResultingVersion = replayed.Result.GetResultingVersion()
			mutation.Replayed = true
			mutation.Operation = replayed.Response
			return nil
		}
		if err := ensureOrganizationTx(ctx, tx, params.OrgID); err != nil {
			return err
		}
		snapshot, current, err := loadSnapshotTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkExpectedVersion(params.ExpectedVersion, current); err != nil {
			return err
		}
		allowlistJSON, err := encodeModelAllowlist(params.ModelAllowlist)
		if err != nil {
			return err
		}
		policyVersion := int64(1)
		if policy := snapshot.GetOrgPolicies()[params.OrgID]; policy != nil && policy.GetPolicyVersion() > 0 {
			policyVersion = policy.GetPolicyVersion()
		}
		now := params.Now.UTC()
		if _, err := tx.ExecContext(ctx, queryInsertAPIKey,
			params.KeyID, apiKeyHash(params.RawKey), rawKeyPrefix(params.RawKey), params.OrgID,
			params.OwnerUserID, params.CreatedBy, "active", params.ExpiresAt, allowlistJSON,
			policyVersion, params.OperationID, now); err != nil {
			return mapAPIKeyWriteError(err, params.OrgID)
		}
		resulting, snapshot, err := saveProjectedSnapshotTx(ctx, tx, snapshot, current+1)
		if err != nil {
			return err
		}
		// Re-read the committed row so the response metadata is exactly what a
		// later ListApiKeys returns (including server-generated timestamps).
		key, err := getAPIKeyTx(ctx, tx, params.KeyID)
		if err != nil {
			return err
		}
		operation := &pb.OperationResult{
			Kind:             operationCreateAPIKey,
			Key:              key.metadata(),
			ResultingVersion: resulting,
			PolicyVersion:    policyVersion,
		}
		response, err := insertOperationTx(ctx, tx, operationRecord{
			OperationID:   params.OperationID,
			Kind:          operationCreateAPIKey,
			PayloadDigest: params.PayloadDigest,
			ActorScope:    params.ActorScope,
			RequestID:     params.RequestID,
			Expected:      params.ExpectedVersion,
			Resulting:     resulting,
			Result:        operation,
			Now:           now,
		})
		if err != nil {
			return err
		}
		if err := insertAuditTx(ctx, tx, AuditEvent{
			Time: now, Action: "api_key.create", Outcome: "success",
			Subject: params.ActorScope, Role: RoleAdmin, TenantID: strconv.FormatInt(params.OrgID, 10),
			KeyID: params.KeyID, RequestID: params.RequestID, ConfigVersion: resulting, KeyCount: 1,
		}); err != nil {
			return err
		}
		mutation.Key = operation.Key
		mutation.ResultingVersion = resulting
		mutation.Snapshot = snapshot
		mutation.Operation = response
		return nil
	})
	if err != nil {
		return nil, err
	}
	return mutation, nil
}

// RevokeApiKey marks one active key revoked. Revoking an already revoked key
// is a no-op that returns the stored metadata without allocating a new
// snapshot version.
func (s *PGAPIKeyStore) RevokeApiKey(ctx context.Context, params RevokeAPIKeyParams) (*APIKeyMutation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("api key store is not configured")
	}
	if err := validateMutationParams(params.OperationID, params.PayloadDigest); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.KeyID) == "" {
		return nil, fmt.Errorf("%w: key_id is required", ErrAdminInvalidArgument)
	}
	mutation := &APIKeyMutation{}
	err := s.withMutationTx(ctx, func(tx *sql.Tx) error {
		replayed, err := replayOperationTx(ctx, tx, params.OperationID, operationRevokeAPIKey, params.PayloadDigest)
		if err != nil {
			return err
		}
		if replayed != nil {
			mutation.Key = replayed.Result.GetKey()
			mutation.ResultingVersion = replayed.Result.GetResultingVersion()
			mutation.Replayed = true
			mutation.Operation = replayed.Response
			return nil
		}
		snapshot, current, err := loadSnapshotTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkExpectedVersion(params.ExpectedVersion, current); err != nil {
			return err
		}
		existing, err := getAPIKeyTx(ctx, tx, params.KeyID)
		if err != nil {
			return err
		}
		// Scope assertion: an internal caller that names the wrong organization
		// must not learn that the key exists elsewhere.
		if params.OrgID != 0 && params.OrgID != existing.orgID {
			return ErrAPIKeyNotFound
		}
		now := params.Now.UTC()
		resulting := current
		var committed *pb.Config
		if existing.status == "active" {
			if _, err := tx.ExecContext(ctx, queryRevokeAPIKey, now, params.KeyID); err != nil {
				return err
			}
			resulting, committed, err = saveProjectedSnapshotTx(ctx, tx, snapshot, current+1)
			if err != nil {
				return err
			}
		}
		key, err := getAPIKeyTx(ctx, tx, params.KeyID)
		if err != nil {
			return err
		}
		operation := &pb.OperationResult{
			Kind:             operationRevokeAPIKey,
			Key:              key.metadata(),
			ResultingVersion: resulting,
			PolicyVersion:    key.policyVersion,
		}
		response, err := insertOperationTx(ctx, tx, operationRecord{
			OperationID:   params.OperationID,
			Kind:          operationRevokeAPIKey,
			PayloadDigest: params.PayloadDigest,
			ActorScope:    params.ActorScope,
			RequestID:     params.RequestID,
			Expected:      params.ExpectedVersion,
			Resulting:     resulting,
			Result:        operation,
			Now:           now,
		})
		if err != nil {
			return err
		}
		if err := insertAuditTx(ctx, tx, AuditEvent{
			Time: now, Action: "api_key.revoke", Outcome: "success",
			Subject: params.ActorScope, Role: RoleAdmin, TenantID: strconv.FormatInt(existing.orgID, 10),
			KeyID: params.KeyID, RequestID: params.RequestID, ConfigVersion: resulting, KeyCount: 1,
		}); err != nil {
			return err
		}
		mutation.Key = operation.Key
		mutation.ResultingVersion = resulting
		// A no-op revoke commits no new snapshot version, so there is nothing
		// new to publish.
		mutation.Snapshot = committed
		mutation.Operation = response
		return nil
	})
	if err != nil {
		return nil, err
	}
	return mutation, nil
}

// ListApiKeys returns redacted metadata only, sorted by key_id.
func (s *PGAPIKeyStore) ListApiKeys(ctx context.Context, params ListAPIKeysParams) ([]*pb.ApiKeyMetadata, string, error) {
	if s == nil || s.db == nil {
		return nil, "", errors.New("api key store is not configured")
	}
	cursor := strings.TrimSpace(params.Cursor)
	pageSize := params.PageSize
	if pageSize <= 0 {
		pageSize = defaultListPageSize
	}
	owner := int64(0)
	if params.OwnerUserID != nil {
		owner = *params.OwnerUserID
	}
	// Fetch one extra row to detect whether another page exists without a
	// second query.
	rows, err := s.db.QueryContext(ctx, queryListAPIKeys, params.OrgID, owner, cursor, pageSize+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := make([]*pb.ApiKeyMetadata, 0, pageSize)
	var last string
	for rows.Next() {
		key, err := scanAPIKeyRow(rows)
		if err != nil {
			return nil, "", err
		}
		if len(out) == pageSize {
			// The extra row only proves there is a next page.
			return out, encodePageToken(last), nil
		}
		out = append(out, key.metadata())
		last = key.keyID
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return out, "", nil
}

// GetOperation returns the stored result metadata of one mutation. The raw
// secret is not part of the stored result and cannot be recovered here.
func (s *PGAPIKeyStore) GetOperation(ctx context.Context, operationID string) (*pb.GetOperationResponse, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("api key store is not configured")
	}
	row := s.db.QueryRowContext(ctx, queryGetOperation, strings.TrimSpace(operationID))
	record, err := scanOperationRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOperationNotFound
	}
	if err != nil {
		return nil, err
	}
	return record.Response, nil
}

// ApplyOrgPolicy replaces the organization's policy inside the versioned
// snapshot and bumps both the snapshot version and the policy version.
func (s *PGAPIKeyStore) ApplyOrgPolicy(ctx context.Context, params ApplyOrgPolicyParams) (*OrgPolicyMutation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("api key store is not configured")
	}
	if err := validateMutationParams(params.OperationID, params.PayloadDigest); err != nil {
		return nil, err
	}
	if params.OrgID <= 0 {
		return nil, fmt.Errorf("%w: org_id is required", ErrAdminInvalidArgument)
	}
	mutation := &OrgPolicyMutation{}
	err := s.withMutationTx(ctx, func(tx *sql.Tx) error {
		replayed, err := replayOperationTx(ctx, tx, params.OperationID, operationApplyOrgPolicy, params.PayloadDigest)
		if err != nil {
			return err
		}
		if replayed != nil {
			mutation.Policy = replayed.Result.GetPolicy()
			mutation.ResultingVersion = replayed.Result.GetResultingVersion()
			mutation.Replayed = true
			mutation.Operation = replayed.Response
			return nil
		}
		if err := ensureOrganizationTx(ctx, tx, params.OrgID); err != nil {
			return err
		}
		snapshot, current, err := loadSnapshotTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkExpectedVersion(params.ExpectedVersion, current); err != nil {
			return err
		}
		allowlistJSON, err := encodeModelAllowlist(params.ModelAllowlist)
		if err != nil {
			return err
		}
		previousVersion := int64(0)
		if previous := snapshot.GetOrgPolicies()[params.OrgID]; previous != nil {
			previousVersion = previous.GetPolicyVersion()
		}
		now := params.Now.UTC()
		policy := &pb.OrgPolicy{
			OrgId:             params.OrgID,
			ModelAllowlist:    modelAllowlistFromJSON(allowlistJSON),
			RequestsPerMinute: params.RequestsPerMinute,
			Burst:             params.Burst,
			PolicyVersion:     previousVersion + 1,
			UpdatedAt:         timestamppb.New(now),
			UpdatedBy:         actorUserIDValue(params.ActorUserID),
		}
		if snapshot.OrgPolicies == nil {
			snapshot.OrgPolicies = map[int64]*pb.OrgPolicy{}
		}
		snapshot.OrgPolicies[params.OrgID] = policy
		resulting, snapshot, err := saveProjectedSnapshotTx(ctx, tx, snapshot, current+1)
		if err != nil {
			return err
		}
		operation := &pb.OperationResult{
			Kind:             operationApplyOrgPolicy,
			ResultingVersion: resulting,
			PolicyVersion:    policy.GetPolicyVersion(),
			Policy:           policy,
		}
		response, err := insertOperationTx(ctx, tx, operationRecord{
			OperationID:   params.OperationID,
			Kind:          operationApplyOrgPolicy,
			PayloadDigest: params.PayloadDigest,
			ActorScope:    params.ActorScope,
			RequestID:     params.RequestID,
			Expected:      params.ExpectedVersion,
			Resulting:     resulting,
			Result:        operation,
			Now:           now,
		})
		if err != nil {
			return err
		}
		if err := insertAuditTx(ctx, tx, AuditEvent{
			Time: now, Action: "org_policy.apply", Outcome: "success",
			Subject: params.ActorScope, Role: RoleAdmin, TenantID: strconv.FormatInt(params.OrgID, 10),
			RequestID: params.RequestID, ConfigVersion: resulting,
		}); err != nil {
			return err
		}
		mutation.Policy = policy
		mutation.ResultingVersion = resulting
		mutation.Snapshot = snapshot
		mutation.Operation = response
		return nil
	})
	if err != nil {
		return nil, err
	}
	return mutation, nil
}

// withMutationTx runs one key mutation under the version-allocation advisory
// lock. Everything inside is local database work; no RPC or network I/O is
// performed while the lock is held.
func (s *PGAPIKeyStore) withMutationTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, queryLockControlVersion, controlVersionAdvisoryLock); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type operationRecord struct {
	OperationID   string
	Kind          string
	PayloadDigest string
	ActorScope    string
	RequestID     string
	Expected      *int64
	Resulting     int64
	Result        *pb.OperationResult
	Now           time.Time
	// Response is filled when the record is read back.
	Response *pb.GetOperationResponse
}

// replayOperationTx returns the stored record when the operation id was
// already applied with the same digest. A different digest (or a different
// RPC kind reusing the id) is a conflict and must not silently overwrite.
func replayOperationTx(ctx context.Context, tx *sql.Tx, operationID, kind, digest string) (*operationRecord, error) {
	row := tx.QueryRowContext(ctx, queryGetOperation, strings.TrimSpace(operationID))
	record, err := scanOperationRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.Kind != kind || record.PayloadDigest != digest {
		return nil, fmt.Errorf("%w: operation %q", ErrOperationConflict, operationID)
	}
	return record, nil
}

func insertOperationTx(ctx context.Context, tx *sql.Tx, record operationRecord) (*pb.GetOperationResponse, error) {
	if record.Result == nil {
		record.Result = &pb.OperationResult{Kind: record.Kind, ResultingVersion: record.Resulting}
	}
	payload, err := protojson.Marshal(record.Result)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, queryInsertOperation,
		record.OperationID, record.Kind, record.PayloadDigest, record.ActorScope,
		record.RequestID, string(payload), record.Expected, record.Resulting, record.Now.UTC()); err != nil {
		return nil, err
	}
	return &pb.GetOperationResponse{
		OperationId:     record.OperationID,
		Kind:            record.Kind,
		PayloadDigest:   record.PayloadDigest,
		ActorScope:      record.ActorScope,
		RequestId:       record.RequestID,
		ExpectedVersion: record.Expected,
		Result:          record.Result,
		CreatedAt:       timestamppb.New(record.Now.UTC()),
	}, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOperationRow(row rowScanner) (*operationRecord, error) {
	var (
		record    operationRecord
		payload   []byte
		expected  sql.NullInt64
		resulting sql.NullInt64
		createdAt time.Time
	)
	if err := row.Scan(&record.OperationID, &record.Kind, &record.PayloadDigest, &record.ActorScope,
		&record.RequestID, &payload, &expected, &resulting, &createdAt); err != nil {
		return nil, err
	}
	if expected.Valid {
		value := expected.Int64
		record.Expected = &value
	}
	if resulting.Valid {
		record.Resulting = resulting.Int64
	}
	record.Now = createdAt.UTC()
	result := &pb.OperationResult{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(payload, result); err != nil {
		return nil, fmt.Errorf("decode stored operation result: %w", err)
	}
	record.Result = result
	record.Response = &pb.GetOperationResponse{
		OperationId:     record.OperationID,
		Kind:            record.Kind,
		PayloadDigest:   record.PayloadDigest,
		ActorScope:      record.ActorScope,
		RequestId:       record.RequestID,
		ExpectedVersion: record.Expected,
		Result:          result,
		CreatedAt:       timestamppb.New(createdAt.UTC()),
	}
	return &record, nil
}

type apiKeyRow struct {
	keyID         string
	keyHash       string
	keyPrefix     string
	orgID         int64
	ownerUserID   sql.NullInt64
	createdBy     sql.NullInt64
	status        string
	expiresAt     sql.NullTime
	allowlist     []byte
	policyVersion int64
	createdAt     time.Time
	revokedAt     sql.NullTime
	legacyTenant  sql.NullString
}

func scanAPIKeyRow(row rowScanner) (*apiKeyRow, error) {
	var record apiKeyRow
	if err := row.Scan(&record.keyID, &record.keyHash, &record.keyPrefix, &record.orgID,
		&record.ownerUserID, &record.createdBy, &record.status, &record.expiresAt,
		&record.allowlist, &record.policyVersion, &record.createdAt, &record.revokedAt,
		&record.legacyTenant); err != nil {
		return nil, err
	}
	return &record, nil
}

// tenantIDString is the legacy tenant projection of an organization: the
// explicit legacy_tenant_id mapping when present, otherwise a namespaced
// org:<id> string that cannot be confused with a historical tenant name.
func (r *apiKeyRow) tenantIDString() string {
	if r.legacyTenant.Valid && strings.TrimSpace(r.legacyTenant.String) != "" {
		return strings.TrimSpace(r.legacyTenant.String)
	}
	return "org:" + strconv.FormatInt(r.orgID, 10)
}

func (r *apiKeyRow) metadata() *pb.ApiKeyMetadata {
	metadata := &pb.ApiKeyMetadata{
		KeyId:         r.keyID,
		KeyPrefix:     r.keyPrefix,
		OrgId:         r.orgID,
		Status:        r.status,
		PolicyVersion: r.policyVersion,
		CreatedAt:     timestamppb.New(r.createdAt.UTC()),
		TenantId:      r.tenantIDString(),
	}
	if r.ownerUserID.Valid {
		metadata.OwnerUserId = proto.Int64(r.ownerUserID.Int64)
	}
	if r.createdBy.Valid {
		metadata.CreatedBy = proto.Int64(r.createdBy.Int64)
	}
	if r.expiresAt.Valid {
		metadata.ExpiresAt = timestamppb.New(r.expiresAt.Time.UTC())
	}
	allowlist, err := decodeModelAllowlist(r.allowlist)
	if err == nil {
		metadata.ModelAllowlist = allowlist
	}
	if r.revokedAt.Valid {
		metadata.RevokedAt = timestamppb.New(r.revokedAt.Time.UTC())
	}
	return metadata
}

// keyRecord projects one stored key into the Gateway-facing snapshot. The
// record carries the hash (never the raw key) plus the new attribution
// metadata; legacy records keep resolving because the merge in
// projectKeyRecordsTx only adds fields.
func (r *apiKeyRow) keyRecord() *pb.KeyRecord {
	return &pb.KeyRecord{
		KeyId:          r.keyID,
		KeyHash:        r.keyHash,
		KeyPrefix:      r.keyPrefix,
		TenantId:       r.tenantIDString(),
		Status:         r.status,
		OrgId:          r.orgID,
		OwnerUserId:    nullableInt64(r.ownerUserID),
		ExpiresAt:      nullableTimestamp(r.expiresAt),
		ModelAllowlist: modelAllowlistFromJSON(r.allowlist),
	}
}

func getAPIKeyTx(ctx context.Context, tx *sql.Tx, keyID string) (*apiKeyRow, error) {
	row := tx.QueryRowContext(ctx, queryGetAPIKey, strings.TrimSpace(keyID))
	record, err := scanAPIKeyRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return record, nil
}

// ensureOrganizationTx fails closed with ErrOrganizationNotFound before the
// insert, so the API can answer NOT_FOUND instead of surfacing a raw foreign
// key violation.
func ensureOrganizationTx(ctx context.Context, tx *sql.Tx, orgID int64) error {
	var found int
	err := tx.QueryRowContext(ctx, queryGetOrganization, orgID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: organization %d", ErrOrganizationNotFound, orgID)
	}
	return err
}

func loadSnapshotTx(ctx context.Context, tx *sql.Tx) (*pb.Config, int64, error) {
	var raw string
	err := tx.QueryRowContext(ctx, queryLatestConfig).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return &pb.Config{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	cfg := &pb.Config{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(raw), cfg); err != nil {
		return nil, 0, err
	}
	// normalizeConfig converts a historical raw-key map into records and drops
	// the deprecated field, so the next admin write also finishes that cleanup.
	cfg = normalizeConfig(cfg)
	return cfg, cfg.GetVersion(), nil
}

// validateMutationParams rejects an incomplete mutation before any SQL runs.
// The control_operations table enforces the same digest shape.
func validateMutationParams(operationID, digest string) error {
	if strings.TrimSpace(operationID) == "" {
		return fmt.Errorf("%w: operation_id is required", ErrAdminInvalidArgument)
	}
	if len(digest) != 64 {
		return fmt.Errorf("%w: payload digest must be a sha256 hex string", ErrAdminInvalidArgument)
	}
	return nil
}

func checkExpectedVersion(expected *int64, current int64) error {
	if expected == nil {
		return nil
	}
	if *expected != current {
		return &versionConflictError{Expected: *expected, Current: current}
	}
	return nil
}

// saveProjectedSnapshotTx derives the Gateway-facing key projection from
// api_keys and inserts it as a new control_config version in the same
// transaction as the key change.
func saveProjectedSnapshotTx(ctx context.Context, tx *sql.Tx, cfg *pb.Config, version int64) (int64, *pb.Config, error) {
	records, err := projectKeyRecordsTx(ctx, tx, cfg.GetKeyRecords())
	if err != nil {
		return 0, nil, err
	}
	next := cloneConfig(cfg)
	next.Version = version
	next.KeyRecords = records
	next.Keys = nil
	payload, err := protojson.Marshal(next)
	if err != nil {
		return 0, nil, err
	}
	if _, err := tx.ExecContext(ctx, querySaveConfig, next.Version, string(payload)); err != nil {
		return 0, nil, err
	}
	return next.Version, next, nil
}

// projectKeyRecordsTx merges the api_keys projection with the records that are
// not managed by api_keys yet. Legacy records (created before 000006 or
// bootstrapped through the legacy surface) must keep resolving, so they are
// preserved unless the projection defines the same key_id or key_hash.
func projectKeyRecordsTx(ctx context.Context, tx *sql.Tx, previous []*pb.KeyRecord) ([]*pb.KeyRecord, error) {
	rows, err := tx.QueryContext(ctx, queryProjectAPIKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projected := make([]*pb.KeyRecord, 0, len(previous))
	ids := make(map[string]struct{}, len(previous))
	hashes := make(map[string]struct{}, len(previous))
	for rows.Next() {
		record, err := scanAPIKeyRow(rows)
		if err != nil {
			return nil, err
		}
		projected = append(projected, record.keyRecord())
		ids[record.keyID] = struct{}{}
		hashes[record.keyHash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, record := range previous {
		if record == nil {
			continue
		}
		if _, managed := ids[record.GetKeyId()]; managed {
			continue
		}
		if _, managed := hashes[record.GetKeyHash()]; managed {
			continue
		}
		projected = append(projected, proto.Clone(record).(*pb.KeyRecord))
	}
	return projected, nil
}

// encodeModelAllowlist converts the Go tri-state into the JSONB value: nil for
// NULL (inherit), the canonical JSON array otherwise. The value is
// re-canonicalized here so the stored JSONB always matches the sorted form the
// payload digest was computed over, even if a caller bypassed the service.
func encodeModelAllowlist(models []string) ([]byte, error) {
	if models == nil {
		return nil, nil
	}
	return json.Marshal(canonicalizeModels(models))
}

// canonicalizeModels trims, de-duplicates and sorts model names. It drops empty
// entries; the service rejects them before reaching the store.
func canonicalizeModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, raw := range models {
		model := strings.TrimSpace(raw)
		if model == "" {
			continue
		}
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

func modelAllowlistFromJSON(raw []byte) *pb.ModelAllowlist {
	allowlist, err := decodeModelAllowlist(raw)
	if err != nil {
		return nil
	}
	return allowlist
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return proto.Int64(value.Int64)
}

func nullableTimestamp(value sql.NullTime) *timestamppb.Timestamp {
	if !value.Valid {
		return nil
	}
	return timestamppb.New(value.Time.UTC())
}

func actorUserIDValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// mapAPIKeyWriteError turns PostgreSQL constraint violations into
// caller-fixable errors instead of leaking driver text.
func mapAPIKeyWriteError(err error, orgID int64) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503": // foreign_key_violation
			switch pgErr.ConstraintName {
			case "api_keys_org_id_fkey":
				return fmt.Errorf("%w: organization %d", ErrOrganizationNotFound, orgID)
			default:
				return fmt.Errorf("%w: referenced user does not exist", ErrAdminInvalidArgument)
			}
		case "23505": // unique_violation
			if pgErr.ConstraintName == "idx_api_keys_operation_id" || pgErr.ConstraintName == "api_keys_key_hash_key" {
				return fmt.Errorf("%w: %s", ErrOperationConflict, pgErr.ConstraintName)
			}
		}
	}
	return err
}

//go:embed queries/lock_control_version.sql
var queryLockControlVersion string

//go:embed queries/get_api_key.sql
var queryGetAPIKey string

//go:embed queries/insert_api_key.sql
var queryInsertAPIKey string

//go:embed queries/revoke_api_key.sql
var queryRevokeAPIKey string

//go:embed queries/list_api_keys.sql
var queryListAPIKeys string

//go:embed queries/project_api_keys.sql
var queryProjectAPIKeys string

//go:embed queries/get_operation.sql
var queryGetOperation string

//go:embed queries/insert_operation.sql
var queryInsertOperation string

//go:embed queries/get_organization.sql
var queryGetOrganization string

//go:embed queries/admin_schema_ready.sql
var queryAdminSchemaReady string
