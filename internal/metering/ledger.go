package metering

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
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

type MemberBill struct {
	UserID     int64 `json:"user_id"`
	CostCents  int64 `json:"cost_cents"`
	EventCount int64 `json:"event_count"`
}

type MemberBillingReader interface {
	MemberBalance(tenantID string, userID int64) (int64, error)
	MemberBills(tenantID string) ([]MemberBill, error)
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

func (l *MemoryLedger) MemberBalance(tenantID string, userID int64) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var total int64
	for _, e := range l.events {
		if e.TenantID == tenantID && e.UserID == userID {
			total += e.CostCents
		}
	}
	return total, nil
}

func (l *MemoryLedger) MemberBills(tenantID string) ([]MemberBill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	byUser := map[int64]*MemberBill{}
	for _, e := range l.events {
		if e.TenantID != tenantID || e.UserID <= 0 {
			continue
		}
		bill := byUser[e.UserID]
		if bill == nil {
			bill = &MemberBill{UserID: e.UserID}
			byUser[e.UserID] = bill
		}
		bill.CostCents += e.CostCents
		bill.EventCount++
	}
	out := make([]MemberBill, 0, len(byUser))
	for _, bill := range byUser {
		out = append(out, *bill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
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
		e.EventID, e.TenantID, e.Model, e.Provider, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CostCents, e.CreatedAt, nullableUserID(e.UserID), nullableString(e.KeyID))
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
		var userID sql.NullInt64
		var keyID sql.NullString
		if err := rows.Scan(&e.EventID, &e.TenantID, &e.Model, &e.Provider, &e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.CostCents, &e.CreatedAt, &userID, &keyID); err != nil {
			return nil, err
		}
		if userID.Valid {
			e.UserID = userID.Int64
		}
		if keyID.Valid {
			e.KeyID = keyID.String
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

func (p *PGLedger) MemberBalance(tenantID string, userID int64) (int64, error) {
	var total int64
	err := p.db.QueryRow(queryMemberBalance, tenantID, userID).Scan(&total)
	return total, err
}

func (p *PGLedger) MemberBills(tenantID string) ([]MemberBill, error) {
	rows, err := p.db.Query(queryMemberBills, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemberBill{}
	for rows.Next() {
		var bill MemberBill
		if err := rows.Scan(&bill.UserID, &bill.CostCents, &bill.EventCount); err != nil {
			return nil, err
		}
		out = append(out, bill)
	}
	return out, rows.Err()
}

func nullableUserID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
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

//go:embed queries/member_balance.sql
var queryMemberBalance string

//go:embed queries/member_bills.sql
var queryMemberBills string
