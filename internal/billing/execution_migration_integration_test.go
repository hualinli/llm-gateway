package billing

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/observability"
	"llm-gateway/migrations"

	"github.com/jackc/pgx/v5/pgconn"
)

// billingExecutionSchemaVersion is the migration this task adds. Like the T05
// test, it pins the *schema* version rather than migrations.Version: the parent
// task owns that constant and raises it once every T06 migration has landed.
const billingExecutionSchemaVersion = 9

// quotedStatus matches one status literal inside a CHECK definition.
var quotedStatus = regexp.MustCompile(`'([a-z_]+)'`)

// executionSchemaDB creates a throwaway schema and returns a DSN/db scoped to
// it, so a business database is never touched.
func executionSchemaDB(t *testing.T, prefix string) (string, *sql.DB) {
	t.Helper()
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
	schema := prefix + "_" + id
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
	return scoped, db
}

// TestMigration000009FreshDownAndUp walks one throwaway schema through the
// sequence an operator would: migrate everything, roll the new migration back,
// then apply it again. A down migration that only works once, or that leaves the
// execution table behind for 000008's down migration to trip over, is a
// production incident waiting to happen.
func TestMigration000009FreshDownAndUp(t *testing.T) {
	scoped, db := executionSchemaDB(t, "billing_exec_migration_test")

	m, err := migrations.Open(scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })

	// 0 -> 1..9.
	if err := m.Migrate(billingExecutionSchemaVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", billingExecutionSchemaVersion, err)
	}
	assertSchemaVersion(t, m, billingExecutionSchemaVersion)
	if !tableExists(t, db, "billing_executions") {
		t.Fatal("billing_executions is missing after the up migration")
	}

	// The column set is the contract T06C/T07 build on: exactly these sixteen
	// columns, and no others.
	wantColumns := []string{
		"created_at", "execution_id", "gateway_id", "input_tokens_upper", "key_id",
		"model", "org_id", "output_tokens_upper", "price_version_id",
		"receipt_recorded_at", "request_id", "reserved_micro", "status",
		"terminal_reason", "updated_at", "user_id",
	}
	if got := tableColumns(t, db, "billing_executions"); strings.Join(got, ",") != strings.Join(wantColumns, ",") {
		t.Fatalf("columns=%v, want %v", got, wantColumns)
	}

	for index, want := range map[string]string{
		"idx_billing_executions_org_created":    "btree (org_id, created_at DESC)",
		"idx_billing_executions_status_updated": "btree (status, updated_at)",
	} {
		if got := indexDefinition(t, db, index); got != want {
			t.Fatalf("%s definition=%q, want %q", index, got, want)
		}
	}

	// The database CHECK is the frozen status vocabulary; the Go constants and
	// the migration must agree, or two services can spell one state
	// differently.
	check := constraintDefinition(t, db, "billing_executions_status_check", "billing_executions")
	var fromDB []string
	for _, match := range quotedStatus.FindAllStringSubmatch(check, -1) {
		fromDB = append(fromDB, match[1])
	}
	sort.Strings(fromDB)
	fromGo := append([]string(nil), ExecutionStatuses...)
	sort.Strings(fromGo)
	if strings.Join(fromDB, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("the CHECK accepts %v but Go knows %v", fromDB, fromGo)
	}

	// The three foreign keys that keep an execution anchored to real facts.
	for _, name := range []string{
		"billing_executions_org_id_fkey",
		"billing_executions_user_id_fkey",
		"billing_executions_price_version_id_fkey",
	} {
		if !constraintExists(t, db, name, "billing_executions") {
			t.Fatalf("constraint %s is missing", name)
		}
	}

	// 9 -> 8: only the new object goes away.
	if err := m.Migrate(billingExecutionSchemaVersion - 1); err != nil {
		t.Fatalf("migrate down to %d: %v", billingExecutionSchemaVersion-1, err)
	}
	assertSchemaVersion(t, m, billingExecutionSchemaVersion-1)
	if tableExists(t, db, "billing_executions") {
		t.Fatal("billing_executions survived the down migration")
	}
	for _, table := range []string{"price_versions", "billing_accounts", "billing_entries", "organizations", "users"} {
		if !tableExists(t, db, table) {
			t.Fatalf("the down migration must not drop %s", table)
		}
	}
	if !functionExists(t, db, "billing_reject_mutation") {
		t.Fatal("the down migration must not drop the 000008 trigger function")
	}

	// 8 -> 9 again: the migration is idempotent from a clean version 8.
	if err := m.Migrate(billingExecutionSchemaVersion); err != nil {
		t.Fatalf("re-apply migration %d: %v", billingExecutionSchemaVersion, err)
	}
	assertSchemaVersion(t, m, billingExecutionSchemaVersion)
	if !tableExists(t, db, "billing_executions") {
		t.Fatal("billing_executions is missing after the re-applied migration")
	}
}

// TestBillingExecutionsConstraintContract proves the schema refuses the states
// the application is not allowed to produce, so no application bug can write
// them, and that the one thing it must allow (a guarded status transition) is
// allowed.
func TestBillingExecutionsConstraintContract(t *testing.T) {
	scoped, db := executionSchemaDB(t, "billing_exec_constraints_test")
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	orgID := seedOrg(t, db, "exec-constraints")
	now := testTime()
	var priceVersionID int64
	if err := db.QueryRow(`INSERT INTO price_versions
  (model, currency, input_price_micro_per_million, output_price_micro_per_million, effective_at, created_at)
VALUES ('gpt-4o', 'CNY', 1000000, 0, $1, $1) RETURNING id`, now).Scan(&priceVersionID); err != nil {
		t.Fatal(err)
	}

	insert := func(executionID, status string, reserved, priceID int64) error {
		t.Helper()
		_, err := db.Exec(`INSERT INTO billing_executions
  (execution_id, org_id, user_id, key_id, gateway_id, price_version_id, model, status,
   reserved_micro, input_tokens_upper, output_tokens_upper, request_id, created_at, updated_at)
VALUES ($1, $2, NULL, 'key-1', 'gw-1', $3, 'gpt-4o', $4, $5, 100, 0, 'req-1', $6, $6)`,
			executionID, orgID, priceID, status, reserved, now)
		return err
	}

	if err := insert("exec_contract_ok", StatusReserved, 100, priceVersionID); err != nil {
		t.Fatalf("a well-formed execution must insert: %v", err)
	}
	if err := insert("exec_contract_ok", StatusReserved, 100, priceVersionID); !isSQLState(err, "23505") {
		t.Fatalf("duplicate execution_id: %v, want 23505", err)
	}
	if err := insert("exec_contract_status", "processing", 100, priceVersionID); !isSQLState(err, "23514") {
		t.Fatalf("unknown status: %v, want 23514", err)
	}
	if err := insert("exec_contract_negative", StatusReserved, -1, priceVersionID); !isSQLState(err, "23514") {
		t.Fatalf("negative reservation: %v, want 23514", err)
	}
	if err := insert(" exec_contract_padded", StatusReserved, 1, priceVersionID); !isSQLState(err, "23514") {
		t.Fatalf("padded execution_id: %v, want 23514", err)
	}
	if err := insert("exec_contract_price", StatusReserved, 1, priceVersionID+999999); !isSQLState(err, "23503") {
		t.Fatalf("unknown price version: %v, want 23503", err)
	}

	// billing_executions is a state machine row: unlike the ledger it *is*
	// updatable, and a status transition is exactly what T06C/T07 do.
	if _, err := db.Exec(`UPDATE billing_executions SET status='executing', updated_at=$1
 WHERE execution_id='exec_contract_ok'`, now.Add(time.Second)); err != nil {
		t.Fatalf("a status transition must be allowed: %v", err)
	}
	// updated_at is monotonic with created_at.
	if _, err := db.Exec(`UPDATE billing_executions SET updated_at=$1
 WHERE execution_id='exec_contract_ok'`, now.Add(-time.Hour)); !isSQLState(err, "23514") {
		t.Fatalf("going back in time: %v, want 23514", err)
	}
	// A receipt cannot predate the execution.
	if _, err := db.Exec(`UPDATE billing_executions SET receipt_recorded_at=$1
 WHERE execution_id='exec_contract_ok'`, now.Add(-time.Hour)); !isSQLState(err, "23514") {
		t.Fatalf("receipt before creation: %v, want 23514", err)
	}
}

func constraintDefinition(t *testing.T, db *sql.DB, constraint, table string) string {
	t.Helper()
	var definition sql.NullString
	if err := db.QueryRow(`SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
  JOIN pg_namespace n ON n.oid = t.relnamespace
 WHERE c.conname = $1 AND t.relname = $2 AND n.nspname = current_schema()`, constraint, table).Scan(&definition); err != nil {
		t.Fatalf("look up constraint %s: %v", constraint, err)
	}
	if !definition.Valid {
		t.Fatalf("constraint %s does not exist", constraint)
	}
	return definition.String
}

func constraintExists(t *testing.T, db *sql.DB, constraint, table string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
  JOIN pg_namespace n ON n.oid = t.relnamespace
 WHERE c.conname = $1 AND t.relname = $2 AND n.nspname = current_schema()`, constraint, table).Scan(&count); err != nil {
		t.Fatalf("look up constraint %s: %v", constraint, err)
	}
	return count > 0
}

func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT column_name FROM information_schema.columns
 WHERE table_schema = current_schema() AND table_name = $1
 ORDER BY column_name`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func isSQLState(err error, state string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == state
}
