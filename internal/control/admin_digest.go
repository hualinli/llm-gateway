package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "llm-gateway/proto/control"
)

// Operation kinds recorded in control_operations.kind.
const (
	operationCreateAPIKey   = "create_api_key"
	operationRevokeAPIKey   = "revoke_api_key"
	operationApplyOrgPolicy = "apply_org_policy"
)

// ErrAdminInvalidArgument marks a caller-fixable AdminService request. The
// gRPC layer maps it to INVALID_ARGUMENT.
var ErrAdminInvalidArgument = errors.New("invalid admin request")

// maxAllowlistModels bounds one model allowlist so a single request cannot
// force an unbounded JSONB value or token bucket comparison.
const maxAllowlistModels = 256

const maxModelNameLength = 128

// canonicalModelAllowlist normalizes a tri-state allowlist and returns its
// canonical JSON. Message presence is meaningful and is preserved:
//
//	nil            -> "null" (inherit the parent scope)
//	&{} (no models)-> "[]"   (explicit deny-all)
//	models         -> sorted, de-duplicated JSON array
//
// The canonical string is used both for the stored JSONB value and for the
// idempotency digest, so two requests that list the same models in a different
// order produce the same payload digest.
func canonicalModelAllowlist(list *pb.ModelAllowlist) (string, []string, error) {
	if list == nil {
		return "null", nil, nil
	}
	if len(list.GetModels()) > maxAllowlistModels {
		return "", nil, fmt.Errorf("%w: model_allowlist has more than %d models", ErrAdminInvalidArgument, maxAllowlistModels)
	}
	models := make([]string, 0, len(list.GetModels()))
	seen := make(map[string]struct{}, len(list.GetModels()))
	for _, raw := range list.GetModels() {
		model := strings.TrimSpace(raw)
		if model == "" {
			return "", nil, fmt.Errorf("%w: model_allowlist contains an empty model name", ErrAdminInvalidArgument)
		}
		if len(model) > maxModelNameLength {
			return "", nil, fmt.Errorf("%w: model name exceeds %d bytes", ErrAdminInvalidArgument, maxModelNameLength)
		}
		if strings.ContainsAny(model, "\x00\r\n") {
			return "", nil, fmt.Errorf("%w: model name contains a control character", ErrAdminInvalidArgument)
		}
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	sort.Strings(models)
	encoded, err := json.Marshal(models)
	if err != nil {
		return "", nil, fmt.Errorf("%w: model_allowlist is not encodable", ErrAdminInvalidArgument)
	}
	return string(encoded), models, nil
}

// decodeModelAllowlist converts the stored JSONB value back into the proto
// tri-state. NULL stays NULL; "[]" becomes a present-but-empty allowlist.
func decodeModelAllowlist(raw []byte) (*pb.ModelAllowlist, error) {
	if raw == nil {
		return nil, nil
	}
	var models []string
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, fmt.Errorf("decode model_allowlist: %w", err)
	}
	if models == nil {
		models = []string{}
	}
	return &pb.ModelAllowlist{Models: models}, nil
}

type digestField struct {
	name  string
	value string
}

// operationDigest hashes the canonical business fields of one AdminService
// mutation. Fields are length-prefixed so no value can be confused with a
// separator, which keeps the digest deterministic across processes and
// releases.
func operationDigest(kind string, fields ...digestField) string {
	digest := sha256.New()
	writeDigestField(digest, "kind", kind)
	for _, field := range fields {
		writeDigestField(digest, field.name, field.value)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeDigestField(digest hash.Hash, name, value string) {
	fmt.Fprintf(digest, "%d:%s=%d:%s;", len(name), name, len(value), value)
}

// createAPIKeyDigest covers only the fields that change the effect of a
// CreateApiKey call. request_id and the actor triple are excluded on purpose:
// a Console retry with a new request id must still replay the stored result.
// The raw key and its generated key_id are excluded because they are outputs,
// not inputs.
func createAPIKeyDigest(orgID int64, ownerUserID *int64, expiresAt time.Time, hasExpiry bool, allowlist string) string {
	return operationDigest(operationCreateAPIKey,
		digestField{"org_id", strconv.FormatInt(orgID, 10)},
		digestField{"owner_user_id", optionalInt64Digest(ownerUserID)},
		digestField{"expires_at", timestampDigest(expiresAt, hasExpiry)},
		digestField{"model_allowlist", allowlist},
	)
}

func revokeAPIKeyDigest(keyID string, orgID int64) string {
	return operationDigest(operationRevokeAPIKey,
		digestField{"key_id", strings.TrimSpace(keyID)},
		digestField{"org_id", strconv.FormatInt(orgID, 10)},
	)
}

func applyOrgPolicyDigest(orgID int64, allowlist string, requestsPerMinute, burst *int32) string {
	return operationDigest(operationApplyOrgPolicy,
		digestField{"org_id", strconv.FormatInt(orgID, 10)},
		digestField{"model_allowlist", allowlist},
		digestField{"requests_per_minute", optionalInt32Digest(requestsPerMinute)},
		digestField{"burst", optionalInt32Digest(burst)},
	)
}

func optionalInt64Digest(value *int64) string {
	if value == nil {
		return "null"
	}
	return strconv.FormatInt(*value, 10)
}

func optionalInt32Digest(value *int32) string {
	if value == nil {
		return "null"
	}
	return strconv.FormatInt(int64(*value), 10)
}

// timestampDigest normalizes an expiry to whole UTC seconds. Sub-second
// precision is not part of the key effect, so a retry that regenerates the
// same Timestamp at a different nanosecond still replays.
func timestampDigest(value time.Time, present bool) string {
	if !present {
		return "null"
	}
	return strconv.FormatInt(value.UTC().Truncate(time.Second).Unix(), 10)
}
