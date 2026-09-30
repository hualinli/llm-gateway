package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PGAccountStore is the PostgreSQL account and ledger store.
//
// Every read is scoped by an explicit organization id and every statement
// filters `WHERE org_id = $1`. There is no "list all entries" method, so a
// handler cannot accidentally serve one organization's money to another; a
// mismatched id simply returns not-found.
type PGAccountStore struct {
	DB *sql.DB
	// Timeout bounds one statement or one adjustment transaction. <= 0 means
	// DefaultQueryTimeout.
	Timeout time.Duration
	// AdmissionTimeout bounds one admission transaction (Reserve/MarkExecuting/
	// Release/RecordFailure). The data plane must fail closed rather than wait:
	// an admission that cannot finish inside this bound is a refused request
	// (503), not a slow one. <= 0 means DefaultAdmissionTimeout.
	AdmissionTimeout time.Duration
}

// DefaultAdmissionTimeout bounds one reservation-state transaction. It is
// shorter than DefaultQueryTimeout on purpose: the gateway holds a client
// request open while this runs, and a hot account row must degrade into a
// refusal rather than into a stalled stream. It is longer than the 300ms
// read-only access-guard bound because admission also takes the account row
// lock, which serializes concurrent requests for the same wallet.
const DefaultAdmissionTimeout = 2 * time.Second

// txExecutor is the subset of *sql.DB and *sql.Tx the tx-scoped helpers need.
// Taking the interface (rather than *sql.Tx) is what lets the same posting code
// run either in a transaction this store owns (Adjust) or in one the caller
// owns (AdjustTx), which is how T06C commits a receipt and its money movement
// together.
type txExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Adjustment audit vocabulary. These strings are the stable contract of the
// console_audit_events rows a money movement writes.
const (
	// ActionBillingAdjustment is the audit action of one platform adjustment.
	ActionBillingAdjustment = "billing.adjustment"
	// TargetBillingAccount is the audit target type: the organization's account.
	TargetBillingAccount = "billing_account"
)

// NewPGAccountStore returns the PostgreSQL account store.
func NewPGAccountStore(db *sql.DB) *PGAccountStore { return &PGAccountStore{DB: db} }

const accountColumns = `org_id, currency, posted_balance_micro, reserved_micro,
       status, version, created_at, updated_at`

const entryColumns = `id, org_id, kind, amount_micro, direction, business_id, payload_digest,
       reason, operator_user_id, request_id, posted_at, created_at`

const (
	// ON CONFLICT DO NOTHING makes EnsureAccount idempotent: a second call (or a
	// concurrent one) leaves exactly one row.
	queryEnsureAccount = `INSERT INTO billing_accounts
  (org_id, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
VALUES ($1, 'CNY', 0, 0, 'active', 1, $2, $2)
ON CONFLICT (org_id) DO NOTHING`

	queryGetAccount = `SELECT ` + accountColumns + `
FROM billing_accounts
WHERE org_id = $1`

	// FOR UPDATE is the serialization point of every posting for one
	// organization: the projection update is a read-modify-write, so it must
	// hold the row.
	queryLockAccount = queryGetAccount + ` FOR UPDATE`

	queryUpdateAccount = `UPDATE billing_accounts
SET posted_balance_micro = $2, reserved_micro = $3, version = version + 1, updated_at = $4
WHERE org_id = $1
RETURNING ` + accountColumns

	queryListEntries = `SELECT ` + entryColumns + `
FROM billing_entries
WHERE org_id = $1
  AND ($2 = '' OR kind = $2)
  AND ($3 = 0 OR (posted_at, id) < ($4::timestamptz, $3::bigint))
ORDER BY posted_at DESC, id DESC
LIMIT $5`

	queryGetEntry = `SELECT ` + entryColumns + `
FROM billing_entries
WHERE org_id = $1 AND id = $2`

	// The business key is global (UNIQUE (business_id, kind)), so the replay
	// lookup deliberately does not filter by organization: the caller must see
	// that the key is taken and refuse, never read another organization's row.
	queryGetEntryByBusiness = `SELECT ` + entryColumns + `
FROM billing_entries
WHERE business_id = $1 AND kind = $2`

	queryInsertEntry = `INSERT INTO billing_entries
  (org_id, kind, amount_micro, direction, business_id, payload_digest, reason,
   operator_user_id, request_id, posted_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING ` + entryColumns

	queryInsertAdjustmentAudit = `INSERT INTO console_audit_events
  (actor_user_id, actor_org_id, action, target_type, target_id, outcome, detail, request_id, trace_id, created_at)
VALUES ($1, $2, $3, $4, $5, 'success', $6, $7, NULL, $8)`

	// The ledger is the fact; this is the proof. SUM(bigint) is numeric in
	// PostgreSQL, so the cast to bigint is deliberate: a sum that does not fit
	// in an int64 is an error here, not a silently narrowed number.
	queryEntrySums = `SELECT
  COALESCE(SUM(CASE WHEN kind IN ('credit', 'debit', 'adjustment')
                    THEN CASE WHEN direction = 'credit' THEN amount_micro ELSE -amount_micro END
                    ELSE 0 END), 0)::bigint AS posted,
  COALESCE(SUM(CASE WHEN kind IN ('reservation', 'release')
                    THEN CASE WHEN direction = 'credit' THEN amount_micro ELSE -amount_micro END
                    ELSE 0 END), 0)::bigint AS reserved,
  count(*)::bigint AS entries
FROM billing_entries
WHERE org_id = $1`
)

// EnsureAccount creates the organization's account if needed and returns it.
// The boolean reports whether this call created the row.
func (s *PGAccountStore) EnsureAccount(ctx context.Context, orgID int64) (Account, bool, error) {
	if s == nil || s.DB == nil {
		return Account{}, false, errors.New("billing: account store is not configured")
	}
	if orgID <= 0 {
		return Account{}, false, fmt.Errorf("%w: org_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	now := time.Now().UTC()
	res, err := s.DB.ExecContext(ctx, queryEnsureAccount, orgID, now)
	if err != nil {
		return Account{}, false, mapAccountWriteError(err)
	}
	created, _ := res.RowsAffected()
	account, err := s.getAccount(ctx, orgID)
	if err != nil {
		return Account{}, false, err
	}
	return account, created == 1, nil
}

// GetAccount returns the organization's projection.
func (s *PGAccountStore) GetAccount(ctx context.Context, orgID int64) (Account, error) {
	if s == nil || s.DB == nil {
		return Account{}, errors.New("billing: account store is not configured")
	}
	if orgID <= 0 {
		return Account{}, fmt.Errorf("%w: org_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	return s.getAccount(ctx, orgID)
}

func (s *PGAccountStore) getAccount(ctx context.Context, orgID int64) (Account, error) {
	account, err := scanAccount(s.DB.QueryRowContext(ctx, queryGetAccount, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: org_id=%d", ErrAccountNotFound, orgID)
	}
	if err != nil {
		return Account{}, err
	}
	return account, nil
}

// ListEntries returns one page ordered by (posted_at, id) DESC.
func (s *PGAccountStore) ListEntries(ctx context.Context, params ListEntriesParams) (EntryPage, error) {
	if s == nil || s.DB == nil {
		return EntryPage{}, errors.New("billing: account store is not configured")
	}
	if params.OrgID <= 0 {
		return EntryPage{}, fmt.Errorf("%w: org_id is required", ErrInvalidArgument)
	}
	limit := params.Limit
	if limit <= 0 || limit > 100 {
		return EntryPage{}, fmt.Errorf("%w: limit must be between 1 and 100", ErrInvalidArgument)
	}
	kind := strings.TrimSpace(params.Kind)
	if kind != "" {
		switch kind {
		case KindCredit, KindDebit, KindAdjustment, KindReservation, KindRelease:
		default:
			return EntryPage{}, fmt.Errorf("%w: unknown entry kind %q", ErrInvalidArgument, kind)
		}
	}
	cursorID := int64(0)
	cursorPostedAt := time.Unix(0, 0).UTC()
	if params.Cursor != nil {
		cursorID = params.Cursor.ID
		cursorPostedAt = params.Cursor.PostedAt.UTC()
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := s.DB.QueryContext(ctx, queryListEntries, params.OrgID, kind, cursorID, cursorPostedAt, limit+1)
	if err != nil {
		return EntryPage{}, err
	}
	defer rows.Close()

	items := make([]Entry, 0, limit)
	hasMore := false
	for rows.Next() {
		if len(items) == limit {
			hasMore = true
			break
		}
		entry, err := scanEntry(rows)
		if err != nil {
			return EntryPage{}, err
		}
		items = append(items, entry)
	}
	if err := rows.Err(); err != nil {
		return EntryPage{}, err
	}

	page := EntryPage{Items: items}
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		page.NextCursor = &EntriesCursor{PostedAt: last.PostedAt, ID: last.ID}
	}
	return page, nil
}

// GetEntry returns one entry inside the organization scope. An id that exists
// in another organization is indistinguishable from an unknown id.
func (s *PGAccountStore) GetEntry(ctx context.Context, orgID, entryID int64) (Entry, error) {
	if s == nil || s.DB == nil {
		return Entry{}, errors.New("billing: account store is not configured")
	}
	if orgID <= 0 || entryID <= 0 {
		return Entry{}, fmt.Errorf("%w: org_id and entry id are required", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	entry, err := scanEntry(s.DB.QueryRowContext(ctx, queryGetEntry, orgID, entryID))
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: org_id=%d id=%d", ErrEntryNotFound, orgID, entryID)
	}
	if err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Adjust applies one posting in a single transaction. The order inside the
// transaction is the important part:
//
//  1. make sure the account row exists, then lock it (FOR UPDATE);
//  2. look for an existing entry with the same (business_id, kind): the same
//     digest is a replay, a different digest is a conflict, another
//     organization's row is invisible and also a conflict;
//  3. insert the append-only entry;
//  4. update the projection (posted/reserved/version);
//  5. write the console_audit_events row.
//
// Committing all five together is what makes "money moved" and "money was
// audited" the same fact: a crash rolls back both.
func (s *PGAccountStore) Adjust(ctx context.Context, params AdjustParams) (AdjustmentResult, error) {
	if s == nil || s.DB == nil {
		return AdjustmentResult{}, errors.New("billing: account store is not configured")
	}
	params = NormalizeAdjustment(params)
	if err := ValidateAdjustment(params); err != nil {
		return AdjustmentResult{}, err
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	var result AdjustmentResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.adjustTx(ctx, tx, params)
		return err
	})
	if err != nil {
		return AdjustmentResult{}, err
	}
	return result, nil
}

// AdjustTx applies one posting inside a transaction the caller owns. It exists
// for T06C/T07, which must commit a receipt, an execution transition and the
// money movement as one fact: a crash may leave none of them, never some of
// them. The caller commits or rolls back; this method never commits.
//
// It applies the same validation as Adjust, so a bad posting is refused before
// it touches the caller's transaction.
func (s *PGAccountStore) AdjustTx(ctx context.Context, tx *sql.Tx, params AdjustParams) (AdjustmentResult, error) {
	if s == nil || s.DB == nil {
		return AdjustmentResult{}, errors.New("billing: account store is not configured")
	}
	if tx == nil {
		return AdjustmentResult{}, errors.New("billing: AdjustTx requires a transaction")
	}
	params = NormalizeAdjustment(params)
	if err := ValidateAdjustment(params); err != nil {
		return AdjustmentResult{}, err
	}
	// The caller owns the transaction's lifetime, so only the statements are
	// bounded here.
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	return s.adjustTx(ctx, tx, params)
}

// adjustTx is the one implementation of the five-step posting order documented
// on Adjust; both entry points share it so there is no second, subtly different
// money path.
func (s *PGAccountStore) adjustTx(ctx context.Context, tx txExecutor, params AdjustParams) (AdjustmentResult, error) {
	if _, err := tx.ExecContext(ctx, queryEnsureAccount, params.OrgID, params.Now); err != nil {
		return AdjustmentResult{}, mapAccountWriteError(err)
	}
	account, err := lockAccountTx(ctx, tx, params.OrgID)
	if err != nil {
		return AdjustmentResult{}, err
	}

	existing, err := getEntryByBusinessTx(ctx, tx, params.BusinessID, params.Kind)
	switch {
	case err == nil:
		if existing.OrgID != params.OrgID {
			// The business key is global. Another organization owns it; the
			// caller must never see that row, so this is a conflict.
			return AdjustmentResult{}, fmt.Errorf("%w: business_id=%q is already used by another organization", ErrIdempotencyConflict, params.BusinessID)
		}
		if existing.PayloadDigest != params.PayloadDigest {
			return AdjustmentResult{}, fmt.Errorf("%w: business_id=%q kind=%q", ErrIdempotencyConflict, params.BusinessID, params.Kind)
		}
		return AdjustmentResult{Entry: *existing, Account: account, Replayed: true}, nil
	case errors.Is(err, ErrEntryNotFound):
	default:
		return AdjustmentResult{}, err
	}

	signed := params.AmountMicro
	if params.Direction == DirectionDebit {
		signed = -signed
	}
	posted, reserved, err := projectEntry(account, params.Kind, signed)
	if err != nil {
		return AdjustmentResult{}, err
	}

	entry, err := insertEntryTx(ctx, tx, params)
	if err != nil {
		return AdjustmentResult{}, err
	}
	account, err = updateAccountTx(ctx, tx, params.OrgID, posted, reserved, params.Now)
	if err != nil {
		return AdjustmentResult{}, err
	}
	if err := insertAdjustmentAuditTx(ctx, tx, params, entry, account); err != nil {
		return AdjustmentResult{}, err
	}
	return AdjustmentResult{Entry: *entry, Account: account}, nil
}

// RecalculateProjection recomputes the account from the ledger and compares.
// It never repairs: a mismatch must be investigated, not papered over.
func (s *PGAccountStore) RecalculateProjection(ctx context.Context, orgID int64) (ProjectionCheck, error) {
	if s == nil || s.DB == nil {
		return ProjectionCheck{}, errors.New("billing: account store is not configured")
	}
	if orgID <= 0 {
		return ProjectionCheck{}, fmt.Errorf("%w: org_id is required", ErrInvalidArgument)
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	account, err := s.getAccount(ctx, orgID)
	if err != nil {
		return ProjectionCheck{}, err
	}
	check := ProjectionCheck{OrgID: orgID, Account: account}
	if err := s.DB.QueryRowContext(ctx, queryEntrySums, orgID).
		Scan(&check.EntryPostedMicro, &check.EntryReservedMicro, &check.EntryCount); err != nil {
		return ProjectionCheck{}, err
	}
	check.Matches = check.EntryPostedMicro == account.PostedBalanceMicro &&
		check.EntryReservedMicro == account.ReservedMicro
	return check, nil
}

// projectEntry applies one signed posting to the projection buckets. It is a
// pure function so the arithmetic is unit-testable without PostgreSQL.
func projectEntry(account Account, kind string, signedMicro int64) (posted, reserved int64, err error) {
	switch kind {
	case KindReservation, KindRelease:
		reserved, err = addMicro(account.ReservedMicro, signedMicro)
		if err != nil {
			return 0, 0, err
		}
		if reserved < 0 {
			return 0, 0, fmt.Errorf("%w: releasing %s micro exceeds the reserved %s micro",
				ErrInvalidArgument, FormatMicro(-signedMicro), FormatMicro(account.ReservedMicro))
		}
		return account.PostedBalanceMicro, reserved, nil
	default:
		posted, err = addMicro(account.PostedBalanceMicro, signedMicro)
		if err != nil {
			return 0, 0, err
		}
		return posted, account.ReservedMicro, nil
	}
}

func (s *PGAccountStore) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PGAccountStore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultQueryTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// withAdmissionTimeout bounds one admission-state transaction. An admission is
// on the request's critical path, so it fails closed on its own (shorter) bound
// instead of the read-oriented DefaultQueryTimeout.
func (s *PGAccountStore) withAdmissionTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.AdmissionTimeout
	if timeout <= 0 {
		timeout = DefaultAdmissionTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func lockAccountTx(ctx context.Context, tx txExecutor, orgID int64) (Account, error) {
	account, err := scanAccount(tx.QueryRowContext(ctx, queryLockAccount, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		// EnsureAccount ran first in this transaction, so this can only happen
		// if the organization vanished between the two statements.
		return Account{}, fmt.Errorf("%w: org_id=%d", ErrOrganizationNotFound, orgID)
	}
	if err != nil {
		return Account{}, err
	}
	return account, nil
}

func getEntryByBusinessTx(ctx context.Context, tx txExecutor, businessID, kind string) (*Entry, error) {
	entry, err := scanEntry(tx.QueryRowContext(ctx, queryGetEntryByBusiness, businessID, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: business_id=%q kind=%q", ErrEntryNotFound, businessID, kind)
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

func insertEntryTx(ctx context.Context, tx txExecutor, params AdjustParams) (*Entry, error) {
	entry, err := scanEntry(tx.QueryRowContext(ctx, queryInsertEntry,
		params.OrgID, params.Kind, params.AmountMicro, params.Direction, params.BusinessID,
		params.PayloadDigest, nullableString(params.Reason), nullableID(params.ActorUserID),
		nullableString(params.RequestID), params.Now, params.Now))
	if err != nil {
		return nil, mapEntryWriteError(err)
	}
	return &entry, nil
}

func updateAccountTx(ctx context.Context, tx txExecutor, orgID, posted, reserved int64, now time.Time) (Account, error) {
	account, err := scanAccount(tx.QueryRowContext(ctx, queryUpdateAccount, orgID, posted, reserved, now))
	if err != nil {
		return Account{}, mapAccountWriteError(err)
	}
	return account, nil
}

// insertAdjustmentAuditTx writes the audit row of one money movement *inside*
// the money transaction. The detail carries micro amounts as decimal strings,
// exactly like the API, so a JSONB reader cannot round them through a float.
func insertAdjustmentAuditTx(ctx context.Context, tx txExecutor, params AdjustParams, entry *Entry, account Account) error {
	detail := map[string]any{
		"entry_id":             strconv.FormatInt(entry.ID, 10),
		"kind":                 entry.Kind,
		"direction":            entry.Direction,
		"amount_micro":         FormatMicro(entry.AmountMicro),
		"business_id":          entry.BusinessID,
		"reason":               params.Reason,
		"posted_balance_micro": FormatMicro(account.PostedBalanceMicro),
		"reserved_micro":       FormatMicro(account.ReservedMicro),
		"account_version":      strconv.FormatInt(account.Version, 10),
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("billing: encode adjustment audit detail: %w", err)
	}
	if _, err := tx.ExecContext(ctx, queryInsertAdjustmentAudit,
		nullableID(params.ActorUserID), params.OrgID, ActionBillingAdjustment,
		TargetBillingAccount, strconv.FormatInt(params.OrgID, 10), encoded,
		nullableString(params.RequestID), params.Now); err != nil {
		return err
	}
	return nil
}

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var account Account
	if err := row.Scan(&account.OrgID, &account.Currency, &account.PostedBalanceMicro,
		&account.ReservedMicro, &account.Status, &account.Version,
		&account.CreatedAt, &account.UpdatedAt); err != nil {
		return Account{}, err
	}
	account.CreatedAt = account.CreatedAt.UTC()
	account.UpdatedAt = account.UpdatedAt.UTC()
	return account, nil
}

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var (
		entry     Entry
		reason    sql.NullString
		operator  sql.NullInt64
		requestID sql.NullString
	)
	if err := row.Scan(&entry.ID, &entry.OrgID, &entry.Kind, &entry.AmountMicro, &entry.Direction,
		&entry.BusinessID, &entry.PayloadDigest, &reason, &operator, &requestID,
		&entry.PostedAt, &entry.CreatedAt); err != nil {
		return Entry{}, err
	}
	if reason.Valid {
		value := reason.String
		entry.Reason = &value
	}
	if operator.Valid {
		value := operator.Int64
		entry.OperatorUserID = &value
	}
	if requestID.Valid {
		value := requestID.String
		entry.RequestID = &value
	}
	entry.PostedAt = entry.PostedAt.UTC()
	entry.CreatedAt = entry.CreatedAt.UTC()
	return entry, nil
}

// mapAccountWriteError turns the constraint breaches this store can cause into
// domain errors.
func mapAccountWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23503":
		// billing_accounts.org_id -> organizations(id): the organization does
		// not exist.
		return fmt.Errorf("%w: %s", ErrOrganizationNotFound, pgErr.ConstraintName)
	case "23514":
		// A CHECK refused the projection: reserved < 0 or an amount out of
		// range. Surface it as an argument error, not as a 500.
		return fmt.Errorf("%w: %s", ErrInvalidArgument, pgErr.ConstraintName)
	default:
		return err
	}
}

// mapEntryWriteError maps the two ways an INSERT INTO billing_entries can fail
// on a constraint this store cares about.
func mapEntryWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		// The row was inserted between the replay lookup and this INSERT. Treat
		// it as a conflict: the caller must retry and read the stored result
		// rather than assume its payload was applied.
		return fmt.Errorf("%w: %s", ErrIdempotencyConflict, pgErr.ConstraintName)
	case "23503", "23514", "22001":
		return fmt.Errorf("%w: %s", ErrInvalidArgument, pgErr.ConstraintName)
	default:
		return err
	}
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
