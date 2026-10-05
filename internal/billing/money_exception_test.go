package billing

import (
	"errors"
	"testing"
	"time"
)

func TestValidateRecordExceptionRejectsMalformedObservations(t *testing.T) {
	valid := RecordExceptionParams{
		Kind:      ExceptionKindUsageMissing,
		OrgID:     7,
		Reason:    "the stream ended without usage",
		DedupeKey: ExceptionDedupeKey("receipts", ExceptionKindUsageMissing, "exec_1"),
	}
	if err := ValidateRecordException(valid); err != nil {
		t.Fatalf("a well-formed exception was refused: %v", err)
	}
	// Organization 0 is "unknown attribution" (a legacy event), not an error: the
	// row is stored with a NULL organization and never appears in an
	// organization-scoped read.
	unknownOrg := valid
	unknownOrg.OrgID = 0
	if err := ValidateRecordException(unknownOrg); err != nil {
		t.Fatalf("an exception with unknown attribution was refused: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*RecordExceptionParams)
	}{
		{"unknown kind", func(p *RecordExceptionParams) { p.Kind = "made_up" }},
		{"negative organization", func(p *RecordExceptionParams) { p.OrgID = -1 }},
		{"negative amount", func(p *RecordExceptionParams) { p.AmountMicro = -1 }},
		{"empty reason", func(p *RecordExceptionParams) { p.Reason = "  " }},
		{"padded reason", func(p *RecordExceptionParams) { p.Reason = " why " }},
		{"empty dedupe key", func(p *RecordExceptionParams) { p.DedupeKey = "" }},
		{"padded execution id", func(p *RecordExceptionParams) { p.ExecutionID = " exec_1 " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := valid
			tc.mutate(&params)
			if err := ValidateRecordException(params); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("validation = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestNormalizeRecordExceptionAppliesTimestamps(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	params := NormalizeRecordException(RecordExceptionParams{
		Kind:      ExceptionKindOverReserved,
		OrgID:     1,
		Reason:    "charge above reservation",
		DedupeKey: "k",
		Now:       now,
	})
	if !params.DetectedAt.Equal(now) || !params.Now.Equal(now) {
		t.Fatalf("timestamps were not defaulted: %+v", params)
	}
	if params.DetectedAt.Location() != time.UTC {
		t.Fatalf("detected_at is not UTC: %v", params.DetectedAt.Location())
	}
}

// TestValidateResolveExceptionKeepsStatusAndResolutionConsistent is the "no
// silent reclassification" rule in its pure form: a decision and the state it
// produces must tell the same story, and every decision carries a note.
func TestValidateResolveExceptionKeepsStatusAndResolutionConsistent(t *testing.T) {
	base := ResolveExceptionParams{
		ExceptionID: 1,
		ActorUserID: 2,
		Status:      ExceptionStatusResolved,
		Resolution:  ExceptionResolutionWaived,
		Note:        "platform absorbs the upstream cost",
	}
	if err := ValidateResolveException(base); err != nil {
		t.Fatalf("a well-formed decision was refused: %v", err)
	}
	if err := ValidateResolveException(ResolveExceptionParams{
		ExceptionID: 1, ActorUserID: 2,
		Status: ExceptionStatusDismissed, Resolution: ExceptionResolutionDismissed, Note: "not a money problem",
	}); err != nil {
		t.Fatalf("a dismissal was refused: %v", err)
	}
	if err := ValidateResolveException(ResolveExceptionParams{
		ExceptionID: 1, ActorUserID: 2,
		Status: ExceptionStatusOpen, Resolution: ExceptionResolutionReopened, Note: "came back after the fix",
	}); err != nil {
		t.Fatalf("a reopen was refused: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ResolveExceptionParams)
	}{
		{"no exception", func(p *ResolveExceptionParams) { p.ExceptionID = 0 }},
		{"no actor", func(p *ResolveExceptionParams) { p.ActorUserID = 0 }},
		{"no note", func(p *ResolveExceptionParams) { p.Note = " " }},
		{"padded note", func(p *ResolveExceptionParams) { p.Note = " note " }},
		{"unknown resolution", func(p *ResolveExceptionParams) { p.Resolution = "guessed" }},
		{"waived without resolving", func(p *ResolveExceptionParams) { p.Status = ExceptionStatusOpen }},
		{"dismissed resolution targeting resolved", func(p *ResolveExceptionParams) { p.Resolution = ExceptionResolutionDismissed }},
		{"reopened as resolved", func(p *ResolveExceptionParams) { p.Resolution = ExceptionResolutionReopened }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := base
			tc.mutate(&params)
			if err := ValidateResolveException(params); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("validation = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestExceptionDedupeKeyIsStableAndScoped(t *testing.T) {
	first := ExceptionDedupeKey("accounts", DiffAccountProjectionMismatchForTest, "org=7")
	second := ExceptionDedupeKey("accounts", DiffAccountProjectionMismatchForTest, "org=7")
	if first != second {
		t.Fatalf("the same observation produced two keys: %q %q", first, second)
	}
	for _, other := range []string{
		ExceptionDedupeKey("entries", DiffAccountProjectionMismatchForTest, "org=7"),
		ExceptionDedupeKey("accounts", "other_kind", "org=7"),
		ExceptionDedupeKey("accounts", DiffAccountProjectionMismatchForTest, "org=8"),
	} {
		if other == first {
			t.Fatalf("distinct observations share the dedupe key %q", first)
		}
	}
}

// DiffAccountProjectionMismatchForTest mirrors the metering difference kind
// without importing internal/metering (which imports this package).
const DiffAccountProjectionMismatchForTest = "account_projection_mismatch"

func TestExceptionVocabulariesAreClosed(t *testing.T) {
	for _, kind := range ExceptionKinds {
		if !IsKnownExceptionKind(kind) {
			t.Fatalf("kind %q is not recognized by its own vocabulary", kind)
		}
	}
	if IsKnownExceptionKind("") || IsKnownExceptionKind("nope") {
		t.Fatal("an unknown kind was accepted")
	}
	for _, status := range ExceptionStatuses {
		if !IsKnownExceptionStatus(status) {
			t.Fatalf("status %q is not recognized by its own vocabulary", status)
		}
	}
	for _, resolution := range ExceptionResolutions {
		if !IsKnownExceptionResolution(resolution) {
			t.Fatalf("resolution %q is not recognized by its own vocabulary", resolution)
		}
	}
}
