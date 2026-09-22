package migrations_test

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"

	"llm-gateway/internal/metering"
	"llm-gateway/internal/observability"
	"llm-gateway/migrations"
)

func isolatedDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("METERING_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set METERING_TEST_POSTGRES_DSN")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "migration_test_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return u.String(), db
}

func TestFreshSchemaRequiresExplicitMigration(t *testing.T) {
	dsn, db := isolatedDB(t)
	if _, err := metering.NewPGLedger(dsn); !errors.Is(err, migrations.ErrSchema) {
		t.Fatalf("unmigrated startup: %v", err)
	}
	var table sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('metering_events')::text`).Scan(&table); err != nil || table.Valid {
		t.Fatalf("constructor created table: %v %v", table, err)
	}
	for i := 0; i < 2; i++ {
		if err := migrations.Up(dsn); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Check(db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"metering_events", "control_config", "control_audit_events", "provider_credentials"} {
		var name sql.NullString
		if err := db.QueryRow(`SELECT to_regclass($1)::text`, table).Scan(&name); err != nil || !name.Valid {
			t.Fatalf("missing %s: %v", table, err)
		}
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET dirty=true`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Check(db); !errors.Is(err, migrations.ErrSchema) {
		t.Fatalf("accepted dirty schema: %v", err)
	}
	if err := migrations.Up(dsn); err == nil {
		t.Fatal("migration ignored dirty state")
	}
}

func TestAdoptUnversionedSchemaAndRollback(t *testing.T) {
	for _, alreadyRenamed := range []bool{false, true} {
		name := "request_id"
		if alreadyRenamed {
			name = "event_id"
		}
		t.Run(name, func(t *testing.T) {
			dsn, db := isolatedDB(t)
			// Model both deployed development schemas, with existing billing data.
			m, err := migrations.Open(dsn)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Migrate(1); err != nil {
				t.Fatal(err)
			}
			a, b := m.Close()
			if a != nil || b != nil {
				t.Fatalf("close: %v %v", a, b)
			}
			if _, err := db.Exec(`INSERT INTO metering_events VALUES ('old-event','tenant','m','p',1,2,3,3,now())`); err != nil {
				t.Fatal(err)
			}
			if alreadyRenamed {
				if _, err := db.Exec(`ALTER TABLE metering_events RENAME COLUMN request_id TO event_id`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`DROP TABLE schema_migrations`); err != nil {
				t.Fatal(err)
			}
			if err := migrations.Up(dsn); err != nil {
				t.Fatal(err)
			}
			var id string
			var cost int
			if err := db.QueryRow(`SELECT event_id,cost_cents FROM metering_events`).Scan(&id, &cost); err != nil || id != "old-event" || cost != 3 {
				t.Fatalf("data lost: %s %d %v", id, cost, err)
			}
			m, err = migrations.Open(dsn)
			if err != nil {
				t.Fatal(err)
			}
			// Version 3 adds provider credentials. Roll back to version 1 so the
			// event_id rename is undone and the credential table is removed.
			if err := m.Migrate(1); err != nil {
				t.Fatal(err)
			}
			a, b = m.Close()
			if a != nil || b != nil {
				t.Fatalf("close: %v %v", a, b)
			}
			if err := db.QueryRow(`SELECT request_id FROM metering_events`).Scan(&id); err != nil || id != "old-event" {
				t.Fatalf("rollback lost data: %s %v", id, err)
			}
			if err := migrations.Up(dsn); err != nil {
				t.Fatal(err)
			}
		})
	}
}
