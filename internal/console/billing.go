package console

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/billing"
)

// T05 billing surface (ADR-0004). The HTTP layer is a mapper here: it
// authenticates, resolves the permission matrix, converts decimal strings into
// int64 micro amounts and back, and maps the typed billing errors onto status
// codes. Every amount that leaves this package is a decimal string of
// micro-yuan, never a JSON number.

// PriceService is the subset of internal/billing consumed by the platform-admin
// price endpoints. *billing.PGPriceVersionStore implements it.
type PriceService interface {
	CreatePriceVersion(ctx context.Context, params billing.CreatePriceVersionParams) (billing.PriceVersion, error)
	ListPriceVersions(ctx context.Context, params billing.ListPriceVersionsParams) (billing.PriceVersionPage, error)
}

// BillingAccountService is the subset of internal/billing consumed by the
// organization billing endpoints. *billing.PGAccountStore implements it.
type BillingAccountService interface {
	GetAccount(ctx context.Context, orgID int64) (billing.Account, error)
	ListEntries(ctx context.Context, params billing.ListEntriesParams) (billing.EntryPage, error)
	Adjust(ctx context.Context, params billing.AdjustParams) (billing.AdjustmentResult, error)
}

// unavailablePriceService is what a Console without a billing store serves:
// 503 unavailable, never an empty price list. An empty list would claim that no
// model has a price, which is a different (and false) statement.
type unavailablePriceService struct{}

var errBillingNotConfigured = errors.New("console: billing store is not configured")

func (unavailablePriceService) CreatePriceVersion(context.Context, billing.CreatePriceVersionParams) (billing.PriceVersion, error) {
	return billing.PriceVersion{}, errBillingNotConfigured
}

func (unavailablePriceService) ListPriceVersions(context.Context, billing.ListPriceVersionsParams) (billing.PriceVersionPage, error) {
	return billing.PriceVersionPage{}, errBillingNotConfigured
}

// unavailableBillingAccountService is the same placeholder for the account
// surface.
type unavailableBillingAccountService struct{}

func (unavailableBillingAccountService) GetAccount(context.Context, int64) (billing.Account, error) {
	return billing.Account{}, errBillingNotConfigured
}

func (unavailableBillingAccountService) ListEntries(context.Context, billing.ListEntriesParams) (billing.EntryPage, error) {
	return billing.EntryPage{}, errBillingNotConfigured
}

func (unavailableBillingAccountService) Adjust(context.Context, billing.AdjustParams) (billing.AdjustmentResult, error) {
	return billing.AdjustmentResult{}, errBillingNotConfigured
}

// ---------------------------------------------------------------------------
// Wire DTOs (ADR-0004 §4)
// ---------------------------------------------------------------------------

// priceVersionDTO is one immutable price version. Provider costs are nullable:
// null means "unknown", never zero.
type priceVersionDTO struct {
	ID                                string  `json:"id"`
	Model                             string  `json:"model"`
	Currency                          string  `json:"currency"`
	InputPriceMicroPerMillion         string  `json:"input_price_micro_per_million"`
	OutputPriceMicroPerMillion        string  `json:"output_price_micro_per_million"`
	ProviderInputCostMicroPerMillion  *string `json:"provider_input_cost_micro_per_million"`
	ProviderOutputCostMicroPerMillion *string `json:"provider_output_cost_micro_per_million"`
	EffectiveAt                       string  `json:"effective_at"`
	CreatedBy                         *string `json:"created_by"`
	CreatedAt                         string  `json:"created_at"`
}

type priceVersionPage struct {
	Items      []priceVersionDTO `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// createPriceVersionRequest publishes a price. Amounts arrive as decimal
// strings, exactly like they are returned.
type createPriceVersionRequest struct {
	Model                             string  `json:"model"`
	Currency                          string  `json:"currency"`
	InputPriceMicroPerMillion         string  `json:"input_price_micro_per_million"`
	OutputPriceMicroPerMillion        string  `json:"output_price_micro_per_million"`
	ProviderInputCostMicroPerMillion  *string `json:"provider_input_cost_micro_per_million"`
	ProviderOutputCostMicroPerMillion *string `json:"provider_output_cost_micro_per_million"`
	EffectiveAt                       string  `json:"effective_at"`
}

// billingAccountDTO is the account projection. available_micro is echoed (not
// recomputed by the client) because it is the number T06's admission compares
// against, and the two must not be able to disagree.
type billingAccountDTO struct {
	OrganizationID     string `json:"organization_id"`
	Currency           string `json:"currency"`
	PostedBalanceMicro string `json:"posted_balance_micro"`
	ReservedMicro      string `json:"reserved_micro"`
	AvailableMicro     string `json:"available_micro"`
	Status             string `json:"status"`
	Version            string `json:"version"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	AvailableNote      string `json:"available_note"`
}

type billingEntryDTO struct {
	ID             string  `json:"id"`
	OrganizationID string  `json:"organization_id"`
	Kind           string  `json:"kind"`
	Direction      string  `json:"direction"`
	AmountMicro    string  `json:"amount_micro"`
	BusinessID     string  `json:"business_id"`
	Reason         *string `json:"reason"`
	OperatorUserID *string `json:"operator_user_id"`
	RequestID      *string `json:"request_id"`
	PostedAt       string  `json:"posted_at"`
	CreatedAt      string  `json:"created_at"`
}

type billingEntryPage struct {
	Items      []billingEntryDTO `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// adjustBillingAccountRequest is one platform adjustment. The idempotency key
// is a header, not a body field, so a client that retries a request cannot
// accidentally change it while editing the JSON.
type adjustBillingAccountRequest struct {
	AmountMicro string `json:"amount_micro"`
	Direction   string `json:"direction"`
	Reason      string `json:"reason"`
}

type adjustmentResponse struct {
	Entry    billingEntryDTO   `json:"entry"`
	Account  billingAccountDTO `json:"account"`
	Replayed bool              `json:"replayed"`
}

// billingAvailableNote is frozen API text: it states the invariant so a UI
// cannot present posted_balance as spendable money.
const billingAvailableNote = "available_micro = posted_balance_micro - reserved_micro. " +
	"A negative available amount is a debt, not a spendable balance."

func toPriceVersionDTO(version billing.PriceVersion) priceVersionDTO {
	dto := priceVersionDTO{
		ID:                         formatID(version.ID),
		Model:                      version.Model,
		Currency:                   version.Currency,
		InputPriceMicroPerMillion:  billing.FormatMicro(version.InputMicroPerMillion),
		OutputPriceMicroPerMillion: billing.FormatMicro(version.OutputMicroPerMillion),
		EffectiveAt:                formatTime(version.EffectiveAt),
		CreatedAt:                  formatTime(version.CreatedAt),
	}
	if version.ProviderInputCostMicroPerMillion != nil {
		value := billing.FormatMicro(*version.ProviderInputCostMicroPerMillion)
		dto.ProviderInputCostMicroPerMillion = &value
	}
	if version.ProviderOutputCostMicroPerMillion != nil {
		value := billing.FormatMicro(*version.ProviderOutputCostMicroPerMillion)
		dto.ProviderOutputCostMicroPerMillion = &value
	}
	if version.CreatedBy != nil {
		value := formatID(*version.CreatedBy)
		dto.CreatedBy = &value
	}
	return dto
}

func toPriceVersionDTOs(versions []billing.PriceVersion) []priceVersionDTO {
	// Always a JSON array, never null.
	out := make([]priceVersionDTO, 0, len(versions))
	for _, version := range versions {
		out = append(out, toPriceVersionDTO(version))
	}
	return out
}

func toBillingAccountDTO(account billing.Account) billingAccountDTO {
	return billingAccountDTO{
		OrganizationID:     formatID(account.OrgID),
		Currency:           account.Currency,
		PostedBalanceMicro: billing.FormatMicro(account.PostedBalanceMicro),
		ReservedMicro:      billing.FormatMicro(account.ReservedMicro),
		AvailableMicro:     billing.FormatMicro(account.AvailableMicro()),
		Status:             account.Status,
		Version:            formatID(account.Version),
		CreatedAt:          formatTime(account.CreatedAt),
		UpdatedAt:          formatTime(account.UpdatedAt),
		AvailableNote:      billingAvailableNote,
	}
}

func toBillingEntryDTO(entry billing.Entry) billingEntryDTO {
	dto := billingEntryDTO{
		ID:             formatID(entry.ID),
		OrganizationID: formatID(entry.OrgID),
		Kind:           entry.Kind,
		Direction:      entry.Direction,
		AmountMicro:    billing.FormatMicro(entry.AmountMicro),
		BusinessID:     entry.BusinessID,
		PostedAt:       formatTime(entry.PostedAt),
		CreatedAt:      formatTime(entry.CreatedAt),
	}
	if entry.Reason != nil {
		value := *entry.Reason
		dto.Reason = &value
	}
	if entry.OperatorUserID != nil {
		value := formatID(*entry.OperatorUserID)
		dto.OperatorUserID = &value
	}
	if entry.RequestID != nil {
		value := *entry.RequestID
		dto.RequestID = &value
	}
	return dto
}

func toBillingEntryDTOs(entries []billing.Entry) []billingEntryDTO {
	out := make([]billingEntryDTO, 0, len(entries))
	for _, entry := range entries {
		out = append(out, toBillingEntryDTO(entry))
	}
	return out
}

// parseRequiredTimestamp parses an RFC3339 instant. A missing value is an
// error: "effective now" is never inferred from an empty string, because a
// price's effective_at is part of its identity.
func parseRequiredTimestamp(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, errors.New("timestamp is required")
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

// ---------------------------------------------------------------------------
// Entry cursor
// ---------------------------------------------------------------------------

// encodeEntriesCursor renders a (posted_at, id) position as an opaque URL-safe
// token, using the same shape as the usage cursor so a client only has to learn
// one cursor format.
func encodeEntriesCursor(cursor billing.EntriesCursor) string {
	stamp := base64.RawURLEncoding.EncodeToString([]byte(cursor.PostedAt.UTC().Format(time.RFC3339Nano)))
	id := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(cursor.ID, 10)))
	return "v1." + stamp + "." + id
}

var errBadEntriesCursor = errors.New("cursor is not a valid billing entry cursor")

func decodeEntriesCursor(raw string) (*billing.EntriesCursor, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, errBadEntriesCursor
	}
	stamp, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errBadEntriesCursor
	}
	postedAt, err := time.Parse(time.RFC3339Nano, string(stamp))
	if err != nil {
		return nil, errBadEntriesCursor
	}
	rawID, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errBadEntriesCursor
	}
	id, err := strconv.ParseInt(string(rawID), 10, 64)
	if err != nil || id <= 0 {
		return nil, errBadEntriesCursor
	}
	// The cursor is echoed back to the client, so it must be canonical: a token
	// that decodes to a different position than it encodes would let a caller
	// craft a page boundary that never advances.
	cursor := billing.EntriesCursor{PostedAt: postedAt.UTC(), ID: id}
	if encodeEntriesCursor(cursor) != trimmed {
		return nil, errBadEntriesCursor
	}
	return &cursor, nil
}
