package billing

import (
	"context"
	"strings"
	"time"
)

// PriceVersion is one immutable price row: a model's prices from effective_at
// onwards (ADR-0004 §2). Publishing a new price *inserts* a row; there is no
// update and no delete, and migration 000008 enforces that in the database.
//
// Provider costs are pointers because "unknown" is a real, load-bearing state:
// a nil cost must never be read as 0, or a margin report would claim a profit
// the platform never made.
type PriceVersion struct {
	ID                                int64
	Model                             string
	Currency                          string
	InputMicroPerMillion              int64
	OutputMicroPerMillion             int64
	ProviderInputCostMicroPerMillion  *int64
	ProviderOutputCostMicroPerMillion *int64
	EffectiveAt                       time.Time
	CreatedBy                         *int64
	CreatedAt                         time.Time
}

// CreatePriceVersionParams is one publish operation. EffectiveAt is part of the
// identity: the same model may be re-priced, but never twice at the same
// instant.
type CreatePriceVersionParams struct {
	Model                             string
	Currency                          string
	InputMicroPerMillion              int64
	OutputMicroPerMillion             int64
	ProviderInputCostMicroPerMillion  *int64
	ProviderOutputCostMicroPerMillion *int64
	EffectiveAt                       time.Time
	// CreatedBy is the platform administrator who published the price. Zero
	// means "no actor recorded" and is stored as SQL NULL.
	CreatedBy int64
	// Now is the row's created_at. Zero means time.Now().UTC().
	Now time.Time
}

// ListPriceVersionsParams pages the admin price list. Model is an optional
// exact-match filter; BeforeID is the previous page's last id (0 = first page).
type ListPriceVersionsParams struct {
	Model    string
	Limit    int
	BeforeID int64
}

// PriceVersionPage is one page of price versions ordered by id DESC.
type PriceVersionPage struct {
	Items      []PriceVersion
	NextCursor string
}

// PriceVersionStore is the price-version persistence contract.
//
// Note what is absent: there is no Update and no Delete. Immutability is not a
// convention here, it is the interface.
type PriceVersionStore interface {
	// CreatePriceVersion publishes one price version. A second version for the
	// same (model, effective_at) fails with ErrPriceVersionExists.
	CreatePriceVersion(ctx context.Context, params CreatePriceVersionParams) (PriceVersion, error)
	// ListPriceVersions returns one page ordered by id DESC.
	ListPriceVersions(ctx context.Context, params ListPriceVersionsParams) (PriceVersionPage, error)
	// LatestPriceVersion returns the version in effect for model at `at`,
	// i.e. the greatest effective_at <= at. ErrPriceNotFound when the model has
	// no price yet at that instant.
	LatestPriceVersion(ctx context.Context, model string, at time.Time) (PriceVersion, error)
	// GetPriceVersion returns one version by id (ErrPriceNotFound if unknown).
	GetPriceVersion(ctx context.Context, id int64) (PriceVersion, error)
}

// MaxModelNameLength mirrors internal/control's limit so a model name cannot
// become a smuggling channel between the two services.
const MaxModelNameLength = 128

// ValidateCreatePriceVersion is a pure validator, so the HTTP layer can reject
// a bad price with a 400 before touching the database.
func ValidateCreatePriceVersion(params CreatePriceVersionParams) error {
	model := strings.TrimSpace(params.Model)
	if model == "" || len(model) > MaxModelNameLength || model != params.Model {
		return ErrPriceInvalid
	}
	if params.Currency != "" && params.Currency != CurrencyCNY {
		return ErrPriceInvalid
	}
	if params.InputMicroPerMillion < 0 || params.OutputMicroPerMillion < 0 {
		return ErrPriceInvalid
	}
	if params.ProviderInputCostMicroPerMillion != nil && *params.ProviderInputCostMicroPerMillion < 0 {
		return ErrPriceInvalid
	}
	if params.ProviderOutputCostMicroPerMillion != nil && *params.ProviderOutputCostMicroPerMillion < 0 {
		return ErrPriceInvalid
	}
	if params.EffectiveAt.IsZero() {
		return ErrInvalidArgument
	}
	return nil
}

// NormalizeCreatePriceVersion applies the documented defaults (currency CNY,
// created_at now) and trims the model name. It is used by both stores so the
// persisted row and the HTTP response agree.
func NormalizeCreatePriceVersion(params CreatePriceVersionParams) CreatePriceVersionParams {
	params.Model = strings.TrimSpace(params.Model)
	if params.Currency == "" {
		params.Currency = CurrencyCNY
	}
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.EffectiveAt = params.EffectiveAt.UTC()
	params.Now = params.Now.UTC()
	return params
}

// ValidateListPriceVersions bounds the page size. The HTTP layer normalizes its
// own limit; this is the second line of defence.
func ValidateListPriceVersions(params ListPriceVersionsParams) error {
	if params.Limit < 1 || params.Limit > 100 {
		return ErrInvalidArgument
	}
	if params.BeforeID < 0 {
		return ErrInvalidArgument
	}
	return nil
}
