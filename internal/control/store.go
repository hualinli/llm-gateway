package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "llm-gateway/proto/control"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Store 是配置持久化抽象：版本号必须落盘单调，控制面重启不回退。
// 只有控制面一个写入方（管理写路径全部经过 ApplyConfig），
// 故 Save 不加分布式锁；多实例部署时需改为 SELECT max(version) FOR UPDATE 仲裁。
type Store interface {
	Save(ctx context.Context, cfg *pb.Config) error
	// LoadLatest 返回最高版本的配置；空库返回 (nil, nil)。
	LoadLatest(ctx context.Context) (*pb.Config, error)
}

// MemoryStore 无 PG 时的演示回退，行为与旧版纯内存一致。
type MemoryStore struct {
	mu   sync.Mutex
	last *pb.Config
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) Save(_ context.Context, cfg *pb.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = proto.Clone(cfg).(*pb.Config) // 存副本，防止调用方复用对象
	return nil
}

func (m *MemoryStore) LoadLatest(_ context.Context) (*pb.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		return nil, nil
	}
	return proto.Clone(m.last).(*pb.Config), nil
}

// PGStore 落 Postgres：一行一个版本，append-only，天然保留配置变更审计轨迹。
type PGStore struct {
	db *sql.DB
}

func NewPGStore(dsn string) (*PGStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS control_config (
  version BIGINT PRIMARY KEY,
  payload JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &PGStore{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.MigrateLegacyPayloads(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate legacy control payloads: %w", err)
	}
	return store, nil
}

func (p *PGStore) Save(ctx context.Context, cfg *pb.Config) error {
	b, err := protojson.Marshal(cfg)
	if err != nil {
		return err
	}
	// 主键冲突 = 版本竞态，报错拒绝而不是覆盖，防止旧版回写
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO control_config (version, payload) VALUES ($1, $2)`,
		cfg.Version, string(b))
	return err
}

func (p *PGStore) LoadLatest(ctx context.Context) (*pb.Config, error) {
	var raw string
	err := p.db.QueryRowContext(ctx,
		`SELECT payload::text FROM control_config ORDER BY version DESC LIMIT 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := &pb.Config{}
	// DiscardUnknown：旧版本存档遇到 proto 演进字段不至于读不出来
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(raw), cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// MigrateLegacyPayloads removes raw-key maps from historical configuration
// payloads. It preserves the version and converts them to KeyRecords in one
// transaction so a partial migration cannot be observed.
func (p *PGStore) MigrateLegacyPayloads(ctx context.Context) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type migration struct {
		version int64
		payload string
	}
	rows, err := tx.QueryContext(ctx, `SELECT version, payload::text FROM control_config ORDER BY version`)
	if err != nil {
		return err
	}
	var updates []migration
	for rows.Next() {
		var version int64
		var raw string
		if err := rows.Scan(&version, &raw); err != nil {
			rows.Close()
			return err
		}
		cfg := &pb.Config{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(raw), cfg); err != nil {
			rows.Close()
			return fmt.Errorf("version %d: %w", version, err)
		}
		if len(cfg.Keys) == 0 {
			continue
		}
		safe := normalizeConfig(cfg)
		payload, err := protojson.Marshal(safe)
		if err != nil {
			rows.Close()
			return fmt.Errorf("version %d: %w", version, err)
		}
		updates = append(updates, migration{version: version, payload: string(payload)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE control_config SET payload=$1 WHERE version=$2`, update.payload, update.version); err != nil {
			return fmt.Errorf("version %d: %w", update.version, err)
		}
	}
	return tx.Commit()
}

// NewStore 工厂：优先 PG；只有非强制模式才允许回退内存。
func NewStore() (Store, error) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://gateway:gateway@localhost:5433/llm_gateway?sslmode=disable"
	}
	if pg, err := NewPGStore(dsn); err == nil {
		log.Printf("control store: postgres dsn=%s", dsn)
		return pg, nil
	} else if requireControlPostgres() {
		return nil, err
	} else {
		log.Printf("control store fallback to memory: %v", err)
		return NewMemoryStore(), nil
	}
}

func requireControlPostgres() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("CONTROL_REQUIRE_POSTGRES")))
	return value == "1" || value == "true" || value == "yes"
}
