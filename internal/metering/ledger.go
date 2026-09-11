package metering

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib"

	"llm-gateway/migrations"
)

// Ledger 台账接口，内存和 PG 实现通用
type Ledger interface {
	Add(e Event) (bool, error)
	Count() (int, error)
	Balance(tenantID string) (int64, error)
	Snapshot() (map[string]Event, error)
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
	key := eventKey(e)
	if _, ok := l.events[key]; ok {
		return false, nil
	}
	l.events[key] = e
	l.balance[e.TenantID] += e.CostCents
	return true, nil
}
func (l *MemoryLedger) Count() (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events), nil
}
func (l *MemoryLedger) Balance(t string) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balance[t], nil
}
func (l *MemoryLedger) Snapshot() (map[string]Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Event, len(l.events))
	for key, e := range l.events {
		out[key] = e
	}
	return out, nil
}

// PGLedger 生产用 PG 版，UNIQUE(event_id) 兜底幂等
type PGLedger struct {
	db *sql.DB
}

func NewPGLedger(dsn string) (*PGLedger, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrations.Check(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	log.Print("pg ledger connected")
	return &PGLedger{db: db}, nil
}

func (p *PGLedger) Close() error { return p.db.Close() }

func (p *PGLedger) Add(e Event) (bool, error) {
	res, err := p.db.Exec(queryInsertEvent,
		e.EventID, e.TenantID, e.Model, e.Provider, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CostCents, e.CreatedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
func (p *PGLedger) Count() (int, error) {
	var n int
	if err := p.db.QueryRow(queryCountEvents).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (p *PGLedger) Snapshot() (map[string]Event, error) {
	rows, err := p.db.Query(querySnapshotEvents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]Event)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.EventID, &e.TenantID, &e.Model, &e.Provider, &e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.CostCents, &e.CreatedAt); err != nil {
			return nil, err
		}
		out[eventKey(e)] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
func (p *PGLedger) Balance(tenantID string) (int64, error) {
	var v int64
	if err := p.db.QueryRow(queryTenantBalance, tenantID).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// NewLedger 工厂：优先 PG；非强制模式下才允许回退内存。
func NewLedger() (Ledger, error) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://gateway:gateway@localhost:5433/llm_gateway?sslmode=disable"
	}
	if pg, err := NewPGLedger(dsn); err == nil {
		return pg, nil
	} else if errors.Is(err, migrations.ErrSchema) || requirePostgres() {
		return nil, fmt.Errorf("postgres ledger required: %w", err)
	} else {
		log.Printf("pg ledger fallback to memory: %v", err)
		return NewMemoryLedger(), nil
	}
}

func requirePostgres() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("METERING_REQUIRE_POSTGRES")))
	return value == "1" || value == "true" || value == "yes"
}

//go:embed queries/insert_event.sql
var queryInsertEvent string

//go:embed queries/count_events.sql
var queryCountEvents string

//go:embed queries/snapshot_events.sql
var querySnapshotEvents string

//go:embed queries/tenant_balance.sql
var queryTenantBalance string
