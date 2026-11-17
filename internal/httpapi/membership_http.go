package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/membership"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// MembershipRuntime is the Console membership/order surface. The concrete
// implementation is internal/membership.Service; tests inject a fake.
type MembershipRuntime interface {
	Snapshot(ctx context.Context, userID string) (membership.Snapshot, error)
	CreateOrder(ctx context.Context, user state.User, request membership.CreateOrderRequest) (membership.CreateOrderResult, error)
	ListOrders(ctx context.Context, userID string, limit int) ([]membership.OrderView, error)
	Order(ctx context.Context, userID, orderID string, refresh bool) (membership.CreateOrderResult, error)
	CloseOrder(ctx context.Context, userID, orderID string) (membership.OrderView, error)
	WorkspaceEntitlement(ctx context.Context, userID, namespace string) (membership.WorkspaceEntitlementView, error)
	AdminListOrders(ctx context.Context, status string, limit int) ([]membership.OrderView, error)
	AdminQuery(ctx context.Context, orderID string) (membership.OrderView, error)
	AdminRefund(ctx context.Context, orderID, reason string) (membership.OrderView, error)
	AdminManual(ctx context.Context, orderID, note string) (membership.OrderView, error)
	HandleNotify(ctx context.Context, values url.Values) error
}

// WithMembership wires the personal membership and payment handlers.
func WithMembership(runtime MembershipRuntime) Option {
	return func(s *Server) { s.membership = runtime }
}

// registerMembershipRoutes mounts the member-facing and administrator billing
// surface plus the unauthenticated Alipay notification callback.
func (s *Server) registerMembershipRoutes() {
	s.router.add("GET", "/v1/membership", authSession, s.handleMembershipSnapshot)
	s.router.add("GET", "/v1/membership/orders", authSession, s.handleMembershipOrderList)
	s.router.add("POST", "/v1/membership/orders", authSession, s.handleCreateMembershipOrder)
	s.router.add("GET", "/v1/membership/orders/{order_id}", authSession, s.handleMembershipOrder)
	s.router.add("POST", "/v1/membership/orders/{order_id}/close", authSession, s.handleCloseMembershipOrder)
	s.router.add("GET", "/v1/workspaces/{namespace}/entitlements", authSession, s.handleWorkspaceEntitlement)
	s.router.add("GET", "/v1/admin/billing/orders", authAdmin, s.handleAdminBillingOrders)
	s.router.add("POST", "/v1/admin/billing/orders/{order_id}/query", authAdmin, s.handleAdminBillingQuery)
	s.router.add("POST", "/v1/admin/billing/orders/{order_id}/refund", authAdmin, s.handleAdminBillingRefund)
	s.router.add("POST", "/v1/admin/billing/orders/{order_id}/manual", authAdmin, s.handleAdminBillingManual)
	// Alipay calls this URL server-to-server. It never accepts a user cookie or
	// CSRF token; authenticity comes from the RSA2 signature and merchant
	// identity checks in the payment client.
	s.router.add("POST", "/v1/payments/alipay/notify", authNone, s.handleAlipayNotify)
}

func (s *Server) requireMembership(w http.ResponseWriter) bool {
	if s.membership != nil {
		return true
	}
	writeDetail(w, http.StatusServiceUnavailable, "会员服务未启用")
	return false
}

func (s *Server) handleMembershipSnapshot(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok || !s.requireMembership(w) {
		return
	}
	payload, err := s.membership.Snapshot(r.Context(), session.User.ID)
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleMembershipOrderList(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok || !s.requireMembership(w) {
		return
	}
	orders, err := s.membership.ListOrders(r.Context(), session.User.ID, 100)
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders})
}

type createMembershipOrderBody struct {
	Plan   string `json:"plan"`
	Period string `json:"period"`
	Intent string `json:"intent"`
}

func (s *Server) handleCreateMembershipOrder(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok || !s.requireMembership(w) {
		return
	}
	var body createMembershipOrderBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	result, err := s.membership.CreateOrder(r.Context(), session.User, membership.CreateOrderRequest{
		Plan: body.Plan, Period: body.Period, Intent: body.Intent,
	})
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleMembershipOrder(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok || !s.requireMembership(w) {
		return
	}
	refresh := strings.TrimSpace(r.URL.Query().Get("refresh")) == "1"
	result, err := s.membership.Order(r.Context(), session.User.ID, pathParam(r, "order_id"), refresh)
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCloseMembershipOrder(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok || !s.requireMembership(w) {
		return
	}
	order, err := s.membership.CloseOrder(r.Context(), session.User.ID, pathParam(r, "order_id"))
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

func (s *Server) handleWorkspaceEntitlement(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok || !s.requireMembership(w) {
		return
	}
	namespace := pathParam(r, "namespace")
	if !namespacePattern.MatchString(namespace) {
		writeDetail(w, http.StatusBadRequest, "Invalid RAG namespace.")
		return
	}
	if s.access == nil {
		writeDetail(w, http.StatusServiceUnavailable, "知识库权限服务未配置")
		return
	}
	scope, err := s.access.ScopeSet(r.Context(), session.User, namespace)
	if err != nil {
		writeDetail(w, http.StatusInternalServerError, "知识库权限服务异常")
		return
	}
	if !scope.AllowsAnyRead() {
		writeDetail(w, http.StatusForbidden, "无权访问该知识库")
		return
	}
	payload, err := s.membership.WorkspaceEntitlement(r.Context(), session.User.ID, namespace)
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleAdminBillingOrders(w http.ResponseWriter, r *http.Request) {
	if !s.requireMembership(w) {
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 200 {
			limit = parsed
		}
	}
	orders, err := s.membership.AdminListOrders(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders})
}

func (s *Server) handleAdminBillingQuery(w http.ResponseWriter, r *http.Request) {
	if !s.requireMembership(w) {
		return
	}
	order, err := s.membership.AdminQuery(r.Context(), pathParam(r, "order_id"))
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

type billingReasonBody struct {
	Reason string `json:"reason"`
	Note   string `json:"note"`
}

func (s *Server) handleAdminBillingRefund(w http.ResponseWriter, r *http.Request) {
	if !s.requireMembership(w) {
		return
	}
	var body billingReasonBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	order, err := s.membership.AdminRefund(r.Context(), pathParam(r, "order_id"), body.Reason)
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

func (s *Server) handleAdminBillingManual(w http.ResponseWriter, r *http.Request) {
	if !s.requireMembership(w) {
		return
	}
	var body billingReasonBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	order, err := s.membership.AdminManual(r.Context(), pathParam(r, "order_id"), body.Note)
	if err != nil {
		writeMembershipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"order": order})
}

func (s *Server) handleAlipayNotify(w http.ResponseWriter, r *http.Request) {
	if s.membership == nil {
		http.Error(w, "failure", http.StatusServiceUnavailable)
		return
	}
	// Alipay posts application/x-www-form-urlencoded. Bound the body before
	// ParseForm so an oversized callback cannot exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "failure", http.StatusBadRequest)
		return
	}
	if err := s.membership.HandleNotify(r.Context(), r.PostForm); err != nil {
		// Never echo the notification body, signature or key material.
		s.logger.Warn("Alipay notification rejected", "error_detail", membership.ErrorMessage(err))
		http.Error(w, "failure", http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("success"))
}

// writeMembershipError maps service categories to the REST error envelope.
func writeMembershipError(w http.ResponseWriter, err error) {
	var typed *membership.Error
	if !errors.As(err, &typed) {
		writeDetail(w, http.StatusInternalServerError, "会员操作失败，请稍后重试")
		return
	}
	switch typed.Code {
	case membership.CodeValidation:
		writeDetail(w, http.StatusUnprocessableEntity, typed.Message)
	case membership.CodeForbidden:
		writeDetail(w, http.StatusForbidden, typed.Message)
	case membership.CodeNotFound:
		writeDetail(w, http.StatusNotFound, typed.Message)
	case membership.CodeConflict:
		writeDetail(w, http.StatusConflict, typed.Message)
	default:
		writeDetail(w, http.StatusServiceUnavailable, typed.Message)
	}
}
