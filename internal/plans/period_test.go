package plans

import (
	"testing"
	"time"
)

func shanghaiDate(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, Shanghai())
}

func TestCatalogPricesAndLimits(t *testing.T) {
	tests := []struct {
		plan     Plan
		monthly  int64
		yearly   int64
		kbs      int
		files    int
		chars    int64
		canShare bool
	}{
		{PlanFree, 0, 0, 5, 10, 100_000, false},
		{PlanPlus, 1_990, 19_104, 20, 100, 10_000_000, true},
		{PlanPro, 19_990, 57_504, -1, -1, 1_000_000_000, true},
	}
	for _, test := range tests {
		product := ProductFor(test.plan)
		if product.MonthlyCents != test.monthly || product.YearlyCents != test.yearly {
			t.Errorf("%s price = %d/%d, want %d/%d", test.plan,
				product.MonthlyCents, product.YearlyCents, test.monthly, test.yearly)
		}
		if product.MaxOwnedKnowledgeBases != test.kbs || product.MaxFilesPerKB != test.files {
			t.Errorf("%s limits = %d/%d, want %d/%d", test.plan,
				product.MaxOwnedKnowledgeBases, product.MaxFilesPerKB, test.kbs, test.files)
		}
		if product.EmbeddingCharsPerMonth != test.chars {
			t.Errorf("%s chars = %d, want %d", test.plan, product.EmbeddingCharsPerMonth, test.chars)
		}
		if product.CanShareOwnedKnowledgeBases != test.canShare {
			t.Errorf("%s can_share = %v, want %v", test.plan,
				product.CanShareOwnedKnowledgeBases, test.canShare)
		}
	}
	if got, ok := Price(PlanPlus, Yearly); !ok || got != PlanPlusYearlyCents {
		t.Fatalf("Plus yearly = %d, %v", got, ok)
	}
	if _, ok := Price("enterprise", Monthly); ok {
		t.Fatal("unknown plan must not be priced")
	}
}

// PlanPlusYearlyCents keeps the documented 19.90*12*0.8 = 191.04 amount visible
// in tests without a float.
const PlanPlusYearlyCents = 19_104

func TestAddMonthsKeepsAnchorAcrossShortMonths(t *testing.T) {
	anchor := shanghaiDate(2026, time.January, 31, 10, 30).UTC()
	want := []time.Time{
		shanghaiDate(2026, time.January, 31, 10, 30).UTC(),
		shanghaiDate(2026, time.February, 28, 10, 30).UTC(),
		shanghaiDate(2026, time.March, 31, 10, 30).UTC(),
		shanghaiDate(2026, time.April, 30, 10, 30).UTC(),
		shanghaiDate(2026, time.May, 31, 10, 30).UTC(),
	}
	for index, target := range want {
		if got := AddMonths(anchor, index); !got.Equal(target) {
			t.Errorf("AddMonths(%d) = %s, want %s", index, got, target)
		}
	}
	if got := TermEnd(anchor, Yearly); !got.Equal(shanghaiDate(2027, time.January, 31, 10, 30).UTC()) {
		t.Fatalf("yearly end = %s", got)
	}
}

func TestPeriodBoundsAndIndex(t *testing.T) {
	anchor := shanghaiDate(2026, time.January, 31, 8, 0).UTC()
	start, end := PeriodBounds(anchor, 1)
	if !start.Equal(shanghaiDate(2026, time.February, 28, 8, 0).UTC()) ||
		!end.Equal(shanghaiDate(2026, time.March, 31, 8, 0).UTC()) {
		t.Fatalf("period 1 = %s..%s", start, end)
	}
	for _, test := range []struct {
		at   time.Time
		want int
	}{
		{anchor.Add(-time.Second), -1},
		{anchor, 0},
		{start, 1},
		{shanghaiDate(2026, time.March, 31, 8, 0).UTC(), 2},
	} {
		if got := CurrentPeriodIndex(anchor, test.at); got != test.want {
			t.Errorf("index at %s = %d, want %d", test.at, got, test.want)
		}
	}
}

func TestYearCardGrantsTwelveDistinctMembershipMonths(t *testing.T) {
	anchor := shanghaiDate(2026, time.January, 31, 10, 0).UTC()
	end := TermEnd(anchor, Yearly)
	previousEnd := anchor
	for index := 0; index < 12; index++ {
		start, periodEnd := PeriodBounds(anchor, index)
		if !start.Equal(previousEnd) {
			t.Fatalf("period %d starts at %s, want %s", index, start, previousEnd)
		}
		if !periodEnd.After(start) {
			t.Fatalf("period %d is empty: %s..%s", index, start, periodEnd)
		}
		previousEnd = periodEnd
	}
	if !previousEnd.Equal(end) {
		t.Fatalf("12th period ends at %s, want term end %s", previousEnd, end)
	}
	// The month-end clamp recovers on the next period after February.
	if got := AddMonths(anchor, 2); !got.Equal(shanghaiDate(2026, time.March, 31, 10, 0).UTC()) {
		t.Fatalf("March anchor = %s", got)
	}
}

func TestFreeMonthBoundsUsesShanghai(t *testing.T) {
	// 2026-01-31 23:30 UTC is 2026-02-01 07:30 in Shanghai.
	now := time.Date(2026, time.January, 31, 23, 30, 0, 0, time.UTC)
	start, end := FreeMonthBounds(now)
	wantStart := shanghaiDate(2026, time.February, 1, 0, 0).UTC()
	wantEnd := shanghaiDate(2026, time.March, 1, 0, 0).UTC()
	if !start.Equal(wantStart) || !end.Equal(wantEnd) {
		t.Fatalf("free bounds = %s..%s, want %s..%s", start, end, wantStart, wantEnd)
	}
}

func TestQuoteUpgradeProratesEachSegment(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	segments := []UpgradeSegment{
		{
			Plan: PlanPlus, Period: Monthly,
			Start: now.Add(-15 * 24 * time.Hour), End: now.Add(15 * 24 * time.Hour),
		},
		{
			Plan: PlanPlus, Period: Yearly,
			Start: now.Add(15 * 24 * time.Hour), End: now.Add(15*24*time.Hour + 365*24*time.Hour),
		},
	}
	quote := QuoteUpgrade(PlanPlus, PlanPro, segments, now)
	// Monthly difference is 19990-1990=18000, half remaining = 9000.
	// Yearly difference is 57504-19104=38400, full remaining ~= 38400.
	if quote.AmountCents < 47_000 || quote.AmountCents > 47_500 {
		t.Fatalf("upgrade quote = %d, want about 47400", quote.AmountCents)
	}
	if len(quote.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(quote.Segments))
	}
	if quote.Segments[0].AmountCents != 9_000 {
		t.Errorf("monthly segment = %d, want 9000", quote.Segments[0].AmountCents)
	}
}
