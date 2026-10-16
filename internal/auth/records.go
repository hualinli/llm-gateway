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

	// 组织编号。0 表示 "legacy/unknown"：记录没有组织归属，不能替代 access guard 的 PG 读取。
	// access guard 没有权威的组织行，无法核对。OrgID 不能替代 access guard 的 PG 读取。
	OrgID int64
	// 用户编号。nil 表示无用户，保留在计量表中的 user_id 为 NULL。
	OwnerUserID *int64
	// 成员身份编号，用于在 PostgreSQL 中核对成员身份。
	MembershipID int64
	// 过期时间。nil 表示永不过期。
	ExpiresAt *time.Time
	// ModelAllowlist 模型访问控制列表。nil 表示不限制，沿用组织的模型策略。
	// 非 nil 切片，包括空切片，表示显式限制。空切片表示拒绝所有模型，不能当作不限制。
	ModelAllowlist *[]string
}

// AllowlistRestricted 报告记录是否携带模型访问控制。false 表示继承，不是拒绝。
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
