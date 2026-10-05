package billing

import (
	"strings"
	"testing"
	"time"
)

// Pure (non-PostgreSQL) tests for the F02-1 abandonment marker vocabulary. The
// durable behaviour lives in abandon_pg_integration_test.go and
// recovery_pg_integration_test.go; what is pinned here is the *shape* of the
// marker, because it is a cross-process contract: cmd/gateway writes it, T07B's
// recovery matches it with a prefix, and both sides must agree without a shared
// type.

func TestValidateAbandonReason(t *testing.T) {
	accepted := []string{
		"client_cancelled",
		"upstream_stream_broken",
		"attempt_ended_without_response",
		"release_write_failed",
		strings.Repeat("a", MaxAbandonReasonLength),
	}
	for _, reason := range accepted {
		if err := ValidateAbandonReason(reason); err != nil {
			t.Fatalf("ValidateAbandonReason(%q)=%v, want nil", reason, err)
		}
	}

	refused := map[string]string{
		"":                           "empty",
		"   ":                        "blank",
		" client_cancelled":          "leading whitespace",
		"client_cancelled ":          "trailing whitespace",
		"abandoned:client_cancelled": "the marker is added by the store, not the caller",
		"line\nbreak":                "control character",
		"tab\tseparated":             "control character",
		strings.Repeat("a", MaxAbandonReasonLength+1): "over the terminal_reason bound",
	}
	for reason, why := range refused {
		err := ValidateAbandonReason(reason)
		if err == nil {
			t.Fatalf("ValidateAbandonReason(%q) succeeded, want a refusal (%s)", reason, why)
		}
		if !strings.Contains(err.Error(), "abandonment reason") {
			t.Fatalf("ValidateAbandonReason(%q)=%v, want an abandonment-reason diagnosis", reason, err)
		}
	}
}

// TestAbandonedMarkerFitsTerminalReason proves the offset arithmetic: the
// longest accepted reason must still leave the full marker inside the column
// bound the migration enforces.
func TestAbandonedMarkerFitsTerminalReason(t *testing.T) {
	reason := strings.Repeat("x", MaxAbandonReasonLength)
	marker := AbandonedReasonPrefix + reason
	if len(marker) > MaxReasonLength {
		t.Fatalf("marker length=%d, the terminal_reason column accepts %d", len(marker), MaxReasonLength)
	}
	if err := ValidateTerminalReason(marker); err != nil {
		t.Fatalf("the marker must satisfy the terminal-reason validation: %v", err)
	}
}

func TestAbandonedReasonHelpers(t *testing.T) {
	reason := "client_cancelled"
	marked := Execution{TerminalReason: &reason}
	// The marker helpers take the raw suffix, so a hand-built row without the
	// prefix is not an abandonment.
	if IsAbandonedExecution(marked) {
		t.Fatal("a reason without the marker prefix is not an abandonment")
	}
	marker := AbandonedReasonPrefix + reason
	marked = Execution{TerminalReason: &marker}
	if !IsAbandonedExecution(marked) {
		t.Fatal("the marker must be recognized")
	}
	if got := AbandonedReason(marked); got != reason {
		t.Fatalf("AbandonedReason=%q, want %q", got, reason)
	}

	// No terminal reason at all: never an abandonment, and never a panic.
	if IsAbandonedExecution(Execution{}) || AbandonedReason(Execution{}) != "" {
		t.Fatal("an execution with no terminal reason must not be an abandonment")
	}
	// Another path's terminal reason must never be mistaken for a marker.
	other := "recovery: owner instance gw-a not seen within the 60s liveness lease"
	if IsAbandonedExecution(Execution{TerminalReason: &other}) {
		t.Fatal("a recovery reason is not an abandonment marker")
	}
	// A prefix that is only a prefix of the *word* is not the marker either.
	near := "abandoned"
	if IsAbandonedExecution(Execution{TerminalReason: &near}) {
		t.Fatal("`abandoned` without the separator is not the marker")
	}
}

// TestRecoveryCandidateScanAcceptsTheMarker guards the one place where the
// marker is matched in SQL: the constant and the query string must not drift,
// and the liveness half must stay a fallback (LEFT JOIN + OR) instead of being
// removed.
func TestRecoveryCandidateScanAcceptsTheMarker(t *testing.T) {
	query := queryListRecoveryCandidates
	if !strings.Contains(query, "LEFT JOIN gateway_instances") {
		t.Fatalf("the candidate scan must keep the liveness JOIN optional:\n%s", query)
	}
	if !strings.Contains(query, "e.terminal_reason LIKE '"+AbandonedReasonPrefix+"%'") {
		t.Fatalf("the candidate scan must recognize the %q marker:\n%s", AbandonedReasonPrefix, query)
	}
	if !strings.Contains(query, "e.status IN ('reserved', 'executing')") {
		t.Fatalf("the marker must not widen the scanned states:\n%s", query)
	}
	if !strings.Contains(query, "e.updated_at < $1") {
		t.Fatalf("the marker must not remove the time window:\n%s", query)
	}
	// The owner-unknown gap must not keep reporting an execution the automatic
	// path can now release.
	if !strings.Contains(queryRecoveryOwnerUnknown, "NOT LIKE '"+AbandonedReasonPrefix+"%'") {
		t.Fatalf("the owner-unknown gap must exclude marked executions:\n%s", queryRecoveryOwnerUnknown)
	}
}

// TestRecoveryReasonNamesTheAbandonment pins the human-readable half: after the
// release overwrites terminal_reason, the abandonment reason must still be
// visible on the execution row and in the audit detail.
func TestRecoveryReasonNamesTheAbandonment(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	marker := AbandonedReasonPrefix + "upstream_stream_broken"
	opts := NormalizeRecoveryOptions(RecoveryOptions{})
	candidate := Execution{
		ExecutionID:    "exec_marked",
		Status:         StatusExecuting,
		UpdatedAt:      now.Add(-10 * time.Minute),
		TerminalReason: &marker,
	}
	reason := recoveryReason(candidate, now, opts)
	if !strings.Contains(reason, "abandoned (upstream_stream_broken)") {
		t.Fatalf("reason=%q, want it to name the abandonment reason", reason)
	}
	if !strings.Contains(reason, "platform absorbs the unknown upstream cost and the customer is not charged") {
		t.Fatalf("reason=%q, want the platform-absorbs policy", reason)
	}

	// Without a marker the owner-liveness story is the one recorded.
	unmarked := candidate
	unmarked.TerminalReason = nil
	owner := "gw-a"
	unmarked.GatewayID = &owner
	reason = recoveryReason(unmarked, now, opts)
	if !strings.Contains(reason, "owner instance gw-a not seen") {
		t.Fatalf("reason=%q, want the liveness story for an unmarked execution", reason)
	}
}
