package metering

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"llm-gateway/internal/billing"
)

// T07C's four durable reconciliation layers, all read-only and all paged.
//
// Every layer follows the same shape:
//
//  1. one aggregate (`count(*)`) establishes how many facts of the layer the
//     window contains — the database computes it, nothing is shipped to Go;
//  2. a keyset query enumerates *only the rows that break an invariant*, one
//     bounded page at a time, ordered by the same tuple the cursor carries;
//  3. at most MaxSamples concrete differences are retained; the count is exact
//     for the pages that were read.
//
// The unhealthy path is therefore the only path that transfers rows, and it is
// bounded by (MaxPages × PageSize + MaxSamples) per layer. The healthy path
// transfers one row per aggregate. That is what makes the 100k-row test's
// "rows fetched" claim meaningful: a consistent database with 100k facts
// fetches tens of rows, not 100k.
//
// Business-id vocabulary the layers understand. A ledger entry that names an
// execution is written by one of two conventions (internal/billing):
//
//	reservation  billing_execution:reservation:<execution_id>   (ExecutionBusinessID)
//	release      billing_execution:release:<execution_id>       (ExecutionBusinessID)
//	debit        billing_receipt:debit:<execution_id>           (ReceiptBusinessID, T07 settlement)
//
// The debit spelling is only knowable from the settlement contract, so the SQL
// below hardcodes both strings. TestReconcileBusinessIDPrefixesMatchBilling
// asserts the literals against billing.ReceiptBusinessID/ExecutionBusinessID,
// so a rename on the billing side fails this package's tests instead of
// silently turning every settled execution into a reconciliation difference.
const (
	executionReservationPrefix = "billing_execution:reservation:"
	executionReleasePrefix     = "billing_execution:release:"
	executionDebitPrefix       = "billing_execution:debit:"
	receiptDebitPrefix         = "billing_receipt:debit:"
	receiptReleasePrefix       = "billing_receipt:release:"
)

// reconcileExecutionStatuses is the non-terminal set: an execution in one of
// these states holds its reservation and must never have a consumption debit.
const reconcileLiveExecutionStatuses = `('reserved','executing','receipt_recorded','review_required')`

// reconcileTerminalExecutionStatuses is the set whose reservation has been (or
// must be) returned.
const reconcileTerminalExecutionStatuses = `('settled','released','failed_no_charge')`

// ---------------------------------------------------------------------------
// Layer (a): executions against their reservation/release/debit entries
// ---------------------------------------------------------------------------

const executionLayerDiffSelect = `SELECT e.execution_id, e.org_id, e.status, e.reserved_micro, e.created_at,
       (r.business_id IS NOT NULL) AS has_reservation,
       (rel.business_id IS NOT NULL) AS has_release,
       (d.business_id IS NOT NULL) AS has_debit,
       COALESCE(rc.charge_micro, 0) AS charge_micro,
       (rc.execution_id IS NOT NULL) AS has_receipt
FROM billing_executions e
LEFT JOIN billing_entries r
  ON r.kind = 'reservation'
 AND r.business_id = '` + executionReservationPrefix + `' || e.execution_id
LEFT JOIN billing_entries rel
  ON rel.kind = 'release'
 AND (rel.business_id = '` + executionReleasePrefix + `' || e.execution_id
   OR rel.business_id = '` + receiptReleasePrefix + `' || e.execution_id)
LEFT JOIN billing_entries d
  ON d.kind = 'debit'
 AND (d.business_id = '` + receiptDebitPrefix + `' || e.execution_id
   OR d.business_id = '` + executionDebitPrefix + `' || e.execution_id)
LEFT JOIN usage_receipts rc
  ON rc.execution_id = e.execution_id
WHERE e.created_at >= $1 AND e.created_at < $2`

// executionLayerDiffPredicate is the exact set of execution-layer invariants:
//
//  1. settled  => a debit exists (unless the receipt's charge is 0, which the
//     ledger's amount_micro > 0 CHECK forbids posting);
//  2. terminal => a reservation that held funds has been released;
//  3. live     => no debit (a charge before settlement is a double-charge risk);
//  4. reserved => a reservation entry exists.
const executionLayerDiffPredicate = `
  AND (
      (e.status = 'settled' AND COALESCE(rc.charge_micro, 0) > 0 AND d.business_id IS NULL)
   OR (e.status IN ` + reconcileTerminalExecutionStatuses + ` AND e.reserved_micro > 0 AND rel.business_id IS NULL)
   OR (e.status IN ` + reconcileLiveExecutionStatuses + ` AND d.business_id IS NOT NULL)
   OR (e.reserved_micro > 0 AND r.business_id IS NULL)
  )`

const (
	queryExecutionLayerDiff = executionLayerDiffSelect + executionLayerDiffPredicate + `
ORDER BY e.created_at, e.execution_id
LIMIT $3`

	queryExecutionLayerDiffAfterCursor = executionLayerDiffSelect + executionLayerDiffPredicate + `
  AND (e.created_at, e.execution_id) > ($3, $4)
ORDER BY e.created_at, e.execution_id
LIMIT $5`

	queryExecutionLayerCount = `SELECT count(*) FROM billing_executions
WHERE created_at >= $1 AND created_at < $2`
)

// executionLayerRow is one difference candidate of layer (a).
type executionLayerRow struct {
	ExecutionID    string
	OrgID          int64
	Status         string
	ReservedMicro  int64
	CreatedAt      time.Time
	HasReservation bool
	HasRelease     bool
	HasDebit       bool
	ChargeMicro    int64
	HasReceipt     bool
}

func reconcileExecutionsLayer(ctx context.Context, params ReconcileParams, maxSamples int) (LayerReport, error) {
	layer := newLayerReport(ReconcileLayerExecutions, "created_at", maxSamples)
	if err := params.DB.QueryRowContext(ctx, queryExecutionLayerCount, params.Since, params.Until).
		Scan(&layer.Checked); err != nil {
		return layer, fmt.Errorf("count executions in the window: %w", err)
	}
	cursor, complete, err := pageLayer(ctx, params, params.Since,
		queryExecutionLayerDiff, queryExecutionLayerDiffAfterCursor,
		func(rows *sql.Rows) (executionLayerRow, error) {
			var row executionLayerRow
			err := rows.Scan(&row.ExecutionID, &row.OrgID, &row.Status, &row.ReservedMicro, &row.CreatedAt,
				&row.HasReservation, &row.HasRelease, &row.HasDebit, &row.ChargeMicro, &row.HasReceipt)
			return row, err
		},
		func(row executionLayerRow) []ReconcileDiff {
			return executionLayerDiffs(params, row)
		},
		&layer, maxSamples, func(row executionLayerRow) rowCursor {
			return rowCursor{Time: row.CreatedAt, Arg: row.ExecutionID, ID: row.ExecutionID}
		})
	if err != nil {
		return layer, err
	}
	layer.HighWater, layer.Truncated = highWater(cursor, complete, params.Until)
	layer.Consistent = layer.Diffs == 0
	return layer, nil
}

// executionLayerDiffs turns one suspicious execution row into the concrete
// differences it breaks. One row can break several invariants at once, which is
// why the loop does not stop at the first.
func executionLayerDiffs(params ReconcileParams, row executionLayerRow) []ReconcileDiff {
	diffs := make([]ReconcileDiff, 0, 3)
	base := ReconcileDiff{
		Layer:       ReconcileLayerExecutions,
		OrgID:       row.OrgID,
		ExecutionID: row.ExecutionID,
		DetectedAt:  params.Until,
	}
	if row.Status == billing.StatusSettled && row.ChargeMicro > 0 && !row.HasDebit {
		diff := base
		diff.Kind = DiffExecutionMissingDebit
		diff.Expected = fmt.Sprintf("settled execution with charge %d has a consumption debit", row.ChargeMicro)
		diff.Observed = "no debit ledger entry for this execution"
		amount := row.ChargeMicro
		diff.DeltaMicro = &amount
		diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerExecutions, diff.Kind, row.ExecutionID)
		diffs = append(diffs, diff)
	}
	if isTerminalExecutionStatus(row.Status) && row.ReservedMicro > 0 && !row.HasRelease {
		diff := base
		diff.Kind = DiffExecutionMissingRelease
		diff.Expected = fmt.Sprintf("terminal execution %s returns its %d micro reservation", row.Status, row.ReservedMicro)
		diff.Observed = "no release ledger entry for this execution"
		amount := row.ReservedMicro
		diff.DeltaMicro = &amount
		diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerExecutions, diff.Kind, row.ExecutionID)
		diffs = append(diffs, diff)
	}
	if !isTerminalExecutionStatus(row.Status) && row.HasDebit {
		diff := base
		diff.Kind = DiffExecutionUnexpectedDebit
		diff.Expected = fmt.Sprintf("execution in status %s has no consumption debit yet", row.Status)
		diff.Observed = "a debit ledger entry exists before settlement"
		diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerExecutions, diff.Kind, row.ExecutionID)
		diffs = append(diffs, diff)
	}
	if row.ReservedMicro > 0 && !row.HasReservation {
		diff := base
		diff.Kind = DiffExecutionMissingReservation
		diff.Expected = fmt.Sprintf("execution holds %d micro and has a reservation entry", row.ReservedMicro)
		diff.Observed = "no reservation ledger entry for this execution"
		amount := row.ReservedMicro
		diff.DeltaMicro = &amount
		diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerExecutions, diff.Kind, row.ExecutionID)
		diffs = append(diffs, diff)
	}
	return diffs
}

func isTerminalExecutionStatus(status string) bool {
	switch status {
	case billing.StatusSettled, billing.StatusReleased, billing.StatusFailedNoCharge:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Layer (b): receipts against their execution and their settlement
// ---------------------------------------------------------------------------

const receiptLayerDiffSelect = `SELECT r.execution_id, r.org_id, r.status, r.charge_micro, r.occurred_at, r.created_at,
       e.status AS execution_status,
       (e.execution_id IS NOT NULL) AS has_execution,
       (d.business_id IS NOT NULL) AS has_debit
FROM usage_receipts r
LEFT JOIN billing_executions e
  ON e.execution_id = r.execution_id
LEFT JOIN billing_entries d
  ON d.kind = 'debit'
 AND (d.business_id = '` + receiptDebitPrefix + `' || r.execution_id
   OR d.business_id = '` + executionDebitPrefix + `' || r.execution_id)
WHERE r.created_at >= $1 AND r.created_at < $2`

// receiptLayerDiffPredicate is the money rule of the logical outbox:
//
//  1. a settled receipt's execution is settled (a receipt cannot outrun its
//     execution);
//  2. a settled receipt with a positive charge has a debit;
//  3. an *unsettled* receipt (pending_dispatch, dispatched, exception) has no
//     debit — settling twice, or settling an exception, is a double-charge.
const receiptLayerDiffPredicate = `
  AND (
      (r.status = 'settled' AND (e.execution_id IS NULL OR e.status <> 'settled'))
   OR (r.status = 'settled' AND r.charge_micro > 0 AND d.business_id IS NULL)
   OR (r.status IN ('pending_dispatch','dispatched','exception') AND d.business_id IS NOT NULL)
  )`

const (
	queryReceiptLayerDiff = receiptLayerDiffSelect + receiptLayerDiffPredicate + `
ORDER BY r.created_at, r.execution_id
LIMIT $3`

	queryReceiptLayerDiffAfterCursor = receiptLayerDiffSelect + receiptLayerDiffPredicate + `
  AND (r.created_at, r.execution_id) > ($3, $4)
ORDER BY r.created_at, r.execution_id
LIMIT $5`

	queryReceiptLayerCount = `SELECT count(*) FROM usage_receipts
WHERE created_at >= $1 AND created_at < $2`

	// The execution side of the same layer: a settled execution must have a
	// receipt, and a terminal non-charged execution must not have a live one.
	receiptLayerExecutionSelect = `SELECT e.execution_id, e.org_id, e.status, e.reserved_micro, e.created_at, r.status AS receipt_status
FROM billing_executions e
LEFT JOIN usage_receipts r
  ON r.execution_id = e.execution_id
WHERE e.created_at >= $1 AND e.created_at < $2`

	receiptLayerExecutionPredicate = `
  AND (
      (e.status = 'settled' AND r.execution_id IS NULL)
   OR (e.status IN ('released','failed_no_charge') AND r.status IN ('pending_dispatch','dispatched'))
  )`

	queryReceiptLayerExecutionDiff = receiptLayerExecutionSelect + receiptLayerExecutionPredicate + `
ORDER BY e.created_at, e.execution_id
LIMIT $3`

	queryReceiptLayerExecutionDiffAfterCursor = receiptLayerExecutionSelect + receiptLayerExecutionPredicate + `
  AND (e.created_at, e.execution_id) > ($3, $4)
ORDER BY e.created_at, e.execution_id
LIMIT $5`

	queryReceiptLayerExecutionCount = `SELECT count(*) FROM billing_executions
WHERE created_at >= $1 AND created_at < $2`
)

type receiptLayerRow struct {
	ExecutionID     string
	OrgID           int64
	Status          string
	ChargeMicro     int64
	OccurredAt      time.Time
	CreatedAt       time.Time
	ExecutionStatus sql.NullString
	HasExecution    bool
	HasDebit        bool
}

type receiptLayerExecutionRow struct {
	ExecutionID   string
	OrgID         int64
	Status        string
	ReservedMicro int64
	CreatedAt     time.Time
	ReceiptStatus sql.NullString
}

func reconcileReceiptsLayer(ctx context.Context, params ReconcileParams, maxSamples int) (LayerReport, error) {
	layer := newLayerReport(ReconcileLayerReceipts, "created_at", maxSamples)
	if err := params.DB.QueryRowContext(ctx, queryReceiptLayerCount, params.Since, params.Until).
		Scan(&layer.Checked); err != nil {
		return layer, fmt.Errorf("count receipts in the window: %w", err)
	}
	if err := params.DB.QueryRowContext(ctx, queryReceiptLayerExecutionCount, params.Since, params.Until).
		Scan(&layer.SecondaryChecked); err != nil {
		return layer, fmt.Errorf("count executions in the window: %w", err)
	}

	cursor, complete, err := pageLayer(ctx, params, params.Since,
		queryReceiptLayerDiff, queryReceiptLayerDiffAfterCursor,
		func(rows *sql.Rows) (receiptLayerRow, error) {
			var (
				row    receiptLayerRow
				execID sql.NullString
			)
			if err := rows.Scan(&row.ExecutionID, &row.OrgID, &row.Status, &row.ChargeMicro,
				&row.OccurredAt, &row.CreatedAt, &execID, &row.HasExecution, &row.HasDebit); err != nil {
				return row, err
			}
			row.ExecutionStatus = execID
			return row, nil
		},
		func(row receiptLayerRow) []ReconcileDiff { return receiptLayerDiffs(params, row) },
		&layer, maxSamples, func(row receiptLayerRow) rowCursor {
			return rowCursor{Time: row.CreatedAt, Arg: row.ExecutionID, ID: row.ExecutionID}
		})
	if err != nil {
		return layer, err
	}
	if !complete {
		layer.HighWater, layer.Truncated = highWater(cursor, complete, params.Until)
		layer.Consistent = layer.Diffs == 0
		return layer, nil
	}

	cursor, complete, err = pageLayer(ctx, params, params.Since,
		queryReceiptLayerExecutionDiff, queryReceiptLayerExecutionDiffAfterCursor,
		func(rows *sql.Rows) (receiptLayerExecutionRow, error) {
			var row receiptLayerExecutionRow
			err := rows.Scan(&row.ExecutionID, &row.OrgID, &row.Status, &row.ReservedMicro, &row.CreatedAt, &row.ReceiptStatus)
			return row, err
		},
		func(row receiptLayerExecutionRow) []ReconcileDiff { return receiptLayerExecutionDiffs(params, row) },
		&layer, maxSamples, func(row receiptLayerExecutionRow) rowCursor {
			return rowCursor{Time: row.CreatedAt, Arg: row.ExecutionID, ID: row.ExecutionID}
		})
	if err != nil {
		return layer, err
	}
	layer.HighWater, layer.Truncated = highWater(cursor, complete, params.Until)
	layer.Consistent = layer.Diffs == 0
	return layer, nil
}

func receiptLayerDiffs(params ReconcileParams, row receiptLayerRow) []ReconcileDiff {
	diffs := make([]ReconcileDiff, 0, 2)
	base := ReconcileDiff{
		Layer:       ReconcileLayerReceipts,
		OrgID:       row.OrgID,
		ExecutionID: row.ExecutionID,
		DetectedAt:  params.Until,
	}
	if row.Status == billing.ReceiptStatusSettled && (!row.HasExecution || row.ExecutionStatus.String != billing.StatusSettled) {
		diff := base
		diff.Kind = DiffReceiptSettledExecutionNotSettled
		diff.Expected = "a settled receipt belongs to a settled execution"
		observed := "the execution row is missing"
		if row.HasExecution {
			observed = "the execution is in status " + row.ExecutionStatus.String
		}
		diff.Observed = observed
		diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerReceipts, diff.Kind, row.ExecutionID)
		diffs = append(diffs, diff)
	}
	if row.Status == billing.ReceiptStatusSettled && row.ChargeMicro > 0 && !row.HasDebit {
		diff := base
		diff.Kind = DiffReceiptSettledMissingDebit
		diff.Expected = fmt.Sprintf("settled receipt of %d micro has a consumption debit", row.ChargeMicro)
		diff.Observed = "no debit ledger entry for this receipt"
		amount := row.ChargeMicro
		diff.DeltaMicro = &amount
		diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerReceipts, diff.Kind, row.ExecutionID)
		diffs = append(diffs, diff)
	}
	if row.Status != billing.ReceiptStatusSettled && row.HasDebit {
		diff := base
		diff.Kind = DiffReceiptUnsettledWithDebit
		diff.Expected = fmt.Sprintf("receipt in status %s has no consumption debit", row.Status)
		diff.Observed = "a debit ledger entry exists for an unsettled receipt"
		diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerReceipts, diff.Kind, row.ExecutionID)
		diffs = append(diffs, diff)
	}
	return diffs
}

func receiptLayerExecutionDiffs(params ReconcileParams, row receiptLayerExecutionRow) []ReconcileDiff {
	base := ReconcileDiff{
		Layer:       ReconcileLayerReceipts,
		OrgID:       row.OrgID,
		ExecutionID: row.ExecutionID,
		DetectedAt:  params.Until,
	}
	if row.Status == billing.StatusSettled && !row.ReceiptStatus.Valid {
		base.Kind = DiffExecutionSettledMissingReceipt
		base.Expected = "a settled execution has a durable usage receipt"
		base.Observed = "no usage receipt row for this execution"
		base.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerReceipts, base.Kind, row.ExecutionID)
		return []ReconcileDiff{base}
	}
	base.Kind = DiffReceiptLiveOnTerminalExecution
	base.Expected = fmt.Sprintf("execution in status %s has no live receipt", row.Status)
	base.Observed = "receipt is still " + row.ReceiptStatus.String
	amount := row.ReservedMicro
	base.DeltaMicro = &amount
	base.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerReceipts, base.Kind, row.ExecutionID)
	return []ReconcileDiff{base}
}

// ---------------------------------------------------------------------------
// Layer (c): ledger entries against the executions they name
// ---------------------------------------------------------------------------

// executionIDFromBusinessID derives the execution id of an execution-scoped
// ledger entry from its business id. The prefixes it knows are the ones the
// reconciliation recognizes; anything else returns false and the entry is not
// treated as execution-scoped (adjustments and credits name no execution).
func executionIDFromBusinessID(businessID string) (string, bool) {
	for _, prefix := range []string{
		executionReservationPrefix, executionReleasePrefix, executionDebitPrefix,
		receiptDebitPrefix, receiptReleasePrefix,
	} {
		if len(businessID) > len(prefix) && businessID[:len(prefix)] == prefix {
			return businessID[len(prefix):], true
		}
	}
	return "", false
}

// entryLayerExecutionCase is the SQL mirror of executionIDFromBusinessID. It is
// a CASE expression rather than a join on a prefix, so the join predicate stays
// `e.execution_id = <expression of b>`, which a nested loop can answer with one
// primary-key probe per candidate row.
const entryLayerExecutionCase = `CASE
       WHEN b.kind = 'reservation' AND b.business_id LIKE '` + executionReservationPrefix + `%'
         THEN substring(b.business_id FROM length('` + executionReservationPrefix + `') + 1)
       WHEN b.kind = 'release' AND b.business_id LIKE '` + executionReleasePrefix + `%'
         THEN substring(b.business_id FROM length('` + executionReleasePrefix + `') + 1)
       WHEN b.kind = 'release' AND b.business_id LIKE '` + receiptReleasePrefix + `%'
         THEN substring(b.business_id FROM length('` + receiptReleasePrefix + `') + 1)
       WHEN b.kind = 'debit' AND b.business_id LIKE '` + receiptDebitPrefix + `%'
         THEN substring(b.business_id FROM length('` + receiptDebitPrefix + `') + 1)
       WHEN b.kind = 'debit' AND b.business_id LIKE '` + executionDebitPrefix + `%'
         THEN substring(b.business_id FROM length('` + executionDebitPrefix + `') + 1)
     END`

const entryLayerDiffSelect = `SELECT b.id, b.org_id, b.kind, b.business_id, b.amount_micro, b.posted_at,
       e.status AS execution_status
FROM billing_entries b
LEFT JOIN billing_executions e
  ON e.execution_id = ` + entryLayerExecutionCase + `
WHERE b.posted_at >= $1 AND b.posted_at < $2
  AND b.kind IN ('reservation', 'release', 'debit')
  AND (` + entryLayerExecutionCase + `) IS NOT NULL`

// entryLayerDiffPredicate is the entry-layer invariant:
//
//	a debit names a settled execution;
//	a release names an execution whose reservation has been returned;
//	a reservation names an execution that exists.
const entryLayerDiffPredicate = `
  AND (
      e.execution_id IS NULL
   OR (b.kind = 'debit' AND e.status <> 'settled')
   OR (b.kind = 'release' AND e.status NOT IN ('settled','released','failed_no_charge'))
  )`

const (
	queryEntryLayerDiff = entryLayerDiffSelect + entryLayerDiffPredicate + `
ORDER BY b.posted_at, b.id
LIMIT $3`

	queryEntryLayerDiffAfterCursor = entryLayerDiffSelect + entryLayerDiffPredicate + `
  AND (b.posted_at, b.id) > ($3, $4)
ORDER BY b.posted_at, b.id
LIMIT $5`

	queryEntryLayerCount = `SELECT count(*) FROM billing_entries
WHERE posted_at >= $1 AND posted_at < $2`
)

type entryLayerRow struct {
	ID              int64
	OrgID           int64
	Kind            string
	BusinessID      string
	AmountMicro     int64
	PostedAt        time.Time
	ExecutionStatus sql.NullString
}

func reconcileEntriesLayer(ctx context.Context, params ReconcileParams, maxSamples int) (LayerReport, error) {
	layer := newLayerReport(ReconcileLayerEntries, "posted_at", maxSamples)
	if err := params.DB.QueryRowContext(ctx, queryEntryLayerCount, params.Since, params.Until).
		Scan(&layer.Checked); err != nil {
		return layer, fmt.Errorf("count ledger entries in the window: %w", err)
	}
	cursor, complete, err := pageLayer(ctx, params, params.Since,
		queryEntryLayerDiff, queryEntryLayerDiffAfterCursor,
		func(rows *sql.Rows) (entryLayerRow, error) {
			var row entryLayerRow
			err := rows.Scan(&row.ID, &row.OrgID, &row.Kind, &row.BusinessID, &row.AmountMicro, &row.PostedAt, &row.ExecutionStatus)
			return row, err
		},
		func(row entryLayerRow) []ReconcileDiff { return entryLayerDiffs(params, row) },
		&layer, maxSamples, func(row entryLayerRow) rowCursor {
			return rowCursor{Time: row.PostedAt, Arg: row.ID, ID: strconv.FormatInt(row.ID, 10)}
		})
	if err != nil {
		return layer, err
	}
	layer.HighWater, layer.Truncated = highWater(cursor, complete, params.Until)
	layer.Consistent = layer.Diffs == 0
	return layer, nil
}

func entryLayerDiffs(params ReconcileParams, row entryLayerRow) []ReconcileDiff {
	executionID, _ := executionIDFromBusinessID(row.BusinessID)
	diff := ReconcileDiff{
		Layer:       ReconcileLayerEntries,
		OrgID:       row.OrgID,
		ExecutionID: executionID,
		BusinessID:  row.BusinessID,
		DetectedAt:  params.Until,
	}
	amount := row.AmountMicro
	if !row.ExecutionStatus.Valid {
		diff.Kind = DiffEntryExecutionMissing
		diff.Expected = fmt.Sprintf("ledger entry %s names an existing execution", row.BusinessID)
		diff.Observed = "no billing_executions row for the named execution"
		diff.DeltaMicro = &amount
		diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerEntries, diff.Kind, row.BusinessID)
		return []ReconcileDiff{diff}
	}
	diff.Kind = DiffEntryKindStatusMismatch
	diff.Expected = fmt.Sprintf("a %s entry belongs to an execution in a compatible state", row.Kind)
	diff.Observed = "the execution is in status " + row.ExecutionStatus.String
	diff.DeltaMicro = &amount
	diff.DedupeKey = billing.ExceptionDedupeKey(ReconcileLayerEntries, diff.Kind, row.BusinessID)
	return []ReconcileDiff{diff}
}

// ---------------------------------------------------------------------------
// Layer (d): account projections against the sum of their ledger
// ---------------------------------------------------------------------------

const (
	// An account projection is cumulative, so this layer has no time window: it
	// is paged by org_id instead. It is the one layer whose "fact" cannot be
	// expressed in [from, to).
	queryAccountLayerPage = `SELECT org_id, posted_balance_micro, reserved_micro, version, updated_at
FROM billing_accounts
WHERE org_id > $1
ORDER BY org_id
LIMIT $2`

	queryAccountLayerCount = `SELECT count(*) FROM billing_accounts`

	queryAccountLedgerSums = `SELECT org_id,
       COALESCE(SUM(CASE WHEN kind IN ('credit', 'debit', 'adjustment')
                         THEN CASE WHEN direction = 'credit' THEN amount_micro ELSE -amount_micro END
                         ELSE 0 END), 0) AS posted_micro,
       COALESCE(SUM(CASE WHEN kind = 'reservation' THEN amount_micro
                         WHEN kind = 'release' THEN -amount_micro
                         ELSE 0 END), 0) AS reserved_micro,
       count(*) AS entries
FROM billing_entries
WHERE org_id = ANY($1)
GROUP BY org_id`

	// Orgs that have a ledger entry in the window but no projection row: their
	// money is real but nothing can answer "what can this org spend".
	queryAccountOrphanCount = `SELECT count(DISTINCT b.org_id) FROM billing_entries b
WHERE b.posted_at >= $1 AND b.posted_at < $2
  AND NOT EXISTS (SELECT 1 FROM billing_accounts a WHERE a.org_id = b.org_id)`

	queryAccountOrphanSample = `SELECT DISTINCT b.org_id FROM billing_entries b
WHERE b.posted_at >= $1 AND b.posted_at < $2
  AND NOT EXISTS (SELECT 1 FROM billing_accounts a WHERE a.org_id = b.org_id)
ORDER BY 1
LIMIT $3`
)

type accountLayerRow struct {
	OrgID              int64
	PostedBalanceMicro int64
	ReservedMicro      int64
	Version            int64
	UpdatedAt          time.Time
}

type accountLedgerSum struct {
	PostedMicro   int64
	ReservedMicro int64
	Entries       int64
}

func reconcileAccountsLayer(ctx context.Context, params ReconcileParams, maxSamples int) (LayerReport, error) {
	layer := newLayerReport(ReconcileLayerAccounts, "org_id", maxSamples)
	if err := params.DB.QueryRowContext(ctx, queryAccountLayerCount).Scan(&layer.Checked); err != nil {
		return layer, fmt.Errorf("count billing accounts: %w", err)
	}

	var cursorOrg int64
	complete := true
	for page := 0; page < params.MaxPages; page++ {
		rows, err := params.DB.QueryContext(ctx, queryAccountLayerPage, cursorOrg, params.PageSize)
		if err != nil {
			return layer, fmt.Errorf("page billing accounts: %w", err)
		}
		accounts := make([]accountLayerRow, 0, params.PageSize)
		for rows.Next() {
			var row accountLayerRow
			if err := rows.Scan(&row.OrgID, &row.PostedBalanceMicro, &row.ReservedMicro, &row.Version, &row.UpdatedAt); err != nil {
				_ = rows.Close()
				return layer, err
			}
			accounts = append(accounts, row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return layer, err
		}
		_ = rows.Close()
		if len(accounts) == 0 {
			break
		}
		layer.Pages++
		layer.RowsFetched += int64(len(accounts))
		cursorOrg = accounts[len(accounts)-1].OrgID

		orgIDs := make([]int64, 0, len(accounts))
		for _, account := range accounts {
			orgIDs = append(orgIDs, account.OrgID)
		}
		sums, err := accountLedgerSums(ctx, params.DB, orgIDs)
		if err != nil {
			return layer, err
		}
		for _, account := range accounts {
			sum := sums[account.OrgID]
			postedDelta := sum.PostedMicro - account.PostedBalanceMicro
			reservedDelta := sum.ReservedMicro - account.ReservedMicro
			if postedDelta == 0 && reservedDelta == 0 {
				continue
			}
			diff := ReconcileDiff{
				Layer:      ReconcileLayerAccounts,
				Kind:       DiffAccountProjectionMismatch,
				OrgID:      account.OrgID,
				BusinessID: fmt.Sprintf("billing_account:%d", account.OrgID),
				Expected: fmt.Sprintf("projection posted=%d reserved=%d equals the ledger sum posted=%d reserved=%d over %d entries",
					account.PostedBalanceMicro, account.ReservedMicro, sum.PostedMicro, sum.ReservedMicro, sum.Entries),
				Observed:   fmt.Sprintf("stored projection posted=%d reserved=%d", account.PostedBalanceMicro, account.ReservedMicro),
				DeltaMicro: &postedDelta,
				DetectedAt: params.Until,
				DedupeKey:  billing.ExceptionDedupeKey(ReconcileLayerAccounts, DiffAccountProjectionMismatch, fmt.Sprintf("org=%d", account.OrgID)),
			}
			layer.addDiff(diff, maxSamples)
		}
		if len(accounts) < params.PageSize {
			break
		}
		if page == params.MaxPages-1 {
			complete = false
		}
	}

	orphans, err := params.DB.QueryContext(ctx, queryAccountOrphanSample, params.Since, params.Until, maxSamples)
	if err != nil {
		return layer, fmt.Errorf("sample ledger orgs without an account: %w", err)
	}
	orphanIDs := make([]int64, 0, maxSamples)
	for orphans.Next() {
		var orgID int64
		if err := orphans.Scan(&orgID); err != nil {
			_ = orphans.Close()
			return layer, err
		}
		orphanIDs = append(orphanIDs, orgID)
	}
	if err := orphans.Err(); err != nil {
		_ = orphans.Close()
		return layer, err
	}
	_ = orphans.Close()
	var orphanCount int64
	if err := params.DB.QueryRowContext(ctx, queryAccountOrphanCount, params.Since, params.Until).Scan(&orphanCount); err != nil {
		return layer, fmt.Errorf("count ledger orgs without an account: %w", err)
	}
	layer.RowsFetched += int64(len(orphanIDs))
	for _, orgID := range orphanIDs {
		diff := ReconcileDiff{
			Layer:      ReconcileLayerAccounts,
			Kind:       DiffAccountMissingForLedger,
			OrgID:      orgID,
			BusinessID: fmt.Sprintf("billing_account:%d", orgID),
			Expected:   "every organization with a ledger entry has an account projection",
			Observed:   "no billing_accounts row for this organization",
			DetectedAt: params.Until,
			DedupeKey:  billing.ExceptionDedupeKey(ReconcileLayerAccounts, DiffAccountMissingForLedger, fmt.Sprintf("org=%d", orgID)),
		}
		layer.addDiff(diff, maxSamples)
	}
	// Orphan organizations beyond the sample are counted but not enumerated; the
	// count is exact, so the report still states the true size of the problem.
	if extra := orphanCount - int64(len(orphanIDs)); extra > 0 {
		layer.Diffs += extra
		layer.SamplesTruncated = true
	}

	if complete {
		end := params.Until.UTC()
		layer.HighWater = &LayerCursor{Time: &end}
	} else {
		layer.HighWater = &LayerCursor{OrgID: int64Ptr(cursorOrg)}
		layer.Truncated = true
	}
	layer.Consistent = layer.Diffs == 0
	sortDiffs(layer.Samples)
	return layer, nil
}

func accountLedgerSums(ctx context.Context, db *sql.DB, orgIDs []int64) (map[int64]accountLedgerSum, error) {
	sums := make(map[int64]accountLedgerSum, len(orgIDs))
	rows, err := db.QueryContext(ctx, queryAccountLedgerSums, orgIDs)
	if err != nil {
		return nil, fmt.Errorf("sum the ledger of an account page: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			orgID int64
			sum   accountLedgerSum
		)
		if err := rows.Scan(&orgID, &sum.PostedMicro, &sum.ReservedMicro, &sum.Entries); err != nil {
			return nil, err
		}
		sums[orgID] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// An account with no ledger row at all sums to zero rather than being
	// absent: "no entries" is a real, comparable projection state.
	for _, orgID := range orgIDs {
		if _, ok := sums[orgID]; !ok {
			sums[orgID] = accountLedgerSum{}
		}
	}
	return sums, nil
}

// ---------------------------------------------------------------------------
// Shared keyset machinery
// ---------------------------------------------------------------------------

// rowCursor is the position of the last row a layer read. Time and ID are the
// reported form; Arg is the value bound to the `(time, id) > ($n, $n+1)`
// comparison, kept separate so a bigint column is compared with a bigint and a
// text column with a text.
type rowCursor struct {
	Time time.Time
	Arg  any
	ID   string
}

func (c rowCursor) layerCursor() LayerCursor {
	return LayerCursor{Time: timePtr(c.Time), ID: c.ID}
}

// pageLayer runs one bounded keyset enumeration.
//
// It reads at most MaxPages pages of PageSize rows, extracting the concrete
// differences each row produces and retaining at most MaxSamples of them. It
// returns the position of the last row it read and whether the layer was
// enumerated to the end of the window.
func pageLayer[Row any](
	ctx context.Context,
	params ReconcileParams,
	since time.Time,
	firstQuery, cursorQuery string,
	scan func(*sql.Rows) (Row, error),
	diffsOf func(Row) []ReconcileDiff,
	layer *LayerReport,
	maxSamples int,
	cursorOf func(Row) rowCursor,
) (rowCursor, bool, error) {
	cursor := rowCursor{Time: since}
	haveCursor := false
	for page := 0; page < params.MaxPages; page++ {
		var (
			rows *sql.Rows
			err  error
		)
		if haveCursor {
			rows, err = params.DB.QueryContext(ctx, cursorQuery,
				params.Since, params.Until, cursor.Time, cursor.Arg, params.PageSize)
		} else {
			rows, err = params.DB.QueryContext(ctx, firstQuery, params.Since, params.Until, params.PageSize)
		}
		if err != nil {
			return cursor, false, fmt.Errorf("page the %s layer: %w", layer.Layer, err)
		}
		count := 0
		for rows.Next() {
			row, err := scan(rows)
			if err != nil {
				_ = rows.Close()
				return cursor, false, err
			}
			count++
			layer.RowsFetched++
			cursor = cursorOf(row)
			haveCursor = true
			for _, diff := range diffsOf(row) {
				layer.addDiff(diff, maxSamples)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return cursor, false, err
		}
		_ = rows.Close()
		if count > 0 {
			layer.Pages++
		}
		if count < params.PageSize {
			sortDiffs(layer.Samples)
			return cursor, true, nil
		}
		if page == params.MaxPages-1 {
			// The page budget ran out with a full page: the layer is not fully
			// enumerated and the cursor is the resume point.
			sortDiffs(layer.Samples)
			return cursor, false, nil
		}
	}
	sortDiffs(layer.Samples)
	return cursor, false, nil
}

// highWater renders the resume position of a layer. A complete enumeration
// reports the exclusive end of the window, which is what an incremental run
// should pass as the next `since`.
func highWater(cursor rowCursor, complete bool, until time.Time) (*LayerCursor, bool) {
	if complete {
		end := until.UTC()
		return &LayerCursor{Time: &end}, false
	}
	position := cursor.layerCursor()
	return &position, true
}

func timePtr(value time.Time) *time.Time {
	utc := value.UTC()
	return &utc
}

func int64Ptr(value int64) *int64 { return &value }
