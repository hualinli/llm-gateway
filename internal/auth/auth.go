package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// contextKey 用于在 r.Context() 中传递租户身份，proxy/metering 后续会用到
type contextKey string

const TenantKey contextKey = "tenant_id"

// Store 抽象 Key 的来源：现在是内存 map，后续控制面 gRPC 推配置时原子替换整个 Store
type Store interface {
	Validate(rawKey string) (tenantID string, ok bool)
}

// MemoryStore 内存实现，只存哈希，不存明文，支持热更
type MemoryStore struct {
	mu sync.RWMutex
	// hash -> tenantID
	keys map[string]string
}

func NewMemoryStore(keys map[string]string) *MemoryStore {
	// Legacy compatibility constructor. New control-plane paths should use
	// NewMemoryStoreFromRecords so raw keys never cross the config boundary.
	m := &MemoryStore{keys: make(map[string]string, len(keys))}
	for k, tenant := range keys {
		m.keys[hash(k)] = tenant
	}
	return m
}

func NewMemoryStoreFromRecords(records []KeyRecord) *MemoryStore {
	m := &MemoryStore{keys: make(map[string]string, len(records))}
	m.UpdateRecords(records)
	return m
}

func (m *MemoryStore) Validate(rawKey string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tenant, ok := m.keys[hash(rawKey)]
	return tenant, ok
}

// Update 热更全量 Keys（控制面推送时调用），原子替换。
func (m *MemoryStore) Update(keys map[string]string) {
	// Legacy compatibility path. It hashes before replacing the snapshot.
	newKeys := make(map[string]string, len(keys))
	for k, tenant := range keys {
		newKeys[hash(k)] = tenant
	}
	m.mu.Lock()
	m.keys = newKeys
	m.mu.Unlock()
}

func (m *MemoryStore) UpdateRecords(records []KeyRecord) {
	newKeys := make(map[string]string, len(records))
	for _, record := range records {
		if record.Hash == "" || record.TenantID == "" || (record.Status != "" && record.Status != "active") {
			continue
		}
		newKeys[record.Hash] = record.TenantID
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
				http.Error(w, "missing Authorization", http.StatusUnauthorized)
				return
			}
			scheme, rawKey, ok := strings.Cut(auth, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				http.Error(w, "invalid Authorization", http.StatusUnauthorized)
				return
			}
			rawKey = strings.TrimSpace(rawKey)
			if rawKey == "" || strings.IndexAny(rawKey, " \t\r\n") >= 0 {
				http.Error(w, "invalid Authorization", http.StatusUnauthorized)
				return
			}

			tenantID, ok := store.Validate(rawKey)
			if !ok {
				http.Error(w, "invalid api key", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), TenantKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// TenantFromContext 供 proxy/limiter/metering 读取租户
func TenantFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(TenantKey).(string); ok {
		return v
	}
	return ""
}
