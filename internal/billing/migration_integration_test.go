package billing

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"llm-gateway/internal/observability"
	"llm-gateway/migrations"

	migrate "github.com/golang-migrate/migrate/v4"
)

// priceAccountsSchemaVersion is the migration this task adds. The test pins the
// *schema* to 8 rather than migrations.Version, because the parent task owns
// that constant and will raise it once every T05 migration has landed.
const priceAccountsSchemaVersion = 8

// TestMigration000008FreshDownAndUp walks one throwaway schema through the
// exact sequence an operator would: apply every migration from zero, roll the
// new one back, then apply it again. A down migration that only works once (or
// that leaves a shared helper behind) is a production incident waiting to
// happen.
func TestMigration000008FreshDownAndUp(t *testing.T) {
	dsn := os.Getenv("IAM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run the migration integration tests")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("IAM_TEST_POSTGRES_DSN must be a PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "billing_migration_test_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped := parsed.String()

	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	m, err := migrations.Open(scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })

	// 0 -> 1..8.
	if err := m.Migrate(priceAccountsSchemaVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", priceAccountsSchemaVersion, err)
	}
	assertSchemaVersion(t, m, priceAccountsSchemaVersion)
	for _, table := range []string{"price_versions", "billing_accounts", "billing_entries"} {
		if !tableExists(t, db, table) {
			t.Fatalf("%s is missing after the up migration", table)
		}
	}
	if !functionExists(t, db, "billing_reject_mutation") {
		t.Fatal("the append-only trigger function is missing after the up migration")
	}
	// The usage paging indexes are upgraded in place: same names, but the
	// (created_at, event_id) ordering is now fully index-provided.
	for _, index := range []string{"idx_metering_events_org_time", "idx_metering_events_user_time"} {
		if got := indexDefinition(t, db, index); got != "btree ("+indexColumnPrefix(index)+", created_at DESC, event_id DESC)" {
			t.Fatalf("%s definition=%q, want the three-column form", index, got)
		}
	}
	// The tables this migration depends on must still be there (000004/000005).
	for _, table := range []string{"organizations", "users", "console_audit_events", "metering_events"} {
		if !tableExists(t, db, table) {
			t.Fatalf("migration 000008 must not disturb %s", table)
		}
	}

	// 8 -> 7: only the new objects go away.
	if err := m.Migrate(priceAccountsSchemaVersion - 1); err != nil {
		t.Fatalf("migrate down to %d: %v", priceAccountsSchemaVersion-1, err)
	}
	assertSchemaVersion(t, m, priceAccountsSchemaVersion-1)
	for _, table := range []string{"price_versions", "billing_accounts", "billing_entries"} {
		if tableExists(t, db, table) {
			t.Fatalf("%s survived the down migration", table)
		}
	}
	if functionExists(t, db, "billing_reject_mutation") {
		t.Fatal("the append-only trigger function survived the down migration")
	}
	for _, table := range []string{"organizations", "console_audit_events", "metering_events"} {
		if !tableExists(t, db, table) {
			t.Fatalf("the down migration must not drop %s", table)
		}
	}
	// The down migration restores the pre-T05 two-column usage indexes.
	for _, index := range []string{"idx_metering_events_org_time", "idx_metering_events_user_time"} {
		if got := indexDefinition(t, db, index); got != "btree ("+indexColumnPrefix(index)+", created_at DESC)" {
			t.Fatalf("after rollback %s definition=%q, want the 000007 two-column form", index, got)
		}
	}

	// 7 -> 8 again: the migration is idempotent from a clean version 7.
	if err := m.Migrate(priceAccountsSchemaVersion); err != nil {
		t.Fatalf("re-apply migration %d: %v", priceAccountsSchemaVersion, err)
	}
	assertSchemaVersion(t, m, priceAccountsSchemaVersion)
	for _, table := range []string{"price_versions", "billing_accounts", "billing_entries"} {
		if !tableExists(t, db, table) {
			t.Fatalf("%s is missing after the re-applied migration", table)
		}
	}
}

// TestMigration000008RequiresExplicitUp proves the service-side posture: a
// database that has not been migrated reports the schema error instead of
// half-working. migrations.Check is read-only and is the only thing a service
// calls at startup.
func TestMigration000008RequiresExplicitUp(t *testing.T) {
	dsn := os.Getenv("IAM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run the migration integration tests")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "billing_unmigrated_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()

	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Check(db); !errors.Is(err, migrations.ErrSchema) {
		t.Fatalf("Check on an empty schema = %v, want migrations.ErrSchema", err)
	}
}

func assertSchemaVersion(t *testing.T, m *migrate.Migrate, want uint) {
	t.Helper()
	version, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if dirty {
		t.Fatalf("schema is dirty at version %d", version)
	}
	if version != want {
		t.Fatalf("schema version=%d, want %d", version, want)
	}
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var found sql.NullString
	if err := db.QueryRow(`SELECT to_regclass($1)::text`, table).Scan(&found); err != nil {
		t.Fatalf("to_regclass(%s): %v", table, err)
	}
	return found.Valid && found.String != ""
}

// indexColumnPrefix maps an index name back to its leading column, so the
// assertion is about the ordering columns rather than the index name.
func indexColumnPrefix(index string) string {
	if index == "idx_metering_events_org_time" {
		return "org_id"
	}
	return "user_id"
}

func indexDefinition(t *testing.T, db *sql.DB, index string) string {
	t.Helper()
	var definition sql.NullString
	if err := db.QueryRow(`SELECT indexdef FROM pg_indexes
 WHERE schemaname = current_schema() AND indexname = $1`, index).Scan(&definition); err != nil {
		t.Fatalf("look up index %s: %v", index, err)
	}
	if !definition.Valid {
		return ""
	}
	// The full definition is "CREATE INDEX name ON schema.table USING btree (...)";
	// only the column list is compared.
	raw := definition.String
	open := strings.Index(raw, " USING ")
	return raw[open+len(" USING "):]
}

func functionExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE p.proname = $1 AND n.nspname = current_schema()`, name).Scan(&count); err != nil {
		t.Fatalf("look up function %s: %v", name, err)
	}
	return count > 0
}
