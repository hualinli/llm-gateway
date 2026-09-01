package auth

// KeyRecord is the non-secret credential metadata distributed to the Gateway.
// Hash is the full SHA-256 digest of the high-entropy API key; Raw is never a
// field in this type.
type KeyRecord struct {
	ID       string
	Hash     string
	Prefix   string
	TenantID string
	Status   string
}

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
	if len(rawKey) <= 8 {
		return rawKey
	}
	return rawKey[:8] + "..."
}
