package billing

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestFormatAndParseMicroRoundTrip(t *testing.T) {
	t.Parallel()
	values := []int64{0, 1, -1, 999_999, 1_000_000, -1_000_000, math.MaxInt64, math.MinInt64}
	for _, value := range values {
		formatted := FormatMicro(value)
		parsed, err := ParseMicro(formatted)
		if err != nil {
			t.Fatalf("ParseMicro(%q): %v", formatted, err)
		}
		if parsed != value {
			t.Fatalf("round trip of %d produced %d (via %q)", value, parsed, formatted)
		}
	}
}

func TestParseMicroRejectsNonCanonicalInput(t *testing.T) {
	t.Parallel()
	// Two strings that mean the same amount would make an idempotency digest
	// ambiguous, so "1.0", "+1", " 1" and "1 " are all refused.
	for _, raw := range []string{
		"", " ", "+1", " 1", "1 ", "1.0", "1e6", "0x10", "one", "-", "--1", "١٢٣",
		"9223372036854775808", "-9223372036854775809",
	} {
		if _, err := ParseMicro(raw); !errors.Is(err, ErrAmountInvalid) {
			t.Fatalf("ParseMicro(%q) = %v, want ErrAmountInvalid", raw, err)
		}
	}
	if value, err := ParseMicro("-5"); err != nil || value != -5 {
		t.Fatalf("ParseMicro(-5) = (%d, %v)", value, err)
	}
}

func TestParsePositiveMicro(t *testing.T) {
	t.Parallel()
	if _, err := ParsePositiveMicro("0"); !errors.Is(err, ErrAmountInvalid) {
		t.Fatalf("zero must not be a valid posting amount: %v", err)
	}
	if _, err := ParsePositiveMicro("-1"); !errors.Is(err, ErrAmountInvalid) {
		t.Fatalf("a negative amount must not be a valid posting amount: %v", err)
	}
	value, err := ParsePositiveMicro("1500000")
	if err != nil || value != 1_500_000 {
		t.Fatalf("ParsePositiveMicro(1500000) = (%d, %v)", value, err)
	}
}

func TestAdjustmentDigestIsStableAndSensitive(t *testing.T) {
	t.Parallel()
	base := AdjustmentDigest(7, 1_500_000, DirectionCredit, KindAdjustment, "goodwill credit")
	if len(base) != 64 {
		t.Fatalf("digest is %d characters, want a sha256 hex string", len(base))
	}
	if again := AdjustmentDigest(7, 1_500_000, DirectionCredit, KindAdjustment, "goodwill credit"); again != base {
		t.Fatalf("digest is not deterministic: %q != %q", again, base)
	}

	changed := map[string]string{
		"organization": AdjustmentDigest(8, 1_500_000, DirectionCredit, KindAdjustment, "goodwill credit"),
		"amount":       AdjustmentDigest(7, 1_500_001, DirectionCredit, KindAdjustment, "goodwill credit"),
		"direction":    AdjustmentDigest(7, 1_500_000, DirectionDebit, KindAdjustment, "goodwill credit"),
		"kind":         AdjustmentDigest(7, 1_500_000, DirectionCredit, KindCredit, "goodwill credit"),
		"reason":       AdjustmentDigest(7, 1_500_000, DirectionCredit, KindAdjustment, "goodwill credit "),
	}
	for field, digest := range changed {
		if digest == base {
			t.Fatalf("changing the %s must change the digest", field)
		}
	}

	// Length-prefixing defeats separator smuggling: these two triples must not
	// hash to the same value.
	left := AdjustmentDigest(1, 1, "a", "b", "c")
	right := AdjustmentDigest(1, 1, "ab", "", "c")
	if left == right {
		t.Fatal("length-prefixed digest fields must not be confusable")
	}
}

func TestValidateAdjustment(t *testing.T) {
	t.Parallel()
	valid := AdjustParams{
		OrgID:          7,
		AmountMicro:    1_500_000,
		Direction:      DirectionCredit,
		Kind:           KindAdjustment,
		Reason:         "goodwill credit",
		IdempotencyKey: "adj-1",
		PayloadDigest:  strings.Repeat("a", 64),
	}
	normalized := NormalizeAdjustment(valid)
	if err := ValidateAdjustment(normalized); err != nil {
		t.Fatalf("valid adjustment refused: %v", err)
	}
	if normalized.BusinessID != "adj-1" || normalized.IdempotencyKey != "adj-1" {
		t.Fatalf("business id and idempotency key must agree: %+v", normalized)
	}

	broken := map[string]func(*AdjustParams){
		"missing organization": func(p *AdjustParams) { p.OrgID = 0 },
		"zero amount":          func(p *AdjustParams) { p.AmountMicro = 0 },
		"negative amount":      func(p *AdjustParams) { p.AmountMicro = -1 },
		"unknown direction":    func(p *AdjustParams) { p.Direction = "sideways" },
		"unknown kind":         func(p *AdjustParams) { p.Kind = "donation" },
		"padded reason":        func(p *AdjustParams) { p.Reason = " spaced " },
		"long reason":          func(p *AdjustParams) { p.Reason = strings.Repeat("x", MaxReasonLength+1) },
		"missing key":          func(p *AdjustParams) { p.IdempotencyKey = ""; p.BusinessID = "" },
		"padded key":           func(p *AdjustParams) { p.IdempotencyKey = " adj "; p.BusinessID = "" },
		"long key": func(p *AdjustParams) {
			p.IdempotencyKey = strings.Repeat("k", MaxBusinessIDLength+1)
			p.BusinessID = ""
		},
		"short digest":   func(p *AdjustParams) { p.PayloadDigest = "abc" },
		"mismatched ids": func(p *AdjustParams) { p.BusinessID = "other" },
	}
	for name, mutate := range broken {
		params := valid
		mutate(&params)
		if err := ValidateAdjustment(params); err == nil {
			t.Fatalf("%s: ValidateAdjustment accepted %+v", name, params)
		}
	}
}

func TestProjectEntryMovesTheRightBucket(t *testing.T) {
	t.Parallel()
	account := Account{PostedBalanceMicro: 10_000_000, ReservedMicro: 2_000_000}

	posted, reserved, err := projectEntry(account, KindAdjustment, 1_500_000)
	if err != nil || posted != 11_500_000 || reserved != 2_000_000 {
		t.Fatalf("credit: posted=%d reserved=%d err=%v", posted, reserved, err)
	}
	posted, reserved, err = projectEntry(account, KindDebit, -1_500_000)
	if err != nil || posted != 8_500_000 || reserved != 2_000_000 {
		t.Fatalf("debit: posted=%d reserved=%d err=%v", posted, reserved, err)
	}
	// A reservation is a credit to the reserved bucket; a release is a debit.
	posted, reserved, err = projectEntry(account, KindReservation, 1_000_000)
	if err != nil || posted != 10_000_000 || reserved != 3_000_000 {
		t.Fatalf("reservation: posted=%d reserved=%d err=%v", posted, reserved, err)
	}
	posted, reserved, err = projectEntry(account, KindRelease, -1_000_000)
	if err != nil || posted != 10_000_000 || reserved != 1_000_000 {
		t.Fatalf("release: posted=%d reserved=%d err=%v", posted, reserved, err)
	}

	// A release larger than the reserved bucket is refused rather than driving
	// the projection negative (the database CHECK would refuse it too).
	if _, _, err := projectEntry(account, KindRelease, -3_000_000); err == nil {
		t.Fatal("releasing more than is reserved must fail")
	}
	// A negative posted balance is allowed (an explicit debt), but it must be
	// reached by arithmetic, never by wrapping.
	posted, _, err = projectEntry(Account{PostedBalanceMicro: 1_000_000}, KindDebit, -2_000_000)
	if err != nil || posted != -1_000_000 {
		t.Fatalf("debt: posted=%d err=%v", posted, err)
	}
	if _, _, err := projectEntry(Account{PostedBalanceMicro: math.MinInt64}, KindDebit, -1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("a projection underflow must be reported, got %v", err)
	}
}

func TestAccountAvailableMicro(t *testing.T) {
	t.Parallel()
	account := Account{PostedBalanceMicro: 10_000_000, ReservedMicro: 3_000_000}
	if got := account.AvailableMicro(); got != 7_000_000 {
		t.Fatalf("AvailableMicro = %d, want 7000000", got)
	}
	debt := Account{PostedBalanceMicro: -1_000_000, ReservedMicro: 0}
	if got := debt.AvailableMicro(); got != -1_000_000 {
		t.Fatalf("a negative available amount is a debt, got %d", got)
	}
	// The one wrap that is theoretically reachable must saturate visibly.
	wrapped := Account{PostedBalanceMicro: math.MinInt64, ReservedMicro: 1}
	if got := wrapped.AvailableMicro(); got != math.MinInt64 {
		t.Fatalf("AvailableMicro wrapped to %d", got)
	}
}

func TestAddMicroOverflow(t *testing.T) {
	t.Parallel()
	if _, err := addMicro(math.MaxInt64, 1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("overflow must be reported, got %v", err)
	}
	if _, err := addMicro(math.MinInt64, -1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("underflow must be reported, got %v", err)
	}
	if sum, err := addMicro(math.MaxInt64, -1); err != nil || sum != math.MaxInt64-1 {
		t.Fatalf("addMicro = (%d, %v)", sum, err)
	}
}

func TestEntrySignedMicro(t *testing.T) {
	t.Parallel()
	credit := Entry{AmountMicro: 5, Direction: DirectionCredit}
	if credit.SignedMicro() != 5 || credit.AffectsReserved() {
		t.Fatalf("credit entry projected wrong: %+v", credit)
	}
	debit := Entry{AmountMicro: 5, Direction: DirectionDebit}
	if debit.SignedMicro() != -5 {
		t.Fatalf("debit entry projected wrong: %+v", debit)
	}
	reservation := Entry{Kind: KindReservation, AmountMicro: 5, Direction: DirectionDebit}
	if !reservation.AffectsReserved() {
		t.Fatalf("a reservation must move the reserved bucket: %+v", reservation)
	}
}

func TestValidateCreatePriceVersion(t *testing.T) {
	t.Parallel()
	negative := int64(-1)
	valid := CreatePriceVersionParams{
		Model:                "gpt-4o",
		Currency:             CurrencyCNY,
		InputMicroPerMillion: 1_500_000,
		EffectiveAt:          testTime(),
	}
	if err := ValidateCreatePriceVersion(valid); err != nil {
		t.Fatalf("valid price refused: %v", err)
	}
	broken := map[string]func(*CreatePriceVersionParams){
		"empty model":            func(p *CreatePriceVersionParams) { p.Model = "" },
		"padded model":           func(p *CreatePriceVersionParams) { p.Model = " gpt-4o" },
		"model too long":         func(p *CreatePriceVersionParams) { p.Model = strings.Repeat("m", MaxModelNameLength+1) },
		"foreign currency":       func(p *CreatePriceVersionParams) { p.Currency = "USD" },
		"negative input price":   func(p *CreatePriceVersionParams) { p.InputMicroPerMillion = -1 },
		"negative output price":  func(p *CreatePriceVersionParams) { p.OutputMicroPerMillion = -2 },
		"negative provider cost": func(p *CreatePriceVersionParams) { p.ProviderInputCostMicroPerMillion = &negative },
		"missing effective_at":   func(p *CreatePriceVersionParams) { p.EffectiveAt = zeroTime() },
	}
	for name, mutate := range broken {
		params := valid
		mutate(&params)
		if err := ValidateCreatePriceVersion(params); err == nil {
			t.Fatalf("%s: ValidateCreatePriceVersion accepted %+v", name, params)
		}
	}
}

func TestValidateListPriceVersions(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1, 101, 1000} {
		if err := ValidateListPriceVersions(ListPriceVersionsParams{Limit: limit}); err == nil {
			t.Fatalf("limit=%d must be refused", limit)
		}
	}
	if err := ValidateListPriceVersions(ListPriceVersionsParams{Limit: 100}); err != nil {
		t.Fatalf("limit=100 must be accepted: %v", err)
	}
	if err := ValidateListPriceVersions(ListPriceVersionsParams{Limit: 20, BeforeID: -1}); err == nil {
		t.Fatal("a negative cursor must be refused")
	}
}
