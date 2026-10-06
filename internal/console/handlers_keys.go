package console

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/controlclient"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

// T03 key management HTTP surface (ADR-0003 §2.2, §2.3, §2.9):
//
//	GET  /api/v1/orgs/{org}/keys                    session + matrix
//	POST /api/v1/orgs/{org}/keys                    session + CSRF + matrix
//	POST /api/v1/orgs/{org}/keys/{key}/revoke       session + CSRF + matrix
//
// The handlers never see the permission matrix: ControlKeyService resolves the
// caller's membership, applies the matrix and only then talks to Control. Here
// we parse, call, map typed errors onto status codes and audit.

// Audit action and target names for key management. They are stable values of
// console_audit_events; the raw secret is never part of them.
const (
	ActionKeyCreate = "org.key.create"
	ActionKeyRevoke = "org.key.revoke"

	TargetAPIKey = "api_key"
)

// maxKeyCursorBytes bounds the opaque Control page token a client may send
// back. Control issues short base64 tokens; anything longer is a malformed
// request, not a cursor.
const maxKeyCursorBytes = 512

// listOrgKeys implements GET /api/v1/orgs/{org}/keys.
//
// With ?operation_id=... it switches to recovery mode: it returns the metadata
// of the key that operation created (or revoked) with replayed=true and never a
// secret, which is the documented answer to a lost create response (ADR-0003
// §2.2). limit/cursor/owner_user_id are ignored in that mode.
func (s *server) listOrgKeys(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}

	if operationID := strings.TrimSpace(c.Query("operation_id")); operationID != "" {
		recovered, err := s.keys.RecoverKey(ctx, auth.User.ID, orgID, operationID)
		if err != nil {
			s.writeKeyError(c, "recover api key operation", err)
			return
		}
		writeJSON(c, http.StatusOK, keyPage{
			Items:    []keyDTO{toKeyDTO(recovered.Key)},
			Replayed: true,
		})
		return
	}

	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	cursor := strings.TrimSpace(c.Query("cursor"))
	if len(cursor) > maxKeyCursorBytes {
		s.writeError(c, http.StatusBadRequest, "", "cursor is invalid")
		return
	}
	ownerUserID, err := parseOptionalUserIDValue(c.Query("owner_user_id"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "owner_user_id must be a positive user id")
		return
	}

	page, err := s.keys.ListKeys(ctx, auth.User.ID, orgID, ListKeysParams{
		OwnerUserID: ownerUserID,
		Limit:       limit,
		Cursor:      cursor,
		RequestID:   requestIDFrom(c),
	})
	if err != nil {
		s.writeKeyError(c, "list api keys", err)
		return
	}
	writeJSON(c, http.StatusOK, keyPage{
		Items:      toKeyDTOs(page.Items),
		NextCursor: optionalCursor(page.NextCursor),
		Replayed:   false,
	})
}

// createOrgKey implements POST /api/v1/orgs/{org}/keys. The response carries
// the raw secret exactly once and says so; on an idempotent replay it carries
// no secret and points at the operation_id recovery route instead.
func (s *server) createOrgKey(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	var req createKeyRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	ownerUserID, err := parseOptionalUserID(req.OwnerUserID)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "owner_user_id must be a positive user id string")
		return
	}
	expiresAt, err := parseOptionalTimestamp(req.ExpiresAt)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "expires_at must be an RFC3339 timestamp")
		return
	}

	created, err := s.keys.CreateKey(ctx, auth.User.ID, orgID, CreateKeyParams{
		Name:           req.Name,
		OwnerUserID:    ownerUserID,
		ExpiresAt:      expiresAt,
		ModelAllowlist: req.ModelAllowlist,
		OperationID:    strings.TrimSpace(req.OperationID),
		RequestID:      requestIDFrom(c),
	})
	if err != nil {
		s.audit.Record(ctx, s.auditMutation(c, ActionKeyCreate, orgID, TargetAPIKey, "", "failure", "key_create_failed", map[string]any{"failure": "request_rejected"}))
		s.writeKeyError(c, "create api key", err)
		return
	}

	// The audit row records the key identity, never the secret.
	s.audit.Record(ctx, s.auditMutation(c, ActionKeyCreate, orgID, TargetAPIKey, created.Key.KeyID,
		"success", "", map[string]any{
			"prefix":        created.Key.KeyPrefix,
			"owner_user_id": formatOptionalID(created.Key.OwnerUserID),
			"replayed":      created.Replayed,
		}))

	response := createKeyResponse{
		Key:              toKeyDTO(created.Key),
		ResultingVersion: formatID(created.ResultingVersion),
		Replayed:         created.Replayed,
		Notice:           keyCreateNoticeReplay,
	}
	if !created.Replayed {
		// The one and only time the raw secret leaves Control.
		response.Secret = created.Secret
		response.SecretReturnedOnce = true
		response.Notice = keyCreateNoticeOnce
	}
	writeJSON(c, http.StatusCreated, response)
}

// revokeOrgKey implements POST /api/v1/orgs/{org}/keys/{key}/revoke.
func (s *server) revokeOrgKey(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	keyID := strings.TrimSpace(c.Param("key"))
	if keyID == "" {
		s.writeError(c, http.StatusBadRequest, "", "key id is required")
		return
	}

	result, err := s.keys.RevokeKey(ctx, auth.User.ID, orgID, keyID)
	if err != nil {
		s.audit.Record(ctx, s.auditMutation(c, ActionKeyRevoke, orgID, TargetAPIKey, "",
			"failure", "key_revoke_failed", map[string]any{"failure": "request_rejected"}))
		s.writeKeyError(c, "revoke api key", err)
		return
	}
	s.audit.Record(ctx, s.auditMutation(c, ActionKeyRevoke, orgID, TargetAPIKey, result.Key.KeyID,
		"success", "", map[string]any{
			"prefix":        result.Key.KeyPrefix,
			"owner_user_id": formatOptionalID(result.Key.OwnerUserID),
			"replayed":      result.Replayed,
		}))
	writeJSON(c, http.StatusOK, revokeKeyResponse{
		Key:              toKeyDTO(result.Key),
		ResultingVersion: formatID(result.ResultingVersion),
		Replayed:         result.Replayed,
	})
}

// writeKeyError maps key-management failures onto the frozen HTTP semantics
// (ADR-0003 §2.2, §2.3, §2.9):
//
//   - 404 not_found        — unknown key, another organization's key, another
//     user's key for a member, or an unknown operation id;
//   - 403 forbidden        — the caller is a member, but the matrix (or the
//     own-key rule) refuses the action;
//   - 409 conflict         — the operation id already exists with a different
//     payload, or expected_version no longer matches;
//   - 400 invalid_request  — malformed owner/expiry/allowlist/operation id;
//   - 502 upstream_unavailable — Control refused the Console identity or could
//     not be reached; the browser cannot fix either, so it is not a 401/403;
//   - 503 unavailable      — no control channel is configured at all.
func (s *server) writeKeyError(c *app.RequestContext, operation string, err error) {
	switch {
	case errors.Is(err, organization.ErrNotFound), errors.Is(err, organization.ErrNotMember):
		s.writeError(c, http.StatusNotFound, "", "organization not found")
	case errors.Is(err, organization.ErrForbidden):
		s.writeError(c, http.StatusForbidden, "", "action is not permitted for your organization role")
	case errors.Is(err, ErrKeyNotFound), errors.Is(err, controlclient.ErrNotFound), errors.Is(err, ErrKeyOperationNotFound):
		s.writeError(c, http.StatusNotFound, "", "api key not found")
	case errors.Is(err, ErrKeyRequestInvalid), errors.Is(err, controlclient.ErrInvalidArgument):
		s.writeError(c, http.StatusBadRequest, "", keyErrorMessage(err))
	case errors.Is(err, controlclient.ErrConflict):
		s.writeError(c, http.StatusConflict, "", "operation_id was already used with a different payload")
	case errors.Is(err, controlclient.ErrPreconditionFailed):
		message := "the configuration changed concurrently; retry the request"
		if version, ok := controlclient.CurrentVersion(err); ok {
			message = "the configuration changed concurrently (current_version=" + strconv.FormatInt(version, 10) + "); retry the request"
		}
		s.writeError(c, http.StatusConflict, "", message)
	case errors.Is(err, ErrControlUnavailable):
		s.writeError(c, http.StatusServiceUnavailable, "", "the control channel is not configured")
	case errors.Is(err, controlclient.ErrUpstreamAuth), errors.Is(err, controlclient.ErrUnavailable):
		// Deliberately 502: the browser did nothing wrong, and a 401/403 would
		// push the operator to re-login instead of looking at the tunnel.
		s.writeInternalControl(c, operation, err)
	default:
		s.writeInternal(c, operation, err)
	}
}

// writeInternalControl logs the cause and answers 502 upstream_unavailable.
func (s *server) writeInternalControl(c *app.RequestContext, operation string, err error) {
	s.logger.Printf("console control error op=%s request_id=%s err=%v", operation, requestIDFrom(c), err)
	s.writeError(c, http.StatusBadGateway, "", "the control plane is unavailable")
}

// keyErrorMessage strips the sentinel prefix so the client sees only the
// caller-fixable part of the message.
func keyErrorMessage(err error) string {
	message := err.Error()
	for _, prefix := range []string{ErrKeyRequestInvalid.Error() + ": ", controlclient.ErrInvalidArgument.Error() + ": "} {
		if strings.HasPrefix(message, prefix) {
			return strings.TrimPrefix(message, prefix)
		}
	}
	return message
}

// parseOptionalUserID parses an optional user id that may arrive as a JSON
// string (body) or a query parameter.
func parseOptionalUserID(raw *string) (*int64, error) {
	if raw == nil {
		return nil, nil
	}
	return parseOptionalUserIDValue(*raw)
}

func parseOptionalUserIDValue(raw string) (*int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || value <= 0 {
		return nil, errBadPathID
	}
	return &value, nil
}

// parseOptionalTimestamp parses an optional RFC3339 timestamp. Only the format
// is validated here; "must be in the future" belongs to the service.
func parseOptionalTimestamp(raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil, err
	}
	utc := value.UTC()
	return &utc, nil
}

// formatOptionalID renders an optional 64-bit id as a JSON string, or nil.
func formatOptionalID(value *int64) any {
	if value == nil {
		return nil
	}
	return formatID(*value)
}
