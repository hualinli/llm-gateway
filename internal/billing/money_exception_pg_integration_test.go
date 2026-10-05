package billing

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/observability"
	"llm-gateway/migrations"
)

// t07SchemaVersion is the migration this task adds. The test pins the *schema*
// rather than migrations.Version: the parent task owns that constant and raises
// it once every T07 migration has landed.
const t07SchemaVersion = 11

// t07TestDB creates a throwaway schema migrated to the T07C schema.
func t07TestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("IAM_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run the billing integration tests")
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
	schema := "billing_t07c_" + id
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

	m, err := migrations.Open(scoped)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(t07SchemaVersion); err != nil {
		t.Fatalf("migrate to %d: %v", t07SchemaVersion, err)
	}
	if a, b := m.Close(); a != nil || b != nil {
		t.Fatalf("close migrator: %v %v", a, b)
	}
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedExceptionOrg(t *testing.T, db *sql.DB, orgID, userID int64, now time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO users (id, email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ($1, $2, 'x', 'active', false, 1, $3, $3)`, userID, "owner"+string(rune('0'+orgID))+"@example.test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO organizations (id, type, name, status, owner_user_id, created_at, updated_at)
VALUES ($1, 'enterprise', $2, 'active', $3, $4, $4)`, orgID, "org-"+string(rune('0'+orgID)), userID, now); err != nil {
		t.Fatal(err)
	}
}

// TestMigration000011VocabularyMatchesTheDatabase compares the Go exception
// vocabularies with the CHECK constraints the migration installs, so the two
// cannot drift: a kind added to one side and not the other must fail here.
func TestMigration000011VocabularyMatchesTheDatabase(t *testing.T) {
	db := t07TestDB(t)
	for constraint, want := range map[string][]string{
		"billing_exceptions_kind_check":       ExceptionKinds,
		"billing_exceptions_status_check":     ExceptionStatuses,
		"billing_exceptions_resolution_check": ExceptionResolutions,
	} {
		got := checkConstraintValues(t, db, constraint)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s = %v, Go vocabulary = %v", constraint, got, want)
		}
	}
	// The audit table's action vocabulary is the exception lifecycle vocabulary.
	actions := checkConstraintValues(t, db, "billing_exception_events_action_check")
	want := []string{ExceptionActionDetected, ExceptionActionRecurred, ExceptionActionResolved, ExceptionActionDismissed, ExceptionActionReopened}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("audit action vocabulary = %v, want %v", actions, want)
	}
}

// checkConstraintValues reads a `col IN (...)` CHECK constraint back as a list.
func checkConstraintValues(t *testing.T, db *sql.DB, constraint string) []string {
	t.Helper()
	var definition string
	// conname is not globally unique: every test schema has its own copy of the
	// constraint, and another test may be dropping its schema concurrently. The
	// namespace join is what makes this a scoped lookup instead of a race.
	err := db.QueryRow(`SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c
JOIN pg_class r ON r.oid = c.conrelid
JOIN pg_namespace n ON n.oid = r.relnamespace
WHERE c.conname = $1 AND n.nspname = current_schema()`, constraint).Scan(&definition)
	if err != nil {
		t.Fatalf("read constraint %s: %v", constraint, err)
	}
	open := strings.Index(definition, "ANY (ARRAY[")
	if open < 0 {
		t.Fatalf("constraint %s is not an IN list: %s", constraint, definition)
	}
	body := definition[open+len("ANY (ARRAY[") : strings.LastIndex(definition, "])")]
	// Each element is rendered as 'value'::text, so the quoted literal is
	// extracted rather than split on commas.
	values := make([]string, 0, 8)
	for _, match := range constraintLiteral.FindAllStringSubmatch(body, -1) {
		values = append(values, match[1])
	}
	return values
}

// constraintLiteral matches a quoted SQL literal inside a CHECK constraint
// definition.
var constraintLiteral = regexp.MustCompile(`'([^']*)'`)

// TestMigration000011DownAndUp walks the new migration through the exact
// sequence an operator would: apply, roll back, apply again.
func TestMigration000011DownAndUp(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("IAM_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run the billing integration tests")
	}
	db := t07TestDB(t)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped := parsed.String()

	for _, table := range []string{"billing_exceptions", "billing_exception_events"} {
		if !tableExists(t, db, table) {
			t.Fatalf("%s is missing after the up migration", table)
		}
	}
	for _, index := range []string{
		"idx_billing_executions_created_id", "idx_usage_receipts_created_id", "idx_billing_entries_posted_id",
	} {
		if !indexExists(t, db, index) {
			t.Fatalf("reconciliation index %s is missing after the up migration", index)
		}
	}

	m, err := migrations.Open(scoped)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(t07SchemaVersion - 1); err != nil {
		t.Fatalf("migrate down to %d: %v", t07SchemaVersion-1, err)
	}
	for _, table := range []string{"billing_exceptions", "billing_exception_events"} {
		if tableExists(t, db, table) {
			t.Fatalf("%s survived the down migration", table)
		}
	}
	for _, index := range []string{
		"idx_billing_executions_created_id", "idx_usage_receipts_created_id", "idx_billing_entries_posted_id",
	} {
		if indexExists(t, db, index) {
			t.Fatalf("reconciliation index %s survived the down migration", index)
		}
	}
	// The down migration must not disturb the tables it depends on.
	for _, table := range []string{"billing_executions", "usage_receipts", "billing_entries", "billing_accounts", "organizations", "users"} {
		if !tableExists(t, db, table) {
			t.Fatalf("the down migration dropped %s", table)
		}
	}
	if err := m.Migrate(t07SchemaVersion); err != nil {
		t.Fatalf("re-apply migration %d: %v", t07SchemaVersion, err)
	}
	if a, b := m.Close(); a != nil || b != nil {
		t.Fatalf("close migrator: %v %v", a, b)
	}
	for _, table := range []string{"billing_exceptions", "billing_exception_events"} {
		if !tableExists(t, db, table) {
			t.Fatalf("%s is missing after the re-applied migration", table)
		}
	}
}

// indexExists reports whether an index of this name exists in the schema the
// connection is scoped to. The shared indexDefinition helper fatals on a missing
// index, which is the opposite of what the down-migration assertions need.
func indexExists(t *testing.T, db *sql.DB, index string) bool {
	t.Helper()
	var found bool
	err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes
  WHERE schemaname = current_schema() AND indexname = $1)`, index).Scan(&found)
	if err != nil {
		t.Fatalf("look up index %s: %v", index, err)
	}
	return found
}

// TestExceptionRecordingIsIdempotentAndAudited covers the durable-queue rules:
// the same observation updates one row instead of creating a second, resolving
// writes the audit row the reviewer needs, and a difference observed again after
// a human closed it becomes an explicit `recurred` audit event instead of a
// silent status reset.
func TestExceptionRecordingIsIdempotentAndAudited(t *testing.T) {
	db := t07TestDB(t)
	store := NewPGExceptionStore(db)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seedExceptionOrg(t, db, 7, 1, now)

	key := ExceptionDedupeKey("accounts", ExceptionKindAccountProjectionMismatch, "org=7")
	first, err := store.RecordException(ctx, RecordExceptionParams{
		Kind:        ExceptionKindAccountProjectionMismatch,
		OrgID:       7,
		AmountMicro: 500,
		Reason:      "projection disagrees with the ledger",
		DedupeKey:   key,
		DetectedAt:  now,
		Now:         now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Exception.Occurrences != 1 || first.Event == nil || first.Event.Action != ExceptionActionDetected {
		t.Fatalf("first record = %+v", first)
	}

	// The next scan observes the same difference: one row, more occurrences.
	later := now.Add(time.Hour)
	second, err := store.RecordException(ctx, RecordExceptionParams{
		Kind:        ExceptionKindAccountProjectionMismatch,
		OrgID:       7,
		AmountMicro: 500,
		Reason:      "projection disagrees with the ledger",
		DedupeKey:   key,
		DetectedAt:  now,
		Now:         later,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.Exception.ID != first.Exception.ID || second.Exception.Occurrences != 2 {
		t.Fatalf("second record = %+v", second)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM billing_exceptions`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("exception rows = %d (%v), want 1", rows, err)
	}

	// A human decides the platform absorbs the difference.
	resolved, err := store.ResolveException(ctx, ResolveExceptionParams{
		ExceptionID: first.Exception.ID,
		ActorUserID: 1,
		Status:      ExceptionStatusResolved,
		Resolution:  ExceptionResolutionWaived,
		Note:        "waived: the drift was a bug in the old projection job",
		Now:         later.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Replayed || resolved.Exception.Status != ExceptionStatusResolved {
		t.Fatalf("resolution = %+v", resolved)
	}
	if resolved.Exception.ResolvedAt == nil || resolved.Exception.ResolvedBy == nil || *resolved.Exception.ResolvedBy != 1 {
		t.Fatalf("the decision is missing its who/when: %+v", resolved.Exception)
	}
	if resolved.Event.Action != ExceptionActionResolved || resolved.Event.ActorUserID == nil ||
		resolved.Event.FromStatus == nil || *resolved.Event.FromStatus != ExceptionStatusOpen ||
		resolved.Event.ToStatus == nil || *resolved.Event.ToStatus != ExceptionStatusResolved ||
		resolved.Event.Note == nil {
		t.Fatalf("the audit row is incomplete: %+v", resolved.Event)
	}

	// The same decision replayed writes no second audit row.
	replay, err := store.ResolveException(ctx, ResolveExceptionParams{
		ExceptionID: first.Exception.ID,
		ActorUserID: 1,
		Status:      ExceptionStatusResolved,
		Resolution:  ExceptionResolutionWaived,
		Note:        "waived: the drift was a bug in the old projection job",
		Now:         later.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed {
		t.Fatalf("an identical decision was not treated as a replay: %+v", replay)
	}
	// A *different* decision on the closed exception is a conflict, not a silent
	// overwrite.
	if _, err := store.ResolveException(ctx, ResolveExceptionParams{
		ExceptionID: first.Exception.ID,
		ActorUserID: 1,
		Status:      ExceptionStatusResolved,
		Resolution:  ExceptionResolutionCharged,
		Note:        "changed my mind",
		Now:         later.Add(3 * time.Minute),
	}); !errors.Is(err, ErrExceptionResolved) {
		t.Fatalf("re-deciding a closed exception = %v, want ErrExceptionResolved", err)
	}

	// The same difference observed again is audited as a recurrence and does not
	// silently reopen the row.
	recurred, err := store.RecordException(ctx, RecordExceptionParams{
		Kind:        ExceptionKindAccountProjectionMismatch,
		OrgID:       7,
		AmountMicro: 500,
		Reason:      "projection disagrees with the ledger",
		DedupeKey:   key,
		DetectedAt:  now,
		Now:         later.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recurred.Replayed || !recurred.Recurred || recurred.Exception.Status != ExceptionStatusResolved {
		t.Fatalf("recurrence = %+v", recurred)
	}
	if recurred.Event == nil || recurred.Event.Action != ExceptionActionRecurred {
		t.Fatalf("recurrence was not audited: %+v", recurred.Event)
	}

	events, err := store.ListExceptionEvents(ctx, 7, first.Exception.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantActions := []string{ExceptionActionDetected, ExceptionActionResolved, ExceptionActionRecurred}
	if len(events) != len(wantActions) {
		t.Fatalf("audit trail = %+v, want %v", events, wantActions)
	}
	for i, action := range wantActions {
		if events[i].Action != action {
			t.Fatalf("audit trail[%d] = %q, want %q", i, events[i].Action, action)
		}
	}

	// Reopening is its own audited decision, and the resolution fields go back to
	// NULL because an open exception has no decision.
	reopened, err := store.ResolveException(ctx, ResolveExceptionParams{
		ExceptionID: first.Exception.ID,
		ActorUserID: 1,
		Status:      ExceptionStatusOpen,
		Resolution:  ExceptionResolutionReopened,
		Note:        "the drift is back after the fix",
		Now:         later.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Exception.Status != ExceptionStatusOpen || reopened.Exception.ResolvedAt != nil ||
		reopened.Exception.Resolution != nil {
		t.Fatalf("reopen left a decision on an open exception: %+v", reopened.Exception)
	}
	if reopened.Event.Action != ExceptionActionReopened {
		t.Fatalf("reopen audit action = %q", reopened.Event.Action)
	}
}

// TestExceptionScopeIsOrganizationBound proves a read cannot cross
// organizations: the same id under the wrong organization is not found, and the
// list is filtered.
func TestExceptionScopeIsOrganizationBound(t *testing.T) {
	db := t07TestDB(t)
	store := NewPGExceptionStore(db)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seedExceptionOrg(t, db, 7, 1, now)
	if _, err := db.Exec(`INSERT INTO organizations (id, type, name, status, owner_user_id, created_at, updated_at)
VALUES (8, 'enterprise', 'org-8', 'active', 1, $1, $1)`, now); err != nil {
		t.Fatal(err)
	}

	written, err := store.RecordException(ctx, RecordExceptionParams{
		Kind:      ExceptionKindUsageMissing,
		OrgID:     7,
		Reason:    "usage missing for exec_1",
		DedupeKey: ExceptionDedupeKey("receipts", ExceptionKindUsageMissing, "exec_1"),
		Now:       now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetException(ctx, 8, written.Exception.ID); !errors.Is(err, ErrExceptionNotFound) {
		t.Fatalf("reading another organization's exception = %v, want ErrExceptionNotFound", err)
	}
	if _, err := store.GetException(ctx, 7, written.Exception.ID); err != nil {
		t.Fatalf("reading the owning organization's exception = %v", err)
	}
	page, err := store.ListExceptions(ctx, ListExceptionsParams{OrgID: 8, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("organization 8 sees organization 7's exceptions: %+v", page.Items)
	}
	page, err = store.ListExceptions(ctx, ListExceptionsParams{OrgID: 7, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Totals.Open != 1 || page.Totals.OldestOpenAt == nil {
		t.Fatalf("organization 7 page = %+v", page)
	}

	// Unknown attribution is a real state, not organization 0: a legacy event
	// with no organization can still be a money exception, and it must be
	// storable without inventing an organization while staying out of every
	// organization-scoped read.
	if _, err := store.RecordException(ctx, RecordExceptionParams{
		Kind:      ExceptionKindPayloadConflict,
		OrgID:     0,
		Reason:    "a legacy event with no attribution conflicts with its ledger row",
		DedupeKey: ExceptionDedupeKey("transport", ExceptionKindPayloadConflict, "tenant-a:legacy"),
		Now:       now,
	}); err != nil {
		t.Fatalf("an exception with unknown attribution was rejected: %v", err)
	}
	var orgID sql.NullInt64
	if err := db.QueryRow(`SELECT org_id FROM billing_exceptions WHERE dedupe_key = $1`,
		ExceptionDedupeKey("transport", ExceptionKindPayloadConflict, "tenant-a:legacy")).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if orgID.Valid {
		t.Fatalf("unknown attribution was stored as organization %d", orgID.Int64)
	}
	for _, scoped := range []int64{7, 8} {
		scopedPage, err := store.ListExceptions(ctx, ListExceptionsParams{OrgID: scoped, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range scopedPage.Items {
			if item.DedupeKey == ExceptionDedupeKey("transport", ExceptionKindPayloadConflict, "tenant-a:legacy") {
				t.Fatalf("an unattributed exception leaked into organization %d's queue", scoped)
			}
		}
	}
	backlog, err := store.ExceptionBacklog(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Open != 2 {
		t.Fatalf("the platform backlog must count the unattributed exception too: %+v", backlog)
	}
}

// TestExceptionListPagingAndBacklog walks the keyset cursor and the backlog
// aggregate, which is what the Console endpoint and the /reconcile metrics read.
func TestExceptionListPagingAndBacklog(t *testing.T) {
	db := t07TestDB(t)
	store := NewPGExceptionStore(db)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seedExceptionOrg(t, db, 7, 1, now)

	for i := 0; i < 5; i++ {
		if _, err := store.RecordException(ctx, RecordExceptionParams{
			Kind:       ExceptionKindOverReserved,
			OrgID:      7,
			Reason:     "charge above reservation",
			DedupeKey:  ExceptionDedupeKey("executions", ExceptionKindOverReserved, "exec-"+string(rune('a'+i))),
			DetectedAt: now.Add(time.Duration(i) * time.Minute),
			Now:        now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	seen := 0
	var cursor *ExceptionCursor
	for page := 0; page < 5; page++ {
		result, err := store.ListExceptions(ctx, ListExceptionsParams{OrgID: 7, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		seen += len(result.Items)
		if result.NextCursor == nil {
			break
		}
		cursor = result.NextCursor
	}
	if seen != 5 {
		t.Fatalf("paging returned %d exceptions, want 5", seen)
	}

	backlog, err := store.ExceptionBacklog(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Open != 5 || backlog.OldestOpenAt == nil || backlog.OldestOpenAge != time.Hour {
		t.Fatalf("backlog = %+v", backlog)
	}

	// The status filter applies to the page *and* to its totals: a filtered page
	// never claims a backlog it is not showing.
	filtered, err := store.ListExceptions(ctx, ListExceptionsParams{OrgID: 7, Status: ExceptionStatusResolved, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 0 || filtered.Totals.Open != 0 || filtered.Totals.Resolved != 0 {
		t.Fatalf("status filter = %+v", filtered)
	}
	unfiltered, err := store.ListExceptions(ctx, ListExceptionsParams{OrgID: 7, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if unfiltered.Totals.Open != 5 {
		t.Fatalf("unfiltered totals = %+v", unfiltered.Totals)
	}
}

// TestExceptionAuditTrailIsAppendOnly is the database-level half of "no silent
// reclassification": the audit table rejects both UPDATE and DELETE, so a
// written decision cannot be rewritten in place.
func TestExceptionAuditTrailIsAppendOnly(t *testing.T) {
	db := t07TestDB(t)
	store := NewPGExceptionStore(db)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seedExceptionOrg(t, db, 7, 1, now)

	written, err := store.RecordException(ctx, RecordExceptionParams{
		Kind:      ExceptionKindPayloadConflict,
		OrgID:     7,
		Reason:    "same business id, different payload",
		DedupeKey: ExceptionDedupeKey("receipts", ExceptionKindPayloadConflict, "exec_1"),
		Now:       now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE billing_exception_events SET note = 'rewritten' WHERE exception_id = $1`, written.Exception.ID); err == nil {
		t.Fatal("the audit trail accepted an UPDATE")
	}
	if _, err := db.Exec(`DELETE FROM billing_exception_events WHERE exception_id = $1`, written.Exception.ID); err == nil {
		t.Fatal("the audit trail accepted a DELETE")
	}
	// The exception row itself is a workflow row: it must be updatable, or a
	// human could never resolve anything.
	if _, err := db.Exec(`UPDATE billing_exceptions SET updated_at = updated_at WHERE id = $1`, written.Exception.ID); err != nil {
		t.Fatalf("the exception row refused an update: %v", err)
	}
	// A closed exception without a resolved_at cannot be written by hand.
	if _, err := db.Exec(`UPDATE billing_exceptions SET status = 'resolved' WHERE id = $1`, written.Exception.ID); err == nil {
		t.Fatal("a resolved status without a resolved_at was accepted")
	}
}
