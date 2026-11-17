package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/membership"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// fakeMembership implements MembershipRuntime for transport-only tests.
type fakeMembership struct {
	notifyValues url.Values
	notifyErr    error
}

func (f *fakeMembership) Snapshot(context.Context, string) (membership.Snapshot, error) {
	return membership.Snapshot{}, nil
}
func (f *fakeMembership) CreateOrder(context.Context, state.User, membership.CreateOrderRequest) (membership.CreateOrderResult, error) {
	return membership.CreateOrderResult{}, nil
}
func (f *fakeMembership) ListOrders(context.Context, string, int) ([]membership.OrderView, error) {
	return nil, nil
}
func (f *fakeMembership) Order(context.Context, string, string, bool) (membership.CreateOrderResult, error) {
	return membership.CreateOrderResult{}, nil
}
func (f *fakeMembership) CloseOrder(context.Context, string, string) (membership.OrderView, error) {
	return membership.OrderView{}, nil
}
func (f *fakeMembership) WorkspaceEntitlement(context.Context, string, string) (membership.WorkspaceEntitlementView, error) {
	return membership.WorkspaceEntitlementView{}, nil
}
func (f *fakeMembership) AdminListOrders(context.Context, string, int) ([]membership.OrderView, error) {
	return nil, nil
}
func (f *fakeMembership) AdminQuery(context.Context, string) (membership.OrderView, error) {
	return membership.OrderView{}, nil
}
func (f *fakeMembership) AdminRefund(context.Context, string, string) (membership.OrderView, error) {
	return membership.OrderView{}, nil
}
func (f *fakeMembership) AdminManual(context.Context, string, string) (membership.OrderView, error) {
	return membership.OrderView{}, nil
}
func (f *fakeMembership) HandleNotify(_ context.Context, values url.Values) error {
	f.notifyValues = values
	return f.notifyErr
}

// TestAlipayNotifyTransportNeedsNoSessionButAcksOnlyVerifiedCallbacks pins the
// callback contract: no cookie/CSRF, form parsing, plain "success" only after
// the service accepted the signature/merchant/amount checks.
func TestAlipayNotifyTransportNeedsNoSessionButAcksOnlyVerifiedCallbacks(t *testing.T) {
	fake := &fakeMembership{}
	server := New(config.ServiceSettings{}, nil, nil, nil, WithMembership(fake))
	form := url.Values{
		"out_trade_no": {"PR-1"}, "trade_no": {"2026"}, "trade_status": {"TRADE_SUCCESS"},
		"total_amount": {"19.90"}, "sign": {"test"},
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/payments/alipay/notify",
		strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "success" {
		t.Fatalf("notify response = %d %q", recorder.Code, recorder.Body.String())
	}
	if fake.notifyValues.Get("out_trade_no") != "PR-1" {
		t.Fatalf("service did not receive the form values: %v", fake.notifyValues)
	}

	fake.notifyErr = &membership.Error{Code: membership.CodeValidation, Message: "bad signature"}
	request = httptest.NewRequest(http.MethodPost, "/v1/payments/alipay/notify",
		strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if strings.Contains(recorder.Body.String(), "success") {
		t.Fatalf("rejected notify must not be acknowledged: %q", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "test") {
		t.Fatalf("response leaked notification signature: %q", recorder.Body.String())
	}
}

func TestMembershipRoutesRequireSession(t *testing.T) {
	server := New(config.ServiceSettings{}, nil, nil, nil,
		WithMembership(&fakeMembership{}))
	request := httptest.NewRequest(http.MethodGet, "/v1/membership", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/membership without login = %d, want 401", recorder.Code)
	}
}

func TestWriteMembershipErrorMapsSafeStatuses(t *testing.T) {
	tests := []struct {
		code   string
		status int
	}{
		{membership.CodeValidation, http.StatusUnprocessableEntity},
		{membership.CodeForbidden, http.StatusForbidden},
		{membership.CodeNotFound, http.StatusNotFound},
		{membership.CodeConflict, http.StatusConflict},
		{membership.CodeUnavailable, http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		recorder := httptest.NewRecorder()
		writeMembershipError(recorder, &membership.Error{Code: test.code, Message: "安全错误"})
		if recorder.Code != test.status {
			t.Errorf("%s mapped to %d, want %d", test.code, recorder.Code, test.status)
		}
		if !strings.Contains(recorder.Body.String(), "安全错误") {
			t.Errorf("%s body = %s", test.code, recorder.Body.String())
		}
	}
}
