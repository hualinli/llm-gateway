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

// Lookup is the per-event read the bounded transport check uses. The in-memory
// ledger is only the demo fallback; the PG ledger is the production one.
func (l *MemoryLedger) Lookup(tenantID, eventID string) (Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	event, ok := l.events[eventKey(Event{TenantID: tenantID, EventID: eventID})]
	if !ok {
		return Event{}, false, nil
	}
	return event, true, nil
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

// Add writes one usage row. Attribution is written as SQL NULL when unknown
// (legacy key without owner, unguarded gateway, pre-000007 event): a zero
// user_id must never become a real user 0, and an empty key/execution id must
// never become an empty-string identity.
func (p *PGLedger) Add(e Event) (bool, error) {
	res, err := p.db.Exec(queryInsertEvent,
		e.EventID, e.TenantID,
		nullableInt64(e.OrgID), nullableInt64(e.UserID), nullableString(e.KeyID), nullableString(e.ExecutionID),
		e.Model, e.Provider, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CostCents, e.CreatedAt)
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
		var (
			e           Event
			orgID       sql.NullInt64
			userID      sql.NullInt64
			keyID       sql.NullString
			executionID sql.NullString
		)
		if err := rows.Scan(&e.EventID, &e.TenantID, &orgID, &userID, &keyID, &executionID,
			&e.Model, &e.Provider, &e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.CostCents, &e.CreatedAt); err != nil {
			return nil, err
		}
		if orgID.Valid {
			e.OrgID = orgID.Int64
		}
		if userID.Valid {
			e.UserID = userID.Int64
		}
		e.KeyID = keyID.String
		e.ExecutionID = executionID.String
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

// DB exposes the ledger's connection so the T07C reconciliation can read the
// four billing layers (executions, receipts, entries, accounts) with the same
// pool instead of opening a second one. It is a read seam, not an invitation to
// write outside the ledger's own statements.
func (p *PGLedger) DB() *sql.DB { return p.db }

// Lookup answers "does this tenant have a durable row for this event id" with
// one primary-key probe. It is the per-event read the bounded transport check
// uses; the pre-T07C reconciliation instead materialized the whole table with
// Snapshot(), which is exactly what T07C removes.
func (p *PGLedger) Lookup(tenantID, eventID string) (Event, bool, error) {
	if eventID == "" {
		return Event{}, false, nil
	}
	var (
		e           Event
		orgID       sql.NullInt64
		userID      sql.NullInt64
		keyID       sql.NullString
		executionID sql.NullString
	)
	// The column list is exactly querySnapshotEvents': metering_events has no
	// usage_missing / usage_invalid columns (those two flags exist only on the
	// Stream payload), so a read that claimed them would be a lie.
	err := p.db.QueryRow(`SELECT event_id, tenant_id, org_id, user_id, key_id, execution_id,
       model, provider, prompt_tokens, completion_tokens, total_tokens, cost_cents, created_at
FROM metering_events WHERE event_id = $1`, eventID).
		Scan(&e.EventID, &e.TenantID, &orgID, &userID, &keyID, &executionID,
			&e.Model, &e.Provider, &e.PromptTokens, &e.CompletionTokens, &e.TotalTokens,
			&e.CostCents, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	if tenantID != "" && e.TenantID != tenantID {
		return Event{}, false, nil
	}
	e.OrgID = orgID.Int64
	e.UserID = userID.Int64
	e.KeyID = keyID.String
	e.ExecutionID = executionID.String
	e.CreatedAt = e.CreatedAt.UTC()
	return e, true, nil
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

// nullableInt64 maps "unknown" (0 or negative) to SQL NULL so the ledger keeps
// legacy rows distinguishable from a real attribution.
func nullableInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
