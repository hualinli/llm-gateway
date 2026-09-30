package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PGPriceVersionStore is the PostgreSQL price-version store. The table is
// append-only (migration 000008 installs a BEFORE UPDATE OR DELETE trigger), so
// this type exposes exactly the write that is legal: one INSERT.
type PGPriceVersionStore struct {
	DB *sql.DB
	// Timeout bounds one statement. <= 0 means DefaultQueryTimeout.
	Timeout time.Duration
}

// DefaultQueryTimeout bounds a single billing statement. A price list or a
// ledger page that takes longer than this is an operational problem that should
// surface as a 500, not as a hung request.
const DefaultQueryTimeout = 10 * time.Second

// NewPGPriceVersionStore returns the PostgreSQL price-version store.
func NewPGPriceVersionStore(db *sql.DB) *PGPriceVersionStore {
	return &PGPriceVersionStore{DB: db}
}

const priceVersionColumns = `id, model, currency,
       input_price_micro_per_million, output_price_micro_per_million,
       provider_input_cost_micro_per_million, provider_output_cost_micro_per_million,
       effective_at, created_by, created_at`

const (
	queryInsertPriceVersion = `INSERT INTO price_versions
  (model, currency, input_price_micro_per_million, output_price_micro_per_million,
   provider_input_cost_micro_per_million, provider_output_cost_micro_per_million,
   effective_at, created_by, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING ` + priceVersionColumns

	queryListPriceVersions = `SELECT ` + priceVersionColumns + `
FROM price_versions
WHERE ($1 = '' OR model = $1) AND ($2 = 0 OR id < $2)
ORDER BY id DESC
LIMIT $3`

	queryLatestPriceVersion = `SELECT ` + priceVersionColumns + `
FROM price_versions
WHERE model = $1 AND effective_at <= $2
ORDER BY effective_at DESC
LIMIT 1`

	queryGetPriceVersion = `SELECT ` + priceVersionColumns + `
FROM price_versions
WHERE id = $1`
)

// CreatePriceVersion publishes one immutable price version.
func (s *PGPriceVersionStore) CreatePriceVersion(ctx context.Context, params CreatePriceVersionParams) (PriceVersion, error) {
	if s == nil || s.DB == nil {
		return PriceVersion{}, errors.New("billing: price version store is not configured")
	}
	params = NormalizeCreatePriceVersion(params)
	if err := ValidateCreatePriceVersion(params); err != nil {
		return PriceVersion{}, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	row := s.DB.QueryRowContext(ctx, queryInsertPriceVersion,
		params.Model, params.Currency,
		params.InputMicroPerMillion, params.OutputMicroPerMillion,
		nullableMicro(params.ProviderInputCostMicroPerMillion),
		nullableMicro(params.ProviderOutputCostMicroPerMillion),
		params.EffectiveAt, nullableID(params.CreatedBy), params.Now)
	version, err := scanPriceVersion(row)
	if err != nil {
		return PriceVersion{}, mapPriceVersionWriteError(err)
	}
	return version, nil
}

// ListPriceVersions returns one page ordered by id DESC plus the cursor of the
// next page ("" when there is none).
func (s *PGPriceVersionStore) ListPriceVersions(ctx context.Context, params ListPriceVersionsParams) (PriceVersionPage, error) {
	if s == nil || s.DB == nil {
		return PriceVersionPage{}, errors.New("billing: price version store is not configured")
	}
	if err := ValidateListPriceVersions(params); err != nil {
		return PriceVersionPage{}, err
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	model := strings.TrimSpace(params.Model)
	rows, err := s.DB.QueryContext(ctx, queryListPriceVersions, model, params.BeforeID, params.Limit+1)
	if err != nil {
		return PriceVersionPage{}, err
	}
	defer rows.Close()

	items := make([]PriceVersion, 0, params.Limit)
	hasMore := false
	for rows.Next() {
		if len(items) == params.Limit {
			// The extra row only proves that another page exists; it is never
			// returned and the cursor stays on the last returned row.
			hasMore = true
			break
		}
		version, err := scanPriceVersion(rows)
		if err != nil {
			return PriceVersionPage{}, err
		}
		items = append(items, version)
	}
	if err := rows.Err(); err != nil {
		return PriceVersionPage{}, err
	}

	page := PriceVersionPage{Items: items}
	if hasMore && len(items) > 0 {
		page.NextCursor = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	return page, nil
}

// LatestPriceVersion returns the version in effect at `at`.
func (s *PGPriceVersionStore) LatestPriceVersion(ctx context.Context, model string, at time.Time) (PriceVersion, error) {
	if s == nil || s.DB == nil {
		return PriceVersion{}, errors.New("billing: price version store is not configured")
	}
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return PriceVersion{}, ErrInvalidArgument
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	version, err := scanPriceVersion(s.DB.QueryRowContext(ctx, queryLatestPriceVersion, trimmed, at.UTC()))
	if errors.Is(err, sql.ErrNoRows) {
		return PriceVersion{}, fmt.Errorf("%w: model=%q effective_at<=%s", ErrPriceNotFound, trimmed, at.UTC().Format(time.RFC3339))
	}
	if err != nil {
		return PriceVersion{}, err
	}
	return version, nil
}

// GetPriceVersion returns one version by id.
func (s *PGPriceVersionStore) GetPriceVersion(ctx context.Context, id int64) (PriceVersion, error) {
	if s == nil || s.DB == nil {
		return PriceVersion{}, errors.New("billing: price version store is not configured")
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	version, err := scanPriceVersion(s.DB.QueryRowContext(ctx, queryGetPriceVersion, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PriceVersion{}, fmt.Errorf("%w: id=%d", ErrPriceNotFound, id)
	}
	if err != nil {
		return PriceVersion{}, err
	}
	return version, nil
}

func (s *PGPriceVersionStore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultQueryTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func scanPriceVersion(row interface{ Scan(...any) error }) (PriceVersion, error) {
	var (
		version     PriceVersion
		providerIn  sql.NullInt64
		providerOut sql.NullInt64
		createdBy   sql.NullInt64
	)
	if err := row.Scan(&version.ID, &version.Model, &version.Currency,
		&version.InputMicroPerMillion, &version.OutputMicroPerMillion,
		&providerIn, &providerOut,
		&version.EffectiveAt, &createdBy, &version.CreatedAt); err != nil {
		return PriceVersion{}, err
	}
	if providerIn.Valid {
		value := providerIn.Int64
		version.ProviderInputCostMicroPerMillion = &value
	}
	if providerOut.Valid {
		value := providerOut.Int64
		version.ProviderOutputCostMicroPerMillion = &value
	}
	if createdBy.Valid {
		value := createdBy.Int64
		version.CreatedBy = &value
	}
	version.EffectiveAt = version.EffectiveAt.UTC()
	version.CreatedAt = version.CreatedAt.UTC()
	return version, nil
}

// mapPriceVersionWriteError turns the two constraint breaches this table can
// produce into domain errors. Anything else stays as it is: a 500 with a real
// database error beat a guessed status code.
func mapPriceVersionWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505":
		return fmt.Errorf("%w: %s", ErrPriceVersionExists, pgErr.ConstraintName)
	case "23503", "23514", "22001":
		return fmt.Errorf("%w: %s", ErrPriceInvalid, pgErr.ConstraintName)
	default:
		return err
	}
}

// nullableMicro stores "unknown" as SQL NULL. A nil provider cost is not zero.
func nullableMicro(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// nullableID stores "no actor" as SQL NULL, never as user 0.
func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}
