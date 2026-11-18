package membership

import (
	"errors"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/payment"
	"github.com/Guan0923/Praxis-RAG/internal/plans"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

func TestPlanViewsOnlyPlusShowsYearDiscount(t *testing.T) {
	views := planViews()
	if len(views) != 3 {
		t.Fatalf("plans = %d, want 3", len(views))
	}
	byPlan := map[string]PlanView{}
	for _, view := range views {
		byPlan[view.Plan] = view
	}
	plus := byPlan["plus"]
	if plus.MonthlyYuan != "19.90" || plus.YearlyYuan != "191.04" || !plus.YearDiscount {
		t.Fatalf("Plus price/discount = %s/%s/%v", plus.MonthlyYuan, plus.YearlyYuan, plus.YearDiscount)
	}
	pro := byPlan["pro"]
	if pro.MonthlyYuan != "199.90" || pro.YearlyYuan != "575.04" || pro.YearDiscount {
		t.Fatalf("Pro price/discount = %s/%s/%v", pro.MonthlyYuan, pro.YearlyYuan, pro.YearDiscount)
	}
	if pro.Limits.KnowledgeBases != -1 || !pro.Limits.UnlimitedKnowledgeBases || !pro.Limits.UnlimitedFiles {
		t.Fatalf("Pro unlimited flags = %+v", pro.Limits)
	}
	if plus.Limits.EmbeddingCharsPerMonth != 10_000_000 {
		t.Fatalf("Plus chars = %d", plus.Limits.EmbeddingCharsPerMonth)
	}
	if byPlan["free"].YearDiscount {
		t.Fatal("Free must not advertise a year discount")
	}
}

func TestOrderViewKeepsServerLabelsAndAmounts(t *testing.T) {
	service := &Service{}
	order := state.BillingOrder{
		ID: "PR1", Status: "paid", Plan: "plus", BillingPeriod: "year", Kind: "renew",
		AmountCents: 19_104, Currency: "CNY", CreatedAt: "2026-01-31T00:00:00Z",
		ExpiresAt: "2026-12-31T00:00:00Z", PaidAt: "2026-01-31T00:00:00Z",
		AlipayTradeNo: "2026013122001", RefundAmountCents: 0,
		Quote: `{"starts_at":"2026-01-31T00:00:00Z","ends_at":"2027-01-31T00:00:00Z",` +
			`"current_plan":"plus","target_plan":"plus"}`,
	}
	view := service.orderView(order, "alice")
	if view.StatusLabel != "已支付" || view.PeriodLabel != "年卡" || view.KindLabel != "续费" {
		t.Fatalf("labels = %s/%s/%s", view.StatusLabel, view.PeriodLabel, view.KindLabel)
	}
	if view.AmountYuan != "191.04" || view.AmountCents != 19_104 {
		t.Fatalf("amount = %s/%d", view.AmountYuan, view.AmountCents)
	}
	if view.AlipayTradeNo == nil || *view.AlipayTradeNo != "2026013122001" {
		t.Fatalf("trade = %v", view.AlipayTradeNo)
	}
	if view.Username != "alice" || view.Quote.EndsAt != "2027-01-31T00:00:00Z" {
		t.Fatalf("username/quote = %q/%q", view.Username, view.Quote.EndsAt)
	}
}

func TestPurchaseStateDistinguishesUnconfiguredAndPaused(t *testing.T) {
	unconfigured := &Service{settings: payment.Settings{}}
	if state := unconfigured.purchaseState(); state.Available || state.Reason == nil {
		t.Fatalf("unconfigured purchase = %+v", state)
	}
	configuredPaused := &Service{settings: payment.Settings{
		AppID: "app", PrivateKeyPEM: "x", PublicKeyPEM: "y", NotifyURL: "https://n",
		ReturnURL: "https://r", SellerID: "2088", PurchaseEnabled: false,
	}}
	if state := configuredPaused.purchaseState(); state.Available || !state.Paused {
		t.Fatalf("paused purchase = %+v", state)
	}
}

func TestMapBillingErrorPreservesSafeCategories(t *testing.T) {
	tests := []struct {
		err  error
		code string
	}{
		{&state.PayableOrderExistsError{OrderID: "PR1", Message: "已有待支付订单，请先处理订单 PR1"}, CodeConflict},
		{&state.BillingConflictError{Message: "订单状态不是待支付，不能发放会员权益"}, CodeConflict},
		{&state.EntitlementChangedError{Message: "会员权益已变更"}, CodeConflict},
		{&state.LookupError{Message: "订单不存在"}, CodeNotFound},
		{errors.New("database down"), CodeUnavailable},
	}
	for _, test := range tests {
		var typed *Error
		if !errors.As(mapBillingError(test.err), &typed) {
			t.Fatalf("%v did not map to membership error", test.err)
		}
		if typed.Code != test.code {
			t.Errorf("%v mapped to %s, want %s", test.err, typed.Code, test.code)
		}
	}
}

func TestMembershipCatalogMatchesDocumentedLaunchConfig(t *testing.T) {
	for _, product := range plans.Catalog() {
		switch product.Plan {
		case plans.PlanFree:
			if product.MaxOwnedKnowledgeBases != 5 || product.MaxFilesPerKB != 10 ||
				product.EmbeddingCharsPerMonth != 100_000 || product.CanShareOwnedKnowledgeBases {
				t.Fatalf("Free config = %+v", product)
			}
		case plans.PlanPlus:
			if product.MonthlyCents != 1_990 || product.YearlyCents != 19_104 ||
				product.MaxOwnedKnowledgeBases != 20 || product.MaxFilesPerKB != 100 ||
				product.EmbeddingCharsPerMonth != 10_000_000 || !product.CanShareOwnedKnowledgeBases {
				t.Fatalf("Plus config = %+v", product)
			}
		case plans.PlanPro:
			if product.MonthlyCents != 19_990 || product.YearlyCents != 57_504 ||
				product.MaxOwnedKnowledgeBases != -1 || product.MaxFilesPerKB != -1 ||
				product.EmbeddingCharsPerMonth != 1_000_000_000 || !product.CanShareOwnedKnowledgeBases {
				t.Fatalf("Pro config = %+v", product)
			}
		}
	}
}
