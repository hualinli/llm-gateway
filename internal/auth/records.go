package auth

import "time"

// KeyRecord is the non-secret credential metadata distributed to the Gateway.
// Hash is the full SHA-256 digest of the high-entropy API key; Raw is never a
// field in this type.
//
// The attribution fields below come from the Control-side key projection
// (ADR-0003 section 2.8/2.9). They are optional on purpose: a record restored
// from an older snapshot carries none of them, and such a legacy record must
// keep resolving exactly as before.
type KeyRecord struct {
	ID       string
	Hash     string
	Prefix   string
	TenantID string
	Status   string

	// OrgID is the Control-side organization attribution. Zero means
	// "legacy/unknown": the record predates org attribution, so the data-plane
	// access guard has no authoritative organization row to check. OrgID is
	// never a substitute for the guard's PG read.
	OrgID int64
	// OwnerUserID is nil for legacy or organization-scoped keys that have no
	// user owner. A nil owner keeps the metering user_id NULL.
	OwnerUserID *int64
	// MembershipID is the membership incarnation bound to this key. It is
	// advisory snapshot data; the PostgreSQL guard revalidates the same id
	// against the key's organization and owner before admission.
	MembershipID int64
	// ExpiresAt is nil when the key has no expiry. The value is always UTC.
	ExpiresAt *time.Time
	// ModelAllowlist is the tri-state key-level model restriction from
	// api_keys.model_allowlist: nil inherits the parent scope (org policy) and a
	// non-nil slice, including an empty one, is explicit. An empty slice is
	// deny-all and must never be treated as "no restriction" (ADR-0003 2.7).
	ModelAllowlist *[]string
}

// AllowlistRestricted reports whether the record carries a key-level model
// restriction. false means "inherit", not "deny".
func (r KeyRecord) AllowlistRestricted() bool { return r.ModelAllowlist != nil }

func NewKeyRecord(id, rawKey, tenantID string) KeyRecord {
	hashValue := HashKey(rawKey)
	return KeyRecord{
		ID:       id,
		Hash:     hashValue,
		Prefix:   keyPrefix(rawKey),
		TenantID: tenantID,
		Status:   "active",
	}
}

// LegacyRecords converts the old raw-key map at a compatibility boundary.
// Callers should not persist or distribute the input map after conversion.
func LegacyRecords(keys map[string]string) []KeyRecord {
	out := make([]KeyRecord, 0, len(keys))
	for rawKey, tenantID := range keys {
		hashValue := HashKey(rawKey)
		out = append(out, KeyRecord{
			ID:       "legacy-" + hashValue,
			Hash:     hashValue,
			Prefix:   keyPrefix(rawKey),
			TenantID: tenantID,
			Status:   "active",
		})
	}
	return out
}

func HashKey(rawKey string) string {
	return hash(rawKey)
}

func keyPrefix(rawKey string) string {
	if len(rawKey) <= 2 {
		return "***"
	}
	prefixLen := 8
	if len(rawKey) < prefixLen {
		prefixLen = len(rawKey) / 2
	}
	return rawKey[:prefixLen] + "..."
}
