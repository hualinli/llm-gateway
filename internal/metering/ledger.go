package metering

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Ledger 台账接口，内存和 PG 实现通用
type Ledger interface {
	Add(e Event) (bool, error)
	Count() int
	Balance(tenantID string) int64
	Reconcile(streamCount int) error
}

// MemoryLedger 演示用内存版
type MemoryLedger struct {
	mu      sync.Mutex
	events  map[string]Event
	balance map[string]int64
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{events: make(map[string]Event), balance: make(map[string]int64)}
}

func (l *MemoryLedger) Add(e Event) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.events[e.RequestID]; ok {
		return false, nil
	}
	l.events[e.RequestID] = e
	l.balance[e.TenantID] += e.CostCents
	return true, nil
}
func (l *MemoryLedger) Count() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.events) }
func (l *MemoryLedger) Balance(t string) int64 { l.mu.Lock(); defer l.mu.Unlock(); return l.balance[t] }
func (l *MemoryLedger) Reconcile(streamCount int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.events) != streamCount {
		return fmt.Errorf("mismatch: stream=%d ledger=%d diff=%d", streamCount, len(l.events), streamCount-len(l.events))
	}
	return nil
}

// PGLedger 生产用 PG 版，UNIQUE(request_id) 兜底幂等
type PGLedger struct {
	db *sql.DB
}

func NewPGLedger(dsn string) (*PGLedger, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	// 建表
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS metering_events (
  request_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  model TEXT NOT NULL,
  provider TEXT NOT NULL,
  prompt_tokens INT NOT NULL,
  completion_tokens INT NOT NULL,
  total_tokens INT NOT NULL,
  cost_cents BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tenant ON metering_events(tenant_id);
`)
	if err != nil {
		return nil, err
	}
	log.Printf("pg ledger connected dsn=%s", dsn)
	return &PGLedger{db: db}, nil
}

func (p *PGLedger) Add(e Event) (bool, error) {
	res, err := p.db.Exec(`
INSERT INTO metering_events (request_id, tenant_id, model, provider, prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (request_id) DO NOTHING`,
		e.RequestID, e.TenantID, e.Model, e.Provider, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CostCents, e.CreatedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
func (p *PGLedger) Count() int {
	var n int
	_ = p.db.QueryRow(`SELECT COUNT(*) FROM metering_events`).Scan(&n)
	return n
}
func (p *PGLedger) Balance(tenantID string) int64 {
	var v sql.NullInt64
	_ = p.db.QueryRow(`SELECT COALESCE(SUM(cost_cents),0) FROM metering_events WHERE tenant_id=$1`, tenantID).Scan(&v)
	if v.Valid {
		return v.Int64
	}
	return 0
}
func (p *PGLedger) Reconcile(streamCount int) error {
	c := p.Count()
	if c != streamCount {
		return fmt.Errorf("mismatch: stream=%d ledger=%d diff=%d", streamCount, c, streamCount-c)
	}
	return nil
}

// NewLedger 工厂：优先 PG，失败回退内存
func NewLedger() Ledger {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://gateway:gateway@localhost:5433/llm_gateway?sslmode=disable"
	}
	if pg, err := NewPGLedger(dsn); err == nil {
		return pg
	} else {
		log.Printf("pg ledger fallback to memory: %v", err)
		return NewMemoryLedger()
	}
}
