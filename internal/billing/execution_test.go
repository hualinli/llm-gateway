package billing

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// These tests are pure: they pin the vocabulary and the request validation that
// the PostgreSQL tests then exercise against a real database. The migration
// file is read directly so a status added to Go but not to the CHECK (or the
// other way round) fails here rather than at 03:00 in production.

func TestReserveValidationIsStrict(t *testing.T) {
	valid := ReserveParams{
		ExecutionID:      "exec_0123456789abcdef0123456789abcdef",
		OrgID:            7,
		PriceVersionID:   3,
		Model:            "gpt-4o",
		InputTokensUpper: 100,
	}
	if err := ValidateReserve(NormalizeReserve(valid)); err != nil {
		t.Fatalf("a well-formed admission must validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ReserveParams)
		want   error
	}{
		{"empty execution id", func(p *ReserveParams) { p.ExecutionID = "" }, ErrInvalidArgument},
		{"padded execution id", func(p *ReserveParams) { p.ExecutionID = " exec_1 " }, ErrInvalidArgument},
		{"over-long execution id", func(p *ReserveParams) { p.ExecutionID = strings.Repeat("e", MaxExecutionIDLength+1) }, ErrInvalidArgument},
		{"missing org", func(p *ReserveParams) { p.OrgID = 0 }, ErrInvalidArgument},
		{"negative user", func(p *ReserveParams) { p.UserID = -1 }, ErrInvalidArgument},
		{"missing price version", func(p *ReserveParams) { p.PriceVersionID = 0 }, ErrInvalidArgument},
		{"empty model", func(p *ReserveParams) { p.Model = "" }, ErrInvalidArgument},
		{"over-long model", func(p *ReserveParams) { p.Model = strings.Repeat("m", MaxModelNameLength+1) }, ErrInvalidArgument},
		{"negative input bound", func(p *ReserveParams) { p.InputTokensUpper = -1 }, ErrUsageInvalid},
		{"negative output bound", func(p *ReserveParams) { p.OutputTokensUpper = -1 }, ErrUsageInvalid},
		{"negative actor", func(p *ReserveParams) { p.ActorUserID = -1 }, ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := valid
			tc.mutate(&params)
			// Deliberately not normalized here: ValidateReserve is the second
			// line of defence for a caller that skipped NormalizeReserve, so it
			// must reject padded input rather than silently accepting it.
			if err := ValidateReserve(params); !errors.Is(err, tc.want) {
				t.Fatalf("ValidateReserve=%v, want %v", err, tc.want)
			}
		})
	}

	// NormalizeReserve trims the labels and defaults Now, but never the
	// execution id: it is a cross-table identity, so rewriting it would break
	// the join with metering_events and usage_receipts.
	normalized := NormalizeReserve(ReserveParams{
		ExecutionID: "  exec_padded  ", Model: "  gpt-4o  ", KeyID: " key-1 ",
		OrgID: 1, PriceVersionID: 1, InputTokensUpper: 1,
	})
	if normalized.ExecutionID != "  exec_padded  " || normalized.Model != "gpt-4o" ||
		normalized.KeyID != "key-1" || normalized.Now.IsZero() {
		t.Fatalf("normalized=%+v", normalized)
	}
	if err := ValidateReserve(normalized); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a padded execution id must be refused, not trimmed: %v", err)
	}
}

func TestExecutionStatusVocabulary(t *testing.T) {
	// The seven statuses of migration 000009, in lifecycle order.
	want := []string{
		"reserved", "executing", "receipt_recorded", "settled",
		"failed_no_charge", "released", "review_required",
	}
	if len(ExecutionStatuses) != len(want) {
		t.Fatalf("ExecutionStatuses=%v, want %v", ExecutionStatuses, want)
	}
	for i, status := range want {
		if ExecutionStatuses[i] != status {
			t.Fatalf("ExecutionStatuses=%v, want %v", ExecutionStatuses, want)
		}
		if !IsKnownExecutionStatus(status) {
			t.Fatalf("%s must be a known status", status)
		}
	}
	if IsKnownExecutionStatus("processing") || IsKnownExecutionStatus("") {
		t.Fatal("an unknown status must not be accepted")
	}

	terminals := map[string]bool{StatusSettled: true, StatusFailedNoCharge: true, StatusReleased: true}
	for _, status := range ExecutionStatuses {
		if got := IsTerminalExecutionStatus(status); got != terminals[status] {
			t.Fatalf("IsTerminalExecutionStatus(%s)=%t, want %t", status, got, terminals[status])
		}
	}
	// The reservation is still held after receipt_recorded: T07 releases it at
	// settlement, so it must not be terminal here.
	if IsTerminalExecutionStatus(StatusReceiptRecorded) {
		t.Fatal("receipt_recorded must not be terminal: T07 still holds the reservation")
	}
}

func TestExecutionStatusVocabularyMatchesMigration000009(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000009_billing_executions.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)

	check := regexp.MustCompile(`(?s)CHECK \(status IN \((.*?)\)\)`).FindStringSubmatch(sql)
	if check == nil {
		t.Fatal("migration 000009 has no status CHECK")
	}
	var fromSQL []string
	for _, match := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(check[1], -1) {
		fromSQL = append(fromSQL, match[1])
	}
	sort.Strings(fromSQL)
	fromGo := append([]string(nil), ExecutionStatuses...)
	sort.Strings(fromGo)
	if strings.Join(fromSQL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("the database CHECK accepts %v but Go knows %v", fromSQL, fromGo)
	}

	for _, index := range []string{"idx_billing_executions_org_created", "idx_billing_executions_status_updated"} {
		if !strings.Contains(sql, index) {
			t.Fatalf("migration 000009 does not create %s", index)
		}
	}
	if !strings.Contains(sql, "CREATE TABLE billing_executions") {
		t.Fatal("migration 000009 does not create billing_executions")
	}

	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000009_billing_executions.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(down), "DROP TABLE IF EXISTS billing_executions") {
		t.Fatal("the down migration must drop billing_executions")
	}
}

func TestReservationDigestIsStableAndIdentitySensitive(t *testing.T) {
	params := ReserveParams{
		ExecutionID: "exec_1", OrgID: 3, UserID: 9, KeyID: "key-1", GatewayID: "gw-1",
		Model: "gpt-4o", PriceVersionID: 5, InputTokensUpper: 100, OutputTokensUpper: 50,
	}
	base := ReservationDigest(params)
	if len(base) != 64 {
		t.Fatalf("digest=%q, want a sha256 hex string", base)
	}
	if again := ReservationDigest(params); again != base {
		t.Fatalf("the digest is not deterministic: %q vs %q", base, again)
	}

	// Every field that changes what was pinned must change the digest; a
	// different digest under the same business id is a conflict, not a replay.
	for name, mutate := range map[string]func(*ReserveParams){
		"execution_id":       func(p *ReserveParams) { p.ExecutionID = "exec_2" },
		"org_id":             func(p *ReserveParams) { p.OrgID = 4 },
		"user_id":            func(p *ReserveParams) { p.UserID = 10 },
		"key_id":             func(p *ReserveParams) { p.KeyID = "key-2" },
		"gateway_id":         func(p *ReserveParams) { p.GatewayID = "gw-2" },
		"model":              func(p *ReserveParams) { p.Model = "gpt-4o-mini" },
		"price_version_id":   func(p *ReserveParams) { p.PriceVersionID = 6 },
		"input_tokens_upper": func(p *ReserveParams) { p.InputTokensUpper = 101 },
		"output_upper":       func(p *ReserveParams) { p.OutputTokensUpper = 51 },
	} {
		changed := params
		mutate(&changed)
		if got := ReservationDigest(changed); got == base {
			t.Fatalf("changing %s must change the digest", name)
		}
	}
	// Fields that do not change what was pinned must not change it either.
	instance := params
	instance.RequestID = "req-1"
	instance.ActorUserID = 42
	instance.Now = params.Now
	if got := ReservationDigest(instance); got != base {
		t.Fatalf("request_id/actor are not part of the pinned payload: %q vs %q", got, base)
	}

	// The release digest is a different fact and depends on the reason.
	release := ReleaseDigest(params.ExecutionID, "client cancelled")
	if release == base || len(release) != 64 {
		t.Fatalf("release digest=%q", release)
	}
	if ReleaseDigest(params.ExecutionID, "another reason") == release {
		t.Fatal("a different release reason must produce a different digest")
	}
	if ReleaseDigest("exec_2", "client cancelled") == release {
		t.Fatal("a different execution must produce a different digest")
	}
}

func TestExecutionBusinessIDIsKindScopedAndFitsTheLedger(t *testing.T) {
	id := strings.Repeat("e", MaxExecutionIDLength)
	reservation := ExecutionBusinessID(id, KindReservation)
	release := ExecutionBusinessID(id, KindRelease)
	if reservation == release {
		t.Fatal("the reservation and the release of one execution must not share a business id")
	}
	if len(reservation) > MaxBusinessIDLength || len(release) > MaxBusinessIDLength {
		t.Fatalf("business id too long for billing_entries: %d/%d bytes", len(reservation), len(release))
	}
	if !strings.HasSuffix(reservation, id) {
		t.Fatalf("business id %q must end with the execution id", reservation)
	}
}

func TestValidateExecutionTransitionRejectsUnguardedTransitions(t *testing.T) {
	valid := ExecutionTransitionParams{
		ExecutionID: "exec_1",
		From:        []string{StatusReserved, StatusExecuting},
		To:          StatusReceiptRecorded,
	}
	if err := ValidateExecutionTransition(valid); err != nil {
		t.Fatalf("a guarded receipt transition must validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ExecutionTransitionParams)
	}{
		{"no execution id", func(p *ExecutionTransitionParams) { p.ExecutionID = "" }},
		{"unknown target", func(p *ExecutionTransitionParams) { p.To = "processing" }},
		{"no guard", func(p *ExecutionTransitionParams) { p.From = nil }},
		{"unknown source", func(p *ExecutionTransitionParams) { p.From = []string{"processing"} }},
		{"back to reserved", func(p *ExecutionTransitionParams) { p.To = StatusReserved }},
		{"receipt timestamp on another transition", func(p *ExecutionTransitionParams) {
			p.To = StatusSettled
			p.ReceiptRecordedAt = time.Now().UTC()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := valid
			tc.mutate(&params)
			if err := ValidateExecutionTransition(params); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("ValidateExecutionTransition=%v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestValidateTerminalReason(t *testing.T) {
	if err := ValidateTerminalReason("upstream failed before the first event"); err != nil {
		t.Fatalf("a stated reason must validate: %v", err)
	}
	for _, reason := range []string{"", "   ", " padded ", strings.Repeat("r", MaxReasonLength+1)} {
		if err := ValidateTerminalReason(reason); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("ValidateTerminalReason(%q)=%v, want ErrInvalidArgument", reason, err)
		}
	}
}

func TestExecutionMatchesOnlyTheSameAdmission(t *testing.T) {
	params := ReserveParams{
		ExecutionID: "exec_1", OrgID: 1, UserID: 2, KeyID: "key-1", GatewayID: "gw-1",
		Model: "gpt-4o", PriceVersionID: 3, InputTokensUpper: 10, OutputTokensUpper: 20,
	}
	user, key, gateway := int64(2), "key-1", "gw-1"
	in, out := int64(10), int64(20)
	stored := Execution{
		ExecutionID: "exec_1", OrgID: 1, UserID: &user, KeyID: &key, GatewayID: &gateway,
		Model: "gpt-4o", PriceVersionID: 3, InputTokensUpper: &in, OutputTokensUpper: &out,
	}
	if !executionMatches(stored, params) {
		t.Fatal("the same admission payload must match")
	}
	for name, mutate := range map[string]func(*ReserveParams){
		"org":         func(p *ReserveParams) { p.OrgID = 2 },
		"user":        func(p *ReserveParams) { p.UserID = 3 },
		"key":         func(p *ReserveParams) { p.KeyID = "key-2" },
		"gateway":     func(p *ReserveParams) { p.GatewayID = "gw-2" },
		"model":       func(p *ReserveParams) { p.Model = "gpt-4o-mini" },
		"price":       func(p *ReserveParams) { p.PriceVersionID = 4 },
		"input bound": func(p *ReserveParams) { p.InputTokensUpper = 11 },
		"output":      func(p *ReserveParams) { p.OutputTokensUpper = 21 },
	} {
		changed := params
		mutate(&changed)
		if executionMatches(stored, changed) {
			t.Fatalf("changing %s must not look like the same admission", name)
		}
	}
	// A NULL user column means "no owner", not user 0 or any other user.
	anonymous := stored
	anonymous.UserID = nil
	if executionMatches(anonymous, params) {
		t.Fatal("a stored NULL user must not match an admission with a user")
	}
	noUser := params
	noUser.UserID = 0
	if !executionMatches(anonymous, noUser) {
		t.Fatal("a stored NULL user must match an admission without a user")
	}
}
