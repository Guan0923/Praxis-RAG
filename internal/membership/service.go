// Package membership coordinates personal membership, order quotes and the
// Console-facing entitlement projection. MySQL state and the Alipay gateway are
// the authorities; this package contains no cache and never trusts a
// client-supplied price, plan limit or payment result.
package membership

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/payment"
	"github.com/Guan0923/Praxis-RAG/internal/plans"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// Error codes map to HTTP status in the API layer.
const (
	CodeValidation  = "validation"
	CodeForbidden   = "forbidden"
	CodeNotFound    = "not_found"
	CodeConflict    = "conflict"
	CodeUnavailable = "unavailable"
)

// Error is a safe, user-facing membership/billing failure.
type Error struct {
	Code    string
	Message string
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// ErrorMessage returns a log-safe message for any membership/billing failure.
// It never includes request bodies, signatures or key material.
func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Message
	}
	return "会员操作失败"
}

func newError(code, message string) *Error { return &Error{Code: code, Message: message} }

// Status labels are kept server-side so every Console surface uses the same
// Chinese wording.
var orderStatusLabels = map[string]string{
	"pending":   "待支付",
	"paid":      "已支付",
	"closed":    "已关闭",
	"refunding": "退款中",
	"refunded":  "已退款",
	"manual":    "待人工处理",
}

var kindLabels = map[string]string{
	"purchase": "新购",
	"renew":    "续费",
	"upgrade":  "升级",
}

var periodLabels = map[plans.BillingPeriod]string{
	plans.Monthly: "月卡",
	plans.Yearly:  "年卡",
}

// Service coordinates membership state, orders and Alipay.
type Service struct {
	stores   *state.Stores
	payments *payment.Client
	settings payment.Settings
	now      func() time.Time
}

// New builds the service. A missing/incomplete Alipay configuration is not a
// startup failure: the Console still shows the plan and usage, while new
// purchases are disabled until the merchant configuration is complete.
func New(stores *state.Stores, settings payment.Settings) (*Service, error) {
	if stores == nil || stores.Membership == nil || stores.Billing == nil {
		return nil, fmt.Errorf("membership service requires the shared state store")
	}
	service := &Service{stores: stores, settings: settings, now: time.Now}
	if settings.Configured() {
		client, err := payment.NewClient(settings)
		if err != nil {
			return nil, err
		}
		service.payments = client
	}
	return service, nil
}

// SetClock is the test seam for quote/period assertions.
func (s *Service) SetClock(clock func() time.Time) {
	if clock != nil {
		s.now = clock
	}
}

// CanShare implements access.EntitlementProvider without importing access:
// only an active Plus/Pro membership enables paid sharing. Role-based VIP and
// administrator compatibility is handled by the access layer.
func (s *Service) CanShare(ctx context.Context, userID string) (bool, error) {
	return s.stores.Membership.CanShare(ctx, userID, s.now())
}

// ---- Console projections -------------------------------------------------

// PlanLimits is the JSON shape of one plan's limits.
type PlanLimits struct {
	KnowledgeBases          int   `json:"knowledge_bases"`
	FilesPerKB              int   `json:"files_per_kb"`
	EmbeddingCharsPerMonth  int64 `json:"embedding_chars_per_month"`
	UnlimitedKnowledgeBases bool  `json:"unlimited_knowledge_bases"`
	UnlimitedFiles          bool  `json:"unlimited_files"`
}

// PlanView is one catalog row rendered for the Console.
type PlanView struct {
	Plan         string     `json:"plan"`
	Name         string     `json:"name"`
	MonthlyCents int64      `json:"monthly_cents"`
	YearlyCents  int64      `json:"yearly_cents"`
	MonthlyYuan  string     `json:"monthly_yuan"`
	YearlyYuan   string     `json:"yearly_yuan"`
	YearDiscount bool       `json:"year_discount"`
	Limits       PlanLimits `json:"limits"`
	CanShare     bool       `json:"can_share"`
}

// MembershipView is the caller's current entitlement and quota summary.
type MembershipView struct {
	Plan        string       `json:"plan"`
	PlanName    string       `json:"plan_name"`
	Source      string       `json:"source"`
	StartedAt   string       `json:"started_at"`
	ExpiresAt   *string      `json:"expires_at"`
	RefreshAt   string       `json:"refresh_at"`
	Limits      PlanLimits   `json:"limits"`
	Usage       UsageView    `json:"usage"`
	CanShare    bool         `json:"can_share"`
	UpgradePlan *string      `json:"upgrade_plan"`
	Purchase    PurchaseView `json:"purchase"`
}

// UsageView is the quota/utilization summary.
type UsageView struct {
	KnowledgeBases     int    `json:"knowledge_bases"`
	EmbeddingAllowance int64  `json:"embedding_allowance"`
	EmbeddingUsed      int64  `json:"embedding_used"`
	EmbeddingReserved  int64  `json:"embedding_reserved"`
	PeriodStart        string `json:"period_start"`
	PeriodEnd          string `json:"period_end"`
}

// PurchaseView reports whether new orders are accepted. Processing payments and
// querying existing orders stay enabled while new purchases are paused.
type PurchaseView struct {
	Available bool    `json:"available"`
	Paused    bool    `json:"paused"`
	Reason    *string `json:"reason"`
}

// OrderQuote is the immutable pricing/period snapshot shown before payment.
type OrderQuote struct {
	StartsAt    string             `json:"starts_at"`
	EndsAt      string             `json:"ends_at"`
	CurrentPlan string             `json:"current_plan"`
	TargetPlan  string             `json:"target_plan"`
	Segments    []QuoteSegmentView `json:"segments,omitempty"`
}

// QuoteSegmentView is one upgraded remaining term.
type QuoteSegmentView struct {
	StartAt     string `json:"starts_at"`
	EndAt       string `json:"ends_at"`
	Plan        string `json:"plan"`
	Period      string `json:"period"`
	AmountCents int64  `json:"amount_cents"`
}

// OrderView is the Console order projection.
type OrderView struct {
	ID               string     `json:"id"`
	Status           string     `json:"status"`
	StatusLabel      string     `json:"status_label"`
	Plan             string     `json:"plan"`
	PlanName         string     `json:"plan_name"`
	Period           string     `json:"period"`
	PeriodLabel      string     `json:"period_label"`
	Kind             string     `json:"kind"`
	KindLabel        string     `json:"kind_label"`
	AmountCents      int64      `json:"amount_cents"`
	AmountYuan       string     `json:"amount_yuan"`
	Currency         string     `json:"currency"`
	Quote            OrderQuote `json:"quote"`
	AlipayTradeNo    *string    `json:"alipay_trade_no"`
	CreatedAt        string     `json:"created_at"`
	ExpiresAt        string     `json:"expires_at"`
	PaidAt           *string    `json:"paid_at"`
	RefundStatus     *string    `json:"refund_status"`
	RefundAmountYuan *string    `json:"refund_amount_yuan"`
	Username         string     `json:"username,omitempty"`
}

// Snapshot is GET /v1/membership.
type Snapshot struct {
	Membership MembershipView `json:"membership"`
	Plans      []PlanView     `json:"plans"`
	Orders     []OrderView    `json:"orders"`
}

// PayView is the browser form target and signed Alipay parameters.
type PayView struct {
	Gateway string            `json:"gateway"`
	Params  map[string]string `json:"params"`
}

// CreateOrderResult is POST /v1/membership/orders.
type CreateOrderResult struct {
	Order OrderView `json:"order"`
	Pay   *PayView  `json:"pay,omitempty"`
}

// CreateOrderRequest is the buyer's intent. Prices and periods are never read
// from this struct.
type CreateOrderRequest struct {
	Plan   string `json:"plan"`
	Period string `json:"period"`
	Intent string `json:"intent"`
}

func planLimits(product plans.Product) PlanLimits {
	return PlanLimits{
		KnowledgeBases:          product.MaxOwnedKnowledgeBases,
		FilesPerKB:              product.MaxFilesPerKB,
		EmbeddingCharsPerMonth:  product.EmbeddingCharsPerMonth,
		UnlimitedKnowledgeBases: product.MaxOwnedKnowledgeBases < 0,
		UnlimitedFiles:          product.MaxFilesPerKB < 0,
	}
}

func planViews() []PlanView {
	catalog := plans.Catalog()
	views := make([]PlanView, 0, len(catalog))
	for _, product := range catalog {
		views = append(views, PlanView{
			Plan:         string(product.Plan),
			Name:         product.Name,
			MonthlyCents: product.MonthlyCents,
			YearlyCents:  product.YearlyCents,
			MonthlyYuan:  payment.FormatCents(product.MonthlyCents),
			YearlyYuan:   payment.FormatCents(product.YearlyCents),
			YearDiscount: product.Plan == plans.PlanPlus,
			Limits:       planLimits(product),
			CanShare:     product.CanShareOwnedKnowledgeBases,
		})
	}
	return views
}

// purchaseState resolves the new-order switch without exposing credentials.
func (s *Service) purchaseState() PurchaseView {
	if !s.settings.Configured() {
		reason := "支付宝支付尚未配置，请联系管理员"
		return PurchaseView{Available: false, Paused: true, Reason: &reason}
	}
	if !s.settings.PurchaseEnabled {
		reason := "购买通道已暂停，已支付订单和会员权益不受影响"
		return PurchaseView{Available: false, Paused: true, Reason: &reason}
	}
	if s.payments == nil {
		reason := "支付宝支付通道初始化失败，请联系管理员"
		return PurchaseView{Available: false, Paused: true, Reason: &reason}
	}
	return PurchaseView{Available: true}
}

// Snapshot returns the caller's membership, catalog and recent orders.
func (s *Service) Snapshot(ctx context.Context, userID string) (Snapshot, error) {
	now := s.now()
	entitlement, err := s.stores.Membership.Effective(ctx, userID, now)
	if err != nil {
		return Snapshot{}, newError(CodeUnavailable, "会员权益读取失败")
	}
	quota, err := s.stores.Membership.QuotaSummary(ctx, userID, now)
	if err != nil {
		return Snapshot{}, newError(CodeUnavailable, "Embedding 额度读取失败")
	}
	owned, err := s.stores.KnowledgeBases.OwnedNamespaces(ctx, userID)
	if err != nil {
		return Snapshot{}, newError(CodeUnavailable, "知识库配额读取失败")
	}
	orders, err := s.ordersForUser(ctx, userID, 10)
	if err != nil {
		return Snapshot{}, err
	}
	membership := MembershipView{
		Plan:      string(entitlement.Plan),
		PlanName:  plans.ProductFor(entitlement.Plan).Name,
		Source:    entitlement.Source,
		StartedAt: plans.FormatTimestamp(entitlement.StartsAt),
		RefreshAt: plans.FormatTimestamp(entitlement.RefreshAt),
		Limits:    planLimits(entitlement.Limits),
		Usage: UsageView{
			KnowledgeBases:     len(owned),
			EmbeddingAllowance: quota.AllowanceChars,
			EmbeddingUsed:      quota.UsedChars,
			EmbeddingReserved:  quota.ReservedChars,
			PeriodStart:        plans.FormatTimestamp(quota.PeriodStart),
			PeriodEnd:          plans.FormatTimestamp(quota.PeriodEnd),
		},
		Purchase: s.purchaseState(),
	}
	if !entitlement.ExpiresAt.IsZero() {
		value := plans.FormatTimestamp(entitlement.ExpiresAt)
		membership.ExpiresAt = &value
	}
	canShare, err := s.stores.Membership.CanShare(ctx, userID, now)
	if err != nil {
		return Snapshot{}, newError(CodeUnavailable, "会员权益读取失败")
	}
	membership.CanShare = canShare
	if entitlement.Plan == plans.PlanPlus {
		target := string(plans.PlanPro)
		membership.UpgradePlan = &target
	}
	return Snapshot{
		Membership: membership,
		Plans:      planViews(),
		Orders:     orders,
	}, nil
}

// ---- Orders --------------------------------------------------------------

func (s *Service) ordersForUser(ctx context.Context, userID string, limit int) ([]OrderView, error) {
	orders, err := s.stores.Billing.ListOrdersByUser(ctx, userID, limit)
	if err != nil {
		return nil, newError(CodeUnavailable, "订单读取失败")
	}
	views := make([]OrderView, 0, len(orders))
	for _, order := range orders {
		views = append(views, s.orderView(order, ""))
	}
	return views, nil
}

// ListOrders returns the caller's orders.
func (s *Service) ListOrders(ctx context.Context, userID string, limit int) ([]OrderView, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return s.ordersForUser(ctx, userID, limit)
}

// CreateOrder quotes and creates (or resumes) one payable order. The quote is
// computed only from MySQL entitlement state and the product catalog.
func (s *Service) CreateOrder(ctx context.Context, user state.User, request CreateOrderRequest) (CreateOrderResult, error) {
	purchase := s.purchaseState()
	if !purchase.Available {
		message := "购买通道暂不可用"
		if purchase.Reason != nil {
			message = *purchase.Reason
		}
		return CreateOrderResult{}, newError(CodeUnavailable, message)
	}
	now := s.now()
	entitlement, err := s.stores.Membership.Effective(ctx, user.ID, now)
	if err != nil {
		return CreateOrderResult{}, newError(CodeUnavailable, "会员权益读取失败")
	}
	planKey := strings.ToLower(strings.TrimSpace(request.Plan))
	plan, planOK := plans.NormalizePlan(planKey)
	if !planOK || plan == plans.PlanFree {
		return CreateOrderResult{}, newError(CodeValidation, "请选择 Plus 或 Pro 套餐")
	}
	intent := strings.ToLower(strings.TrimSpace(request.Intent))
	upgrade := intent == "upgrade" || (entitlement.Plan == plans.PlanPlus && plan == plans.PlanPro)
	if entitlement.Plan == plans.PlanPro && plan == plans.PlanPlus {
		return CreateOrderResult{}, newError(CodeConflict, "当前付费期内不支持降档，可到期后再选择")
	}
	var (
		kind   = "purchase"
		period plans.BillingPeriod
		amount int64
		quote  OrderQuote
	)
	if upgrade {
		if entitlement.Plan != plans.PlanPlus || plan != plans.PlanPro {
			return CreateOrderResult{}, newError(CodeConflict, "当前套餐不支持该升级")
		}
		terms, err := s.stores.Membership.ActiveBaseTerms(ctx, user.ID, now)
		if err != nil {
			return CreateOrderResult{}, newError(CodeUnavailable, "会员权益读取失败")
		}
		segments := make([]plans.UpgradeSegment, 0, len(terms))
		for _, term := range terms {
			periodValue, ok := plans.NormalizePeriod(term.BillingPeriod)
			if !ok {
				periodValue = plans.Monthly
			}
			segments = append(segments, plans.UpgradeSegment{
				Plan: term.Plan, Period: periodValue,
				Start: term.StartsAt, End: term.EndsAt,
			})
		}
		price := plans.QuoteUpgrade(entitlement.Plan, plan, segments, now)
		if price.AmountCents <= 0 {
			return CreateOrderResult{}, newError(CodeConflict, "当前没有可升级的剩余服务期")
		}
		amount = price.AmountCents
		kind = "upgrade"
		period = plans.Monthly
		if len(terms) > 0 {
			if parsed, ok := plans.NormalizePeriod(terms[0].BillingPeriod); ok {
				period = parsed
			}
		}
		quote = OrderQuote{
			StartsAt:    plans.FormatTimestamp(now),
			EndsAt:      plans.FormatTimestamp(entitlement.ExpiresAt),
			CurrentPlan: string(entitlement.Plan),
			TargetPlan:  string(plan),
			Segments:    make([]QuoteSegmentView, 0, len(price.Segments)),
		}
		for _, segment := range price.Segments {
			quote.Segments = append(quote.Segments, QuoteSegmentView{
				StartAt:     plans.FormatTimestamp(segment.Start),
				EndAt:       plans.FormatTimestamp(segment.End),
				Plan:        string(segment.Plan),
				Period:      string(segment.Period),
				AmountCents: segment.AmountCents,
			})
		}
	} else {
		normalizedPeriod, ok := plans.NormalizePeriod(strings.ToLower(strings.TrimSpace(request.Period)))
		if !ok {
			return CreateOrderResult{}, newError(CodeValidation, "请选择月卡或年卡")
		}
		period = normalizedPeriod
		start := now
		if entitlement.Source == "paid" && entitlement.ExpiresAt.After(now) {
			start = entitlement.ExpiresAt
			if entitlement.Plan == plan {
				kind = "renew"
			}
		}
		listPrice, ok := plans.Price(plan, period)
		if !ok {
			return CreateOrderResult{}, newError(CodeValidation, "套餐价格不存在")
		}
		amount = listPrice
		quote = OrderQuote{
			StartsAt:    plans.FormatTimestamp(start),
			EndsAt:      plans.FormatTimestamp(plans.TermEnd(start, period)),
			CurrentPlan: string(entitlement.Plan),
			TargetPlan:  string(plan),
		}
	}
	if amount <= 0 {
		return CreateOrderResult{}, newError(CodeValidation, "订单金额无效")
	}
	quoteJSON, err := json.Marshal(quote)
	if err != nil {
		return CreateOrderResult{}, newError(CodeUnavailable, "订单报价生成失败")
	}
	product := plans.ProductFor(plan)
	productJSON, err := json.Marshal(product)
	if err != nil {
		return CreateOrderResult{}, newError(CodeUnavailable, "订单快照生成失败")
	}
	input := state.CreateBillingOrderInput{
		UserID:             user.ID,
		Plan:               string(plan),
		BillingPeriod:      string(period),
		Kind:               kind,
		AmountCents:        amount,
		Quote:              string(quoteJSON),
		ProductSnapshot:    string(productJSON),
		EntitlementVersion: entitlement.EntitlementVersion,
		ExpiresAt:          now.Add(time.Duration(s.settings.TimeoutMinutes) * time.Minute),
		Now:                now,
	}
	created, err := s.stores.Billing.CreateOrder(ctx, input)
	if err != nil {
		return CreateOrderResult{}, mapBillingError(err)
	}
	order := created.Order
	view := s.orderView(order, user.Username)
	pay, payErr := s.pagePay(order)
	if payErr != nil {
		return CreateOrderResult{}, payErr
	}
	return CreateOrderResult{Order: view, Pay: pay}, nil
}

// pagePay builds the signed browser form. The gateway is the only destination
// the Console submits to.
func (s *Service) pagePay(order state.BillingOrder) (*PayView, error) {
	if s.payments == nil {
		return nil, newError(CodeUnavailable, "支付宝支付尚未配置")
	}
	product := plans.ProductFor(plans.Plan(order.Plan))
	period := plans.BillingPeriod(order.BillingPeriod)
	subject := "Praxis-RAG " + product.Name + " " + periodLabels[period]
	if order.Kind == "upgrade" {
		subject = "Praxis-RAG " + product.Name + " 升级差价"
	}
	page, err := s.payments.PagePay(order.ID, order.AmountCents, subject, "个人会员权益")
	if err != nil {
		return nil, newError(CodeUnavailable, "支付宝下单失败："+err.Error())
	}
	return &PayView{Gateway: page.GatewayURL, Params: page.Params}, nil
}

// Order returns one order owned by the caller. refresh runs an Alipay query and
// settles a paid order even when the browser/notify path was lost. A still
// pending order also returns fresh signed page-pay parameters so the Console can
// resume an interrupted payment without creating a second order.
func (s *Service) Order(ctx context.Context, userID, orderID string, refresh bool) (CreateOrderResult, error) {
	order, err := s.stores.Billing.GetOrder(ctx, orderID)
	if err != nil {
		return CreateOrderResult{}, newError(CodeUnavailable, "订单读取失败")
	}
	if order == nil || order.UserID != userID {
		return CreateOrderResult{}, newError(CodeNotFound, "订单不存在")
	}
	if refresh && order.Status == "pending" {
		if s.payments == nil {
			return CreateOrderResult{}, newError(CodeUnavailable, "支付宝支付尚未配置，无法查单")
		}
		trade, queryErr := s.payments.Query(ctx, order.ID)
		if queryErr != nil && !payment.IsTradeNotExist(queryErr) {
			return CreateOrderResult{}, newError(CodeUnavailable, "支付宝查单失败："+queryErr.Error())
		}
		if queryErr == nil {
			if trade.TradeStatus == "TRADE_SUCCESS" || trade.TradeStatus == "TRADE_FINISHED" {
				amount, amountErr := payment.ParseAmountToCents(trade.TotalAmount)
				if amountErr != nil {
					return CreateOrderResult{}, newError(CodeConflict, "支付宝返回金额无效，请转人工处理")
				}
				confirmed, _, confirmErr := s.stores.Billing.ConfirmPayment(ctx, order.ID,
					trade.TradeNo, amount, s.now())
				if confirmErr != nil {
					return CreateOrderResult{}, mapBillingError(confirmErr)
				}
				order = confirmed
			} else if trade.TradeStatus == "TRADE_CLOSED" {
				if closed, closeErr := s.stores.Billing.CloseOrder(ctx, order.ID, s.now()); closeErr == nil {
					order = closed
				}
			}
		}
	}
	result := CreateOrderResult{Order: s.orderView(*order, "")}
	if order.Status == "pending" && s.payments != nil {
		if pay, payErr := s.pagePay(*order); payErr == nil {
			result.Pay = pay
		}
	}
	return result, nil
}

// CloseOrder closes one pending local/Alipay order. A trade that no longer
// exists at Alipay is still closed locally.
func (s *Service) CloseOrder(ctx context.Context, userID, orderID string) (OrderView, error) {
	order, err := s.stores.Billing.GetOrder(ctx, orderID)
	if err != nil {
		return OrderView{}, newError(CodeUnavailable, "订单读取失败")
	}
	if order == nil || order.UserID != userID {
		return OrderView{}, newError(CodeNotFound, "订单不存在")
	}
	if order.Status == "paid" || order.Status == "refunded" {
		return OrderView{}, newError(CodeConflict, "已支付订单不能关闭")
	}
	if order.Status == "pending" && s.payments != nil {
		// Query before closing: a payer may have completed the trade while the
		// Console still shows "pending". Closing a paid trade is refused by
		// Alipay anyway; settling it here gives the correct state immediately.
		trade, queryErr := s.payments.Query(ctx, order.ID)
		if queryErr != nil && !payment.IsTradeNotExist(queryErr) {
			return OrderView{}, newError(CodeUnavailable, "支付宝查单失败："+queryErr.Error())
		}
		if queryErr == nil {
			if trade.TradeStatus == "TRADE_SUCCESS" || trade.TradeStatus == "TRADE_FINISHED" {
				amount, amountErr := payment.ParseAmountToCents(trade.TotalAmount)
				if amountErr != nil {
					return OrderView{}, newError(CodeConflict, "支付宝返回金额无效，请转人工处理")
				}
				paid, _, confirmErr := s.stores.Billing.ConfirmPayment(ctx, order.ID,
					trade.TradeNo, amount, s.now())
				if confirmErr != nil {
					return OrderView{}, mapBillingError(confirmErr)
				}
				return s.orderView(*paid, ""), nil
			}
			if trade.TradeStatus == "TRADE_CLOSED" {
				closed, closeErr := s.stores.Billing.CloseOrder(ctx, order.ID, s.now())
				if closeErr != nil {
					return OrderView{}, mapBillingError(closeErr)
				}
				return s.orderView(*closed, ""), nil
			}
		}
		if closeErr := s.payments.Close(ctx, order.ID); closeErr != nil &&
			!payment.IsTradeNotExist(closeErr) {
			return OrderView{}, newError(CodeUnavailable, "支付宝关单失败："+closeErr.Error())
		}
	}
	closed, err := s.stores.Billing.CloseOrder(ctx, order.ID, s.now())
	if err != nil {
		return OrderView{}, mapBillingError(err)
	}
	return s.orderView(*closed, ""), nil
}

// HandleNotify verifies and settles one asynchronous Alipay notification. The
// caller answers "success" only when this returns nil.
func (s *Service) HandleNotify(ctx context.Context, values url.Values) error {
	if s.payments == nil {
		return newError(CodeUnavailable, "支付宝支付尚未配置")
	}
	notify, err := s.payments.VerifyNotify(values)
	if err != nil {
		return newError(CodeValidation, "支付宝通知验签或商户校验失败")
	}
	order, err := s.stores.Billing.GetOrder(ctx, notify.OutTradeNo)
	if err != nil {
		return newError(CodeUnavailable, "订单读取失败")
	}
	if order == nil {
		return newError(CodeNotFound, "订单不存在")
	}
	amount, err := payment.ParseAmountToCents(notify.TotalAmount)
	if err != nil {
		return newError(CodeValidation, "支付宝通知金额无效")
	}
	if amount != order.AmountCents {
		// A signed notification with the wrong amount is still a real payment
		// trace; park it for manual reconciliation instead of dropping the
		// Alipay trade number.
		_, _ = s.stores.Billing.MarkManual(ctx, order.ID,
			"支付金额不一致：通知 "+notify.TotalAmount+"，订单 "+payment.FormatCents(order.AmountCents)+
				"；支付宝交易号："+notify.TradeNo, s.now())
		return newError(CodeValidation, "支付宝通知金额与订单不一致")
	}
	if _, _, err := s.stores.Billing.ConfirmPayment(ctx, order.ID, notify.TradeNo, amount, s.now()); err != nil {
		// A late payment or entitlement conflict must stay traceable for manual
		// reconciliation instead of being silently acknowledged.
		_, _ = s.stores.Billing.MarkManual(ctx, order.ID,
			"支付通知异常："+err.Error()+"；支付宝交易号："+notify.TradeNo, s.now())
		return mapBillingError(err)
	}
	return nil
}

// ---- Admin reconciliation -------------------------------------------------

// AdminListOrders returns all orders, optionally filtered by status.
func (s *Service) AdminListOrders(ctx context.Context, status string, limit int) ([]OrderView, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	orders, err := s.stores.Billing.ListAllOrders(ctx, strings.TrimSpace(status), limit)
	if err != nil {
		return nil, newError(CodeUnavailable, "订单读取失败")
	}
	usernames := map[string]string{}
	views := make([]OrderView, 0, len(orders))
	for _, order := range orders {
		username, ok := usernames[order.UserID]
		if !ok {
			if user, userErr := s.stores.Users.ByID(ctx, order.UserID); userErr == nil && user != nil {
				username = user.Username
			}
			usernames[order.UserID] = username
		}
		views = append(views, s.orderView(order, username))
	}
	return views, nil
}

// AdminQuery actively queries Alipay for one order and settles a successful
// payment exactly like a notification.
func (s *Service) AdminQuery(ctx context.Context, orderID string) (OrderView, error) {
	order, err := s.stores.Billing.GetOrder(ctx, orderID)
	if err != nil || order == nil {
		return OrderView{}, newError(CodeNotFound, "订单不存在")
	}
	if s.payments == nil {
		return OrderView{}, newError(CodeUnavailable, "支付宝支付尚未配置")
	}
	trade, err := s.payments.Query(ctx, order.ID)
	if err != nil {
		return OrderView{}, newError(CodeUnavailable, "支付宝查单失败："+err.Error())
	}
	if trade.TradeStatus == "TRADE_SUCCESS" || trade.TradeStatus == "TRADE_FINISHED" {
		amount, amountErr := payment.ParseAmountToCents(trade.TotalAmount)
		if amountErr != nil {
			return OrderView{}, newError(CodeConflict, "支付宝返回金额无效")
		}
		confirmed, _, confirmErr := s.stores.Billing.ConfirmPayment(ctx, order.ID,
			trade.TradeNo, amount, s.now())
		if confirmErr != nil {
			return OrderView{}, mapBillingError(confirmErr)
		}
		order = confirmed
	}
	return s.orderView(*order, ""), nil
}

// AdminRefund performs one whole-order Alipay refund. It uses a stable request
// number per order so a retry is idempotent at Alipay.
func (s *Service) AdminRefund(ctx context.Context, orderID, reason string) (OrderView, error) {
	order, err := s.stores.Billing.GetOrder(ctx, orderID)
	if err != nil || order == nil {
		return OrderView{}, newError(CodeNotFound, "订单不存在")
	}
	if s.payments == nil {
		return OrderView{}, newError(CodeUnavailable, "支付宝支付尚未配置")
	}
	if order.Status != "paid" && order.Status != "refunding" {
		return OrderView{}, newError(CodeConflict, "只有已支付订单可以退款")
	}
	requestNo := order.RefundRequestNo
	if requestNo == "" {
		generated, genErr := randomRefundNo()
		if genErr != nil {
			return OrderView{}, newError(CodeUnavailable, "退款请求号生成失败")
		}
		requestNo = generated
	}
	begin, err := s.stores.Billing.BeginRefund(ctx, order.ID, requestNo, order.AmountCents,
		strings.TrimSpace(reason), s.now())
	if err != nil {
		return OrderView{}, mapBillingError(err)
	}
	refund, err := s.payments.Refund(ctx, begin.ID, requestNo, begin.AmountCents, reason)
	if err != nil {
		return OrderView{}, newError(CodeUnavailable, "支付宝退款失败："+err.Error())
	}
	refunded, err := s.stores.Billing.CompleteRefund(ctx, begin.ID, requestNo, refund.TradeNo, s.now())
	if err != nil {
		return OrderView{}, mapBillingError(err)
	}
	return s.orderView(*refunded, ""), nil
}

// AdminManual marks one order for manual reconciliation with an audit note.
func (s *Service) AdminManual(ctx context.Context, orderID, note string) (OrderView, error) {
	order, err := s.stores.Billing.MarkManual(ctx, orderID, strings.TrimSpace(note), s.now())
	if err != nil {
		return OrderView{}, mapBillingError(err)
	}
	return s.orderView(*order, ""), nil
}

// ---- Workspace entitlement ------------------------------------------------

// WorkspaceFilesView is the file allowance visible to every collaborator.
type WorkspaceFilesView struct {
	Used      int     `json:"used"`
	Limit     int     `json:"limit"`
	Unlimited bool    `json:"unlimited"`
	CanAdd    bool    `json:"can_add"`
	Reason    *string `json:"reason"`
}

// WorkspaceEmbeddingView hides balances from non-owners.
type WorkspaceEmbeddingView struct {
	CanUse    bool   `json:"can_use"`
	OwnerView bool   `json:"owner_view"`
	RefreshAt string `json:"refresh_at"`
	Limit     *int64 `json:"limit,omitempty"`
	Used      *int64 `json:"used,omitempty"`
	Reserved  *int64 `json:"reserved,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
}

// WorkspaceEntitlementView is GET /v1/workspaces/{namespace}/entitlements.
type WorkspaceEntitlementView struct {
	Namespace     string                 `json:"namespace"`
	Owner         bool                   `json:"owner"`
	Shared        bool                   `json:"shared"`
	OwnerUsername *string                `json:"owner_username"`
	Files         WorkspaceFilesView     `json:"files"`
	Embedding     WorkspaceEmbeddingView `json:"embedding"`
}

// WorkspaceEntitlement reads the owner's limits for one knowledge base. The
// caller must have read access; detailed embedding balances are owner-only.
func (s *Service) WorkspaceEntitlement(ctx context.Context, userID, namespace string) (WorkspaceEntitlementView, error) {
	now := s.now()
	usage, err := s.stores.Membership.FileUsage(ctx, namespace, now)
	if err != nil {
		return WorkspaceEntitlementView{}, newError(CodeUnavailable, "知识库配额读取失败")
	}
	files := WorkspaceFilesView{
		Used: usage.Used, Limit: usage.Limit, Unlimited: usage.Unlimited, CanAdd: usage.CanAdd,
	}
	if usage.Reason != "" {
		files.Reason = &usage.Reason
	}
	owner := usage.OwnerUserID != "" && usage.OwnerUserID == userID
	view := WorkspaceEntitlementView{
		Namespace: namespace,
		Owner:     owner,
		Shared:    !owner,
		Files:     files,
		Embedding: WorkspaceEmbeddingView{CanUse: true, OwnerView: owner},
	}
	if usage.OwnerUserID != "" {
		if ownerUser, lookupErr := s.stores.Users.ByID(ctx, usage.OwnerUserID); lookupErr == nil && ownerUser != nil {
			username := ownerUser.Username
			view.OwnerUsername = &username
		}
	}
	if usage.OwnerUserID == "" {
		// System/unowned knowledge bases keep the administrative path.
		view.Shared = false
		view.Embedding.OwnerView = true
		return view, nil
	}
	quota, err := s.stores.Membership.QuotaSummary(ctx, usage.OwnerUserID, now)
	if err != nil {
		return WorkspaceEntitlementView{}, newError(CodeUnavailable, "Embedding 额度读取失败")
	}
	view.Embedding.RefreshAt = plans.FormatTimestamp(quota.PeriodEnd)
	remaining := quota.AllowanceChars - quota.UsedChars - quota.ReservedChars
	if remaining < 0 {
		remaining = 0
	}
	view.Embedding.CanUse = remaining > 0
	if owner {
		limit, used, reserved, left := quota.AllowanceChars, quota.UsedChars, quota.ReservedChars, remaining
		view.Embedding.Limit = &limit
		view.Embedding.Used = &used
		view.Embedding.Reserved = &reserved
		view.Embedding.Remaining = &left
	} else if !view.Embedding.CanUse {
		reason := "所有者的文档向量化额度已用尽，暂时无法处理新的向量化"
		files.CanAdd = false
		files.Reason = &reason
		view.Files = files
	}
	return view, nil
}

func (s *Service) orderView(order state.BillingOrder, username string) OrderView {
	period := plans.BillingPeriod(order.BillingPeriod)
	periodLabel := periodLabels[period]
	if periodLabel == "" {
		periodLabel = string(period)
	}
	view := OrderView{
		ID:            order.ID,
		Status:        order.Status,
		StatusLabel:   orderStatusLabels[order.Status],
		Plan:          order.Plan,
		PlanName:      plans.ProductFor(plans.Plan(order.Plan)).Name,
		Period:        order.BillingPeriod,
		PeriodLabel:   periodLabel,
		Kind:          order.Kind,
		KindLabel:     kindLabels[order.Kind],
		AmountCents:   order.AmountCents,
		AmountYuan:    payment.FormatCents(order.AmountCents),
		Currency:      order.Currency,
		CreatedAt:     order.CreatedAt,
		ExpiresAt:     order.ExpiresAt,
		RefundStatus:  stringPointer(order.RefundStatus),
		AlipayTradeNo: stringPointer(order.AlipayTradeNo),
		Username:      username,
	}
	if view.StatusLabel == "" {
		view.StatusLabel = order.Status
	}
	if view.KindLabel == "" {
		view.KindLabel = order.Kind
	}
	if order.PaidAt != "" {
		value := order.PaidAt
		view.PaidAt = &value
	}
	if order.RefundAmountCents > 0 {
		value := payment.FormatCents(order.RefundAmountCents)
		view.RefundAmountYuan = &value
	}
	quote := OrderQuote{}
	if order.Quote != "" {
		_ = json.Unmarshal([]byte(order.Quote), &quote)
	}
	view.Quote = quote
	return view
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func randomRefundNo() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "RF" + strings.ToUpper(hex.EncodeToString(buffer)), nil
}

// mapBillingError translates state billing failures into safe API categories.
func mapBillingError(err error) error {
	var payable *state.PayableOrderExistsError
	if errors.As(err, &payable) {
		return newError(CodeConflict, payable.Message)
	}
	var entitlementChanged *state.EntitlementChangedError
	if errors.As(err, &entitlementChanged) {
		return newError(CodeConflict, "会员权益已变化，请刷新后重新报价并下单")
	}
	var billingConflict *state.BillingConflictError
	if errors.As(err, &billingConflict) {
		return newError(CodeConflict, billingConflict.Message)
	}
	var lookup *state.LookupError
	if errors.As(err, &lookup) {
		return newError(CodeNotFound, lookup.Message)
	}
	var validation *state.ValueError
	if errors.As(err, &validation) {
		return newError(CodeValidation, validation.Message)
	}
	return newError(CodeUnavailable, "订单操作失败，请稍后重试")
}
