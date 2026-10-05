package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
	"time"
)

// MaxBusinessIDLength bounds the business/idempotency key, mirroring the
// database CHECK in migration 000008.
const MaxBusinessIDLength = 200

// MaxReasonLength bounds the human reason stored with an adjustment.
const MaxReasonLength = 512

// AdjustParams is one idempotent balance adjustment.
//
// BusinessID and IdempotencyKey are the same string in T05: the platform
// adjustment endpoint derives business_id from the Idempotency-Key header, and
// (business_id, kind) is the uniqueness rule that makes a retry a replay
// instead of a second posting. They stay separate fields because T06's
// settlement passes an execution/receipt business id that is not a client
// header. When both are set they must be equal: ValidateAdjustment rejects the
// ambiguous case rather than silently picking one.
type AdjustParams struct {
	ActorUserID    int64
	OrgID          int64
	AmountMicro    int64
	Direction      string
	Kind           string
	Reason         string
	BusinessID     string
	IdempotencyKey string
	PayloadDigest  string
	RequestID      string
	// Now is posted_at/created_at. Zero means time.Now().UTC().
	Now time.Time
}

// AdjustmentResult is what a repeated call returns: the entry that is already
// stored, the account as it is now, and Replayed=true. Replayed is the only
// signal that distinguishes "applied now" from "already applied"; a caller must
// never infer it from a nil error.
type AdjustmentResult struct {
	Entry    Entry
	Account  Account
	Replayed bool
}

// ValidateAdjustment is the pure validation shared by the HTTP layer and the
// store, so a bad posting is refused before a transaction is opened.
func ValidateAdjustment(params AdjustParams) error {
	if params.OrgID <= 0 {
		return fmt.Errorf("%w: org_id is required", ErrInvalidArgument)
	}
	if params.AmountMicro <= 0 {
		return fmt.Errorf("%w: amount_micro must be a positive integer micro amount", ErrAmountInvalid)
	}
	if params.Direction != DirectionCredit && params.Direction != DirectionDebit {
		return fmt.Errorf("%w: direction must be %q or %q", ErrInvalidArgument, DirectionCredit, DirectionDebit)
	}
	switch params.Kind {
	case KindReservation:
		// One fact, one encoding: a reservation is a credit to the reserved
		// bucket. Accepting direction=debit here would create a second spelling
		// of the same posting and make the ledger impossible to review.
		if params.Direction != DirectionCredit {
			return fmt.Errorf("%w: a %s entry must have direction=%s", ErrInvalidArgument, KindReservation, DirectionCredit)
		}
	case KindRelease:
		if params.Direction != DirectionDebit {
			return fmt.Errorf("%w: a %s entry must have direction=%s", ErrInvalidArgument, KindRelease, DirectionDebit)
		}
	case KindCredit, KindDebit, KindAdjustment:
	default:
		return fmt.Errorf("%w: unknown entry kind %q", ErrInvalidArgument, params.Kind)
	}
	if strings.TrimSpace(params.Reason) != params.Reason {
		return fmt.Errorf("%w: reason must not have surrounding whitespace", ErrInvalidArgument)
	}
	if len(params.Reason) > MaxReasonLength {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrInvalidArgument, MaxReasonLength)
	}
	businessID := params.BusinessID
	if businessID == "" {
		businessID = params.IdempotencyKey
	}
	if params.IdempotencyKey != "" && params.BusinessID != "" && params.BusinessID != params.IdempotencyKey {
		return fmt.Errorf("%w: business_id and idempotency_key must be the same string when both are given", ErrInvalidArgument)
	}
	if businessID != strings.TrimSpace(businessID) || businessID == "" || len(businessID) > MaxBusinessIDLength {
		return fmt.Errorf("%w: business_id must be 1..%d non-padded bytes", ErrInvalidArgument, MaxBusinessIDLength)
	}
	if len(params.PayloadDigest) != 64 {
		return fmt.Errorf("%w: payload digest must be a sha256 hex string", ErrInvalidArgument)
	}
	return nil
}

// NormalizeAdjustment applies the documented defaults (kind, business id,
// timestamps) and trims the reason, so the digest of a request is stable no
// matter how the caller spaced its JSON.
func NormalizeAdjustment(params AdjustParams) AdjustParams {
	if params.Kind == "" {
		params.Kind = KindAdjustment
	}
	if params.BusinessID == "" {
		params.BusinessID = params.IdempotencyKey
	}
	if params.IdempotencyKey == "" {
		params.IdempotencyKey = params.BusinessID
	}
	params.Reason = strings.TrimSpace(params.Reason)
	if params.Now.IsZero() {
		params.Now = time.Now().UTC()
	}
	params.Now = params.Now.UTC()
	return params
}

// AdjustmentDigest is the canonical payload digest of one adjustment. Two
// requests that mean the same posting produce the same digest; a request that
// changes the amount, the direction, the entry kind, the organization or the
// reason produces a different one and is therefore a conflict rather than a
// silent replay. Fields are length-prefixed so no value can be confused with a
// separator.
func AdjustmentDigest(orgID, amountMicro int64, direction, kind, reason string) string {
	digest := sha256.New()
	writeDigestField(digest, "kind", "billing_adjustment")
	writeDigestField(digest, "org_id", FormatMicro(orgID))
	writeDigestField(digest, "amount_micro", FormatMicro(amountMicro))
	writeDigestField(digest, "direction", direction)
	writeDigestField(digest, "entry_kind", kind)
	writeDigestField(digest, "reason", reason)
	return hex.EncodeToString(digest.Sum(nil))
}

func writeDigestField(digest hash.Hash, name, value string) {
	fmt.Fprintf(digest, "%d:%s=%d:%s;", len(name), name, len(value), value)
}

// addMicro adds two int64 micro amounts and returns ErrAmountOverflow instead
// of wrapping. A projection that silently changes sign is worse than an error.
func addMicro(left, right int64) (int64, error) {
	sum := left + right
	if (right > 0 && sum < left) || (right < 0 && sum > left) {
		return 0, fmt.Errorf("%w: %s + %s", ErrAmountOverflow, FormatMicro(left), FormatMicro(right))
	}
	return sum, nil
}
