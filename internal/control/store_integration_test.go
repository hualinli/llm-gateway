package control

import (
	"context"
	"database/sql"
	"llm-gateway/migrations"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	pb "llm-gateway/proto/control"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresMigratesLegacyControlPayload(t *testing.T) {
	dsn := os.Getenv("CONTROL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set CONTROL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	if err := migrations.Up(dsn); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS control_config (
  version BIGINT PRIMARY KEY,
  payload JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		t.Fatal(err)
	}
	const version int64 = 900000001
	_, _ = db.ExecContext(ctx, `DELETE FROM control_config WHERE version=$1`, version)
	legacyPayload, err := protojson.Marshal(&pb.Config{
		Version: version,
		Keys:    map[string]string{"legacy-key": "tenant-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO control_config(version, payload) VALUES($1, $2)`, version, string(legacyPayload)); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(context.Background(), `DELETE FROM control_config WHERE version=$1`, version)

	store, err := NewPGStore(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	if stats := store.db.Stats(); stats.MaxOpenConnections <= 0 || stats.MaxOpenConnections > 16 {
		t.Fatalf("control PostgreSQL pool is not bounded: MaxOpenConnections=%d", stats.MaxOpenConnections)
	}
	cfg, err := store.LoadLatest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Keys) != 0 || len(cfg.KeyRecords) != 1 || cfg.KeyRecords[0].TenantId != "tenant-a" {
		t.Fatalf("legacy payload was not migrated: %+v", cfg)
	}
	var payload string
	if err := db.QueryRowContext(ctx, `SELECT payload::text FROM control_config WHERE version=$1`, version).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if containsRawKey(payload, "legacy-key") {
		t.Fatalf("migrated payload still contains raw key: %s", payload)
	}
}

func containsRawKey(payload, raw string) bool {
	return len(raw) > 0 && strings.Contains(payload, raw)
}
