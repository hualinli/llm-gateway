package console

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/billing"

	"github.com/cloudwego/hertz/pkg/app"
)

// ReviewService reuses the audited money transactions. Only a session-bound
// platform administrator may reach it; actors never come from request bodies.
type ReviewService interface {
	billing.ManualDispositionStore
	ListReviewRequiredExecutions(context.Context, billing.ReviewQueueParams) (billing.ReviewQueuePage, error)
}

type reviewItemDTO struct {
	ExecutionID    string  `json:"execution_id"`
	OrganizationID string  `json:"organization_id"`
	Model          string  `json:"model"`
	ReservedMicro  string  `json:"reserved_micro"`
	UpdatedAt      string  `json:"updated_at"`
	AgeSeconds     float64 `json:"age_seconds"`
	SLABreached    bool    `json:"sla_breached"`
	Reason         *string `json:"reason"`
}

func (s *server) listReviewQueue(ctx context.Context, c *app.RequestContext) {
	if s.review == nil {
		s.writeBillingError(c, "review queue", errBillingNotConfigured)
		return
	}
	params := billing.NormalizeReviewQueuePage(billing.ReviewQueueParams{Now: s.now()})
	if raw := string(c.Query("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > billing.MaxReviewQueuePageSize {
			s.writeError(c, 400, "", "limit must be 1..200")
			return
		}
		params.Limit = limit
	}
	cursorAt, cursorID := string(c.Query("cursor_updated_at")), string(c.Query("cursor_execution_id"))
	if cursorAt != "" || cursorID != "" {
		at, err := time.Parse(time.RFC3339Nano, cursorAt)
		if err != nil || cursorID == "" || len(cursorID) > billing.MaxExecutionIDLength {
			s.writeError(c, 400, "", "invalid review cursor")
			return
		}
		params.Cursor = &billing.ReviewCursor{UpdatedAt: at, ExecutionID: cursorID}
	}
	policy, ok := s.currentReviewPolicy(ctx, c)
	if !ok {
		return
	}
	params.SLA = policy.HumanReviewSLA
	page, err := s.review.ListReviewRequiredExecutions(ctx, params)
	if err != nil {
		s.writeReviewError(c, err)
		return
	}
	items := make([]reviewItemDTO, 0, len(page.Items))
	for _, item := range page.Items {
		e := item.Execution
		items = append(items, reviewItemDTO{
			ExecutionID: e.ExecutionID, OrganizationID: strconv.FormatInt(e.OrgID, 10), Model: e.Model,
			ReservedMicro: billing.FormatMicro(e.ReservedMicro), UpdatedAt: e.UpdatedAt.Format(time.RFC3339Nano),
			AgeSeconds: item.Age.Seconds(), SLABreached: item.Breached, Reason: item.Reason,
		})
	}
	var cursor any
	if page.NextCursor != nil {
		cursor = map[string]string{"updated_at": page.NextCursor.UpdatedAt.Format(time.RFC3339Nano), "execution_id": page.NextCursor.ExecutionID}
	}
	c.JSON(http.StatusOK, map[string]any{"items": items, "next_cursor": cursor, "now": page.Now,
		"totals":          map[string]any{"open": strconv.FormatInt(page.Totals.Open, 10), "sla_breaches": strconv.FormatInt(page.Totals.Breaches, 10), "oldest_age_seconds": page.Totals.OldestAge(page.Now).Seconds()},
		"min_age_seconds": policy.WindowMinAge.Seconds(), "sla_seconds": policy.HumanReviewSLA.Seconds()})
}

func (s *server) disposeReviewExecution(ctx context.Context, c *app.RequestContext) {
	if s.review == nil {
		s.writeBillingError(c, "review disposition", errBillingNotConfigured)
		return
	}
	action, id := c.Param("action"), strings.TrimSpace(c.Param("execution"))
	if action != billing.ManualActionRelease && action != billing.ManualActionSettle {
		s.writeError(c, 404, "", "unknown disposition")
		return
	}
	if id == "" || len(id) > billing.MaxExecutionIDLength {
		s.writeError(c, 400, "", "invalid execution id")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !s.decodeJSON(c, &body) {
		return
	}
	if err := billing.ValidateTerminalReason(body.Reason); err != nil {
		s.writeReviewError(c, err)
		return
	}
	policy, ok := s.currentReviewPolicy(ctx, c)
	if !ok {
		return
	}
	auth, _ := authFrom(c)
	now := s.now()
	params := billing.NormalizeManualDisposition(billing.ManualDispositionParams{
		ExecutionID: id, Action: action, Reason: body.Reason, ActorUserID: auth.User.ID,
		MinAge: policy.WindowMinAge, LivenessLease: policy.LivenessLease, ObservedAt: now, Now: now, RequestID: requestIDFrom(c),
	})
	if err := billing.ValidateManualDisposition(params); err != nil {
		s.writeReviewError(c, err)
		return
	}
	var result billing.ManualDispositionResult
	var err error
	if action == billing.ManualActionRelease {
		result, err = s.review.ReleaseExecutionManually(ctx, params)
	} else {
		result, err = s.review.SettleExecutionManually(ctx, params)
	}
	if err != nil {
		s.writeReviewError(c, err)
		return
	}
	// These are the account and postings committed by the same transaction,
	// rather than a second read that could race another operator.
	var debit, release any
	if result.DebitEntry != nil {
		debit = toBillingEntryDTO(*result.DebitEntry)
	}
	if result.ReleaseEntry != nil {
		release = toBillingEntryDTO(*result.ReleaseEntry)
	}
	c.JSON(http.StatusOK, map[string]any{"execution_id": result.Execution.ExecutionID, "organization_id": strconv.FormatInt(result.Execution.OrgID, 10), "status": result.Execution.Status, "action": result.Action,
		"charge_micro": billing.FormatMicro(result.ChargeMicro), "policy": result.Policy, "account": toBillingAccountDTO(result.Account), "debit_entry": debit, "release_entry": release, "request_id": requestIDFrom(c)})
}

func (s *server) currentReviewPolicy(ctx context.Context, c *app.RequestContext) (ReviewPolicy, bool) {
	if s.reviewPolicy == nil {
		s.writeError(c, http.StatusServiceUnavailable, "", "review policy is not configured")
		return ReviewPolicy{}, false
	}
	policy, err := s.reviewPolicy.GetReviewPolicy(ctx)
	if err != nil || ValidateReviewPolicy(policy) != nil {
		s.logger.Printf("review policy unavailable request_id=%s", requestIDFrom(c))
		s.writeError(c, http.StatusServiceUnavailable, "", "review policy is unavailable; try again later")
		return ReviewPolicy{}, false
	}
	return policy, true
}

func (s *server) writeReviewError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, billing.ErrExecutionNotFound):
		s.writeError(c, 404, "", "execution not found")
	case errors.Is(err, billing.ErrExecutionState), errors.Is(err, billing.ErrExecutionHasReceipt), errors.Is(err, billing.ErrRecoveryTooYoung), errors.Is(err, billing.ErrIdempotencyConflict):
		s.writeError(c, 409, "", "execution changed or cannot safely be disposed; refresh the queue")
	case errors.Is(err, billing.ErrInvalidArgument):
		s.writeError(c, 400, "", err.Error())
	default:
		s.logger.Printf("review operation failed: %v", err)
		s.writeError(c, 503, "", "review operation unavailable; refresh before retrying")
	}
}
