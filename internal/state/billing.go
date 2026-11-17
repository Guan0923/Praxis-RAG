package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/plans"
)

// Billing order statuses. Only pending orders are payable; paid orders may be
// refunded manually; manual marks an order that needs reconciliation.
const (
	BillingOrderPending   = "pending"
	BillingOrderPaid      = "paid"
	BillingOrderClosed    = "closed"
	BillingOrderRefunding = "refunding"
	BillingOrderRefunded  = "refunded"
	BillingOrderManual    = "manual"
)

// BillingCurrencyCNY is the only supported currency of the personal catalog.
const BillingCurrencyCNY = "CNY"

// Refund request statuses recorded on the order.
const (
	BillingRefundProcessing = "processing"
	BillingRefundSuccess    = "success"
	BillingRefundFailed     = "failed"
)

// Billing event types stored in billing_events.event_type.
const (
	BillingEventPaid   = "paid"
	BillingEventRefund = "refund"
)

// BillingOrder is the merchant order of record. Empty strings represent NULL
// columns.
type BillingOrder struct {
	ID                 string
	UserID             string
	Plan               string
	BillingPeriod      string
	Kind               string
	Status             string
	Currency           string
	Quote              string
	ProductSnapshot    string
	AmountCents        int64
	EntitlementVersion int64
	TermStart          string
	TermEnd            string
	AlipayTradeNo      string
	ExpiresAt          string
	PaidAt             string
	ClosedAt           string
	RefundRequestNo    string
	RefundStatus       string
	RefundReason       string
	ManualNote         string
	RefundAmountCents  int64
	CreatedAt          string
	UpdatedAt          string
}

// CreateBillingOrderInput is one server-generated order request. The price,
// quote and product snapshot are always produced by the server from the plans
// catalog; the Console only submits the plan/period/kind intent.
type CreateBillingOrderInput struct {
	UserID             string
	Plan               string
	BillingPeriod      string
	Kind               string
	AmountCents        int64
	Quote              string
	ProductSnapshot    string
	EntitlementVersion int64
	ExpiresAt          time.Time
	Now                time.Time
}

// CreateBillingOrderResult reports the stored order and whether an existing
// pending order was returned instead of creating a new one.
type CreateBillingOrderResult struct {
	Order  BillingOrder
	Reused bool
}

// PayableOrderExistsError reports that the account already has a different
// payable order; the caller must check/close it before creating a new one.
type PayableOrderExistsError struct {
	OrderID string
	Message string
}

// Error implements error.
func (e *PayableOrderExistsError) Error() string { return e.Message }

// BillingConflictError reports a state, amount or identity conflict that the
// caller must reconcile instead of granting entitlement.
type BillingConflictError struct {
	Message string
}

// Error implements error.
func (e *BillingConflictError) Error() string { return e.Message }

// EntitlementChangedError reports that an upgrade order was quoted against an
// entitlement version that no longer exists. The order is marked manual.
type EntitlementChangedError struct {
	Message string
}

// Error implements error.
func (e *EntitlementChangedError) Error() string { return e.Message }

// BillingRepository is the MySQL implementation of the Alipay order state and
// the manual refund flow. It never verifies signatures or merchant identity:
// that stays in the HTTP layer, and only a verified notification/reconciliation
// reaches ConfirmPayment.
type BillingRepository struct{ db *sql.DB }

func newBillingRepository(db *sql.DB) *BillingRepository { return &BillingRepository{db: db} }

const billingOrderColumns = "id, user_id, plan, billing_period, kind, status, amount_cents, currency, " +
	"quote, product_snapshot, entitlement_version, term_start, term_end, alipay_trade_no, expires_at, " +
	"paid_at, closed_at, refund_request_no, refund_amount_cents, refund_status, refund_reason, " +
	"manual_note, created_at, updated_at"

func scanBillingOrder(scanner rowScanner) (BillingOrder, error) {
	var order BillingOrder
	var tradeNo, paidAt, closedAt, refundRequestNo, refundStatus, refundReason, manualNote sql.NullString
	if err := scanner.Scan(&order.ID, &order.UserID, &order.Plan, &order.BillingPeriod, &order.Kind,
		&order.Status, &order.AmountCents, &order.Currency, &order.Quote, &order.ProductSnapshot,
		&order.EntitlementVersion, &order.TermStart, &order.TermEnd, &tradeNo, &order.ExpiresAt,
		&paidAt, &closedAt, &refundRequestNo, &order.RefundAmountCents, &refundStatus,
		&refundReason, &manualNote, &order.CreatedAt, &order.UpdatedAt); err != nil {
		return BillingOrder{}, err
	}
	if tradeNo.Valid {
		order.AlipayTradeNo = tradeNo.String
	}
	if paidAt.Valid {
		order.PaidAt = paidAt.String
	}
	if closedAt.Valid {
		order.ClosedAt = closedAt.String
	}
	if refundRequestNo.Valid {
		order.RefundRequestNo = refundRequestNo.String
	}
	if refundStatus.Valid {
		order.RefundStatus = refundStatus.String
	}
	if refundReason.Valid {
		order.RefundReason = refundReason.String
	}
	if manualNote.Valid {
		order.ManualNote = manualNote.String
	}
	return order, nil
}

// billingOrderForUpdate loads one order and locks it for the rest of the
// transaction. A missing order is nil,nil.
func billingOrderForUpdate(ctx context.Context, tx *sql.Tx, orderID string) (*BillingOrder, error) {
	order, err := scanBillingOrder(tx.QueryRowContext(ctx,
		"SELECT "+billingOrderColumns+" FROM billing_orders WHERE id = ? FOR UPDATE", orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, newStateError("订单读取失败")
	}
	return &order, nil
}

// billingOrderID generates the merchant order id: "PR" + a UTC timestamp + 16
// random hex characters, exactly 32 alphanumeric characters so it satisfies the
// Alipay out_trade_no contract.
func billingOrderID(now time.Time) (string, error) {
	suffix, err := randomHex(8)
	if err != nil {
		return "", err
	}
	return "PR" + now.UTC().Format("20060102150405") + suffix, nil
}

// validateBillingOrderInput normalizes the requested plan, period and kind.
func validateBillingOrderInput(input CreateBillingOrderInput) (plans.Plan, plans.BillingPeriod, string, error) {
	if strings.TrimSpace(input.UserID) == "" {
		return "", "", "", newValueError("账号不能为空")
	}
	plan, ok := plans.NormalizePlan(input.Plan)
	if !ok {
		return "", "", "", newValueError("套餐不存在")
	}
	period, ok := plans.NormalizePeriod(input.BillingPeriod)
	if !ok {
		return "", "", "", newValueError("套餐周期不存在")
	}
	kind := input.Kind
	switch kind {
	case MembershipKindPurchase, MembershipKindRenew, MembershipKindUpgrade:
	default:
		return "", "", "", newValueError("订单类型不存在")
	}
	if input.AmountCents < 0 {
		return "", "", "", newValueError("订单金额不能为负数")
	}
	return plan, period, kind, nil
}

// pendingOrdersTx lists the account's pending orders newest first. The account
// row is already locked by the caller, so no extra row lock is needed.
func pendingOrdersTx(ctx context.Context, tx *sql.Tx, userID string) ([]BillingOrder, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT "+billingOrderColumns+" FROM billing_orders WHERE user_id = ? AND status = ? "+
			"ORDER BY created_at DESC", userID, BillingOrderPending)
	if err != nil {
		return nil, newStateError("订单读取失败")
	}
	defer func() { _ = rows.Close() }()
	orders := make([]BillingOrder, 0, 2)
	for rows.Next() {
		order, err := scanBillingOrder(rows)
		if err != nil {
			return nil, newStateError("订单读取失败")
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("订单读取失败")
	}
	return orders, nil
}

// CreateOrder creates one pending order. An account may only have one pending
// order at a time, whatever its local expires_at says: an expired pending order
// still blocks a new one because it may be paid late and the caller must check
// it, close it or reconcile it first. A duplicate click for exactly the same
// plan/period/kind/amount reuses the still-valid pending order; an expired one
// is never reused and always reports PayableOrderExistsError.
func (r *BillingRepository) CreateOrder(ctx context.Context, input CreateBillingOrderInput) (CreateBillingOrderResult, error) {
	now := normalizeMembershipNow(input.Now)
	plan, period, kind, err := validateBillingOrderInput(input)
	if err != nil {
		return CreateBillingOrderResult{}, err
	}
	userID := strings.TrimSpace(input.UserID)
	expiresAt := input.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(30 * time.Minute)
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateBillingOrderResult{}, newStateError("订单创建失败")
	}
	defer func() { _ = transaction.Rollback() }()
	// The account row serializes order creation: two clicks on "pay" cannot
	// create two payable orders.
	if err := lockUserRow(ctx, transaction, userID); err != nil {
		return CreateBillingOrderResult{}, err
	}
	var accountExists string
	err = transaction.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ?", userID).Scan(&accountExists)
	if errors.Is(err, sql.ErrNoRows) {
		return CreateBillingOrderResult{}, newValueError("账号不存在")
	}
	if err != nil {
		return CreateBillingOrderResult{}, newStateError("订单创建失败")
	}
	pending, err := pendingOrdersTx(ctx, transaction, userID)
	if err != nil {
		return CreateBillingOrderResult{}, err
	}
	if len(pending) > 0 {
		// pendingOrdersTx returns the newest first; that is the order the
		// caller has to look up or close before creating a new one.
		existing := pending[0]
		if plans.ParseTimestamp(existing.ExpiresAt).After(now) &&
			existing.Plan == string(plan) && existing.BillingPeriod == string(period) &&
			existing.Kind == kind && existing.AmountCents == input.AmountCents {
			return CreateBillingOrderResult{Order: existing, Reused: true}, nil
		}
		return CreateBillingOrderResult{}, &PayableOrderExistsError{
			OrderID: existing.ID,
			Message: fmt.Sprintf("已有待支付订单，请先处理订单 %s", existing.ID),
		}
	}
	timestamp := plans.FormatTimestamp(now)
	order := BillingOrder{
		UserID:             userID,
		Plan:               string(plan),
		BillingPeriod:      string(period),
		Kind:               kind,
		Status:             BillingOrderPending,
		Currency:           BillingCurrencyCNY,
		Quote:              input.Quote,
		ProductSnapshot:    input.ProductSnapshot,
		AmountCents:        input.AmountCents,
		EntitlementVersion: input.EntitlementVersion,
		ExpiresAt:          plans.FormatTimestamp(expiresAt),
		CreatedAt:          timestamp,
		UpdatedAt:          timestamp,
	}
	inserted := false
	for attempt := 0; attempt < 3 && !inserted; attempt++ {
		orderID, err := billingOrderID(now)
		if err != nil {
			return CreateBillingOrderResult{}, newStateError("订单创建失败")
		}
		order.ID = orderID
		_, err = transaction.ExecContext(ctx,
			"INSERT INTO billing_orders (id, user_id, plan, billing_period, kind, status, amount_cents, "+
				"currency, quote, product_snapshot, entitlement_version, term_start, term_end, "+
				"alipay_trade_no, expires_at, paid_at, closed_at, refund_request_no, refund_amount_cents, "+
				"refund_status, refund_reason, manual_note, created_at, updated_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', NULL, ?, NULL, NULL, NULL, 0, NULL, NULL, NULL, ?, ?)",
			order.ID, order.UserID, order.Plan, order.BillingPeriod, order.Kind, order.Status,
			order.AmountCents, order.Currency, order.Quote, order.ProductSnapshot,
			order.EntitlementVersion, order.ExpiresAt, timestamp, timestamp)
		if err == nil {
			inserted = true
			break
		}
		if !isUniqueViolation(err) {
			return CreateBillingOrderResult{}, newStateError("订单创建失败")
		}
	}
	if !inserted {
		return CreateBillingOrderResult{}, newStateError("订单创建失败")
	}
	if err := transaction.Commit(); err != nil {
		return CreateBillingOrderResult{}, newStateError("订单创建失败")
	}
	return CreateBillingOrderResult{Order: order}, nil
}

// GetOrder returns one order, or nil when it does not exist.
func (r *BillingRepository) GetOrder(ctx context.Context, orderID string) (*BillingOrder, error) {
	order, err := scanBillingOrder(r.db.QueryRowContext(ctx,
		"SELECT "+billingOrderColumns+" FROM billing_orders WHERE id = ?", orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, newStateError("订单读取失败")
	}
	return &order, nil
}

func normalizeOrderLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

// ListOrdersByUser returns one account's orders, newest first.
func (r *BillingRepository) ListOrdersByUser(ctx context.Context, userID string, limit int) ([]BillingOrder, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+billingOrderColumns+" FROM billing_orders WHERE user_id = ? "+
			"ORDER BY created_at DESC LIMIT ?", userID, normalizeOrderLimit(limit))
	if err != nil {
		return nil, newStateError("订单读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanBillingOrders(rows)
}

// ListAllOrders returns the newest orders, optionally filtered by status. It
// backs the administrator reconciliation page.
func (r *BillingRepository) ListAllOrders(ctx context.Context, status string, limit int) ([]BillingOrder, error) {
	query := "SELECT " + billingOrderColumns + " FROM billing_orders"
	arguments := make([]any, 0, 2)
	if strings.TrimSpace(status) != "" {
		query += " WHERE status = ?"
		arguments = append(arguments, status)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	arguments = append(arguments, normalizeOrderLimit(limit))
	rows, err := r.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, newStateError("订单读取失败")
	}
	defer func() { _ = rows.Close() }()
	return scanBillingOrders(rows)
}

func scanBillingOrders(rows *sql.Rows) ([]BillingOrder, error) {
	orders := make([]BillingOrder, 0, 8)
	for rows.Next() {
		order, err := scanBillingOrder(rows)
		if err != nil {
			return nil, newStateError("订单读取失败")
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("订单读取失败")
	}
	return orders, nil
}

// insertBillingEvent appends one audit/idempotency row. event_key is unique, so
// a duplicated notification is silently ignored instead of granting twice.
func insertBillingEvent(ctx context.Context, tx *sql.Tx, orderID, eventKey, eventType, tradeNo string, amountCents int64, detail string, now time.Time) error {
	eventID, err := newStateID()
	if err != nil {
		return newStateError("账单事件无法保存")
	}
	var tradeValue, detailValue any
	if tradeNo != "" {
		tradeValue = tradeNo
	}
	if detail != "" {
		detailValue = detail
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT IGNORE INTO billing_events (id, order_id, event_key, event_type, alipay_trade_no, "+
			"amount_cents, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		eventID, orderID, eventKey, eventType, tradeValue, amountCents, detailValue,
		plans.FormatTimestamp(now)); err != nil {
		return newStateError("账单事件无法保存")
	}
	return nil
}

// ConfirmPayment applies one verified payment notification. It is the only
// place entitlement is granted. A repeated call for the same trade id is
// idempotent; a different trade id, a wrong amount, an order in any other state
// or an upgrade quoted against a stale entitlement version is a conflict that
// must be reconciled manually.
func (r *BillingRepository) ConfirmPayment(ctx context.Context, orderID, tradeNo string, amountCents int64, now time.Time) (*BillingOrder, bool, error) {
	now = normalizeMembershipNow(now)
	orderID = strings.TrimSpace(orderID)
	tradeNo = strings.TrimSpace(tradeNo)
	if orderID == "" {
		return nil, false, newValueError("订单号不能为空")
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, newStateError("订单状态无法保存")
	}
	defer func() { _ = transaction.Rollback() }()
	order, err := billingOrderForUpdate(ctx, transaction, orderID)
	if err != nil {
		return nil, false, err
	}
	if order == nil {
		return nil, false, newLookupError("订单不存在")
	}
	if order.Status == BillingOrderPaid {
		if tradeNo != "" && order.AlipayTradeNo == tradeNo {
			return order, true, nil
		}
		return nil, false, &BillingConflictError{Message: "订单已支付，支付宝交易号不一致"}
	}
	if order.Status != BillingOrderPending {
		return nil, false, &BillingConflictError{Message: "订单状态不是待支付，不能发放会员权益"}
	}
	if tradeNo == "" {
		return nil, false, &BillingConflictError{Message: "缺少支付宝交易号"}
	}
	if amountCents != order.AmountCents {
		return nil, false, &BillingConflictError{Message: "支付金额与订单金额不一致"}
	}
	var otherOrderID string
	err = transaction.QueryRowContext(ctx,
		"SELECT id FROM billing_orders WHERE alipay_trade_no = ? AND id <> ? LIMIT 1",
		tradeNo, order.ID).Scan(&otherOrderID)
	if err == nil {
		return nil, false, &BillingConflictError{Message: "支付宝交易号已被其他订单使用"}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, newStateError("订单状态无法保存")
	}
	if err := lockUserRow(ctx, transaction, order.UserID); err != nil {
		return nil, false, err
	}
	if err := ensureMembershipRow(ctx, transaction, order.UserID, now); err != nil {
		return nil, false, err
	}
	snapshot, err := loadMembershipSnapshot(ctx, transaction, order.UserID, true)
	if err != nil {
		return nil, false, err
	}
	terms, err := loadTerms(ctx, transaction, order.UserID)
	if err != nil {
		return nil, false, err
	}
	currentPlan, maxEnd := membershipState(terms, now)
	kind := order.Kind
	start := now
	var ends time.Time
	if kind == MembershipKindUpgrade {
		if order.EntitlementVersion != snapshot.EntitlementVersion {
			// The quote is stale. Park the order for manual reconciliation and
			// keep the payment trace instead of granting a wrong entitlement.
			if _, err := transaction.ExecContext(ctx,
				"UPDATE billing_orders SET status = ?, updated_at = ? WHERE id = ?",
				BillingOrderManual, plans.FormatTimestamp(now), order.ID); err != nil {
				return nil, false, newStateError("订单状态无法保存")
			}
			if err := transaction.Commit(); err != nil {
				return nil, false, newStateError("订单状态无法保存")
			}
			return nil, false, &EntitlementChangedError{Message: "会员权益已变更，升级订单需要重新报价"}
		}
		// Keep the original expiry: the override only raises the plan for the
		// remainder of the already paid period.
		start = now
		ends = maxEnd
		if !ends.After(now) {
			ends = plans.TermEnd(now, plans.BillingPeriod(order.BillingPeriod))
		}
		product := plans.ProductFor(plans.Plan(order.Plan))
		if _, err := transaction.ExecContext(ctx,
			"UPDATE embedding_quota_periods SET allowance_chars = GREATEST(allowance_chars, ?), "+
				"plan = ?, updated_at = ? WHERE user_id = ? AND source = ? AND period_end > ?",
			product.EmbeddingCharsPerMonth, order.Plan, plans.FormatTimestamp(now), order.UserID,
			MembershipSourcePaid, plans.FormatTimestamp(now)); err != nil {
			return nil, false, newStateError("额度账本无法保存")
		}
	} else if kind == MembershipKindRenew {
		start = maxEnd
		if !start.After(now) {
			start = now
		}
		ends = plans.TermEnd(start, plans.BillingPeriod(order.BillingPeriod))
	} else {
		// A purchase of the same tier while it is still active is a renewal:
		// it extends the existing expiry instead of starting a second window.
		if currentPlan == plans.Plan(order.Plan) && maxEnd.After(now) {
			kind = MembershipKindRenew
			start = maxEnd
		} else {
			start = now
		}
		ends = plans.TermEnd(start, plans.BillingPeriod(order.BillingPeriod))
	}
	termID, err := newStateID()
	if err != nil {
		return nil, false, newStateError("会员权益无法保存")
	}
	timestamp := plans.FormatTimestamp(now)
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO membership_terms (id, user_id, order_id, plan, billing_period, kind, starts_at, "+
			"ends_at, anchor_day, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		termID, order.UserID, order.ID, order.Plan, order.BillingPeriod, kind,
		plans.FormatTimestamp(start), plans.FormatTimestamp(ends), shanghaiDay(start),
		MembershipTermActive, timestamp, timestamp); err != nil {
		if isUniqueViolation(err) {
			return nil, false, &BillingConflictError{Message: "该订单已发放过会员权益"}
		}
		return nil, false, newStateError("会员权益无法保存")
	}
	result, err := transaction.ExecContext(ctx,
		"UPDATE billing_orders SET status = ?, alipay_trade_no = ?, paid_at = ?, term_start = ?, "+
			"term_end = ?, updated_at = ? WHERE id = ? AND status = ?",
		BillingOrderPaid, tradeNo, timestamp, plans.FormatTimestamp(start),
		plans.FormatTimestamp(ends), timestamp, order.ID, BillingOrderPending)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, false, &BillingConflictError{Message: "支付宝交易号已被其他订单使用"}
		}
		return nil, false, newStateError("订单状态无法保存")
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return nil, false, &BillingConflictError{Message: "订单状态已变更"}
	}
	if err := insertBillingEvent(ctx, transaction, order.ID, "order:"+order.ID+":paid",
		BillingEventPaid, tradeNo, amountCents, "", now); err != nil {
		return nil, false, err
	}
	terms, err = loadTerms(ctx, transaction, order.UserID)
	if err != nil {
		return nil, false, err
	}
	plan, expires := membershipState(terms, now)
	source := MembershipSourceFree
	if plan != plans.PlanFree {
		source = MembershipSourcePaid
	}
	if err := updateMembershipSnapshot(ctx, transaction, order.UserID, plan, source, expires, now, true); err != nil {
		return nil, false, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, false, newStateError("订单状态无法保存")
	}
	order.Status = BillingOrderPaid
	order.AlipayTradeNo = tradeNo
	order.PaidAt = timestamp
	order.TermStart = plans.FormatTimestamp(start)
	order.TermEnd = plans.FormatTimestamp(ends)
	order.UpdatedAt = timestamp
	return order, true, nil
}

// CloseOrder closes one pending order. Closing an already closed order is
// idempotent; a paid or refunding order cannot be closed.
func (r *BillingRepository) CloseOrder(ctx context.Context, orderID string, now time.Time) (*BillingOrder, error) {
	now = normalizeMembershipNow(now)
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, newStateError("订单状态无法保存")
	}
	defer func() { _ = transaction.Rollback() }()
	order, err := billingOrderForUpdate(ctx, transaction, orderID)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, newLookupError("订单不存在")
	}
	if order.Status == BillingOrderClosed {
		return order, nil
	}
	if order.Status != BillingOrderPending {
		return nil, &BillingConflictError{Message: "订单状态不可关闭"}
	}
	timestamp := plans.FormatTimestamp(now)
	if _, err := transaction.ExecContext(ctx,
		"UPDATE billing_orders SET status = ?, closed_at = ?, updated_at = ? WHERE id = ?",
		BillingOrderClosed, timestamp, timestamp, order.ID); err != nil {
		return nil, newStateError("订单状态无法保存")
	}
	if err := transaction.Commit(); err != nil {
		return nil, newStateError("订单状态无法保存")
	}
	order.Status = BillingOrderClosed
	order.ClosedAt = timestamp
	order.UpdatedAt = timestamp
	return order, nil
}

// MarkManual parks one order for reconciliation and records the operator note.
// A paid order keeps its paid state: the note is appended, not overwritten.
func (r *BillingRepository) MarkManual(ctx context.Context, orderID, note string, now time.Time) (*BillingOrder, error) {
	now = normalizeMembershipNow(now)
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, newStateError("订单状态无法保存")
	}
	defer func() { _ = transaction.Rollback() }()
	order, err := billingOrderForUpdate(ctx, transaction, orderID)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, newLookupError("订单不存在")
	}
	status := order.Status
	if status == BillingOrderPending || status == BillingOrderManual {
		status = BillingOrderManual
	}
	timestamp := plans.FormatTimestamp(now)
	if _, err := transaction.ExecContext(ctx,
		"UPDATE billing_orders SET status = ?, manual_note = ?, updated_at = ? WHERE id = ?",
		status, note, timestamp, order.ID); err != nil {
		return nil, newStateError("订单状态无法保存")
	}
	if err := transaction.Commit(); err != nil {
		return nil, newStateError("订单状态无法保存")
	}
	order.Status = status
	order.ManualNote = note
	order.UpdatedAt = timestamp
	return order, nil
}

// BeginRefund starts one manual full-order refund of a paid order. The request
// number is unique: repeating the same request is idempotent.
func (r *BillingRepository) BeginRefund(ctx context.Context, orderID, requestNo string, amountCents int64, reason string, now time.Time) (*BillingOrder, error) {
	now = normalizeMembershipNow(now)
	requestNo = strings.TrimSpace(requestNo)
	if requestNo == "" {
		return nil, newValueError("退款请求号不能为空")
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, newStateError("退款状态无法保存")
	}
	defer func() { _ = transaction.Rollback() }()
	order, err := billingOrderForUpdate(ctx, transaction, orderID)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, newLookupError("订单不存在")
	}
	if order.Status == BillingOrderRefunding && order.RefundRequestNo == requestNo {
		return order, nil
	}
	if order.Status != BillingOrderPaid {
		return nil, &BillingConflictError{Message: "订单状态不可退款"}
	}
	if amountCents <= 0 {
		amountCents = order.AmountCents
	}
	if amountCents > order.AmountCents {
		return nil, &BillingConflictError{Message: "退款金额超过订单金额"}
	}
	timestamp := plans.FormatTimestamp(now)
	if _, err := transaction.ExecContext(ctx,
		"UPDATE billing_orders SET status = ?, refund_request_no = ?, refund_amount_cents = ?, "+
			"refund_status = ?, refund_reason = ?, updated_at = ? WHERE id = ?",
		BillingOrderRefunding, requestNo, amountCents, BillingRefundProcessing,
		nullableText(reason), timestamp, order.ID); err != nil {
		if isUniqueViolation(err) {
			return nil, &BillingConflictError{Message: "退款请求号已存在"}
		}
		return nil, newStateError("退款状态无法保存")
	}
	if err := transaction.Commit(); err != nil {
		return nil, newStateError("退款状态无法保存")
	}
	order.Status = BillingOrderRefunding
	order.RefundRequestNo = requestNo
	order.RefundAmountCents = amountCents
	order.RefundStatus = BillingRefundProcessing
	order.RefundReason = reason
	order.UpdatedAt = timestamp
	return order, nil
}

// CompleteRefund finishes one manual refund: the order becomes refunded, the
// grant term of that order is revoked and the membership snapshot is recomputed
// from the remaining terms. Used quota and documents are kept untouched.
func (r *BillingRepository) CompleteRefund(ctx context.Context, orderID, requestNo, tradeNo string, now time.Time) (*BillingOrder, error) {
	now = normalizeMembershipNow(now)
	requestNo = strings.TrimSpace(requestNo)
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, newStateError("退款状态无法保存")
	}
	defer func() { _ = transaction.Rollback() }()
	order, err := billingOrderForUpdate(ctx, transaction, orderID)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, newLookupError("订单不存在")
	}
	if order.Status == BillingOrderRefunded {
		if requestNo == "" || order.RefundRequestNo == requestNo {
			return order, nil
		}
		return nil, &BillingConflictError{Message: "订单已退款，退款请求号不一致"}
	}
	if order.Status != BillingOrderRefunding {
		return nil, &BillingConflictError{Message: "订单状态不是退款中"}
	}
	if order.RefundRequestNo != requestNo {
		return nil, &BillingConflictError{Message: "退款请求号不匹配"}
	}
	timestamp := plans.FormatTimestamp(now)
	if _, err := transaction.ExecContext(ctx,
		"UPDATE billing_orders SET status = ?, refund_status = ?, updated_at = ? WHERE id = ?",
		BillingOrderRefunded, BillingRefundSuccess, timestamp, order.ID); err != nil {
		return nil, newStateError("退款状态无法保存")
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE membership_terms SET status = ?, updated_at = ? WHERE order_id = ?",
		MembershipTermRefunded, timestamp, order.ID); err != nil {
		return nil, newStateError("会员权益无法保存")
	}
	if err := ensureMembershipRow(ctx, transaction, order.UserID, now); err != nil {
		return nil, err
	}
	if _, err := loadMembershipSnapshot(ctx, transaction, order.UserID, true); err != nil {
		return nil, err
	}
	terms, err := loadTerms(ctx, transaction, order.UserID)
	if err != nil {
		return nil, err
	}
	plan, expires := membershipState(terms, now)
	source := MembershipSourceFree
	if plan != plans.PlanFree {
		source = MembershipSourcePaid
	}
	if err := updateMembershipSnapshot(ctx, transaction, order.UserID, plan, source, expires, now, true); err != nil {
		return nil, err
	}
	eventTradeNo := order.AlipayTradeNo
	if strings.TrimSpace(tradeNo) != "" {
		eventTradeNo = tradeNo
	}
	if err := insertBillingEvent(ctx, transaction, order.ID, "order:"+order.ID+":refund",
		BillingEventRefund, eventTradeNo, order.RefundAmountCents, "", now); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, newStateError("退款状态无法保存")
	}
	order.Status = BillingOrderRefunded
	order.RefundStatus = BillingRefundSuccess
	order.UpdatedAt = timestamp
	return order, nil
}
