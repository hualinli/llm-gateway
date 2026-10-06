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

// T07C read surface: the monthly consumption statement and the durable money
// exception queue.
//
// Both endpoints are organization-scoped reads under the same ADR-0003 §2.3
// matrix action as the account and the ledger (`view_org_usage`), plus one
// platform-admin write that resolves an exception. Every amount that leaves
// this package is a decimal string of micro-yuan, never a JSON number.

// StatementService is the subset of internal/billing consumed by the statement
// endpoint. *billing.PGStatementStore implements it.
type StatementService interface {
	MonthlyStatement(ctx context.Context, params billing.StatementParams) (billing.StatementPage, error)
}

// MoneyExceptionService is the subset of internal/billing consumed by the
// exception endpoints. *billing.PGExceptionStore implements it.
type MoneyExceptionService interface {
	ListExceptions(ctx context.Context, params billing.ListExceptionsParams) (billing.ExceptionPage, error)
	GetException(ctx context.Context, orgID, exceptionID int64) (billing.Exception, error)
	ListExceptionEvents(ctx context.Context, orgID, exceptionID int64) ([]billing.ExceptionEvent, error)
	ResolveException(ctx context.Context, params billing.ResolveExceptionParams) (billing.ResolveExceptionResult, error)
}

// unavailableStatementService is what a Console without a statement store
// serves: 503, never an empty statement. An empty statement would claim the
// organization spent nothing this month, which is a different (and false)
// statement.
type unavailableStatementService struct{}

func (unavailableStatementService) MonthlyStatement(context.Context, billing.StatementParams) (billing.StatementPage, error) {
	return billing.StatementPage{}, errBillingNotConfigured
}

// unavailableMoneyExceptionService is the same placeholder for the exception
// surface.
type unavailableMoneyExceptionService struct{}

func (unavailableMoneyExceptionService) ListExceptions(context.Context, billing.ListExceptionsParams) (billing.ExceptionPage, error) {
	return billing.ExceptionPage{}, errBillingNotConfigured
}

func (unavailableMoneyExceptionService) GetException(context.Context, int64, int64) (billing.Exception, error) {
	return billing.Exception{}, errBillingNotConfigured
}

func (unavailableMoneyExceptionService) ListExceptionEvents(context.Context, int64, int64) ([]billing.ExceptionEvent, error) {
	return nil, errBillingNotConfigured
}

func (unavailableMoneyExceptionService) ResolveException(context.Context, billing.ResolveExceptionParams) (billing.ResolveExceptionResult, error) {
	return billing.ResolveExceptionResult{}, errBillingNotConfigured
}

// ---------------------------------------------------------------------------
// Monthly statement DTOs
// ---------------------------------------------------------------------------

// statementAxisNote is frozen API text: it states the two time axes so a UI
// cannot present a posted_at statement as usage analysis (or the reverse).
const statementAxisNote = "occurred_at is when the usage happened (usage analysis); " +
	"posted_at is when the money moved (money). This statement groups by posted_at, " +
	"so a late settlement appears in the month it was posted, not the month of its usage."

type statementWindowDTO struct {
	From string `json:"from"`
	// To is exclusive: the window is [from, to).
	To string `json:"to"`
}

type statementTotalsDTO struct {
	Entries          string `json:"entries"`
	CreditMicro      string `json:"credit_micro"`
	DebitMicro       string `json:"debit_micro"`
	NetMicro         string `json:"net_micro"`
	ReservedMicro    string `json:"reserved_micro"`
	ReleasedMicro    string `json:"released_micro"`
	ReservedNetMicro string `json:"reserved_net_micro"`
	LateEntries      string `json:"late_entries"`
	LateMicro        string `json:"late_micro"`
}

type statementLineDTO struct {
	ID                string  `json:"id"`
	OrganizationID    string  `json:"organization_id"`
	Kind              string  `json:"kind"`
	Direction         string  `json:"direction"`
	AmountMicro       string  `json:"amount_micro"`
	BusinessID        string  `json:"business_id"`
	Reason            *string `json:"reason"`
	OperatorUserID    *string `json:"operator_user_id"`
	RequestID         *string `json:"request_id"`
	ManualOperation   *string `json:"manual_operation"`
	PaymentReference  *string `json:"payment_reference"`
	ReversalOfEntryID *string `json:"reversal_of_entry_id"`
	PostedAt          string  `json:"posted_at"`
	CreatedAt         string  `json:"created_at"`
	// OccurredAt is the usage instant this entry settles; null for entries that
	// settle no usage.
	OccurredAt *string `json:"occurred_at"`
	LatePosted bool    `json:"late_posted"`
	LateDays   int     `json:"late_days"`
}

type statementDTO struct {
	OrganizationID string `json:"organization_id"`
	Month          string `json:"month"`
	Currency       string `json:"currency"`
	Timezone       string `json:"timezone"`
	// Basis names the time axis the totals use. It is always "posted_at".
	Basis string `json:"basis"`
	// CutoffAt is the exclusive end of the statement month (Beijing time): the first
	// instant of the next month. It is a *period* boundary, not a close.
	CutoffAt    string             `json:"cutoff_at"`
	GeneratedAt string             `json:"generated_at"`
	Window      statementWindowDTO `json:"window"`
	Totals      statementTotalsDTO `json:"totals"`
	Items       []statementLineDTO `json:"items"`
	NextCursor  *string            `json:"next_cursor"`
	// Disclaimer is frozen: this is an internal consumption report, not a legal
	// invoice.
	Disclaimer string `json:"disclaimer"`
	AxisNote   string `json:"axis_note"`
}

func formatOptionalTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := formatTime(*value)
	return &formatted
}

func toStatementLineDTO(line billing.StatementLine) statementLineDTO {
	dto := statementLineDTO{
		ID:             formatID(line.ID),
		OrganizationID: formatID(line.OrgID),
		Kind:           line.Kind,
		Direction:      line.Direction,
		AmountMicro:    billing.FormatMicro(line.AmountMicro),
		BusinessID:     line.BusinessID,
		PostedAt:       formatTime(line.PostedAt),
		CreatedAt:      formatTime(line.CreatedAt),
		OccurredAt:     formatOptionalTime(line.OccurredAt),
		LatePosted:     line.LatePosted,
		LateDays:       line.LateDays,
	}
	if line.Reason != nil {
		value := *line.Reason
		dto.Reason = &value
	}
	if line.OperatorUserID != nil {
		value := formatID(*line.OperatorUserID)
		dto.OperatorUserID = &value
	}
	if line.RequestID != nil {
		value := *line.RequestID
		dto.RequestID = &value
	}
	if line.ManualOperation != "" {
		value := line.ManualOperation
		dto.ManualOperation = &value
	}
	if line.PaymentReference != nil {
		value := *line.PaymentReference
		dto.PaymentReference = &value
	}
	if line.ReversalOfEntryID != nil {
		value := formatID(*line.ReversalOfEntryID)
		dto.ReversalOfEntryID = &value
	}
	return dto
}

func toStatementLineDTOs(lines []billing.StatementLine) []statementLineDTO {
	out := make([]statementLineDTO, 0, len(lines))
	for _, line := range lines {
		out = append(out, toStatementLineDTO(line))
	}
	return out
}

func toStatementTotalsDTO(totals billing.StatementTotals) statementTotalsDTO {
	return statementTotalsDTO{
		Entries:          formatID(totals.Entries),
		CreditMicro:      billing.FormatMicro(totals.CreditMicro),
		DebitMicro:       billing.FormatMicro(totals.DebitMicro),
		NetMicro:         billing.FormatMicro(totals.NetMicro),
		ReservedMicro:    billing.FormatMicro(totals.ReservedMicro),
		ReleasedMicro:    billing.FormatMicro(totals.ReleasedMicro),
		ReservedNetMicro: billing.FormatMicro(totals.ReservedNetMicro),
		LateEntries:      formatID(totals.LateEntries),
		LateMicro:        billing.FormatMicro(totals.LateMicro),
	}
}

func toStatementDTO(page billing.StatementPage) statementDTO {
	dto := statementDTO{
		OrganizationID: "",
		Month:          page.Month.Label,
		Currency:       page.Currency,
		Timezone:       "Asia/Shanghai",
		Basis:          billing.StatementBasis,
		CutoffAt:       formatTime(page.Month.CutoffAt),
		GeneratedAt:    formatTime(page.GeneratedAt),
		Window: statementWindowDTO{
			From: formatTime(page.Month.From),
			To:   formatTime(page.Month.CutoffAt),
		},
		Totals:     toStatementTotalsDTO(page.Totals),
		Items:      toStatementLineDTOs(page.Items),
		Disclaimer: billing.StatementDisclaimer,
		AxisNote:   statementAxisNote,
	}
	if page.NextCursor != nil {
		next := encodeEntriesCursor(*page.NextCursor)
		dto.NextCursor = &next
	}
	return dto
}

// ---------------------------------------------------------------------------
// Money exception DTOs
// ---------------------------------------------------------------------------

type billingExceptionDTO struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	OrganizationID string  `json:"organization_id"`
	ExecutionID    *string `json:"execution_id"`
	AmountMicro    string  `json:"amount_micro"`
	Currency       string  `json:"currency"`
	Reason         string  `json:"reason"`
	Status         string  `json:"status"`
	DetectedAt     string  `json:"detected_at"`
	LastSeenAt     string  `json:"last_seen_at"`
	Occurrences    string  `json:"occurrences"`
	ResolvedAt     *string `json:"resolved_at"`
	ResolvedBy     *string `json:"resolved_by"`
	Resolution     *string `json:"resolution"`
	// ResolutionNote is the human explanation of the decision.
	ResolutionNote *string `json:"resolution_note"`
	Version        string  `json:"version"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type exceptionEventDTO struct {
	ID          string  `json:"id"`
	Action      string  `json:"action"`
	FromStatus  *string `json:"from_status"`
	ToStatus    *string `json:"to_status"`
	ActorUserID *string `json:"actor_user_id"`
	Note        *string `json:"note"`
	CreatedAt   string  `json:"created_at"`
}

type exceptionBacklogDTO struct {
	Open                 string  `json:"open"`
	Resolved             string  `json:"resolved"`
	Dismissed            string  `json:"dismissed"`
	OldestOpenAt         *string `json:"oldest_open_at"`
	OldestOpenAgeSeconds *string `json:"oldest_open_age_seconds"`
}

type billingExceptionPage struct {
	Items      []billingExceptionDTO `json:"items"`
	NextCursor *string               `json:"next_cursor"`
	Totals     exceptionBacklogDTO   `json:"totals"`
	Note       string                `json:"note"`
}

type billingExceptionDetail struct {
	Exception billingExceptionDTO `json:"exception"`
	Events    []exceptionEventDTO `json:"events"`
	Note      string              `json:"note"`
}

// resolveExceptionRequest is one human decision. `resolution` and `status` must
// tell the same story (see billing.ValidateResolveException): `charged`/`waived`
// close as resolved, `dismissed` closes as dismissed, `reopened` reopens.
type resolveExceptionRequest struct {
	Resolution string `json:"resolution"`
	Status     string `json:"status"`
	Note       string `json:"note"`
}

type resolveExceptionResponse struct {
	Exception billingExceptionDTO `json:"exception"`
	Event     exceptionEventDTO   `json:"event"`
	Replayed  bool                `json:"replayed"`
}

// exceptionQueueNote is frozen API text: it states that resolving an exception
// is an audited decision and that it never moves money by itself.
const exceptionQueueNote = "A money exception is a durable PostgreSQL fact, not a log line: " +
	"the Redis DLQ is only a bounded diagnostic cache. Resolving one records who decided what and when " +
	"(billing_exception_events is append-only), and never moves money by itself — " +
	"a correction is a separate reversing ledger entry."

func toBillingExceptionDTO(exception billing.Exception) billingExceptionDTO {
	dto := billingExceptionDTO{
		ID:             formatID(exception.ID),
		Kind:           exception.Kind,
		OrganizationID: formatID(exception.OrgID),
		AmountMicro:    billing.FormatMicro(exception.AmountMicro),
		Currency:       exception.Currency,
		Reason:         exception.Reason,
		Status:         exception.Status,
		DetectedAt:     formatTime(exception.DetectedAt),
		LastSeenAt:     formatTime(exception.LastSeenAt),
		Occurrences:    formatID(exception.Occurrences),
		Version:        formatID(exception.Version),
		CreatedAt:      formatTime(exception.CreatedAt),
		UpdatedAt:      formatTime(exception.UpdatedAt),
	}
	if exception.ExecutionID != nil {
		value := *exception.ExecutionID
		dto.ExecutionID = &value
	}
	if exception.ResolvedAt != nil {
		value := formatTime(*exception.ResolvedAt)
		dto.ResolvedAt = &value
	}
	if exception.ResolvedBy != nil {
		value := formatID(*exception.ResolvedBy)
		dto.ResolvedBy = &value
	}
	if exception.Resolution != nil {
		value := *exception.Resolution
		dto.Resolution = &value
	}
	if exception.ResolutionNote != nil {
		value := *exception.ResolutionNote
		dto.ResolutionNote = &value
	}
	return dto
}

func toBillingExceptionDTOs(exceptions []billing.Exception) []billingExceptionDTO {
	out := make([]billingExceptionDTO, 0, len(exceptions))
	for _, exception := range exceptions {
		out = append(out, toBillingExceptionDTO(exception))
	}
	return out
}

func toExceptionEventDTO(event billing.ExceptionEvent) exceptionEventDTO {
	dto := exceptionEventDTO{
		ID:        formatID(event.ID),
		Action:    event.Action,
		CreatedAt: formatTime(event.CreatedAt),
	}
	if event.FromStatus != nil {
		value := *event.FromStatus
		dto.FromStatus = &value
	}
	if event.ToStatus != nil {
		value := *event.ToStatus
		dto.ToStatus = &value
	}
	if event.ActorUserID != nil {
		value := formatID(*event.ActorUserID)
		dto.ActorUserID = &value
	}
	if event.Note != nil {
		value := *event.Note
		dto.Note = &value
	}
	return dto
}

func toExceptionBacklogDTO(backlog billing.ExceptionBacklog) exceptionBacklogDTO {
	dto := exceptionBacklogDTO{
		Open:      formatID(backlog.Open),
		Resolved:  formatID(backlog.Resolved),
		Dismissed: formatID(backlog.Dismissed),
	}
	if backlog.OldestOpenAt != nil {
		value := formatTime(*backlog.OldestOpenAt)
		dto.OldestOpenAt = &value
		seconds := strconv.FormatInt(int64(backlog.OldestOpenAge.Seconds()), 10)
		dto.OldestOpenAgeSeconds = &seconds
	}
	return dto
}

// ---------------------------------------------------------------------------
// Exception cursor: the (detected_at, id) pair, same shape as the ledger cursor
// ---------------------------------------------------------------------------

func encodeExceptionsCursor(cursor billing.ExceptionCursor) string {
	stamp := base64.RawURLEncoding.EncodeToString([]byte(cursor.DetectedAt.UTC().Format(time.RFC3339Nano)))
	id := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(cursor.ID, 10)))
	return "v1." + stamp + "." + id
}

var errBadExceptionsCursor = errors.New("cursor is not a valid billing exception cursor")

func decodeExceptionsCursor(raw string) (*billing.ExceptionCursor, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, errBadExceptionsCursor
	}
	stamp, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errBadExceptionsCursor
	}
	detectedAt, err := time.Parse(time.RFC3339Nano, string(stamp))
	if err != nil {
		return nil, errBadExceptionsCursor
	}
	rawID, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errBadExceptionsCursor
	}
	id, err := strconv.ParseInt(string(rawID), 10, 64)
	if err != nil || id <= 0 {
		return nil, errBadExceptionsCursor
	}
	cursor := billing.ExceptionCursor{DetectedAt: detectedAt.UTC(), ID: id}
	if encodeExceptionsCursor(cursor) != trimmed {
		return nil, errBadExceptionsCursor
	}
	return &cursor, nil
}

// validExceptionStatus / validExceptionKind reuse the frozen billing
// vocabularies so a filter cannot name a state the database can never store.
func validExceptionStatus(status string) bool { return billing.IsKnownExceptionStatus(status) }

func validExceptionKind(kind string) bool { return billing.IsKnownExceptionKind(kind) }
