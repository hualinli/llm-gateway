package billing

import "math/bits"

// Price is the list price of one model: micro-yuan per one million tokens,
// input and output priced separately (ADR-0004 §1). Both fields are
// non-negative; a price is never a float and never a per-token decimal.
type Price struct {
	InputMicroPerMillion  int64
	OutputMicroPerMillion int64
}

// TokenUsage is the billable usage of one execution. Upstream `usage` fields
// are counts and are never negative; a negative count is rejected rather than
// clamped, because clamping would quietly hide a parsing bug in the provider
// adapter.
type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
}

// ChargeMicro computes the customer charge for one usage under one price:
//
//	charge = ceil((input_tokens * input_price + output_tokens * output_price) / 1_000_000)
//
// Rounding happens exactly once, on the weighted sum. Rounding per token or per
// partial batch would accumulate error in the platform's favour — for example
// 3 input tokens at a price of 1 micro per million tokens costs 1 micro here,
// while three separate one-token charges would cost 3 micro.
//
// The product is evaluated in 128 bits and the quotient is proven to fit in an
// int64 *before* it is converted. An out-of-range charge returns
// ErrAmountOverflow; it never wraps and never saturates.
func ChargeMicro(price Price, usage TokenUsage) (int64, error) {
	if price.InputMicroPerMillion < 0 || price.OutputMicroPerMillion < 0 {
		return 0, ErrPriceInvalid
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return 0, ErrUsageInvalid
	}

	// Invariant: every operand is a non-negative int64, so the uint64
	// conversions below are exact and no sign is lost.
	productHi, productLo := bits.Mul64(uint64(usage.InputTokens), uint64(price.InputMicroPerMillion))
	outputHi, outputLo := bits.Mul64(uint64(usage.OutputTokens), uint64(price.OutputMicroPerMillion))

	// 128-bit sum of the two products.
	var carry uint64
	productLo, carry = bits.Add64(productLo, outputLo, 0)
	productHi, _ = bits.Add64(productHi, outputHi, carry)

	// ceil(sum / 1_000_000) == floor((sum + 999_999) / 1_000_000). The addend is
	// small enough that the carry can only propagate by one word.
	productLo, carry = bits.Add64(productLo, uint64(MicrosPerMillionTokens)-1, 0)
	productHi, _ = bits.Add64(productHi, 0, carry)

	// bits.Div64 requires hi < y, and that precondition is exactly the overflow
	// test: if the high word is >= 1_000_000 the quotient is >= 2^64 and cannot
	// be represented at all.
	if productHi >= uint64(MicrosPerMillionTokens) {
		return 0, ErrAmountOverflow
	}
	quotient, _ := bits.Div64(productHi, productLo, uint64(MicrosPerMillionTokens))
	if quotient > maxInt64 {
		return 0, ErrAmountOverflow
	}
	return int64(quotient), nil
}

// maxInt64 is math.MaxInt64 expressed in the uint64 domain so the comparison
// above needs no conversion and cannot accidentally compare a float.
const maxInt64 = uint64(1)<<63 - 1

// ChargeMicro is the customer charge for this pinned price version. T06 calls
// it once per terminal usage record with the price version it froze at
// admission, never with "the price that is current now".
func (p PriceVersion) ChargeMicro(inputTokens, outputTokens int64) (int64, error) {
	return ChargeMicro(p.Price(), TokenUsage{InputTokens: inputTokens, OutputTokens: outputTokens})
}

// ProviderCostMicro is what the platform owes the provider for the same usage.
// It is a separate number from the customer charge (ADR-0004 §5): a failed or
// retried attempt can cost the platform money without being billed to the
// customer.
//
// The second result reports whether both cost fields are known. A missing
// provider cost is *not* zero: callers must record "unknown" rather than a
// fabricated 0, so a margin computed from it stays visibly incomplete.
func (p PriceVersion) ProviderCostMicro(inputTokens, outputTokens int64) (int64, bool, error) {
	if p.ProviderInputCostMicroPerMillion == nil || p.ProviderOutputCostMicroPerMillion == nil {
		return 0, false, nil
	}
	cost, err := ChargeMicro(Price{
		InputMicroPerMillion:  *p.ProviderInputCostMicroPerMillion,
		OutputMicroPerMillion: *p.ProviderOutputCostMicroPerMillion,
	}, TokenUsage{InputTokens: inputTokens, OutputTokens: outputTokens})
	if err != nil {
		return 0, false, err
	}
	return cost, true, nil
}

// Price is the price pair of this version, ready for ChargeMicro.
func (p PriceVersion) Price() Price {
	return Price{
		InputMicroPerMillion:  p.InputMicroPerMillion,
		OutputMicroPerMillion: p.OutputMicroPerMillion,
	}
}
