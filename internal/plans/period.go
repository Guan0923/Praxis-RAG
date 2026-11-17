package plans

import (
	"math"
	"sync"
	"time"
)

// TimestampLayout is the storage layout for membership/order timestamps. All
// timestamps are stored in UTC and rendered in Asia/Shanghai only at the edge.
const TimestampLayout = time.RFC3339

var (
	shanghaiOnce sync.Once
	shanghai     *time.Location
)

// Shanghai returns the product timezone. Go on Windows may not find the IANA
// database in every build; the fixed UTC+8 fallback is exact because Shanghai
// has no daylight-saving transitions.
func Shanghai() *time.Location {
	shanghaiOnce.Do(func() {
		location, err := time.LoadLocation("Asia/Shanghai")
		if err != nil || location == nil {
			location = time.FixedZone("Asia/Shanghai", 8*60*60)
		}
		shanghai = location
	})
	return shanghai
}

// FormatTimestamp renders one instant for MySQL.
func FormatTimestamp(value time.Time) string {
	return value.UTC().Format(TimestampLayout)
}

// ParseTimestamp parses a stored timestamp. The zero value is returned for an
// empty or malformed value so a corrupt row degrades to "not yet valid" instead
// of panicking a request.
func ParseTimestamp(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// AddMonths anchors month arithmetic to the original local day-of-month. A
// target month without that day clamps to its last day (Jan 31 -> Feb 28), and
// the following call from the original anchor restores it (Mar 31).
func AddMonths(anchor time.Time, months int) time.Time {
	local := anchor.In(Shanghai())
	year, month, day := local.Date()
	hour, minute, second := local.Clock()
	targetMonth := time.Month(int(month) + months)
	targetYear := year + (int(targetMonth)-1)/12
	targetMonth = time.Month((int(targetMonth)-1)%12 + 1)
	if targetMonth < time.January {
		targetMonth += 12
		targetYear--
	}
	lastDay := time.Date(targetYear, targetMonth+1, 0, 12, 0, 0, 0, Shanghai()).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(targetYear, targetMonth, day, hour, minute, second, local.Nanosecond(), Shanghai()).UTC()
}

// TermEnd returns the exclusive end of one card bought at start.
func TermEnd(start time.Time, period BillingPeriod) time.Time {
	return AddMonths(start, PeriodMonths(period))
}

// PeriodBounds returns the left-closed/right-open membership month number index
// of one paid term. index 0 is the first month; the yearly card has twelve.
func PeriodBounds(termStart time.Time, index int) (time.Time, time.Time) {
	return AddMonths(termStart, index), AddMonths(termStart, index+1)
}

// CurrentPeriodIndex returns the membership month containing now for a term
// anchored at termStart. It returns -1 before the term starts and the number of
// elapsed membership months after the term ends (callers should only use a
// result whose bounds still contain now).
func CurrentPeriodIndex(termStart, now time.Time) int {
	if now.Before(termStart) {
		return -1
	}
	// Twelve months per year; walk rather than divide so the Jan-31 month-end
	// clamp participates in the comparison.
	for index := 0; index < 240; index++ {
		start, end := PeriodBounds(termStart, index)
		if !now.Before(start) && now.Before(end) {
			return index
		}
		if !now.Before(end) {
			continue
		}
		return -1
	}
	return -1
}

// FreeMonthBounds returns the current Shanghai natural month as a UTC
// left-closed/right-open interval.
func FreeMonthBounds(now time.Time) (time.Time, time.Time) {
	local := now.In(Shanghai())
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, Shanghai())
	return start.UTC(), AddMonths(start.UTC(), 1)
}

// UpgradeSegment is one paid term still in force at quote time. Period is the
// term's original card length and is used to pick the two list prices.
type UpgradeSegment struct {
	Plan        Plan
	Period      BillingPeriod
	Start       time.Time
	End         time.Time
	AmountCents int64
}

// UpgradeQuote is the server-side price of moving from current to target for
// every remaining segment. The amount is rounded to cents once, after summing
// all segment fractions.
type UpgradeQuote struct {
	CurrentPlan Plan
	TargetPlan  Plan
	AmountCents int64
	Segments    []UpgradeSegment
}

// QuoteUpgrade calculates the Plus->Pro price difference for every term that
// has not ended at now. The caller validates the entitlement version and that
// the target outranks the current plan.
func QuoteUpgrade(current, target Plan, segments []UpgradeSegment, now time.Time) UpgradeQuote {
	quote := UpgradeQuote{CurrentPlan: current, TargetPlan: target, Segments: make([]UpgradeSegment, 0, len(segments))}
	var total float64
	for _, segment := range segments {
		if !segment.End.After(now) || segment.Plan == target {
			continue
		}
		start := segment.Start
		if start.Before(now) {
			start = now
		}
		if !segment.End.After(start) {
			continue
		}
		fromPrice, fromOK := Price(segment.Plan, segment.Period)
		toPrice, toOK := Price(target, segment.Period)
		if !fromOK || !toOK || toPrice <= fromPrice {
			continue
		}
		totalDuration := segment.End.Sub(segment.Start).Seconds()
		remainingDuration := segment.End.Sub(start).Seconds()
		if totalDuration <= 0 {
			continue
		}
		fraction := remainingDuration / totalDuration
		if fraction < 0 {
			fraction = 0
		}
		if fraction > 1 {
			fraction = 1
		}
		delta := float64(toPrice-fromPrice) * fraction
		total += delta
		detail := segment
		detail.AmountCents = int64(math.Round(delta))
		quote.Segments = append(quote.Segments, detail)
	}
	quote.AmountCents = int64(math.Round(total))
	return quote
}
