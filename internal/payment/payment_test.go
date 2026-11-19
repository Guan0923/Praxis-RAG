package payment

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test fixtures. The AppID, seller PID and order numbers below are fake values
// that only exist to make the signed payloads realistic.
const (
	testAppID     = "2021000000000001"
	testSellerID  = "2088000000000001"
	testNotifyURL = "https://rag.example.com/v1/membership/alipay/notify"
	testReturnURL = "https://rag.example.com/console/membership/result"
	testGateway   = "https://openapi.alipay.com/gateway.do"
	testOrderID   = "ORDER-20261117-0001"
	testTradeNo   = "2026111722001400000000000001"
)

var (
	testKeyOnce  sync.Once
	testKeyPairV *rsa.PrivateKey
	testPrivPEM  string
	testPubPEM   string
)

// testKeys generates one RSA-2048 key pair and caches its PEM encodings.
func testKeys(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("rsa.GenerateKey: %v", err)
		}
		pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("x509.MarshalPKCS8PrivateKey: %v", err)
		}
		testKeyPairV = key
		testPrivPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
		testPubPEM = publicKeyPEM(t, key)
	})
	return testKeyPairV, testPrivPEM, testPubPEM
}

// publicKeyPEM encodes the public half of key as PKIX PEM.
func publicKeyPEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("x509.MarshalPKIXPublicKey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// testCertificatePEM builds a self-signed certificate that carries key's public
// key, mirroring how the Alipay public key can be configured as a certificate.
func testCertificatePEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(20261117),
		Subject:      pkix.Name{CommonName: "alipay-openapi-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// testSettings returns one complete Alipay configuration for the given gateway.
func testSettings(t *testing.T, gateway string) Settings {
	t.Helper()
	_, privatePEM, publicPEM := testKeys(t)
	return Settings{
		AppID:           testAppID,
		PrivateKeyPEM:   privatePEM,
		PublicKeyPEM:    publicPEM,
		Gateway:         gateway,
		NotifyURL:       testNotifyURL,
		ReturnURL:       testReturnURL,
		SellerID:        testSellerID,
		TimeoutMinutes:  30,
		PurchaseEnabled: true,
		Environment:     EnvironmentProduction,
	}
}

// newTestClient builds a Client for the given gateway URL.
func newTestClient(t *testing.T, gateway string) *Client {
	t.Helper()
	client, err := NewClient(testSettings(t, gateway))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

// copyValues returns a shallow copy of one parameter map.
func copyValues(values map[string]string) map[string]string {
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

// signTestPayload signs raw bytes exactly like the package does, so a gateway
// stub can produce a valid RSA2 response.
func signTestPayload(t *testing.T, key *rsa.PrivateKey, content string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Errorf("rsa.SignPKCS1v15: %v", err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(signature)
}

func TestSignValuesRoundTrip(t *testing.T) {
	_, privatePEM, publicPEM := testKeys(t)
	values := map[string]string{
		"app_id":      testAppID,
		"method":      methodPagePay,
		"sign_type":   SignTypeRSA2,
		"timestamp":   "2026-11-17 20:00:00",
		"biz_content": `{"out_trade_no":"ORDER-1","total_amount":"19.90"}`,
		"notify_url":  testNotifyURL,
	}
	signature, err := SignValues(values, privatePEM)
	if err != nil {
		t.Fatalf("SignValues() error = %v", err)
	}
	if signature == "" {
		t.Fatal("SignValues() returned an empty signature")
	}

	signed := copyValues(values)
	signed["sign"] = signature
	if err := VerifyValues(signed, publicPEM); err != nil {
		t.Fatalf("VerifyValues() error = %v", err)
	}

	tampered := copyValues(signed)
	tampered["biz_content"] = strings.Replace(tampered["biz_content"], "19.90", "0.01", 1)
	if err := VerifyValues(tampered, publicPEM); err == nil || !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("VerifyValues(tampered) error = %v, want ErrInvalidSignature", err)
	}

	// sign and sign_type are excluded, empty values are excluded and RSA
	// PKCS#1 v1.5 signatures are deterministic.
	withExtras := copyValues(values)
	withExtras["sign"] = "ignored"
	withExtras["sign_type"] = "RSA"
	withExtras["body"] = ""
	withExtras["return_url"] = ""
	second, err := SignValues(withExtras, privatePEM)
	if err != nil {
		t.Fatalf("SignValues(with extras) error = %v", err)
	}
	if second != signature {
		t.Fatalf("signature changed when empty/sign/sign_type values were added")
	}

	if got := signContent(map[string]string{"b": "2", "a": "1", "sign": "x", "sign_type": "RSA2", "c": ""}); got != "a=1&b=2" {
		t.Fatalf("signContent() = %q, want %q", got, "a=1&b=2")
	}

	form := signedForm(values, signature)
	if err := VerifyFormValues(form, publicPEM); err != nil {
		t.Fatalf("VerifyFormValues() error = %v", err)
	}

	if err := VerifyValues(signed, ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("VerifyValues(without public key) error = %v, want ErrInvalidKey", err)
	}
	if _, err := SignValues(values, ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("SignValues(without private key) error = %v, want ErrInvalidKey", err)
	}
}

// signedForm converts one parameter map plus its signature into url.Values.
func signedForm(values map[string]string, signature string) url.Values {
	form := make(url.Values, len(values)+1)
	for key, value := range values {
		form.Set(key, value)
	}
	form.Set("sign", signature)
	return form
}

func TestSignValuesAcceptsKeyFormats(t *testing.T) {
	key, privatePEM, publicPEM := testKeys(t)
	content := map[string]string{
		"method":      methodQuery,
		"biz_content": `{"out_trade_no":"ORDER-1"}`,
	}
	want, err := SignValues(content, privatePEM)
	if err != nil {
		t.Fatalf("SignValues() error = %v", err)
	}
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("x509.MarshalPKCS8PrivateKey: %v", err)
	}
	pkcs1PEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	pkcs8BareDER := base64.StdEncoding.EncodeToString(pkcs8DER)
	pkixDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("x509.MarshalPKIXPublicKey: %v", err)
	}
	publicKeys := map[string]string{
		"pkix pem":    publicPEM,
		"certificate": testCertificatePEM(t, key),
		"base64 der":  base64.StdEncoding.EncodeToString(pkixDER),
	}
	privateKeys := map[string]string{
		"pkcs8 pem":  privatePEM,
		"pkcs1 pem":  pkcs1PEM,
		"base64 der": pkcs8BareDER,
	}
	for name, material := range privateKeys {
		t.Run(name, func(t *testing.T) {
			signature, err := SignValues(content, material)
			if err != nil {
				t.Fatalf("SignValues(%s) error = %v", name, err)
			}
			if signature != want {
				t.Fatalf("SignValues(%s) signature differs from the PKCS#8 PEM signature", name)
			}
			signed := copyValues(content)
			signed["sign"] = signature
			for publicName, publicMaterial := range publicKeys {
				if err := VerifyValues(signed, publicMaterial); err != nil {
					t.Fatalf("VerifyValues(%s) error = %v", publicName, err)
				}
			}
		})
	}

	// A bare base64 DER private key may use the PKCS#1 encoding too.
	pkcs1BareDER := base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(key))
	if signature, err := SignValues(content, pkcs1BareDER); err != nil || signature != want {
		t.Fatalf("SignValues(pkcs1 base64 der) = %q, %v", signature, err)
	}
}

func TestParseAmountToCents(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{name: "two decimals", input: "19.90", want: 1990},
		{name: "integer", input: "20", want: 2000},
		{name: "zero", input: "0", want: 0},
		{name: "zero with decimals", input: "0.00", want: 0},
		{name: "one decimal", input: "19.9", want: 1990},
		{name: "smallest cent", input: "0.01", want: 1},
		{name: "surrounding spaces", input: " 19.90 ", want: 1990},
		{name: "leading zeros", input: "007", want: 700},
		{name: "three decimals", input: "19.999", wantErr: true},
		{name: "negative", input: "-1", wantErr: true},
		{name: "negative decimal", input: "-0.01", wantErr: true},
		{name: "not a number", input: "abc", wantErr: true},
		{name: "empty", input: "", wantErr: true},
		{name: "blank", input: "   ", wantErr: true},
		{name: "missing integer part", input: ".90", wantErr: true},
		{name: "missing fraction part", input: "19.", wantErr: true},
		{name: "thousands separator", input: "1,000.00", wantErr: true},
		{name: "space inside", input: "1 9.90", wantErr: true},
		{name: "scientific notation", input: "1e3", wantErr: true},
		{name: "overflow", input: "92233720368547759", wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ParseAmountToCents(testCase.input)
			if testCase.wantErr {
				if err == nil || !errors.Is(err, ErrInvalidAmount) {
					t.Fatalf("ParseAmountToCents(%q) error = %v, want ErrInvalidAmount", testCase.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAmountToCents(%q) error = %v", testCase.input, err)
			}
			if got != testCase.want {
				t.Fatalf("ParseAmountToCents(%q) = %d, want %d", testCase.input, got, testCase.want)
			}
		})
	}
}

func TestFormatCents(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{cents: 1990, want: "19.90"},
		{cents: 0, want: "0.00"},
		{cents: 1, want: "0.01"},
		{cents: 100, want: "1.00"},
		{cents: 123456789, want: "1234567.89"},
		{cents: -1990, want: "-19.90"},
	}
	for _, testCase := range cases {
		if got := FormatCents(testCase.cents); got != testCase.want {
			t.Fatalf("FormatCents(%d) = %q, want %q", testCase.cents, got, testCase.want)
		}
	}
}

func TestPagePay(t *testing.T) {
	_, _, publicPEM := testKeys(t)
	client := newTestClient(t, testGateway)
	pay, err := client.PagePay(testOrderID, 1990, "Praxis-RAG 会员", "年度会员")
	if err != nil {
		t.Fatalf("PagePay() error = %v", err)
	}
	if pay.GatewayURL != testGateway {
		t.Fatalf("GatewayURL = %q, want %q", pay.GatewayURL, testGateway)
	}
	required := map[string]string{
		"app_id":     testAppID,
		"method":     methodPagePay,
		"format":     "JSON",
		"charset":    "utf-8",
		"sign_type":  SignTypeRSA2,
		"version":    "1.0",
		"notify_url": testNotifyURL,
		"return_url": testReturnURL,
	}
	for key, want := range required {
		if got := pay.Params[key]; got != want {
			t.Fatalf("Params[%q] = %q, want %q", key, got, want)
		}
	}
	if pay.Params["sign"] == "" {
		t.Fatal("PagePay() returned params without sign")
	}
	if matched, _ := regexp.MatchString(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`, pay.Params["timestamp"]); !matched {
		t.Fatalf("timestamp = %q does not use the gateway layout", pay.Params["timestamp"])
	}

	var bizContent struct {
		OutTradeNo     string `json:"out_trade_no"`
		ProductCode    string `json:"product_code"`
		TotalAmount    string `json:"total_amount"`
		Subject        string `json:"subject"`
		Body           string `json:"body"`
		TimeoutExpress string `json:"timeout_express"`
	}
	if err := json.Unmarshal([]byte(pay.Params["biz_content"]), &bizContent); err != nil {
		t.Fatalf("biz_content is not JSON: %v", err)
	}
	if bizContent.OutTradeNo != testOrderID {
		t.Fatalf("out_trade_no = %q", bizContent.OutTradeNo)
	}
	if bizContent.ProductCode != ProductCodeFastInstantTradePay {
		t.Fatalf("product_code = %q", bizContent.ProductCode)
	}
	if bizContent.TotalAmount != "19.90" {
		t.Fatalf("total_amount = %q", bizContent.TotalAmount)
	}
	if bizContent.Subject != "Praxis-RAG 会员" || bizContent.Body != "年度会员" {
		t.Fatalf("subject/body = %q/%q", bizContent.Subject, bizContent.Body)
	}
	if bizContent.TimeoutExpress != "30m" {
		t.Fatalf("timeout_express = %q, want 30m", bizContent.TimeoutExpress)
	}

	if err := VerifyValues(pay.Params, publicPEM); err != nil {
		t.Fatalf("PagePay() signature does not verify: %v", err)
	}
	tampered := copyValues(pay.Params)
	tampered["total_amount"] = "0.01"
	if err := VerifyValues(tampered, publicPEM); err == nil {
		t.Fatal("tampered PagePay params still verify")
	}

	if _, err := client.PagePay("", 1990, "会员", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("PagePay(empty order) error = %v, want ErrInvalidRequest", err)
	}
	if _, err := client.PagePay(testOrderID, 0, "会员", ""); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("PagePay(0 cents) error = %v, want ErrInvalidAmount", err)
	}
	if _, err := client.PagePay(testOrderID, 1990, "", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("PagePay(empty subject) error = %v, want ErrInvalidRequest", err)
	}

	settings := testSettings(t, testGateway)
	settings.NotifyURL = ""
	withoutNotify, err := NewClient(settings)
	if err != nil {
		t.Fatalf("NewClient(without notify url) error = %v", err)
	}
	if _, err := withoutNotify.PagePay(testOrderID, 1990, "会员", ""); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("PagePay(without notify url) error = %v, want ErrNotConfigured", err)
	}
}

// gatewayReply is one canned signed gateway answer.
type gatewayReply struct {
	node    string
	payload string
	// tamper signs a different payload than the one that is returned.
	tamper bool
}

// newSignedGateway starts an httptest server that verifies every request
// signature and answers with a signed response node.
func newSignedGateway(t *testing.T, reply func(t *testing.T, form url.Values) gatewayReply) *httptest.Server {
	t.Helper()
	key, _, publicPEM := testKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("gateway received %s, want POST", r.Method)
		}
		if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
			t.Errorf("gateway received Content-Type %q", contentType)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("gateway cannot parse form: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		form := r.PostForm
		if err := VerifyFormValues(form, publicPEM); err != nil {
			t.Errorf("gateway request signature invalid: %v", err)
		}
		if got := form.Get("sign_type"); got != SignTypeRSA2 {
			t.Errorf("sign_type = %q, want RSA2", got)
		}
		if got := form.Get("app_id"); got != testAppID {
			t.Errorf("app_id = %q, want %q", got, testAppID)
		}
		answer := reply(t, form)
		signedPayload := answer.payload
		if answer.tamper {
			signedPayload += " "
		}
		signature := signTestPayload(t, key, signedPayload)
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		_, _ = fmt.Fprintf(w, `{"%s":%s,"sign":"%s"}`, answer.node, answer.payload, signature)
	}))
	t.Cleanup(server.Close)
	return server
}

// decodeBizContent unmarshals one request biz_content into a map.
func decodeBizContent(t *testing.T, form url.Values) map[string]string {
	t.Helper()
	var business map[string]string
	if err := json.Unmarshal([]byte(form.Get("biz_content")), &business); err != nil {
		t.Errorf("biz_content is not a JSON object: %v", err)
	}
	return business
}

func TestQuery(t *testing.T) {
	t.Run("transport failures map to their error categories", func(t *testing.T) {
		unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "gateway maintenance", http.StatusServiceUnavailable)
		}))
		t.Cleanup(unavailable.Close)
		_, err := newTestClient(t, unavailable.URL).Query(context.Background(), testOrderID)
		if !errors.Is(err, ErrGatewayUnavailable) ||
			!strings.Contains(err.Error(), "支付宝网关返回 HTTP 503") {
			t.Fatalf("http-503 error = %v, want ErrGatewayUnavailable with the status", err)
		}

		oversize := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 1024*1024+1))
		}))
		t.Cleanup(oversize.Close)
		_, err = newTestClient(t, oversize.URL).Query(context.Background(), testOrderID)
		if !errors.Is(err, ErrInvalidResponse) ||
			!strings.Contains(err.Error(), "支付宝返回内容超过 1 MiB") {
			t.Fatalf("oversize error = %v, want ErrInvalidResponse with the size message", err)
		}

		empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(nil)
		}))
		t.Cleanup(empty.Close)
		_, err = newTestClient(t, empty.URL).Query(context.Background(), testOrderID)
		if !errors.Is(err, ErrInvalidResponse) ||
			!strings.Contains(err.Error(), "支付宝返回内容为空") {
			t.Fatalf("empty error = %v, want ErrInvalidResponse with the empty message", err)
		}

		closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		closed.Close()
		_, err = newTestClient(t, closed.URL).Query(context.Background(), testOrderID)
		if !errors.Is(err, ErrGatewayUnavailable) {
			t.Fatalf("transport error = %v, want ErrGatewayUnavailable", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		payload := `{"code":"10000","msg":"Success","trade_no":"` + testTradeNo +
			`","out_trade_no":"` + testOrderID +
			`","trade_status":"TRADE_SUCCESS","total_amount":"19.90","seller_id":"` + testSellerID +
			`","buyer_user_id":"2088100000000001"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			if got := form.Get("method"); got != methodQuery {
				t.Errorf("method = %q, want %q", got, methodQuery)
			}
			if business := decodeBizContent(t, form); business["out_trade_no"] != testOrderID {
				t.Errorf("biz_content out_trade_no = %q", business["out_trade_no"])
			}
			if form.Get("notify_url") != "" {
				t.Errorf("query request must not carry notify_url")
			}
			return gatewayReply{node: nodeQuery, payload: payload}
		})
		client := newTestClient(t, server.URL)
		trade, err := client.Query(context.Background(), testOrderID)
		if err != nil {
			t.Fatalf("Query() error = %v", err)
		}
		if trade.OutTradeNo != testOrderID || trade.TradeNo != testTradeNo {
			t.Fatalf("trade identifiers = %q/%q", trade.OutTradeNo, trade.TradeNo)
		}
		if trade.TradeStatus != TradeStatusSuccess || trade.TotalAmount != "19.90" {
			t.Fatalf("trade status/amount = %q/%q", trade.TradeStatus, trade.TotalAmount)
		}
		if trade.SellerID != testSellerID || trade.AppID != testAppID || trade.BuyerID != "2088100000000001" {
			t.Fatalf("trade parties = %q/%q/%q", trade.SellerID, trade.AppID, trade.BuyerID)
		}
	})

	t.Run("business error", func(t *testing.T) {
		payload := `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_NOT_EXIST","sub_msg":"交易不存在"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			return gatewayReply{node: nodeQuery, payload: payload}
		})
		client := newTestClient(t, server.URL)
		_, err := client.Query(context.Background(), testOrderID)
		if err == nil {
			t.Fatal("Query() succeeded on a business error")
		}
		var apiError *APIError
		if !errors.As(err, &apiError) {
			t.Fatalf("Query() error = %v, want *APIError", err)
		}
		if apiError.Code != "40004" || apiError.SubCode != SubCodeTradeNotExist {
			t.Fatalf("APIError = %+v", apiError)
		}
		if !IsTradeNotExist(err) {
			t.Fatalf("IsTradeNotExist(%v) = false", err)
		}
		if strings.Contains(err.Error(), "sign") {
			t.Fatalf("error message leaks signature material: %v", err)
		}
	})

	t.Run("tampered response", func(t *testing.T) {
		payload := `{"code":"10000","msg":"Success","trade_no":"` + testTradeNo +
			`","out_trade_no":"` + testOrderID + `","trade_status":"TRADE_SUCCESS","total_amount":"19.90"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			return gatewayReply{node: nodeQuery, payload: payload, tamper: true}
		})
		client := newTestClient(t, server.URL)
		_, err := client.Query(context.Background(), testOrderID)
		if err == nil || !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("Query() error = %v, want ErrInvalidSignature", err)
		}
	})

	t.Run("unsigned response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"alipay_trade_query_response":{"code":"10000"}}`))
		}))
		t.Cleanup(server.Close)
		client := newTestClient(t, server.URL)
		_, err := client.Query(context.Background(), testOrderID)
		if err == nil || !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("Query() error = %v, want ErrInvalidSignature", err)
		}
	})
}

func TestRefundAndQueryRefund(t *testing.T) {
	t.Run("refund", func(t *testing.T) {
		payload := `{"code":"10000","msg":"Success","trade_no":"` + testTradeNo +
			`","out_trade_no":"` + testOrderID + `","fund_change":"Y","refund_fee":"19.90"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			if got := form.Get("method"); got != methodRefund {
				t.Errorf("method = %q, want %q", got, methodRefund)
			}
			business := decodeBizContent(t, form)
			if business["out_trade_no"] != testOrderID {
				t.Errorf("out_trade_no = %q", business["out_trade_no"])
			}
			if business["refund_amount"] != "19.90" {
				t.Errorf("refund_amount = %q", business["refund_amount"])
			}
			if business["out_request_no"] != "RF-0001" {
				t.Errorf("out_request_no = %q", business["out_request_no"])
			}
			if business["refund_reason"] != "用户申请退款" {
				t.Errorf("refund_reason = %q", business["refund_reason"])
			}
			return gatewayReply{node: nodeRefund, payload: payload}
		})
		client := newTestClient(t, server.URL)
		refund, err := client.Refund(context.Background(), testOrderID, "RF-0001", 1990, "用户申请退款")
		if err != nil {
			t.Fatalf("Refund() error = %v", err)
		}
		if refund.OutTradeNo != testOrderID || refund.TradeNo != testTradeNo {
			t.Fatalf("refund identifiers = %q/%q", refund.OutTradeNo, refund.TradeNo)
		}
		if refund.FundChange != "Y" || refund.RefundFee != "19.90" {
			t.Fatalf("refund result = %+v", refund)
		}
		if _, err := client.Refund(context.Background(), testOrderID, "", 100, ""); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Refund(empty request no) error = %v, want ErrInvalidRequest", err)
		}
		if _, err := client.Refund(context.Background(), testOrderID, "RF-0002", 0, ""); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("Refund(0 cents) error = %v, want ErrInvalidAmount", err)
		}
	})

	t.Run("refund business error", func(t *testing.T) {
		payload := `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.SELLER_BALANCE_NOT_ENOUGH","sub_msg":"卖家余额不足"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			return gatewayReply{node: nodeRefund, payload: payload}
		})
		client := newTestClient(t, server.URL)
		_, err := client.Refund(context.Background(), testOrderID, "RF-0001", 1990, "")
		var apiError *APIError
		if !errors.As(err, &apiError) || apiError.SubCode != "ACQ.SELLER_BALANCE_NOT_ENOUGH" {
			t.Fatalf("Refund() error = %v", err)
		}
	})

	t.Run("query refund", func(t *testing.T) {
		payload := `{"code":"10000","msg":"Success","trade_no":"` + testTradeNo +
			`","out_trade_no":"` + testOrderID + `","refund_amount":"5.00","refund_status":"REFUND_SUCCESS","out_request_no":"RF-0003"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			if got := form.Get("method"); got != methodQueryRefund {
				t.Errorf("method = %q, want %q", got, methodQueryRefund)
			}
			business := decodeBizContent(t, form)
			if business["out_trade_no"] != testOrderID || business["out_request_no"] != "RF-0003" {
				t.Errorf("biz_content = %v", business)
			}
			return gatewayReply{node: nodeQueryRefund, payload: payload}
		})
		client := newTestClient(t, server.URL)
		refund, err := client.QueryRefund(context.Background(), testOrderID, "RF-0003")
		if err != nil {
			t.Fatalf("QueryRefund() error = %v", err)
		}
		if refund.RefundFee != "5.00" || refund.RefundStatus != "REFUND_SUCCESS" || refund.OutRequestNo != "RF-0003" {
			t.Fatalf("query refund result = %+v", refund)
		}
	})
}

func TestClose(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		payload := `{"code":"10000","msg":"Success","trade_no":"` + testTradeNo + `","out_trade_no":"` + testOrderID + `"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			if got := form.Get("method"); got != methodClose {
				t.Errorf("method = %q, want %q", got, methodClose)
			}
			if business := decodeBizContent(t, form); business["out_trade_no"] != testOrderID {
				t.Errorf("out_trade_no = %q", business["out_trade_no"])
			}
			return gatewayReply{node: nodeClose, payload: payload}
		})
		client := newTestClient(t, server.URL)
		if err := client.Close(context.Background(), testOrderID); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	t.Run("trade not exist is not an error", func(t *testing.T) {
		payload := `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_NOT_EXIST","sub_msg":"交易不存在"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			return gatewayReply{node: nodeClose, payload: payload}
		})
		client := newTestClient(t, server.URL)
		if err := client.Close(context.Background(), testOrderID); err != nil {
			t.Fatalf("Close(trade not exist) error = %v, want nil", err)
		}
	})

	t.Run("unknown failure is returned", func(t *testing.T) {
		payload := `{"code":"40004","msg":"Business Failed","sub_code":"ACQ.PARAM_ERROR","sub_msg":"参数错误"}`
		server := newSignedGateway(t, func(t *testing.T, form url.Values) gatewayReply {
			return gatewayReply{node: nodeClose, payload: payload}
		})
		client := newTestClient(t, server.URL)
		err := client.Close(context.Background(), testOrderID)
		if err == nil {
			t.Fatal("Close() swallowed an unknown gateway error")
		}
		if IsTradeNotExist(err) {
			t.Fatalf("IsTradeNotExist(%v) = true", err)
		}
	})
}

// signedNotify builds a correctly signed asynchronous notification.
func signedNotify(t *testing.T, overrides map[string]string) url.Values {
	t.Helper()
	_, privatePEM, _ := testKeys(t)
	fields := map[string]string{
		"app_id":       testAppID,
		"seller_id":    testSellerID,
		"trade_status": TradeStatusSuccess,
		"out_trade_no": testOrderID,
		"trade_no":     testTradeNo,
		"total_amount": "19.90",
		"notify_id":    "notify-0001",
		"notify_type":  "trade_status_sync",
		"subject":      "Praxis-RAG 会员",
		"gmt_payment":  "2026-11-17 20:00:01",
	}
	for key, value := range overrides {
		if value == "" {
			delete(fields, key)
			continue
		}
		fields[key] = value
	}
	signature, err := SignValues(fields, privatePEM)
	if err != nil {
		t.Fatalf("SignValues(notify) error = %v", err)
	}
	fields["sign"] = signature
	fields["sign_type"] = SignTypeRSA2
	values := make(url.Values, len(fields))
	for key, value := range fields {
		values.Set(key, value)
	}
	return values
}

func TestVerifyNotify(t *testing.T) {
	client := newTestClient(t, testGateway)

	notify, err := client.VerifyNotify(signedNotify(t, nil))
	if err != nil {
		t.Fatalf("VerifyNotify() error = %v", err)
	}
	if notify.OutTradeNo != testOrderID || notify.TradeNo != testTradeNo {
		t.Fatalf("notify identifiers = %q/%q", notify.OutTradeNo, notify.TradeNo)
	}
	if notify.TradeStatus != TradeStatusSuccess || notify.TotalAmount != "19.90" {
		t.Fatalf("notify status/amount = %q/%q", notify.TradeStatus, notify.TotalAmount)
	}
	if notify.AppID != testAppID || notify.SellerID != testSellerID || notify.NotifyID != "notify-0001" {
		t.Fatalf("notify parties = %q/%q/%q", notify.AppID, notify.SellerID, notify.NotifyID)
	}

	// sign_type in the notification must not select another algorithm.
	ignoredSignType := signedNotify(t, nil)
	ignoredSignType.Set("sign_type", "RSA")
	if _, err := client.VerifyNotify(ignoredSignType); err != nil {
		t.Fatalf("VerifyNotify(ignored sign_type) error = %v", err)
	}

	cases := []struct {
		name      string
		overrides map[string]string
	}{
		{name: "wrong app id", overrides: map[string]string{"app_id": "2021999999999999"}},
		{name: "wrong seller id", overrides: map[string]string{"seller_id": "2088999999999999"}},
		{name: "missing seller id", overrides: map[string]string{"seller_id": ""}},
		{name: "closed trade", overrides: map[string]string{"trade_status": TradeStatusClosed}},
		{name: "waiting trade", overrides: map[string]string{"trade_status": "WAIT_BUYER_PAY"}},
		{name: "missing trade status", overrides: map[string]string{"trade_status": ""}},
		{name: "missing trade no", overrides: map[string]string{"trade_no": ""}},
		{name: "missing out trade no", overrides: map[string]string{"out_trade_no": ""}},
		{name: "bad amount", overrides: map[string]string{"total_amount": "19.999"}},
		{name: "missing amount", overrides: map[string]string{"total_amount": ""}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := client.VerifyNotify(signedNotify(t, testCase.overrides))
			if err == nil || !errors.Is(err, ErrInvalidNotify) {
				t.Fatalf("VerifyNotify() error = %v, want ErrInvalidNotify", err)
			}
		})
	}

	if _, err := client.VerifyNotify(url.Values{}); !errors.Is(err, ErrInvalidNotify) {
		t.Fatalf("VerifyNotify(empty) error = %v, want ErrInvalidNotify", err)
	}

	tampered := signedNotify(t, nil)
	tampered.Set("total_amount", "0.01")
	if _, err := client.VerifyNotify(tampered); !errors.Is(err, ErrInvalidNotify) {
		t.Fatalf("VerifyNotify(tampered amount) error = %v, want ErrInvalidNotify", err)
	}

	repeated := signedNotify(t, nil)
	repeated.Add("app_id", testAppID)
	if _, err := client.VerifyNotify(repeated); !errors.Is(err, ErrInvalidNotify) {
		t.Fatalf("VerifyNotify(repeated app_id) error = %v, want ErrInvalidNotify", err)
	}

	// TRADE_FINISHED is a successful notification too.
	if _, err := client.VerifyNotify(signedNotify(t, map[string]string{"trade_status": TradeStatusFinished})); err != nil {
		t.Fatalf("VerifyNotify(TRADE_FINISHED) error = %v", err)
	}
}

func TestSettingsFromEnvDefaults(t *testing.T) {
	clearAlipayEnv(t)
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if settings.Gateway != DefaultGateway {
		t.Fatalf("Gateway = %q, want %q", settings.Gateway, DefaultGateway)
	}
	if settings.Environment != EnvironmentProduction {
		t.Fatalf("Environment = %q, want production", settings.Environment)
	}
	if settings.TimeoutMinutes != DefaultTimeoutMinutes {
		t.Fatalf("TimeoutMinutes = %d, want %d", settings.TimeoutMinutes, DefaultTimeoutMinutes)
	}
	if settings.PurchaseEnabled {
		t.Fatal("PurchaseEnabled = true, want false by default")
	}
	if settings.Configured() || settings.PurchaseAvailable() {
		t.Fatalf("empty settings report configured=%v purchase=%v", settings.Configured(), settings.PurchaseAvailable())
	}
}

func TestSettingsFromEnvSandboxAndPurchaseFlag(t *testing.T) {
	clearAlipayEnv(t)
	t.Setenv("RAG_ALIPAY_ENV", "sandbox")
	t.Setenv("RAG_MEMBERSHIP_PURCHASE_ENABLED", "true")
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if settings.Gateway != SandboxGateway {
		t.Fatalf("Gateway = %q, want %q", settings.Gateway, SandboxGateway)
	}
	if settings.Environment != EnvironmentSandbox {
		t.Fatalf("Environment = %q, want sandbox", settings.Environment)
	}
	if !settings.PurchaseEnabled {
		t.Fatal("PurchaseEnabled = false, want true")
	}
	if settings.PurchaseAvailable() {
		t.Fatal("PurchaseAvailable() = true without credentials")
	}
	if _, err := NewClient(settings); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("NewClient(empty credentials) error = %v, want ErrNotConfigured", err)
	}
}

func TestSettingsFromEnvFullConfiguration(t *testing.T) {
	clearAlipayEnv(t)
	_, privatePEM, publicPEM := testKeys(t)
	t.Setenv("RAG_ALIPAY_APP_ID", testAppID)
	t.Setenv("RAG_ALIPAY_PRIVATE_KEY", strings.ReplaceAll(privatePEM, "\n", `\n`))
	t.Setenv("RAG_ALIPAY_PUBLIC_KEY", publicPEM)
	t.Setenv("RAG_ALIPAY_NOTIFY_URL", testNotifyURL)
	t.Setenv("RAG_ALIPAY_RETURN_URL", testReturnURL)
	t.Setenv("RAG_ALIPAY_SELLER_ID", testSellerID)
	t.Setenv("RAG_ALIPAY_TIMEOUT_MINUTES", "45")
	t.Setenv("RAG_MEMBERSHIP_PURCHASE_ENABLED", "true")

	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if !settings.Configured() {
		t.Fatal("Configured() = false for a complete configuration")
	}
	if !settings.PurchaseAvailable() {
		t.Fatal("PurchaseAvailable() = false for a complete enabled configuration")
	}
	if settings.TimeoutMinutes != 45 {
		t.Fatalf("TimeoutMinutes = %d, want 45", settings.TimeoutMinutes)
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	client, err := NewClient(settings)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	pay, err := client.PagePay(testOrderID, 1990, "会员", "")
	if err != nil {
		t.Fatalf("PagePay() error = %v", err)
	}
	var business map[string]string
	if err := json.Unmarshal([]byte(pay.Params["biz_content"]), &business); err != nil {
		t.Fatalf("biz_content is not JSON: %v", err)
	}
	if business["timeout_express"] != "45m" {
		t.Fatalf("timeout_express = %q, want 45m", business["timeout_express"])
	}
	if err := VerifyValues(pay.Params, publicPEM); err != nil {
		t.Fatalf("PagePay() signature does not verify: %v", err)
	}
}

func TestSettingsFromEnvRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "unknown environment", key: "RAG_ALIPAY_ENV", value: "staging"},
		{name: "non numeric timeout", key: "RAG_ALIPAY_TIMEOUT_MINUTES", value: "abc"},
		{name: "zero timeout", key: "RAG_ALIPAY_TIMEOUT_MINUTES", value: "0"},
		{name: "negative timeout", key: "RAG_ALIPAY_TIMEOUT_MINUTES", value: "-5"},
		{name: "huge timeout", key: "RAG_ALIPAY_TIMEOUT_MINUTES", value: "21601"},
		{name: "bad purchase flag", key: "RAG_MEMBERSHIP_PURCHASE_ENABLED", value: "maybe"},
		{name: "bad gateway", key: "RAG_ALIPAY_GATEWAY", value: "ftp://openapi.alipay.com/gateway.do"},
		{name: "bad notify url", key: "RAG_ALIPAY_NOTIFY_URL", value: "not-a-url"},
		{name: "bad return url", key: "RAG_ALIPAY_RETURN_URL", value: "/console/membership"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearAlipayEnv(t)
			t.Setenv(testCase.key, testCase.value)
			if _, err := SettingsFromEnv(); err == nil {
				t.Fatalf("SettingsFromEnv() accepted %s=%q", testCase.key, testCase.value)
			}
		})
	}
}

func TestSettingsValidate(t *testing.T) {
	if err := (Settings{}).Validate(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Validate(empty) error = %v, want ErrNotConfigured", err)
	}
	settings := testSettings(t, testGateway)
	if err := settings.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !settings.Configured() || !settings.PurchaseAvailable() {
		t.Fatalf("Configured/PurchaseAvailable = %v/%v", settings.Configured(), settings.PurchaseAvailable())
	}

	settings.PurchaseEnabled = false
	if settings.PurchaseAvailable() {
		t.Fatal("PurchaseAvailable() = true while the feature switch is off")
	}
	if err := settings.Validate(); err != nil {
		t.Fatalf("Validate(switched off) error = %v", err)
	}

	settings = testSettings(t, testGateway)
	settings.SellerID = ""
	if err := settings.Validate(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Validate(without seller) error = %v, want ErrNotConfigured", err)
	}

	settings = testSettings(t, testGateway)
	settings.PrivateKeyPEM = "not-a-private-key"
	if err := settings.Validate(); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Validate(bad private key) error = %v, want ErrInvalidKey", err)
	}

	settings = testSettings(t, testGateway)
	settings.PublicKeyPEM = "not-a-public-key"
	if err := settings.Validate(); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Validate(bad public key) error = %v, want ErrInvalidKey", err)
	}

	settings = testSettings(t, testGateway)
	settings.Gateway = "ftp://openapi.alipay.com/gateway.do"
	if err := settings.Validate(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Validate(bad gateway) error = %v, want ErrNotConfigured", err)
	}
}

// clearAlipayEnv resets every Alipay environment variable for one test.
func clearAlipayEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"RAG_ALIPAY_APP_ID",
		"RAG_ALIPAY_PRIVATE_KEY",
		"RAG_ALIPAY_PUBLIC_KEY",
		"RAG_ALIPAY_GATEWAY",
		"RAG_ALIPAY_ENV",
		"RAG_ALIPAY_NOTIFY_URL",
		"RAG_ALIPAY_RETURN_URL",
		"RAG_ALIPAY_SELLER_ID",
		"RAG_ALIPAY_TIMEOUT_MINUTES",
		"RAG_MEMBERSHIP_PURCHASE_ENABLED",
	} {
		t.Setenv(name, "")
	}
}
