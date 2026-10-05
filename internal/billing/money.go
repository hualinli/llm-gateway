package billing

import (
	"strconv"
	"strings"
)

// The ledger unit (ADR-0004 §1). Money is an integer count of micro-yuan:
// 1 CNY = 1_000_000 micro-yuan. Nothing in this package ever converts to
// float64, to cents or to a decimal string with a fractional part before the
// API boundary, because each of those loses precision the ledger must keep.
const (
	// MicroPerUnit is how many micro-yuan make one currency unit.
	MicroPerUnit int64 = 1_000_000

	// MicrosPerMillionTokens is the price denominator: a price is "this many
	// micro-yuan per one million tokens".
	MicrosPerMillionTokens int64 = 1_000_000

	// CurrencyCNY is the only currency of this version (ADR-0004 §1). Migration
	// 000008 enforces the same constant with a CHECK constraint, so a mixed
	// currency ledger is impossible even for a hand-written SQL statement.
	CurrencyCNY = "CNY"
)

// FormatMicro renders an integer micro amount as a decimal string. This is the
// wire format for every amount in the Console API: charges, prices and balances
// are strings, never JSON numbers, so a JavaScript client cannot silently round
// an int64 through a float64.
func FormatMicro(micro int64) string { return strconv.FormatInt(micro, 10) }

// ParseMicro parses a canonical decimal integer micro amount. It is stricter
// than strconv.ParseInt on purpose: leading '+', surrounding whitespace,
// fractional parts, exponents and non-ASCII digits are all rejected, so two
// different strings can never mean the same amount (that would break request
// digests).
func ParseMicro(raw string) (int64, error) {
	if raw == "" {
		return 0, ErrAmountInvalid
	}
	digits := raw
	if strings.HasPrefix(digits, "-") {
		digits = digits[1:]
	}
	if digits == "" {
		return 0, ErrAmountInvalid
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, ErrAmountInvalid
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		// Only an int64 range overflow can reach here: the shape is already
		// validated above.
		return 0, ErrAmountInvalid
	}
	return value, nil
}

// ParsePositiveMicro parses a strictly positive micro amount. Credit and debit
// adjustments use it: the sign of a posting lives in `direction`, so a
// non-positive `amount_micro` is always a caller bug.
func ParsePositiveMicro(raw string) (int64, error) {
	value, err := ParseMicro(raw)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, ErrAmountInvalid
	}
	return value, nil
}
