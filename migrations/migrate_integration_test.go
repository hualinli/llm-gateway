package migrations_test

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"llm-gateway/internal/control"
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
	for _, table := range []string{"metering_events", "control_config", "control_audit_events", "users", "tenants", "tenant_memberships"} {
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

func TestDirectoryAndMemberLedgerPersist(t *testing.T) {
	dsn, db := isolatedDB(t)
	if err := migrations.Up(dsn); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	directory := control.NewPGDirectory(db)
	owner, personal, err := directory.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member, _, err := directory.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if owner == member || owner <= 0 || member <= 0 {
		t.Fatalf("invalid user IDs: %d %d", owner, member)
	}
	enterprise, err := directory.CreateEnterprise(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.AddMember(ctx, enterprise, member, control.MembershipMember); err != nil {
		t.Fatal(err)
	}
	if err := directory.AddMember(ctx, enterprise, owner, control.MembershipMember); err == nil {
		t.Fatal("enterprise owner was demoted")
	}
	if err := directory.AddMember(ctx, personal, member, control.MembershipMember); err == nil {
		t.Fatal("personal tenant accepted member")
	}
	ledger, err := metering.NewPGLedger(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	for _, event := range []metering.Event{
		{EventID: "enterprise-one", TenantID: enterprise, UserID: member, KeyID: "key-one", CostCents: 3, CreatedAt: time.Now()},
		{EventID: "enterprise-two", TenantID: enterprise, UserID: member, KeyID: "key-two", CostCents: 4, CreatedAt: time.Now()},
		{EventID: "personal-one", TenantID: personal, UserID: owner, CostCents: 2, CreatedAt: time.Now()},
		{EventID: "legacy", TenantID: enterprise, CostCents: 5, CreatedAt: time.Now()},
	} {
		if _, err := ledger.Add(event); err != nil {
			t.Fatal(err)
		}
	}
	if total, err := ledger.Balance(enterprise); err != nil || total != 12 {
		t.Fatalf("enterprise total %d %v", total, err)
	}
	if total, err := ledger.MemberBalance(enterprise, member); err != nil || total != 7 {
		t.Fatalf("member total %d %v", total, err)
	}
	bills, err := ledger.MemberBills(enterprise)
	if err != nil || len(bills) != 1 || bills[0].UserID != member || bills[0].CostCents != 7 {
		t.Fatalf("member bills %+v %v", bills, err)
	}
	snapshot, err := ledger.Snapshot()
	if err != nil || snapshot[enterprise+"\x00enterprise-one"].KeyID != "key-one" {
		t.Fatalf("snapshot %+v %v", snapshot, err)
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
			if err := m.Steps(-2); err != nil {
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
