package state

import (
	"context"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/plans"
)

// billingTestOrder builds one purchase order input at now.
func billingTestOrder(userID, plan string, period plans.BillingPeriod, amountCents, entitlementVersion int64, now time.Time) CreateBillingOrderInput {
	return CreateBillingOrderInput{
		UserID:             userID,
		Plan:               plan,
		BillingPeriod:      string(period),
		Kind:               MembershipKindPurchase,
		AmountCents:        amountCents,
		Quote:              `{"amount_cents":0}`,
		ProductSnapshot:    `{"plan":"` + plan + `"}`,
		EntitlementVersion: entitlementVersion,
		ExpiresAt:          now.Add(30 * time.Minute),
		Now:                now,
	}
}

func TestBillingConfirmPaymentIdempotent(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "billing-paid-user")

	created, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if created.Reused {
		t.Fatal("the first CreateOrder was reported as reused")
	}
	order := created.Order
	if order.Status != BillingOrderPending || order.Currency != BillingCurrencyCNY || len(order.ID) != 32 {
		t.Fatalf("created order = %+v", order)
	}
	paid, applied, err := stores.Billing.ConfirmPayment(ctx, order.ID, "2026111722001400000001", 1_990, now)
	if err != nil {
		t.Fatalf("ConfirmPayment() error = %v", err)
	}
	if !applied || paid == nil || paid.Status != BillingOrderPaid || paid.AlipayTradeNo == "" {
		t.Fatalf("ConfirmPayment() = %+v/%v", paid, applied)
	}
	terms, err := stores.Membership.Terms(ctx, user.ID)
	if err != nil {
		t.Fatalf("Terms() error = %v", err)
	}
	if len(terms) != 1 {
		t.Fatalf("terms = %d, want 1", len(terms))
	}
	term := terms[0]
	if term.Kind != MembershipKindPurchase || term.Plan != plans.PlanPlus ||
		term.Status != MembershipTermActive || term.BillingPeriod != string(plans.Monthly) {
		t.Fatalf("purchase term = %+v", term)
	}
	wantEnd := plans.TermEnd(now, plans.Monthly)
	if !term.StartsAt.Equal(now) || !term.EndsAt.Equal(wantEnd) {
		t.Fatalf("purchase term bounds = %v..%v, want %v..%v", term.StartsAt, term.EndsAt, now, wantEnd)
	}
	if term.AnchorDay != 17 {
		t.Fatalf("purchase term anchor day = %d, want 17", term.AnchorDay)
	}
	entitlement, err := stores.Membership.Effective(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	if entitlement.Plan != plans.PlanPlus || entitlement.Source != MembershipSourcePaid ||
		entitlement.EntitlementVersion != 2 || !entitlement.ExpiresAt.Equal(wantEnd) {
		t.Fatalf("paid entitlement = %+v", entitlement)
	}
	canShare, err := stores.Membership.CanShare(ctx, user.ID, now)
	if err != nil || !canShare {
		t.Fatalf("CanShare() = %v/%v, want true", canShare, err)
	}

	// A duplicated notification for the same trade id is a no-op.
	replayed, applied, err := stores.Billing.ConfirmPayment(ctx, order.ID, "2026111722001400000001", 1_990, now)
	if err != nil || !applied || replayed.Status != BillingOrderPaid {
		t.Fatalf("replayed ConfirmPayment() = %+v/%v/%v", replayed, applied, err)
	}
	if terms, _ := stores.Membership.Terms(ctx, user.ID); len(terms) != 1 {
		t.Fatalf("terms after the replay = %d, want 1", len(terms))
	}
	entitlement, err = stores.Membership.Effective(ctx, user.ID, now)
	if err != nil || entitlement.EntitlementVersion != 2 {
		t.Fatalf("entitlement version after the replay = %d (%v), want 2", entitlement.EntitlementVersion, err)
	}
	var eventCount int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM billing_events WHERE order_id = ?", order.ID).Scan(&eventCount); err != nil {
		t.Fatalf("counting billing events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("billing events = %d, want 1", eventCount)
	}

	// A different trade id for an already paid order is a conflict.
	_, _, err = stores.Billing.ConfirmPayment(ctx, order.ID, "2026111722001400000002", 1_990, now)
	mustBeError[*BillingConflictError](t, err)
	// A wrong amount on a pending order is a conflict and grants nothing.
	second, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "pro", plans.Monthly, 19_990, 2, now))
	if err != nil {
		t.Fatalf("CreateOrder(pro) error = %v", err)
	}
	_, _, err = stores.Billing.ConfirmPayment(ctx, second.Order.ID, "2026111722001400000003", 19_989, now)
	mustBeError[*BillingConflictError](t, err)
	if _, err := stores.Billing.CloseOrder(ctx, second.Order.ID, now); err != nil {
		t.Fatalf("CloseOrder() error = %v", err)
	}
	_, _, err = stores.Billing.ConfirmPayment(ctx, second.Order.ID, "2026111722001400000003", 19_990, now)
	mustBeError[*BillingConflictError](t, err)

	// A second purchase of the same tier extends the existing expiry.
	renewal, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 2, now))
	if err != nil {
		t.Fatalf("CreateOrder(renewal) error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, renewal.Order.ID, "2026111722001400000004", 1_990, now); err != nil {
		t.Fatalf("ConfirmPayment(renewal) error = %v", err)
	}
	terms, err = stores.Membership.Terms(ctx, user.ID)
	if err != nil {
		t.Fatalf("Terms() error = %v", err)
	}
	if len(terms) != 2 {
		t.Fatalf("terms after the renewal = %d, want 2", len(terms))
	}
	renewed := terms[1]
	if renewed.Kind != MembershipKindRenew || !renewed.StartsAt.Equal(wantEnd) {
		t.Fatalf("renewal term = %+v, want kind renew starting %v", renewed, wantEnd)
	}
	if wantRenewedEnd := plans.AddMonths(wantEnd, 1); !renewed.EndsAt.Equal(wantRenewedEnd) {
		t.Fatalf("renewal end = %v, want %v", renewed.EndsAt, wantRenewedEnd)
	}
	entitlement, err = stores.Membership.Effective(ctx, user.ID, now)
	if err != nil || !entitlement.ExpiresAt.Equal(renewed.EndsAt) {
		t.Fatalf("entitlement expiry after the renewal = %v (%v)", entitlement.ExpiresAt, err)
	}
}

func TestBillingPendingOrderReuseAndConflict(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "billing-pending-user")

	first, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	duplicate, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder(duplicate) error = %v", err)
	}
	if !duplicate.Reused || duplicate.Order.ID != first.Order.ID {
		t.Fatalf("duplicate order = %+v, want reused %q", duplicate, first.Order.ID)
	}
	_, err = stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "pro", plans.Monthly, 19_990, 1, now))
	payableErr := mustBeError[*PayableOrderExistsError](t, err)
	if payableErr.OrderID != first.Order.ID {
		t.Fatalf("PayableOrderExistsError.OrderID = %q, want %q", payableErr.OrderID, first.Order.ID)
	}

	closed, err := stores.Billing.CloseOrder(ctx, first.Order.ID, now)
	if err != nil {
		t.Fatalf("CloseOrder() error = %v", err)
	}
	if closed.Status != BillingOrderClosed || closed.ClosedAt == "" {
		t.Fatalf("closed order = %+v", closed)
	}
	again, err := stores.Billing.CloseOrder(ctx, first.Order.ID, now)
	if err != nil || again.Status != BillingOrderClosed {
		t.Fatalf("repeated CloseOrder() = %+v/%v", again, err)
	}
	replacement, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "pro", plans.Monthly, 19_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder after close error = %v", err)
	}
	if replacement.Reused || replacement.Order.ID == first.Order.ID {
		t.Fatalf("replacement order = %+v", replacement)
	}
	if _, err := stores.Billing.CloseOrder(ctx, replacement.Order.ID, now); err != nil {
		t.Fatalf("CloseOrder(replacement) error = %v", err)
	}

	// A locally expired pending order is still the account's one payable order:
	// it blocks a new one (it may be paid late) and is never reused.
	expiredInput := billingTestOrder(user.ID, "plus", plans.Yearly, 19_104, 1, now)
	expiredInput.ExpiresAt = now.Add(-time.Minute)
	expired, err := stores.Billing.CreateOrder(ctx, expiredInput)
	if err != nil {
		t.Fatalf("CreateOrder(expired) error = %v", err)
	}
	_, err = stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Yearly, 19_104, 1, now))
	blocked := mustBeError[*PayableOrderExistsError](t, err)
	if blocked.OrderID != expired.Order.ID {
		t.Fatalf("PayableOrderExistsError.OrderID = %q, want the expired pending order %q",
			blocked.OrderID, expired.Order.ID)
	}
	_, err = stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "pro", plans.Monthly, 19_990, 1, now))
	blocked = mustBeError[*PayableOrderExistsError](t, err)
	if blocked.OrderID != expired.Order.ID {
		t.Fatalf("PayableOrderExistsError.OrderID = %q, want %q", blocked.OrderID, expired.Order.ID)
	}
	if _, err := stores.Billing.CloseOrder(ctx, expired.Order.ID, now); err != nil {
		t.Fatalf("CloseOrder(expired) error = %v", err)
	}
	fresh, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Yearly, 19_104, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder(after closing the expired pending order) error = %v", err)
	}
	if fresh.Reused || fresh.Order.ID == expired.Order.ID {
		t.Fatalf("fresh order = %+v", fresh)
	}

	orders, err := stores.Billing.ListOrdersByUser(ctx, user.ID, 10)
	if err != nil {
		t.Fatalf("ListOrdersByUser() error = %v", err)
	}
	if len(orders) != 4 {
		t.Fatalf("ListOrdersByUser() = %d orders, want 4", len(orders))
	}
	all, err := stores.Billing.ListAllOrders(ctx, BillingOrderPending, 10)
	if err != nil {
		t.Fatalf("ListAllOrders() error = %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("ListAllOrders(pending) = %d orders, want 1", len(all))
	}
	if missing, err := stores.Billing.GetOrder(ctx, "PR-does-not-exist"); err != nil || missing != nil {
		t.Fatalf("GetOrder(unknown) = %+v/%v", missing, err)
	}
	if _, err := stores.Billing.CreateOrder(ctx, CreateBillingOrderInput{
		UserID: user.ID, Plan: "enterprise", BillingPeriod: "month", Kind: MembershipKindPurchase, Now: now,
	}); err == nil {
		t.Fatal("CreateOrder(invalid plan) did not fail")
	}
}

func TestBillingMonthlyAnchorClamp(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	// 2027-01-31 10:00 Asia/Shanghai.
	now := time.Date(2027, 1, 31, 2, 0, 0, 0, time.UTC)
	user := createMembershipTestUser(t, stores, "anchor-user")

	monthly, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder(monthly) error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, monthly.Order.ID, "2027013122001400000001", 1_990, now); err != nil {
		t.Fatalf("ConfirmPayment(monthly) error = %v", err)
	}
	terms, err := stores.Membership.Terms(ctx, user.ID)
	if err != nil || len(terms) != 1 {
		t.Fatalf("Terms() = %v/%v", terms, err)
	}
	wantMonthlyEnd := time.Date(2027, 2, 28, 2, 0, 0, 0, time.UTC)
	if !terms[0].EndsAt.Equal(wantMonthlyEnd) {
		t.Fatalf("1/31 monthly end = %v, want %v", terms[0].EndsAt, wantMonthlyEnd)
	}
	yearly, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Yearly, 19_104, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder(yearly) error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, yearly.Order.ID, "2027013122001400000002", 19_104, now); err != nil {
		t.Fatalf("ConfirmPayment(yearly) error = %v", err)
	}
	terms, err = stores.Membership.Terms(ctx, user.ID)
	if err != nil || len(terms) != 2 {
		t.Fatalf("Terms() after the yearly renewal = %v/%v", terms, err)
	}
	wantYearlyEnd := time.Date(2028, 2, 28, 2, 0, 0, 0, time.UTC)
	if !terms[1].StartsAt.Equal(wantMonthlyEnd) || !terms[1].EndsAt.Equal(wantYearlyEnd) {
		t.Fatalf("yearly renewal = %v..%v, want %v..%v",
			terms[1].StartsAt, terms[1].EndsAt, wantMonthlyEnd, wantYearlyEnd)
	}
}

func TestBillingUpgradeKeepsExpiryAndRaisesAllowance(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "upgrade-user")

	purchase, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, purchase.Order.ID, "2026111722001400000010", 1_990, now); err != nil {
		t.Fatalf("ConfirmPayment() error = %v", err)
	}
	if _, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
		UserID: user.ID, OperationID: "op-upgrade-usage", Namespace: "kb", Chars: 100, Now: now,
	}); err != nil {
		t.Fatalf("ReserveEmbedding() error = %v", err)
	}
	before, err := stores.Membership.Effective(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	upgradeInput := billingTestOrder(user.ID, "pro", plans.Monthly, 12_000, before.EntitlementVersion, now)
	upgradeInput.Kind = MembershipKindUpgrade
	upgrade, err := stores.Billing.CreateOrder(ctx, upgradeInput)
	if err != nil {
		t.Fatalf("CreateOrder(upgrade) error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, upgrade.Order.ID, "2026111722001400000011", 12_000, now); err != nil {
		t.Fatalf("ConfirmPayment(upgrade) error = %v", err)
	}
	after, err := stores.Membership.Effective(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("Effective() after the upgrade error = %v", err)
	}
	if after.Plan != plans.PlanPro || after.Source != MembershipSourcePaid {
		t.Fatalf("upgraded entitlement = %+v", after)
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("upgrade changed the expiry: %v -> %v", before.ExpiresAt, after.ExpiresAt)
	}
	if after.EntitlementVersion != before.EntitlementVersion+1 {
		t.Fatalf("entitlement version = %d, want %d", after.EntitlementVersion, before.EntitlementVersion+1)
	}
	terms, err := stores.Membership.Terms(ctx, user.ID)
	if err != nil || len(terms) != 2 {
		t.Fatalf("Terms() = %v/%v", terms, err)
	}
	var override MembershipTerm
	found := false
	for _, term := range terms {
		if term.Kind == MembershipKindUpgrade {
			override = term
			found = true
		}
	}
	if !found {
		t.Fatalf("no upgrade term in %+v", terms)
	}
	if override.Plan != plans.PlanPro || !override.StartsAt.Equal(now) ||
		!override.EndsAt.Equal(before.ExpiresAt) {
		t.Fatalf("upgrade term = %+v", override)
	}
	summary, err := stores.Membership.QuotaSummary(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("QuotaSummary() error = %v", err)
	}
	if summary.Plan != plans.PlanPro || summary.AllowanceChars != plans.ProductFor(plans.PlanPro).EmbeddingCharsPerMonth {
		t.Fatalf("upgraded quota summary = %+v", summary)
	}
	if summary.UsedChars != 0 || summary.ReservedChars != 100 {
		t.Fatalf("upgraded quota counters = used %d reserved %d", summary.UsedChars, summary.ReservedChars)
	}

	// An upgrade quoted against the previous entitlement version is parked for
	// manual reconciliation and grants nothing.
	staleInput := billingTestOrder(user.ID, "pro", plans.Monthly, 11_000, before.EntitlementVersion, now)
	staleInput.Kind = MembershipKindUpgrade
	stale, err := stores.Billing.CreateOrder(ctx, staleInput)
	if err != nil {
		t.Fatalf("CreateOrder(stale upgrade) error = %v", err)
	}
	_, _, err = stores.Billing.ConfirmPayment(ctx, stale.Order.ID, "2026111722001400000012", 11_000, now)
	mustBeError[*EntitlementChangedError](t, err)
	parked, err := stores.Billing.GetOrder(ctx, stale.Order.ID)
	if err != nil {
		t.Fatalf("GetOrder(stale) error = %v", err)
	}
	if parked.Status != BillingOrderManual {
		t.Fatalf("stale upgrade status = %q, want manual", parked.Status)
	}
	if terms, _ := stores.Membership.Terms(ctx, user.ID); len(terms) != 2 {
		t.Fatalf("terms after the stale upgrade = %d, want 2", len(terms))
	}
}

func TestBillingCloseAndManual(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "billing-close-user")

	created, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	manual, err := stores.Billing.MarkManual(ctx, created.Order.ID, "用户申请人工处理", now)
	if err != nil {
		t.Fatalf("MarkManual() error = %v", err)
	}
	if manual.Status != BillingOrderManual || manual.ManualNote != "用户申请人工处理" {
		t.Fatalf("manual order = %+v", manual)
	}
	if _, err := stores.Billing.CloseOrder(ctx, created.Order.ID, now); err == nil {
		t.Fatal("CloseOrder(manual) did not fail")
	}
	other, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder(after manual) error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, other.Order.ID, "2026111722001400000020", 1_990, now); err != nil {
		t.Fatalf("ConfirmPayment() error = %v", err)
	}
	if _, err := stores.Billing.CloseOrder(ctx, other.Order.ID, now); err == nil {
		t.Fatal("CloseOrder(paid) did not fail")
	}
	if _, err := stores.Billing.MarkManual(ctx, other.Order.ID, "对账备注", now); err != nil {
		t.Fatalf("MarkManual(paid) error = %v", err)
	}
	if paid, _ := stores.Billing.GetOrder(ctx, other.Order.ID); paid.Status != BillingOrderPaid || paid.ManualNote != "对账备注" {
		t.Fatalf("paid order after MarkManual = %+v", paid)
	}
}

func TestBillingRefundRevokesTerm(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "billing-refund-user")

	created, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, created.Order.ID, "2026111722001400000030", 1_990, now); err != nil {
		t.Fatalf("ConfirmPayment() error = %v", err)
	}
	refunding, err := stores.Billing.BeginRefund(ctx, created.Order.ID, "RF-20261117-1", 1_990, "用户申请退款", now)
	if err != nil {
		t.Fatalf("BeginRefund() error = %v", err)
	}
	if refunding.Status != BillingOrderRefunding || refunding.RefundStatus != BillingRefundProcessing {
		t.Fatalf("refunding order = %+v", refunding)
	}
	repeated, err := stores.Billing.BeginRefund(ctx, created.Order.ID, "RF-20261117-1", 1_990, "用户申请退款", now)
	if err != nil || repeated.Status != BillingOrderRefunding {
		t.Fatalf("repeated BeginRefund() = %+v/%v", repeated, err)
	}
	refunded, err := stores.Billing.CompleteRefund(ctx, created.Order.ID, "RF-20261117-1", "2026111722001400000030", now)
	if err != nil {
		t.Fatalf("CompleteRefund() error = %v", err)
	}
	if refunded.Status != BillingOrderRefunded || refunded.RefundStatus != BillingRefundSuccess {
		t.Fatalf("refunded order = %+v", refunded)
	}
	terms, err := stores.Membership.Terms(ctx, user.ID)
	if err != nil || len(terms) != 1 {
		t.Fatalf("Terms() = %v/%v", terms, err)
	}
	if terms[0].Status != MembershipTermRefunded {
		t.Fatalf("refunded term status = %q", terms[0].Status)
	}
	entitlement, err := stores.Membership.Effective(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("Effective() after the refund error = %v", err)
	}
	if entitlement.Plan != plans.PlanFree || entitlement.Source != MembershipSourceFree {
		t.Fatalf("entitlement after the refund = %+v", entitlement)
	}
	if entitlement.EntitlementVersion != 3 {
		t.Fatalf("entitlement version after the refund = %d, want 3", entitlement.EntitlementVersion)
	}
	var eventCount int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM billing_events WHERE order_id = ?", created.Order.ID).Scan(&eventCount); err != nil {
		t.Fatalf("counting billing events: %v", err)
	}
	if eventCount != 2 {
		t.Fatalf("billing events = %d, want 2 (paid+refund)", eventCount)
	}
	// Repeating the completion is idempotent and keeps a single refund event.
	if _, err := stores.Billing.CompleteRefund(ctx, created.Order.ID, "RF-20261117-1", "", now); err != nil {
		t.Fatalf("repeated CompleteRefund() error = %v", err)
	}
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM billing_events WHERE order_id = ? AND event_type = ?",
		created.Order.ID, BillingEventRefund).Scan(&eventCount); err != nil {
		t.Fatalf("counting refund events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("refund events = %d, want 1", eventCount)
	}

	// A non-paid order cannot be refunded.
	pending, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 3, now))
	if err != nil {
		t.Fatalf("CreateOrder(pending) error = %v", err)
	}
	_, err = stores.Billing.BeginRefund(ctx, pending.Order.ID, "RF-20261117-2", 1_990, "", now)
	mustBeError[*BillingConflictError](t, err)
	if _, err := stores.Billing.CloseOrder(ctx, pending.Order.ID, now); err != nil {
		t.Fatalf("CloseOrder() error = %v", err)
	}
	_, err = stores.Billing.CompleteRefund(ctx, pending.Order.ID, "RF-20261117-2", "", now)
	if err == nil {
		t.Fatal("CompleteRefund(closed) did not fail")
	}
}

func TestBillingUpgradeRefundRestoresBasePlan(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "billing-upgrade-refund-user")

	purchase, err := stores.Billing.CreateOrder(ctx, billingTestOrder(user.ID, "plus", plans.Monthly, 1_990, 1, now))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, purchase.Order.ID, "2026111722001400000040", 1_990, now); err != nil {
		t.Fatalf("ConfirmPayment() error = %v", err)
	}
	before, err := stores.Membership.Effective(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	upgradeInput := billingTestOrder(user.ID, "pro", plans.Monthly, 12_000, before.EntitlementVersion, now)
	upgradeInput.Kind = MembershipKindUpgrade
	upgrade, err := stores.Billing.CreateOrder(ctx, upgradeInput)
	if err != nil {
		t.Fatalf("CreateOrder(upgrade) error = %v", err)
	}
	if _, _, err := stores.Billing.ConfirmPayment(ctx, upgrade.Order.ID, "2026111722001400000041", 12_000, now); err != nil {
		t.Fatalf("ConfirmPayment(upgrade) error = %v", err)
	}
	if upgraded, _ := stores.Membership.Effective(ctx, user.ID, now); upgraded.Plan != plans.PlanPro {
		t.Fatalf("plan after the upgrade = %v, want pro", upgraded.Plan)
	}
	if _, err := stores.Billing.BeginRefund(ctx, upgrade.Order.ID, "RF-upgrade-1", 12_000, "", now); err != nil {
		t.Fatalf("BeginRefund(upgrade) error = %v", err)
	}
	if _, err := stores.Billing.CompleteRefund(ctx, upgrade.Order.ID, "RF-upgrade-1", "", now); err != nil {
		t.Fatalf("CompleteRefund(upgrade) error = %v", err)
	}
	after, err := stores.Membership.Effective(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("Effective() after the upgrade refund error = %v", err)
	}
	if after.Plan != plans.PlanPlus {
		t.Fatalf("plan after the upgrade refund = %v, want plus", after.Plan)
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("expiry after the upgrade refund = %v, want %v", after.ExpiresAt, before.ExpiresAt)
	}
}
