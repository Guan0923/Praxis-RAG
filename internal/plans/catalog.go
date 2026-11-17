// Package plans owns the first-release personal membership catalog: plan
// limits, CNY prices in integer cents, and the Asia/Shanghai calendar rules
// shared by billing, entitlement and quota accounting. It is deliberately free
// of database and HTTP dependencies so state, runtime and the Console can use
// the same rules.
package plans

// Plan is one personal membership tier.
type Plan string

const (
	// PlanFree is the default tier for every account without a paid term.
	PlanFree Plan = "free"
	// PlanPlus is the entry paid tier.
	PlanPlus Plan = "plus"
	// PlanPro is the highest personal tier.
	PlanPro Plan = "pro"
)

// BillingPeriod is one purchasable card length.
type BillingPeriod string

const (
	// Monthly is one calendar month.
	Monthly BillingPeriod = "month"
	// Yearly is twelve calendar months.
	Yearly BillingPeriod = "year"
)

// Product is one row of the first-release catalog. All money values are CNY
// cents so no float ever reaches an order or a payment gateway.
type Product struct {
	Plan Plan
	Name string
	// MonthlyCents and YearlyCents are the list prices. The yearly price is the
	// server-generated amount; the Console never submits a price.
	MonthlyCents int64
	YearlyCents  int64
	// MaxOwnedKnowledgeBases and MaxFilesPerKB use -1 for unlimited.
	MaxOwnedKnowledgeBases int
	MaxFilesPerKB          int
	// EmbeddingCharsPerMonth is one membership month of platform-provided
	// document Embedding input characters. The yearly card grants it twelve
	// times, one grant per membership month.
	EmbeddingCharsPerMonth int64
	// CanShareOwnedKnowledgeBases reports whether the tier may issue new share
	// invitations for its own knowledge bases.
	CanShareOwnedKnowledgeBases bool
}

// Catalog is the ordered first-release product list.
func Catalog() []Product {
	return []Product{
		{
			Plan: PlanFree, Name: "Free",
			MonthlyCents: 0, YearlyCents: 0,
			MaxOwnedKnowledgeBases: 5, MaxFilesPerKB: 10,
			EmbeddingCharsPerMonth:      100_000,
			CanShareOwnedKnowledgeBases: false,
		},
		{
			Plan: PlanPlus, Name: "Plus",
			MonthlyCents: 1_990, YearlyCents: 19_104,
			MaxOwnedKnowledgeBases: 20, MaxFilesPerKB: 100,
			EmbeddingCharsPerMonth:      10_000_000,
			CanShareOwnedKnowledgeBases: true,
		},
		{
			Plan: PlanPro, Name: "Pro",
			MonthlyCents: 19_990, YearlyCents: 57_504,
			MaxOwnedKnowledgeBases: -1, MaxFilesPerKB: -1,
			EmbeddingCharsPerMonth:      1_000_000_000,
			CanShareOwnedKnowledgeBases: true,
		},
	}
}

// ProductFor resolves one stored plan key. Unknown keys fall back to Free so a
// stale database row can never grant paid limits by accident.
func ProductFor(plan Plan) Product {
	for _, product := range Catalog() {
		if product.Plan == plan {
			return product
		}
	}
	for _, product := range Catalog() {
		if product.Plan == PlanFree {
			return product
		}
	}
	return Product{Plan: PlanFree, Name: "Free"}
}

// NormalizePlan validates a request/catalog plan key.
func NormalizePlan(value string) (Plan, bool) {
	plan := Plan(value)
	for _, product := range Catalog() {
		if product.Plan == plan {
			return plan, true
		}
	}
	return "", false
}

// NormalizePeriod validates a request period key.
func NormalizePeriod(value string) (BillingPeriod, bool) {
	period := BillingPeriod(value)
	if period == Monthly || period == Yearly {
		return period, true
	}
	return "", false
}

// PeriodMonths returns the number of membership months in one card.
func PeriodMonths(period BillingPeriod) int {
	if period == Yearly {
		return 12
	}
	return 1
}

// Price returns the server-generated list price for one plan/period pair.
func Price(plan Plan, period BillingPeriod) (int64, bool) {
	product := ProductFor(plan)
	switch period {
	case Monthly:
		return product.MonthlyCents, product.Plan == plan
	case Yearly:
		return product.YearlyCents, product.Plan == plan
	default:
		return 0, false
	}
}

// PlanRank orders tiers for current-entitlement resolution: a higher rank
// overrides a lower overlapping term (for example a Pro upgrade over a Plus
// term without destroying the Plus purchase record).
func PlanRank(plan Plan) int {
	switch plan {
	case PlanPro:
		return 3
	case PlanPlus:
		return 2
	default:
		return 1
	}
}

// FreeLimits returns the limits used when no paid term exists.
func FreeLimits() Product { return ProductFor(PlanFree) }
