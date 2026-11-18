package membership

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/payment"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// openMembershipTestService opens the disposable MySQL schema and returns a
// service wired to a local RSA test merchant. It skips (instead of failing)
// when the developer did not export RAG_TEST_MYSQL_URL.
func openMembershipTestService(t *testing.T) (*Service, *state.Stores, *rsa.PrivateKey) {
	t.Helper()
	url, _ := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: statetest.TestEncryptionKey,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey() error = %v", err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey() error = %v", err)
	}
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	settings := payment.Settings{
		AppID:           "2026000000000001",
		PrivateKeyPEM:   privatePEM,
		PublicKeyPEM:    publicPEM,
		Gateway:         "https://openapi.alipay.test/gateway.do",
		NotifyURL:       "https://rag.test/v1/payments/alipay/notify",
		ReturnURL:       "https://rag.test/console#/membership",
		SellerID:        "2088000000000001",
		TimeoutMinutes:  30,
		PurchaseEnabled: true,
		Environment:     payment.EnvironmentSandbox,
	}
	service, err := New(stores, settings)
	if err != nil {
		t.Fatalf("membership.New() error = %v", err)
	}
	service.SetClock(func() time.Time { return time.Date(2026, 1, 31, 10, 30, 0, 0, time.UTC) })
	return service, stores, key
}

func signedNotify(t *testing.T, key *rsa.PrivateKey, values map[string]string) url.Values {
	t.Helper()
	form := url.Values{}
	for name, value := range values {
		form.Set(name, value)
	}
	flat := map[string]string{}
	for name, list := range form {
		flat[name] = list[0]
	}
	signature, err := payment.SignValues(flat, string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})))
	if err != nil {
		t.Fatalf("SignValues() error = %v", err)
	}
	form.Set("sign", signature)
	form.Set("sign_type", "RSA2")
	return form
}

func notifyValues(order state.BillingOrder, tradeNo string) map[string]string {
	return map[string]string{
		"app_id":       "2026000000000001",
		"seller_id":    "2088000000000001",
		"out_trade_no": order.ID,
		"trade_no":     tradeNo,
		"trade_status": "TRADE_SUCCESS",
		"total_amount": payment.FormatCents(order.AmountCents),
		"notify_id":    "notify-" + order.ID,
	}
}

func TestMembershipOrderLifecycleOverMySQL(t *testing.T) {
	if strings.TrimSpace(os.Getenv("RAG_TEST_MYSQL_URL")) == "" {
		t.Skip("RAG_TEST_MYSQL_URL is not set")
	}
	service, stores, key := openMembershipTestService(t)
	ctx := context.Background()
	user, err := stores.Users.Create(ctx, "membership-buyer", "hash")
	if err != nil {
		t.Fatalf("Users.Create() error = %v", err)
	}

	snapshot, err := service.Snapshot(ctx, user.ID)
	if err != nil {
		t.Fatalf("Snapshot(free) error = %v", err)
	}
	if snapshot.Membership.Plan != "free" || snapshot.Membership.Purchase.Available != true {
		t.Fatalf("initial snapshot = %+v", snapshot.Membership)
	}
	if len(snapshot.Plans) != 3 || snapshot.Plans[1].YearlyYuan != "191.04" {
		t.Fatalf("plan catalog = %+v", snapshot.Plans)
	}

	created, err := service.CreateOrder(ctx, user, CreateOrderRequest{Plan: "plus", Period: "month"})
	if err != nil {
		t.Fatalf("CreateOrder(plus/month) error = %v", err)
	}
	if created.Order.AmountCents != 1_990 || created.Order.Kind != "purchase" || created.Pay == nil {
		t.Fatalf("created order = %+v pay=%v", created.Order, created.Pay)
	}
	if created.Pay.Params["method"] != "alipay.trade.page.pay" ||
		!strings.Contains(created.Pay.Params["biz_content"], `"total_amount":"19.90"`) ||
		created.Pay.Params["sign"] == "" {
		t.Fatalf("pay params = %+v", created.Pay.Params)
	}
	// A second click with the same intent reuses the same payable order.
	reused, err := service.CreateOrder(ctx, user, CreateOrderRequest{Plan: "plus", Period: "month"})
	if err != nil {
		t.Fatalf("CreateOrder(replay) error = %v", err)
	}
	if reused.Order.ID != created.Order.ID {
		t.Fatalf("duplicate click created %s instead of %s", reused.Order.ID, created.Order.ID)
	}
	pendingResult, err := service.Order(ctx, user.ID, created.Order.ID, false)
	if err != nil || pendingResult.Pay == nil || pendingResult.Pay.Gateway == "" {
		t.Fatalf("pending order resume = %+v, %v", pendingResult, err)
	}

	orderRecord, err := stores.Billing.GetOrder(ctx, created.Order.ID)
	if err != nil || orderRecord == nil {
		t.Fatalf("GetOrder() = %+v, %v", orderRecord, err)
	}
	notify := signedNotify(t, key, notifyValues(*orderRecord, "2026013122001000000001"))
	if err := service.HandleNotify(ctx, notify); err != nil {
		t.Fatalf("HandleNotify() error = %v", err)
	}
	// A duplicate delivery must be idempotent.
	if err := service.HandleNotify(ctx, notify); err != nil {
		t.Fatalf("HandleNotify(duplicate) error = %v", err)
	}
	paidResult, err := service.Order(ctx, user.ID, created.Order.ID, false)
	if err != nil || paidResult.Order.Status != "paid" || paidResult.Order.PaidAt == nil {
		t.Fatalf("paid order = %+v, %v", paidResult, err)
	}
	snapshot, err = service.Snapshot(ctx, user.ID)
	if err != nil {
		t.Fatalf("Snapshot(plus) error = %v", err)
	}
	if snapshot.Membership.Plan != "plus" || snapshot.Membership.ExpiresAt == nil {
		t.Fatalf("plus membership = %+v", snapshot.Membership)
	}
	if snapshot.Membership.UpgradePlan == nil || *snapshot.Membership.UpgradePlan != "pro" {
		t.Fatalf("upgrade target = %v", snapshot.Membership.UpgradePlan)
	}
	plusExpiry := *snapshot.Membership.ExpiresAt

	upgrade, err := service.CreateOrder(ctx, user, CreateOrderRequest{Plan: "pro", Intent: "upgrade"})
	if err != nil {
		t.Fatalf("CreateOrder(upgrade) error = %v", err)
	}
	if upgrade.Order.Kind != "upgrade" || upgrade.Order.AmountCents <= 0 {
		t.Fatalf("upgrade order = %+v", upgrade.Order)
	}
	upgradeRecord, err := stores.Billing.GetOrder(ctx, upgrade.Order.ID)
	if err != nil || upgradeRecord == nil {
		t.Fatalf("GetOrder(upgrade) = %+v, %v", upgradeRecord, err)
	}
	if err := service.HandleNotify(ctx, signedNotify(t, key, notifyValues(*upgradeRecord, "2026013122001000000002"))); err != nil {
		t.Fatalf("HandleNotify(upgrade) error = %v", err)
	}
	snapshot, err = service.Snapshot(ctx, user.ID)
	if err != nil {
		t.Fatalf("Snapshot(pro) error = %v", err)
	}
	if snapshot.Membership.Plan != "pro" || snapshot.Membership.ExpiresAt == nil || *snapshot.Membership.ExpiresAt != plusExpiry {
		t.Fatalf("upgrade changed plan/expiry: %+v (want expiry %s)", snapshot.Membership, plusExpiry)
	}

	// A wrong amount must never grant a second order.
	foreign, err := stores.Users.Create(ctx, "membership-wrong-amount", "hash")
	if err != nil {
		t.Fatalf("Users.Create(foreign) error = %v", err)
	}
	wrong, err := service.CreateOrder(ctx, foreign, CreateOrderRequest{Plan: "plus", Period: "month"})
	if err != nil {
		t.Fatalf("CreateOrder(wrong amount) error = %v", err)
	}
	wrongRecord, _ := stores.Billing.GetOrder(ctx, wrong.Order.ID)
	badValues := notifyValues(*wrongRecord, "2026013122001000000003")
	badValues["total_amount"] = "0.01"
	if err := service.HandleNotify(ctx, signedNotify(t, key, badValues)); err == nil {
		t.Fatal("wrong amount notification was accepted")
	}
	wrongAfter, _ := stores.Billing.GetOrder(ctx, wrong.Order.ID)
	if wrongAfter.Status != "manual" || wrongAfter.ManualNote == "" ||
		!strings.Contains(wrongAfter.ManualNote, "2026013122001000000003") {
		t.Fatalf("wrong amount order = %+v, want manual with trade trace", wrongAfter)
	}

	// Ownership: another account cannot read someone else's order.
	if _, err := service.Order(ctx, foreign.ID, created.Order.ID, false); err == nil {
		t.Fatal("another account read the buyer's order")
	}
}
