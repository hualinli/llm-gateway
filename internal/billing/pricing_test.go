package billing

import (
	"errors"
	"math"
	"testing"
)

// Every expected value in this file is computed by hand in the comment next to
// it. The pricing function is the one place in the repository where a rounding
// mistake becomes a wrong invoice, so the test states the arithmetic rather
// than re-deriving it with the same code under test.

func TestChargeMicroHandComputed(t *testing.T) {
	t.Parallel()
	const (
		cnyPerMillion = int64(1_000_000) // 1 CNY per million tokens
	)

	cases := []struct {
		name       string
		price      Price
		usage      TokenUsage
		wantMicro  int64
		wantErrIs  error
		arithmetic string
	}{
		{
			name:       "zero tokens cost zero",
			price:      Price{InputMicroPerMillion: cnyPerMillion, OutputMicroPerMillion: 2 * cnyPerMillion},
			usage:      TokenUsage{},
			wantMicro:  0,
			arithmetic: "ceil(0/1e6) = 0",
		},
		{
			name:       "one input token at one micro per million rounds up",
			price:      Price{InputMicroPerMillion: 1},
			usage:      TokenUsage{InputTokens: 1},
			wantMicro:  1,
			arithmetic: "ceil(1*1/1e6) = ceil(0.000001) = 1",
		},
		{
			name:       "one input token at one CNY per million is exact",
			price:      Price{InputMicroPerMillion: cnyPerMillion},
			usage:      TokenUsage{InputTokens: 1},
			wantMicro:  1,
			arithmetic: "ceil(1*1e6/1e6) = ceil(1) = 1, no spurious round-up",
		},
		{
			name:       "one million tokens at one micro each is exactly one micro",
			price:      Price{InputMicroPerMillion: 1},
			usage:      TokenUsage{InputTokens: 1_000_000},
			wantMicro:  1,
			arithmetic: "ceil(1e6*1/1e6) = 1",
		},
		{
			name:       "exact half rounds up",
			price:      Price{InputMicroPerMillion: 1},
			usage:      TokenUsage{InputTokens: 500_000},
			wantMicro:  1,
			arithmetic: "ceil(500000/1e6) = ceil(0.5) = 1",
		},
		{
			name:       "just above an exact multiple rounds up",
			price:      Price{InputMicroPerMillion: 1},
			usage:      TokenUsage{InputTokens: 1_000_001},
			wantMicro:  2,
			arithmetic: "ceil(1000001/1e6) = ceil(1.000001) = 2",
		},
		{
			name:       "three tokens are rounded once, not per token",
			price:      Price{InputMicroPerMillion: 1},
			usage:      TokenUsage{InputTokens: 3},
			wantMicro:  1,
			arithmetic: "ceil(3/1e6) = 1; three separate one-token charges would be 3",
		},
		{
			name:       "input and output share one rounding",
			price:      Price{InputMicroPerMillion: 1, OutputMicroPerMillion: 1},
			usage:      TokenUsage{InputTokens: 1, OutputTokens: 1},
			wantMicro:  1,
			arithmetic: "ceil((1+1)/1e6) = ceil(2e-6) = 1; rounding each side would give 2",
		},
		{
			name:       "mixed input and output",
			price:      Price{InputMicroPerMillion: 1_500_000, OutputMicroPerMillion: 6_000_000},
			usage:      TokenUsage{InputTokens: 1_000_000, OutputTokens: 2_000_000},
			wantMicro:  13_500_000,
			arithmetic: "ceil((1e6*1.5e6 + 2e6*6e6)/1e6) = ceil((1.5e12+1.2e13)/1e6) = 13.5e6",
		},
		{
			name:       "large but exact",
			price:      Price{InputMicroPerMillion: cnyPerMillion},
			usage:      TokenUsage{InputTokens: 1_000_000_000},
			wantMicro:  1_000_000_000,
			arithmetic: "ceil(1e9*1e6/1e6) = 1e9 micro = 1000 CNY",
		},
		{
			name:       "largest charge that still fits int64",
			price:      Price{InputMicroPerMillion: cnyPerMillion},
			usage:      TokenUsage{InputTokens: math.MaxInt64},
			wantMicro:  math.MaxInt64,
			arithmetic: "ceil(MaxInt64*1e6/1e6) = MaxInt64, exactly the boundary",
		},
		{
			name:       "one micro more than the boundary overflows",
			price:      Price{InputMicroPerMillion: cnyPerMillion, OutputMicroPerMillion: 1},
			usage:      TokenUsage{InputTokens: math.MaxInt64, OutputTokens: 1},
			wantErrIs:  ErrAmountOverflow,
			arithmetic: "ceil((MaxInt64*1e6 + 1)/1e6) = MaxInt64+1, one past int64",
		},
		{
			name:       "double price at MaxInt64 tokens overflows",
			price:      Price{InputMicroPerMillion: 2 * cnyPerMillion},
			usage:      TokenUsage{InputTokens: math.MaxInt64},
			wantErrIs:  ErrAmountOverflow,
			arithmetic: "ceil(MaxInt64*2e6/1e6) = 2*MaxInt64",
		},
		{
			name:       "max price times max tokens overflows without wrapping",
			price:      Price{InputMicroPerMillion: math.MaxInt64, OutputMicroPerMillion: math.MaxInt64},
			usage:      TokenUsage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64},
			wantErrIs:  ErrAmountOverflow,
			arithmetic: "the 128-bit numerator exceeds 1e6*2^64, so the quotient does not fit",
		},
		{
			name:       "large price alone overflows",
			price:      Price{InputMicroPerMillion: 1_000_000_000},
			usage:      TokenUsage{InputTokens: math.MaxInt64},
			wantErrIs:  ErrAmountOverflow,
			arithmetic: "ceil(MaxInt64*1e9/1e6) = MaxInt64*1000",
		},
		{
			name:      "negative input tokens are refused",
			price:     Price{InputMicroPerMillion: 1},
			usage:     TokenUsage{InputTokens: -1},
			wantErrIs: ErrUsageInvalid,
		},
		{
			name:      "negative output tokens are refused",
			price:     Price{InputMicroPerMillion: 1, OutputMicroPerMillion: 1},
			usage:     TokenUsage{OutputTokens: -1},
			wantErrIs: ErrUsageInvalid,
		},
		{
			name:      "a negative price is refused",
			price:     Price{InputMicroPerMillion: -1},
			usage:     TokenUsage{InputTokens: 1},
			wantErrIs: ErrPriceInvalid,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ChargeMicro(tc.price, tc.usage)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("ChargeMicro = (%d, %v), want error %v (%s)", got, err, tc.wantErrIs, tc.arithmetic)
				}
				if got != 0 {
					t.Fatalf("an overflowed charge must return 0, never a wrapped %d (%s)", got, tc.arithmetic)
				}
				return
			}
			if err != nil {
				t.Fatalf("ChargeMicro: %v (%s)", err, tc.arithmetic)
			}
			if got != tc.wantMicro {
				t.Fatalf("ChargeMicro = %d micro, want %d micro (%s)", got, tc.wantMicro, tc.arithmetic)
			}
		})
	}
}

// TestChargeMicroMonotonic pins the property an invoice depends on: adding
// usage never lowers the charge. It is a cheap guard against an accidental
// signed/unsigned mix-up in the 128-bit path.
func TestChargeMicroMonotonic(t *testing.T) {
	t.Parallel()
	price := Price{InputMicroPerMillion: 1_500_000, OutputMicroPerMillion: 6_000_000}
	previous := int64(0)
	for tokens := int64(0); tokens <= 5_000_000; tokens += 123_457 {
		got, err := ChargeMicro(price, TokenUsage{InputTokens: tokens, OutputTokens: tokens})
		if err != nil {
			t.Fatalf("tokens=%d: %v", tokens, err)
		}
		if got < previous {
			t.Fatalf("charge decreased: %d tokens -> %d micro after %d", tokens, got, previous)
		}
		previous = got
	}
}

func TestPriceVersionChargeAndProviderCost(t *testing.T) {
	t.Parallel()
	input, output := int64(1_500_000), int64(6_000_000)
	version := PriceVersion{
		InputMicroPerMillion:              input,
		OutputMicroPerMillion:             output,
		ProviderInputCostMicroPerMillion:  &input,
		ProviderOutputCostMicroPerMillion: &output,
	}
	charge, err := version.ChargeMicro(1_000_000, 2_000_000)
	if err != nil || charge != 13_500_000 {
		t.Fatalf("ChargeMicro = (%d, %v), want 13500000", charge, err)
	}
	// The customer is charged the list price and the provider is paid the same
	// list price in this fixture: a zero margin, stated explicitly.
	cost, known, err := version.ProviderCostMicro(1_000_000, 2_000_000)
	if err != nil || !known || cost != charge {
		t.Fatalf("ProviderCostMicro = (%d, %v, %v), want (%d, true, nil)", cost, known, err, charge)
	}

	unknown := PriceVersion{InputMicroPerMillion: input, OutputMicroPerMillion: output}
	cost, known, err = unknown.ProviderCostMicro(1_000_000, 2_000_000)
	if err != nil {
		t.Fatalf("unknown provider cost: %v", err)
	}
	if known || cost != 0 {
		t.Fatalf("missing provider cost must be reported as unknown, got (%d, %v)", cost, known)
	}
}

// TestChargeMicroNeverWraps walks the inputs around the int64 boundary and
// asserts the failure mode is always an error, never a negative or truncated
// amount. A wrapped charge is the worst possible bug in this package.
func TestChargeMicroNeverWraps(t *testing.T) {
	t.Parallel()
	prices := []int64{1, 999_999, 1_000_000, 1_000_001, 1 << 40, math.MaxInt64}
	tokens := []int64{0, 1, 999_999, 1_000_000, 1 << 40, math.MaxInt64}
	for _, price := range prices {
		for _, token := range tokens {
			charge, err := ChargeMicro(Price{InputMicroPerMillion: price}, TokenUsage{InputTokens: token})
			if err != nil {
				if !errors.Is(err, ErrAmountOverflow) {
					t.Fatalf("price=%d tokens=%d: unexpected error %v", price, token, err)
				}
				if charge != 0 {
					t.Fatalf("price=%d tokens=%d: overflow returned %d instead of 0", price, token, charge)
				}
				continue
			}
			if charge < 0 {
				t.Fatalf("price=%d tokens=%d: negative charge %d", price, token, charge)
			}
			// A successful charge is never smaller than the product divided by
			// the denominator, rounded down. This catches a truncating bug that
			// silently under-charges.
			if price > 0 && token > 0 {
				floor := int64(0)
				if token <= math.MaxInt64/price {
					floor = (token * price) / MicrosPerMillionTokens
				}
				if charge < floor {
					t.Fatalf("price=%d tokens=%d: charge %d is below the floor %d", price, token, charge, floor)
				}
			}
		}
	}
}
