package main

import (
	"context"
	"errors"
	"llm-gateway/internal/billing"
	"llm-gateway/internal/observability"
	"net/http"
	"strings"
	"time"
)

type dispatchRetryStore interface {
	RetryDispatchException(context.Context, billing.DispatchRetryParams) (billing.Receipt, error)
}

func (r *recoveryRuntime) handleDispatchRetry(w http.ResponseWriter, req *http.Request) {
	store, ok := r.store.(dispatchRetryStore)
	if !ok {
		writeSettlementJSON(w, 503, map[string]any{"ok": false, "error": "dispatch retry unavailable"})
		return
	}
	body, err := decodeManualDisposition(w, req)
	if err != nil {
		writeSettlementJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if body.ActorUserID <= 0 {
		writeSettlementJSON(w, 403, map[string]any{"ok": false, "error": "an acting operator is required"})
		return
	}
	receipt, err := store.RetryDispatchException(req.Context(), billing.DispatchRetryParams{ExecutionID: req.PathValue("execution"), ActorUserID: body.ActorUserID, Reason: strings.TrimSpace(body.Reason), RequestID: observability.RequestID(req.Context()), Now: time.Now().UTC()})
	if err != nil {
		code := 503
		switch {
		case errors.Is(err, billing.ErrInvalidArgument):
			code = 400
		case errors.Is(err, billing.ErrExecutionNotFound), errors.Is(err, billing.ErrReceiptNotFound):
			code = 404
		case errors.Is(err, billing.ErrExecutionState), errors.Is(err, billing.ErrSettlementState), errors.Is(err, billing.ErrReceiptConflict), errors.Is(err, billing.ErrReceiptClaimLost):
			code = 409
		}
		writeSettlementJSON(w, code, map[string]any{"ok": false, "error": "retry refused or unavailable; inspect the receipt and audit before retrying"})
		return
	}
	writeSettlementJSON(w, 200, map[string]any{"ok": true, "execution_id": receipt.ExecutionID, "status": receipt.Status, "money_moved": false})
}
