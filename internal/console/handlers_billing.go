package console

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/organization"

	"github.com/cloudwego/hertz/pkg/app"
)

// Audit vocabulary of the platform adjustment (ADR-0004 §2.6). The success row
// is written by internal/billing *inside* the money transaction; these
// constants are the same strings, used here for the refused attempts.
const (
	ActionBillingAdjustment = billing.ActionBillingAdjustment
	TargetBillingAccount    = billing.TargetBillingAccount
)

// idempotencyKeyHeader is the standard request header carrying the business id
// of one adjustment.
const idempotencyKeyHeader = "Idempotency-Key"

// listAdminPrices implements GET /api/v1/admin/prices (platform admin).
//
// The list is not filtered by "currently effective": it is the price history,
// ordered by id DESC, because that is what an operator needs when a bill is
// disputed. `model` narrows it to one model's versions.
func (s *server) listAdminPrices(ctx context.Context, c *app.RequestContext) {
	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	beforeID, err := parseCursor(c.Query("cursor"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if len(model) > maxModelNameLength {
		s.writeError(c, http.StatusBadRequest, "", "model is too long")
		return
	}

	page, err := s.prices.ListPriceVersions(ctx, billing.ListPriceVersionsParams{
		Model:    model,
		Limit:    limit,
		BeforeID: beforeID,
	})
	if err != nil {
		s.writeBillingError(c, "list price versions", err)
		return
	}
	writeJSON(c, http.StatusOK, priceVersionPage{
		Items:      toPriceVersionDTOs(page.Items),
		NextCursor: optionalCursor(page.NextCursor),
	})
}

// createAdminPrice implements POST /api/v1/admin/prices (session + CSRF +
// platform admin).
//
// Publishing a price is append-only: the same (model, effective_at) is a 409,
// never an update. Every validation that does not need the database happens
// before the insert so a typo cannot reach the ledger's price history.
func (s *server) createAdminPrice(ctx context.Context, c *app.RequestContext) {
	var req createPriceVersionRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	auth, _ := authFrom(c)

	model := strings.TrimSpace(req.Model)
	if model == "" {
		s.writeError(c, http.StatusBadRequest, "", "model is required")
		return
	}
	if len(model) > maxModelNameLength {
		s.writeError(c, http.StatusBadRequest, "", "model is too long")
		return
	}
	if currency := strings.TrimSpace(req.Currency); currency != "" && currency != billing.CurrencyCNY {
		s.writeError(c, http.StatusBadRequest, "", `currency must be "CNY"`)
		return
	}
	inputPrice, err := billing.ParseMicro(req.InputPriceMicroPerMillion)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "input_price_micro_per_million must be a non-negative integer micro amount as a string")
		return
	}
	outputPrice, err := billing.ParseMicro(req.OutputPriceMicroPerMillion)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "output_price_micro_per_million must be a non-negative integer micro amount as a string")
		return
	}
	providerInput, err := parseOptionalMicro(req.ProviderInputCostMicroPerMillion)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "provider_input_cost_micro_per_million must be a non-negative integer micro amount as a string or null")
		return
	}
	providerOutput, err := parseOptionalMicro(req.ProviderOutputCostMicroPerMillion)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "provider_output_cost_micro_per_million must be a non-negative integer micro amount as a string or null")
		return
	}
	effectiveAt, err := parseRequiredTimestamp(req.EffectiveAt)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "effective_at must be an RFC3339 timestamp")
		return
	}

	params := billing.CreatePriceVersionParams{
		Model:                             model,
		Currency:                          billing.CurrencyCNY,
		InputMicroPerMillion:              inputPrice,
		OutputMicroPerMillion:             outputPrice,
		ProviderInputCostMicroPerMillion:  providerInput,
		ProviderOutputCostMicroPerMillion: providerOutput,
		EffectiveAt:                       effectiveAt,
		CreatedBy:                         auth.User.ID,
		Now:                               s.now(),
	}
	// The domain validator runs here as well as inside the store: a bad price is
	// a 400 before a transaction is opened, and the store keeps its own guard
	// for non-HTTP callers.
	if err := billing.ValidateCreatePriceVersion(params); err != nil {
		s.writeError(c, http.StatusBadRequest, "", "prices must be non-negative integer micro amounts for a known model and currency")
		return
	}

	version, err := s.prices.CreatePriceVersion(ctx, params)
	if err != nil {
		event := s.auditFrom(c, "admin.price.create", "failure", err.Error())
		event.Detail = map[string]any{"model": model}
		s.audit.Record(ctx, event)
		s.writeBillingError(c, "create price version", err)
		return
	}
	event := s.auditFrom(c, "admin.price.create", "success", "")
	event.TargetType = "price_version"
	event.TargetID = formatID(version.ID)
	event.Detail = map[string]any{"model": model, "effective_at": formatTime(version.EffectiveAt)}
	s.audit.Record(ctx, event)
	writeJSON(c, http.StatusCreated, toPriceVersionDTO(version))
}

// getOrgBillingAccount implements GET /api/v1/orgs/{org}/billing/account.
//
// Read scope is the ADR-0003 §2.3 matrix action view_org_usage: owner, admin
// and the read-only billing_viewer. A plain member is refused with 403 (they
// are a member, so the organization's existence is not a secret), a non-member
// with 404.
//
// An organization without a billing account row answers 404: this endpoint is
// read-only and never creates the row as a side effect of a GET.
func (s *server) getOrgBillingAccount(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	if _, err := s.orgs.AuthorizeOrgAction(ctx, auth.User.ID, orgID, organization.ActionViewOrgUsage); err != nil {
		s.writeOrganizationRBACError(c, "authorize billing account read", err)
		return
	}
	s.writeBillingAccount(ctx, c, orgID)
}

// getAdminBillingAccount exposes the same account projection to platform
// administrators without granting organization membership or changing the
// organization-scoped authorization matrix.
func (s *server) getAdminBillingAccount(ctx context.Context, c *app.RequestContext) {
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	s.writeBillingAccount(ctx, c, orgID)
}

func (s *server) writeBillingAccount(ctx context.Context, c *app.RequestContext, orgID int64) {
	account, err := s.accounts.GetAccount(ctx, orgID)
	if err != nil {
		s.writeBillingError(c, "get billing account", err)
		return
	}
	writeJSON(c, http.StatusOK, toBillingAccountDTO(account))
}

// listOrgBillingEntries implements GET /api/v1/orgs/{org}/billing/entries.
// Same read scope as the account; the page is ordered by (posted_at, id) DESC
// and the cursor is the opaque token of that pair.
func (s *server) listOrgBillingEntries(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	if _, err := s.orgs.AuthorizeOrgAction(ctx, auth.User.ID, orgID, organization.ActionViewOrgUsage); err != nil {
		s.writeOrganizationRBACError(c, "authorize billing entries read", err)
		return
	}
	s.writeBillingEntries(ctx, c, orgID)
}

// listAdminBillingEntries lets platform administrators inspect the same
// append-only ledger page without needing an organization membership.
func (s *server) listAdminBillingEntries(ctx context.Context, c *app.RequestContext) {
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	s.writeBillingEntries(ctx, c, orgID)
}

func (s *server) writeBillingEntries(ctx context.Context, c *app.RequestContext, orgID int64) {
	limit, err := parseListLimit(c.Query("limit"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	cursor, err := decodeEntriesCursor(c.Query("cursor"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	kind := strings.TrimSpace(c.Query("kind"))
	if kind != "" && !validEntryKind(kind) {
		s.writeError(c, http.StatusBadRequest, "", "kind must be one of credit, debit, adjustment, reservation, release")
		return
	}

	page, err := s.accounts.ListEntries(ctx, billing.ListEntriesParams{
		OrgID:  orgID,
		Limit:  limit,
		Cursor: cursor,
		Kind:   kind,
	})
	if err != nil {
		s.writeBillingError(c, "list billing entries", err)
		return
	}
	response := billingEntryPage{Items: toBillingEntryDTOs(page.Items)}
	if page.NextCursor != nil {
		next := encodeEntriesCursor(*page.NextCursor)
		response.NextCursor = &next
	}
	writeJSON(c, http.StatusOK, response)
}

// adjustBillingAccount implements
// POST /api/v1/admin/billing/accounts/{org}/adjustments (session + CSRF +
// platform admin).
//
// Scope: platform administrators only, on any organization — a platform
// adjustment is not an organization-scoped write, and migrations 000005/000006
// already freeze that split (`organization.Allowed(role, ActionPlatformAdminOps)`
// is false for every organization role).
//
// Idempotency: the `Idempotency-Key` header becomes the ledger's business_id.
// Replaying it with the same payload returns the stored posting with
// `replayed: true` and a 200; changing the payload under the same key is a 409
// and moves no money.
func (s *server) adjustBillingAccount(ctx context.Context, c *app.RequestContext) {
	auth, ok := authFrom(c)
	if !ok {
		s.writeError(c, http.StatusUnauthorized, "", "authentication required")
		return
	}
	orgID, err := parsePathID(c.Param("org"))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	idempotencyKey, err := parseIdempotencyKey(string(c.GetHeader(idempotencyKeyHeader)))
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", err.Error())
		return
	}
	var req adjustBillingAccountRequest
	if !s.decodeJSON(c, &req) {
		return
	}
	amount, err := billing.ParsePositiveMicro(req.AmountMicro)
	if err != nil {
		s.writeError(c, http.StatusBadRequest, "", "amount_micro must be a positive integer micro amount as a string")
		return
	}
	operation := strings.TrimSpace(req.Type)
	var direction, kind, paymentReference string
	var reversalOf *int64
	switch operation {
	case billing.ManualOperationRecharge:
		direction, kind = billing.DirectionCredit, billing.KindCredit
		paymentReference = strings.TrimSpace(req.PaymentReference)
		if paymentReference == "" || req.ReverseOf != "" || len(paymentReference) > 256 {
			s.writeError(c, http.StatusBadRequest, "", "recharge requires payment_reference and must not include reverse_of")
			return
		}
	case billing.ManualOperationReversal:
		direction, kind = billing.DirectionDebit, billing.KindAdjustment
		if strings.TrimSpace(req.PaymentReference) != "" {
			s.writeError(c, http.StatusBadRequest, "", "reversal must not include payment_reference")
			return
		}
		entryID, parseErr := parsePathID(req.ReverseOf)
		if parseErr != nil {
			s.writeError(c, http.StatusBadRequest, "", "reverse_of must be the original recharge entry id as a string")
			return
		}
		reversalOf = &entryID
	default:
		s.writeError(c, http.StatusBadRequest, "", `type must be "recharge" or "reversal"`)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		s.writeError(c, http.StatusBadRequest, "", "reason is required: every money movement must state why")
		return
	}
	if len(reason) > billing.MaxReasonLength {
		s.writeError(c, http.StatusBadRequest, "", "reason is too long")
		return
	}
	reversalEntryID := int64(0)
	if reversalOf != nil {
		reversalEntryID = *reversalOf
	}

	result, err := s.accounts.Adjust(ctx, billing.AdjustParams{
		ActorUserID:       auth.User.ID,
		OrgID:             orgID,
		AmountMicro:       amount,
		Direction:         direction,
		Kind:              kind,
		Reason:            reason,
		ManualOperation:   operation,
		PaymentReference:  paymentReference,
		ReversalOfEntryID: reversalOf,
		BusinessID:        idempotencyKey,
		IdempotencyKey:    idempotencyKey,
		PayloadDigest:     billing.ManualPaymentDigest(orgID, amount, direction, kind, operation, paymentReference, reversalEntryID, reason),
		RequestID:         requestIDFrom(c),
		Now:               s.now(),
	})
	if err != nil {
		// A refused adjustment is still audited; the successful one is audited
		// by internal/billing inside the money transaction.
		event := s.auditMutation(c, ActionBillingAdjustment, orgID, TargetBillingAccount, formatID(orgID),
			"failure", err.Error(), map[string]any{
				"amount_micro": billing.FormatMicro(amount),
				"direction":    direction,
				"type":         operation,
				"business_id":  idempotencyKey,
			})
		s.audit.Record(ctx, event)
		s.writeBillingError(c, "adjust billing account", err)
		return
	}

	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(c, status, adjustmentResponse{
		Entry:    toBillingEntryDTO(result.Entry),
		Account:  toBillingAccountDTO(result.Account),
		Replayed: result.Replayed,
	})
}

// writeBillingError maps internal/billing sentinels onto the frozen status
// codes (ADR-0004 §4). A missing organization or account is 404 (no existence
// disclosure), a conflicting idempotency key is 409, a caller-fixable value is
// 400, and a missing billing store is 503 rather than a fabricated empty page.
func (s *server) writeBillingError(c *app.RequestContext, operation string, err error) {
	switch {
	case errors.Is(err, errBillingNotConfigured):
		s.writeError(c, http.StatusServiceUnavailable, "", "billing is not configured on this Console")
	case errors.Is(err, billing.ErrOrganizationNotFound):
		s.writeError(c, http.StatusNotFound, "", "organization not found")
	case errors.Is(err, billing.ErrAccountNotFound):
		s.writeError(c, http.StatusNotFound, "", "this organization has no billing account yet")
	case errors.Is(err, billing.ErrEntryNotFound), errors.Is(err, billing.ErrPriceNotFound),
		errors.Is(err, billing.ErrExceptionNotFound):
		s.writeError(c, http.StatusNotFound, "", "not found")
	case errors.Is(err, billing.ErrPriceVersionExists), errors.Is(err, billing.ErrIdempotencyConflict):
		s.writeError(c, http.StatusConflict, "", sanitizeConflictMessage(err))
	case errors.Is(err, billing.ErrExceptionResolved):
		// A closed money exception is not silently reclassified: reopen it first,
		// which writes its own audit row (T07C).
		s.writeError(c, http.StatusConflict, "", "this money exception is already closed with a different resolution; reopen it first")
	case errors.Is(err, billing.ErrAmountInvalid), errors.Is(err, billing.ErrPriceInvalid),
		errors.Is(err, billing.ErrUsageInvalid), errors.Is(err, billing.ErrAmountOverflow),
		errors.Is(err, billing.ErrInvalidArgument):
		s.writeError(c, http.StatusBadRequest, "", err.Error())
	default:
		s.writeInternal(c, operation, err)
	}
}

// sanitizeConflictMessage keeps the two conflict cases distinguishable in the
// API without leaking SQL: a reused price slot versus a reused idempotency key.
func sanitizeConflictMessage(err error) string {
	if errors.Is(err, billing.ErrPriceVersionExists) {
		return "a price version for this model and effective_at already exists; publish a new effective_at"
	}
	return "this Idempotency-Key was already used with a different payload"
}

// parseOptionalMicro parses a nullable decimal-string micro amount. A missing
// field and an explicit null both mean "unknown", never zero.
func parseOptionalMicro(raw *string) (*int64, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil, nil
	}
	value, err := billing.ParseMicro(trimmed)
	if err != nil || value < 0 {
		return nil, billing.ErrAmountInvalid
	}
	return &value, nil
}

// parseIdempotencyKey validates the header before it becomes a business id.
func parseIdempotencyKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", errors.New("the Idempotency-Key header is required")
	}
	if len(key) > billing.MaxBusinessIDLength {
		return "", errors.New("the Idempotency-Key header is too long")
	}
	if strings.ContainsAny(key, "\x00\r\n") {
		return "", errors.New("the Idempotency-Key header contains a control character")
	}
	return key, nil
}

func validEntryKind(kind string) bool {
	switch kind {
	case billing.KindCredit, billing.KindDebit, billing.KindAdjustment,
		billing.KindReservation, billing.KindRelease:
		return true
	default:
		return false
	}
}
