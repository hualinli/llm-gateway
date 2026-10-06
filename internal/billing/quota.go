package billing

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

const beijingMonthLayout = "2006-01"

var beijingLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// The zone database is embedded below by the package's tzdata import in
		// quota_pg.go. This fallback is safe for the current Beijing calendar.
		return time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	return location
}()

// BillingMonth is a canonical Beijing calendar month. Start and End are real
// instants; StartDate is the SQL DATE key persisted on admissions/counters.
type BillingMonth struct {
	Label     string
	StartDate string
	Start     time.Time
	End       time.Time
}

// ParseBillingMonth parses YYYY-MM in Asia/Shanghai. An empty month means the
// month containing now. The same resolved month is used by usage, member
// summaries and quota reads.
func ParseBillingMonth(raw string, now time.Time) (BillingMonth, error) {
	if now.IsZero() {
		now = time.Now()
	}
	label := strings.TrimSpace(raw)
	if label == "" {
		label = now.In(beijingLocation).Format(beijingMonthLayout)
	}
	start, err := time.ParseInLocation(beijingMonthLayout, label, beijingLocation)
	if err != nil || start.Format(beijingMonthLayout) != label {
		return BillingMonth{}, fmt.Errorf("%w: month must be YYYY-MM in Asia/Shanghai", ErrInvalidArgument)
	}
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, beijingLocation)
	return BillingMonth{
		Label:     label,
		StartDate: start.Format("2006-01-02"),
		Start:     start,
		End:       start.AddDate(0, 1, 0),
	}, nil
}

// BillingMonthAt returns the Beijing month containing an instant.
func BillingMonthAt(at time.Time) BillingMonth {
	month, _ := ParseBillingMonth(at.In(beijingLocation).Format(beijingMonthLayout), at)
	return month
}

// MemberQuota is the current account user's monthly quota projection. Used is
// final billed spend; Reserved is admitted upper-bound spend that has not been
// settled or released. Limit and Remaining are nil for an unlimited member.
type MemberQuota struct {
	OrgID          int64
	UserID         int64
	MembershipID   int64
	Month          BillingMonth
	LimitMicro     *int64
	UsedMicro      int64
	ReservedMicro  int64
	RemainingMicro *int64
}

func quotaRemaining(limit *int64, used, reserved int64) (*int64, error) {
	if limit == nil {
		return nil, nil
	}
	committed, err := addMicro(used, reserved)
	if err != nil {
		return nil, err
	}
	remaining := int64(0)
	if committed < *limit {
		remaining = *limit - committed
	}
	return &remaining, nil
}

func quotaMonthDate(now time.Time) string {
	return BillingMonthAt(now).StartDate
}

func quotaCapAllows(limit *int64, used, reserved, requested int64) error {
	if limit == nil {
		return nil
	}
	committed, err := addMicro(used, reserved)
	if err != nil {
		return err
	}
	projected, err := addMicro(committed, requested)
	if err != nil {
		return err
	}
	if projected > *limit {
		return fmt.Errorf("%w: limit=%s used=%s reserved=%s requested=%s",
			ErrMemberMonthlyLimit, FormatMicro(*limit), FormatMicro(used),
			FormatMicro(reserved), FormatMicro(requested))
	}
	return nil
}
