package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"llm-gateway/internal/httperr"
)

// contextKey 用于在 r.Context() 中传递租户身份，proxy/metering 后续会用到
type contextKey string

const TenantKey contextKey = "tenant_id"

// keyRecordKey 携带快照解析出来的完整 KeyRecord（而不是只有 tenant）：
// data-plane access guard 需要 key_id/org/owner/expiry/allowlist 才能做
// 权威校验与归属，limiter 需要 org 才能选组织级 RPM 桶。
const keyRecordKey contextKey = "key_record"

// Store 抽象 Key 的来源：现在是内存 map，后续控制面 gRPC 推配置时原子替换整个 Store
type Store interface {
	// Validate 是历史入口：只返回 tenant，供只需要租户维度的调用方使用。
	Validate(rawKey string) (tenantID string, ok bool)
	// Lookup 返回快照里的完整凭据元数据。第二个返回值为 false 表示未知 key。
	Lookup(rawKey string) (KeyRecord, bool)
}

// MemoryStore 内存实现，只存哈希，不存明文，支持热更
type MemoryStore struct {
	mu sync.RWMutex
	// hash -> record
	keys map[string]KeyRecord
}

func NewMemoryStore(keys map[string]string) *MemoryStore {
	// Legacy compatibility constructor. New control-plane paths should use
	// NewMemoryStoreFromRecords so raw keys never cross the config boundary.
	m := &MemoryStore{keys: make(map[string]KeyRecord, len(keys))}
	for k, tenant := range keys {
		record := NewKeyRecord("legacy-"+hash(k), k, tenant)
		m.keys[record.Hash] = record
	}
	return m
}

func NewMemoryStoreFromRecords(records []KeyRecord) *MemoryStore {
	m := &MemoryStore{keys: make(map[string]KeyRecord, len(records))}
	m.UpdateRecords(records)
	return m
}

func (m *MemoryStore) Validate(rawKey string) (string, bool) {
	record, ok := m.Lookup(rawKey)
	return record.TenantID, ok
}

// Lookup resolves a raw key to its snapshot record. It is read-only and never
// consults the database: authoritative org/user/member state is the access
// guard's job (ADR-0003 §2.8).
func (m *MemoryStore) Lookup(rawKey string) (KeyRecord, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.keys[hash(rawKey)]
	return record, ok
}

// Update 热更全量 Keys（控制面推送时调用），原子替换。
func (m *MemoryStore) Update(keys map[string]string) {
	// Legacy compatibility path. It hashes before replacing the snapshot.
	m.UpdateRecords(LegacyRecords(keys))
}

func (m *MemoryStore) UpdateRecords(records []KeyRecord) {
	newKeys := make(map[string]KeyRecord, len(records))
	for _, record := range records {
		// A record without any attribution (no legacy tenant, no org) cannot be
		// attributed to a tenant or guarded, so it is not a usable credential.
		if record.Hash == "" || (record.TenantID == "" && record.OrgID <= 0) {
			continue
		}
		if record.Status != "" && record.Status != "active" {
			continue
		}
		newKeys[record.Hash] = record
	}
	m.mu.Lock()
	m.keys = newKeys
	m.mu.Unlock()
}

// RevokeMissing 只撤销当前快照中不存在的 key，不引入新 key。
// Gateway 收到路由配置错误时也可以优先执行撤销，避免一个无效路由阻塞
// 管理员刚刚删除的凭据；后续完整配置仍需修正后再原子替换。
func (m *MemoryStore) RevokeMissing(keys map[string]string) {
	m.RevokeMissingRecords(LegacyRecords(keys))
}

func (m *MemoryStore) RevokeMissingRecords(records []KeyRecord) {
	allowed := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record.Hash != "" && (record.Status == "" || record.Status == "active") {
			allowed[record.Hash] = struct{}{}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for keyHash := range m.keys {
		if _, ok := allowed[keyHash]; !ok {
			delete(m.keys, keyHash)
		}
	}
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Middleware 校验 Authorization: Bearer sk-xxx
// 通过则把 tenant_id 注入 context，失败直接 401，不进入限流/代理
func Middleware(store Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 健康检查放行
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}

			auth := r.Header.Get("Authorization")
			if auth == "" {
				httperr.WriteStatus(w, r, http.StatusUnauthorized, "missing Authorization")
				return
			}
			scheme, rawKey, ok := strings.Cut(auth, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				httperr.WriteStatus(w, r, http.StatusUnauthorized, "invalid Authorization")
				return
			}
			rawKey = strings.TrimSpace(rawKey)
			if rawKey == "" || strings.IndexAny(rawKey, " \t\r\n") >= 0 {
				httperr.WriteStatus(w, r, http.StatusUnauthorized, "invalid Authorization")
				return
			}

			record, ok := store.Lookup(rawKey)
			if !ok {
				httperr.WriteStatus(w, r, http.StatusUnauthorized, "invalid api key")
				return
			}

			ctx := WithKeyRecord(context.WithValue(r.Context(), TenantKey, record.TenantID), record)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WithKeyRecord attaches the snapshot-resolved credential to the context.
func WithKeyRecord(ctx context.Context, record KeyRecord) context.Context {
	return context.WithValue(ctx, keyRecordKey, record)
}

// KeyFromContext returns the snapshot-resolved credential. false means the
// request never passed the auth middleware (or a caller replaced the context).
func KeyFromContext(ctx context.Context) (KeyRecord, bool) {
	record, ok := ctx.Value(keyRecordKey).(KeyRecord)
	return record, ok
}

// OrgIDFromContext returns the snapshot's organization attribution for the
// request, or 0 when the key is legacy or anonymous.
func OrgIDFromContext(ctx context.Context) int64 {
	record, ok := KeyFromContext(ctx)
	if !ok {
		return 0
	}
	return record.OrgID
}

// TenantFromContext 供 proxy/limiter/metering 读取租户
func TenantFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(TenantKey).(string); ok {
		return v
	}
	return ""
}
