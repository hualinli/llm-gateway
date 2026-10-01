package billing

import (
	"errors"
	"testing"
	"time"
)

func TestParseStatementMonthIsUTCCanonical(t *testing.T) {
	now := time.Date(2026, 10, 1, 23, 30, 0, 0, time.UTC)
	cases := []struct {
		raw       string
		wantLabel string
		wantFrom  time.Time
		wantTo    time.Time
	}{
		{"", "2026-10", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-09", "2026-09", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-02", "2026-02", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"2024-02", "2024-02", time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		month, err := ParseStatementMonth(tc.raw, now)
		if err != nil {
			t.Fatalf("ParseStatementMonth(%q): %v", tc.raw, err)
		}
		if month.Label != tc.wantLabel || !month.From.Equal(tc.wantFrom) || !month.CutoffAt.Equal(tc.wantTo) {
			t.Fatalf("ParseStatementMonth(%q) = %+v", tc.raw, month)
		}
	}
	for _, bad := range []string{"2026", "2026-1", "2026-13", "2026-10-01", "October", "2026-00"} {
		if _, err := ParseStatementMonth(bad, now); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("ParseStatementMonth(%q) = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

func TestStatementParamsClampAndValidate(t *testing.T) {
	month, err := ParseStatementMonth("2026-09", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := NormalizeStatementParams(StatementParams{OrgID: 1, Month: month, Limit: 10_000}).Limit; got != MaxStatementLimit {
		t.Fatalf("limit = %d, want %d", got, MaxStatementLimit)
	}
	if got := NormalizeStatementParams(StatementParams{OrgID: 1, Month: month}).Limit; got != DefaultStatementLimit {
		t.Fatalf("default limit = %d, want %d", got, DefaultStatementLimit)
	}
	if err := ValidateStatementParams(StatementParams{Month: month}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a statement without an organization = %v", err)
	}
	if err := ValidateStatementParams(StatementParams{OrgID: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a statement without a month = %v", err)
	}
}

// TestStatementDebitPrefixMatchesTheSQLLiteral pins the prefix the statement SQL
// hardcodes (it joins a debit entry back to its receipt) against the constructor
// that produces it.
func TestStatementDebitPrefixMatchesTheSQLLiteral(t *testing.T) {
	if StatementDebitPrefix() != statementDebitPrefixLiteral {
		t.Fatalf("StatementDebitPrefix() = %q, SQL uses %q", StatementDebitPrefix(), statementDebitPrefixLiteral)
	}
}
