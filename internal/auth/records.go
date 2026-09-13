package auth

// KeyRecord is the non-secret credential metadata distributed to the Gateway.
// Hash is the full SHA-256 digest of the high-entropy API key; Raw is never a
// field in this type.
type KeyRecord struct {
	ID       string
	Hash     string
	Prefix   string
	TenantID string
	UserID   int64
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

func NewOwnedKeyRecord(id, rawKey, tenantID string, userID int64) KeyRecord {
	record := NewKeyRecord(id, rawKey, tenantID)
	record.UserID = userID
	return record
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
