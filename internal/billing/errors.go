// Package billing owns the money domain of the gateway: immutable price
// versions, exact integer charges, organisation billing accounts and the
// append-only ledger behind them.
//
// The rules are frozen by docs/decisions/ADR-0004-money-and-price-versions.md:
//
//   - the ledger unit is the micro-yuan (10^-6 CNY) held in an int64; float64 is
//     never used for money anywhere in this package;
//   - a price is the number of micro-yuan per 1_000_000 tokens, input and output
//     priced separately;
//   - a charge is computed once, over the weighted sum:
//     ceil((in*in_price + out*out_price) / 1_000_000), overflow-checked;
//   - amounts cross the HTTP boundary as decimal strings of micro-yuan, never as
//     JSON numbers (a float64 cannot hold an int64 exactly);
//   - price versions are immutable and are pinned at admission time (T06);
//   - list price, customer charge and provider cost are separate numbers, and an
//     unknown provider cost is not zero;
//   - the legacy metering_events.cost_cents column is untouched: it stays the
//     historical demo price, and is never recomputed from a price version.
//
// This package deliberately knows nothing about HTTP: internal/console declares
// the interfaces it needs and cmd/console wires the PostgreSQL stores in. The
// only shared-surface coupling is the console_audit_events table, which a
// successful adjustment writes inside its own transaction so the money fact and
// its audit row commit or roll back together.
package billing

import "errors"

// Domain errors. The HTTP layer only maps them onto status codes; it never
// re-derives the condition from a database error string.
var (
	// ErrAmountInvalid marks a value that is not a canonical integer micro
	// amount (empty, signed, fractional, non-numeric or out of int64 range).
	ErrAmountInvalid = errors.New("billing: not a decimal integer micro amount")

	// ErrPriceInvalid marks a negative price or an unsupported currency. A price
	// is a non-negative integer micro amount per million tokens.
	ErrPriceInvalid = errors.New("billing: price must be a non-negative integer micro amount per million tokens")

	// ErrUsageInvalid marks a negative token count. Token counts are counts; a
	// caller that can produce a negative one has a bug upstream of pricing.
	ErrUsageInvalid = errors.New("billing: token counts must be non-negative")

	// ErrAmountOverflow marks a charge that does not fit in an int64 micro
	// amount. It is returned *instead of* a wrapped number: silently truncating
	// money is never acceptable.
	ErrAmountOverflow = errors.New("billing: the amount exceeds the int64 micro range")

	// ErrPriceVersionExists marks a second price version for the same
	// (model, effective_at). Prices are immutable: publish a new effective_at.
	ErrPriceVersionExists = errors.New("billing: a price version for this model and effective_at already exists")

	// ErrPriceNotFound marks "no price version is effective for this model at
	// this time". T06 refuses to admit a request rather than guessing a price.
	ErrPriceNotFound = errors.New("billing: no price version is effective for this model at this time")

	// ErrAccountNotFound marks an organization without a billing account row.
	ErrAccountNotFound = errors.New("billing: billing account not found")

	// ErrEntryNotFound marks an unknown ledger entry, including an entry that
	// exists in another organization: the scoped lookup cannot see it.
	ErrEntryNotFound = errors.New("billing: billing entry not found")

	// ErrOrganizationNotFound marks an organization id that does not exist.
	ErrOrganizationNotFound = errors.New("billing: organization not found")

	// ErrIdempotencyConflict marks a repeated business id whose payload digest
	// differs from the stored one. The stored posting is left untouched and no
	// second entry is written.
	ErrIdempotencyConflict = errors.New("billing: this business id was already applied with a different payload")

	// ErrInvalidArgument marks a caller-fixable request that is not covered by a
	// more specific sentinel above.
	ErrInvalidArgument = errors.New("billing: invalid argument")
)
