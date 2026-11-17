package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Alipay gateway methods used by the membership purchase flow.
const (
	methodPagePay     = "alipay.trade.page.pay"
	methodQuery       = "alipay.trade.query"
	methodClose       = "alipay.trade.close"
	methodRefund      = "alipay.trade.refund"
	methodQueryRefund = "alipay.trade.fastpay.refund.query"
)

// Response node names of the gateway payloads.
const (
	nodeQuery       = "alipay_trade_query_response"
	nodeClose       = "alipay_trade_close_response"
	nodeRefund      = "alipay_trade_refund_response"
	nodeQueryRefund = "alipay_trade_fastpay_refund_query_response"
)

// Fixed request values and protocol constants.
const (
	formatJSON     = "JSON"
	charsetUTF8    = "utf-8"
	versionV1      = "1.0"
	apiSuccessCode = "10000"
	// SubCodeTradeNotExist is returned by Alipay when the trade does not exist.
	SubCodeTradeNotExist = "ACQ.TRADE_NOT_EXIST"
	// ProductCodeFastInstantTradePay is the page-pay product code.
	ProductCodeFastInstantTradePay = "FAST_INSTANT_TRADE_PAY"
	// TimestampLayout is the timestamp format expected by the gateway.
	TimestampLayout = "2006-01-02 15:04:05"
)

// Trade statuses that mean the buyer has paid.
const (
	// TradeStatusSuccess means the trade succeeded and cannot be closed.
	TradeStatusSuccess = "TRADE_SUCCESS"
	// TradeStatusFinished means the trade completed and cannot be refunded by
	// the merchant through this flow.
	TradeStatusFinished = "TRADE_FINISHED"
	// TradeStatusClosed means the trade was closed or cancelled.
	TradeStatusClosed = "TRADE_CLOSED"
)

// Limits and network bounds.
const (
	// requestTimeout bounds one gateway HTTP round trip.
	requestTimeout = 15 * time.Second
	// maxResponseBytes caps one gateway response body (1 MiB).
	maxResponseBytes = 1 << 20
	// maxOrderIDLength is Alipay's out_trade_no limit.
	maxOrderIDLength = 64
	// maxRequestNoLength is Alipay's out_request_no limit.
	maxRequestNoLength = 64
	// maxSubjectRunes is Alipay's subject limit.
	maxSubjectRunes = 256
	// maxBodyRunes is Alipay's body limit.
	maxBodyRunes = 128
	// maxReasonRunes is Alipay's refund_reason limit.
	maxReasonRunes = 256
	// maxErrorTextRunes bounds Alipay-provided error text kept in an APIError.
	maxErrorTextRunes = 200
)

// alipayLocation is the Beijing time zone the gateway expects in timestamp. A
// fixed zone is used so the server needs no tzdata.
var alipayLocation = time.FixedZone("UTC+8", 8*60*60)

// Errors returned by the gateway API.
var (
	// ErrInvalidRequest reports invalid caller input such as an empty order ID.
	ErrInvalidRequest = errors.New("支付宝请求参数无效")
	// ErrInvalidResponse reports a gateway answer that cannot be parsed.
	ErrInvalidResponse = errors.New("支付宝返回内容无法解析")
	// ErrInvalidNotify reports an asynchronous notification that cannot be
	// trusted, including an unexpected seller, app or trade status.
	ErrInvalidNotify = errors.New("支付宝异步通知校验失败")
	// ErrGatewayUnavailable reports a transport failure before Alipay answered.
	ErrGatewayUnavailable = errors.New("支付宝网关不可达")
	// ErrTradeNotExist reports that the trade does not exist.
	ErrTradeNotExist = errors.New("支付宝交易不存在")
)

// APIError is one safe Alipay gateway failure. It carries the gateway codes and
// a Chinese message built from them; it never contains a signature, a key or
// request parameters.
type APIError struct {
	// Code is the gateway result code (for example "40004").
	Code string
	// SubCode is the detailed result code (for example "ACQ.TRADE_NOT_EXIST").
	SubCode string
	// Message is the safe user-facing description.
	Message string
}

// Error implements error.
func (e *APIError) Error() string { return e.Message }

// IsTradeNotExist reports whether err means the Alipay trade does not exist.
func IsTradeNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTradeNotExist) {
		return true
	}
	var apiError *APIError
	if errors.As(err, &apiError) {
		return apiError.SubCode == SubCodeTradeNotExist || apiError.Code == SubCodeTradeNotExist
	}
	return false
}

// PagePay is one prepared alipay.trade.page.pay submission. The browser POSTs
// Params as a form to GatewayURL, which redirects to the Alipay cashier.
type PagePay struct {
	// GatewayURL is the Alipay gateway the form must be posted to.
	GatewayURL string
	// Params are the signed form fields, including sign.
	Params map[string]string
}

// FormValues renders the signed parameters as a url.Values form.
func (p PagePay) FormValues() url.Values {
	values := make(url.Values, len(p.Params))
	for key, value := range p.Params {
		values.Set(key, value)
	}
	return values
}

// Trade is one queried Alipay trade.
type Trade struct {
	OutTradeNo  string
	TradeNo     string
	TradeStatus string
	TotalAmount string
	SellerID    string
	AppID       string
	BuyerID     string
}

// Notify is one verified asynchronous payment notification.
type Notify struct {
	OutTradeNo  string
	TradeNo     string
	TradeStatus string
	TotalAmount string
	AppID       string
	SellerID    string
	NotifyID    string
}

// Refund is one refund result or refund query result.
type Refund struct {
	OutTradeNo   string
	TradeNo      string
	RefundFee    string
	TradeStatus  string
	RefundStatus string
	// FundChange is "Y" when Alipay moved money back to the buyer.
	FundChange string
	// OutRequestNo echoes the merchant refund request number.
	OutRequestNo string
}

// Client talks to the Alipay open gateway.
type Client struct {
	settings   Settings
	httpClient *http.Client
	now        func() time.Time
}

// NewClient builds a gateway client from settings. It requires an AppID, a
// parsable application private key, a parsable Alipay public key and an http(s)
// gateway; the callback addresses are only needed by PagePay.
func NewClient(settings Settings) (*Client, error) {
	normalized := settings.normalized()
	if normalized.AppID == "" {
		return nil, fmt.Errorf("%w：未配置支付宝 AppID", ErrNotConfigured)
	}
	if normalized.PrivateKeyPEM == "" {
		return nil, fmt.Errorf("%w：未配置支付宝应用私钥", ErrNotConfigured)
	}
	if normalized.PublicKeyPEM == "" {
		return nil, fmt.Errorf("%w：未配置支付宝公钥", ErrNotConfigured)
	}
	if _, err := parsePrivateKey(normalized.PrivateKeyPEM); err != nil {
		return nil, err
	}
	if _, err := parsePublicKey(normalized.PublicKeyPEM); err != nil {
		return nil, err
	}
	if err := validateGatewayURL(normalized.Gateway); err != nil {
		return nil, err
	}
	return &Client{
		settings:   normalized,
		httpClient: &http.Client{Timeout: requestTimeout},
		now:        time.Now,
	}, nil
}

// PagePay builds the signed form for alipay.trade.page.pay. amountCents is the
// exact order amount in cents. The caller is responsible for checking
// Settings.PurchaseAvailable before creating an order.
func (c *Client) PagePay(orderID string, amountCents int64, subject, body string) (PagePay, error) {
	if err := validateOrderID(orderID); err != nil {
		return PagePay{}, err
	}
	if amountCents <= 0 {
		return PagePay{}, fmt.Errorf("%w：支付金额必须大于 0", ErrInvalidAmount)
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return PagePay{}, fmt.Errorf("%w：缺少订单标题", ErrInvalidRequest)
	}
	if utf8.RuneCountInString(subject) > maxSubjectRunes {
		return PagePay{}, fmt.Errorf("%w：订单标题超过 %d 个字符", ErrInvalidRequest, maxSubjectRunes)
	}
	body = strings.TrimSpace(body)
	if utf8.RuneCountInString(body) > maxBodyRunes {
		return PagePay{}, fmt.Errorf("%w：订单描述超过 %d 个字符", ErrInvalidRequest, maxBodyRunes)
	}
	if c.settings.NotifyURL == "" || c.settings.ReturnURL == "" {
		return PagePay{}, fmt.Errorf("%w：缺少支付宝异步通知地址或同步跳转地址", ErrNotConfigured)
	}
	bizContent, err := marshalBiz(pagePayBiz{
		OutTradeNo:     orderID,
		ProductCode:    ProductCodeFastInstantTradePay,
		TotalAmount:    FormatCents(amountCents),
		Subject:        subject,
		Body:           body,
		TimeoutExpress: timeoutExpress(c.settings.TimeoutMinutes),
	})
	if err != nil {
		return PagePay{}, err
	}
	params, err := c.signedParams(methodPagePay, bizContent, map[string]string{
		"notify_url": c.settings.NotifyURL,
		"return_url": c.settings.ReturnURL,
	})
	if err != nil {
		return PagePay{}, err
	}
	return PagePay{GatewayURL: c.settings.Gateway, Params: params}, nil
}

// Query reads one trade. Alipay answers with a signed response which is
// verified before it is decoded.
func (c *Client) Query(ctx context.Context, orderID string) (Trade, error) {
	if err := validateOrderID(orderID); err != nil {
		return Trade{}, err
	}
	bizContent, err := marshalBiz(tradeBiz{OutTradeNo: orderID})
	if err != nil {
		return Trade{}, err
	}
	node, err := c.call(ctx, methodQuery, nodeQuery, bizContent)
	if err != nil {
		return Trade{}, err
	}
	var payload struct {
		responseHead
		TradeNo     string `json:"trade_no"`
		OutTradeNo  string `json:"out_trade_no"`
		TradeStatus string `json:"trade_status"`
		TotalAmount string `json:"total_amount"`
		SellerID    string `json:"seller_id"`
		AppID       string `json:"app_id"`
		BuyerUserID string `json:"buyer_user_id"`
	}
	if err := json.Unmarshal(node, &payload); err != nil {
		return Trade{}, fmt.Errorf("%w：交易查询响应无法解析", ErrInvalidResponse)
	}
	if err := payload.failure(); err != nil {
		return Trade{}, err
	}
	if payload.OutTradeNo != orderID {
		return Trade{}, fmt.Errorf("%w：交易查询响应与请求订单不一致", ErrInvalidResponse)
	}
	appID := payload.AppID
	if appID == "" {
		appID = c.settings.AppID
	}
	return Trade{
		OutTradeNo:  payload.OutTradeNo,
		TradeNo:     payload.TradeNo,
		TradeStatus: payload.TradeStatus,
		TotalAmount: payload.TotalAmount,
		SellerID:    payload.SellerID,
		AppID:       appID,
		BuyerID:     payload.BuyerUserID,
	}, nil
}

// Close closes one unpaid trade. Alipay reports ACQ.TRADE_NOT_EXIST when the
// trade was never created or is already closed; that case means there is
// nothing left to close, so Close returns nil. Every other failure is returned,
// so an unknown error is never silently treated as success. Use
// IsTradeNotExist when a caller needs the distinction.
func (c *Client) Close(ctx context.Context, orderID string) error {
	if err := validateOrderID(orderID); err != nil {
		return err
	}
	bizContent, err := marshalBiz(tradeBiz{OutTradeNo: orderID})
	if err != nil {
		return err
	}
	node, err := c.call(ctx, methodClose, nodeClose, bizContent)
	if err != nil {
		return err
	}
	var payload responseHead
	if err := json.Unmarshal(node, &payload); err != nil {
		return fmt.Errorf("%w：关闭交易响应无法解析", ErrInvalidResponse)
	}
	if err := payload.failure(); err != nil {
		if IsTradeNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// Refund refunds all or part of one trade. requestNo is the unique merchant
// refund request number; it must identify the refund idempotently and is
// supplied by the caller. amountCents is the refund amount in cents.
func (c *Client) Refund(ctx context.Context, orderID, requestNo string, amountCents int64, reason string) (Refund, error) {
	if err := validateOrderID(orderID); err != nil {
		return Refund{}, err
	}
	if err := validateRequestNo(requestNo); err != nil {
		return Refund{}, err
	}
	if amountCents <= 0 {
		return Refund{}, fmt.Errorf("%w：退款金额必须大于 0", ErrInvalidAmount)
	}
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return Refund{}, fmt.Errorf("%w：退款原因超过 %d 个字符", ErrInvalidRequest, maxReasonRunes)
	}
	bizContent, err := marshalBiz(refundBiz{
		OutTradeNo:   orderID,
		RefundAmount: FormatCents(amountCents),
		OutRequestNo: requestNo,
		RefundReason: reason,
	})
	if err != nil {
		return Refund{}, err
	}
	node, err := c.call(ctx, methodRefund, nodeRefund, bizContent)
	if err != nil {
		return Refund{}, err
	}
	var payload struct {
		responseHead
		TradeNo     string `json:"trade_no"`
		OutTradeNo  string `json:"out_trade_no"`
		FundChange  string `json:"fund_change"`
		RefundFee   string `json:"refund_fee"`
		TradeStatus string `json:"trade_status"`
	}
	if err := json.Unmarshal(node, &payload); err != nil {
		return Refund{}, fmt.Errorf("%w：退款响应无法解析", ErrInvalidResponse)
	}
	if err := payload.failure(); err != nil {
		return Refund{}, err
	}
	if payload.OutTradeNo != orderID {
		return Refund{}, fmt.Errorf("%w：退款响应与请求订单不一致", ErrInvalidResponse)
	}
	return Refund{
		OutTradeNo:   payload.OutTradeNo,
		TradeNo:      payload.TradeNo,
		RefundFee:    payload.RefundFee,
		TradeStatus:  payload.TradeStatus,
		FundChange:   payload.FundChange,
		OutRequestNo: requestNo,
	}, nil
}

// QueryRefund reads one refund by its merchant request number.
func (c *Client) QueryRefund(ctx context.Context, orderID, requestNo string) (Refund, error) {
	if err := validateOrderID(orderID); err != nil {
		return Refund{}, err
	}
	if err := validateRequestNo(requestNo); err != nil {
		return Refund{}, err
	}
	bizContent, err := marshalBiz(queryRefundBiz{
		OutTradeNo:   orderID,
		OutRequestNo: requestNo,
	})
	if err != nil {
		return Refund{}, err
	}
	node, err := c.call(ctx, methodQueryRefund, nodeQueryRefund, bizContent)
	if err != nil {
		return Refund{}, err
	}
	var payload struct {
		responseHead
		TradeNo      string `json:"trade_no"`
		OutTradeNo   string `json:"out_trade_no"`
		RefundAmount string `json:"refund_amount"`
		RefundStatus string `json:"refund_status"`
		OutRequestNo string `json:"out_request_no"`
	}
	if err := json.Unmarshal(node, &payload); err != nil {
		return Refund{}, fmt.Errorf("%w：退款查询响应无法解析", ErrInvalidResponse)
	}
	if err := payload.failure(); err != nil {
		return Refund{}, err
	}
	if payload.OutTradeNo != orderID {
		return Refund{}, fmt.Errorf("%w：退款查询响应与请求订单不一致", ErrInvalidResponse)
	}
	return Refund{
		OutTradeNo:   payload.OutTradeNo,
		TradeNo:      payload.TradeNo,
		RefundFee:    payload.RefundAmount,
		RefundStatus: payload.RefundStatus,
		OutRequestNo: payload.OutRequestNo,
	}, nil
}

// VerifyNotify validates one asynchronous notification form: the RSA2
// signature over the raw values, the app_id, the seller_id, the trade status,
// the presence of out_trade_no/trade_no and the amount. sign_type from the form
// is ignored; RSA2 is always used. Other trade statuses are rejected so the
// caller can decide whether a non-success notification is acceptable.
func (c *Client) VerifyNotify(values url.Values) (Notify, error) {
	if len(values) == 0 {
		return Notify{}, fmt.Errorf("%w：通知内容为空", ErrInvalidNotify)
	}
	flat, err := flattenForm(values)
	if err != nil {
		return Notify{}, fmt.Errorf("%w：%w", ErrInvalidNotify, err)
	}
	if err := VerifyValues(flat, c.settings.PublicKeyPEM); err != nil {
		return Notify{}, fmt.Errorf("%w：%w", ErrInvalidNotify, err)
	}
	if appID := strings.TrimSpace(flat["app_id"]); appID != "" && appID != c.settings.AppID {
		return Notify{}, fmt.Errorf("%w：app_id 与配置不一致", ErrInvalidNotify)
	}
	if c.settings.SellerID == "" {
		return Notify{}, fmt.Errorf("%w：未配置收款方 PID，无法校验异步通知", ErrNotConfigured)
	}
	if sellerID := strings.TrimSpace(flat["seller_id"]); sellerID != c.settings.SellerID {
		return Notify{}, fmt.Errorf("%w：seller_id 与配置不一致", ErrInvalidNotify)
	}
	outTradeNo := strings.TrimSpace(flat["out_trade_no"])
	if outTradeNo == "" {
		return Notify{}, fmt.Errorf("%w：缺少 out_trade_no", ErrInvalidNotify)
	}
	tradeNo := strings.TrimSpace(flat["trade_no"])
	if tradeNo == "" {
		return Notify{}, fmt.Errorf("%w：缺少 trade_no", ErrInvalidNotify)
	}
	tradeStatus := strings.TrimSpace(flat["trade_status"])
	switch tradeStatus {
	case TradeStatusSuccess, TradeStatusFinished:
	default:
		return Notify{}, fmt.Errorf("%w：交易状态 %s 不能开通会员", ErrInvalidNotify, safeText(tradeStatus))
	}
	totalAmount := strings.TrimSpace(flat["total_amount"])
	if _, err := ParseAmountToCents(totalAmount); err != nil {
		return Notify{}, fmt.Errorf("%w：金额格式不正确", ErrInvalidNotify)
	}
	return Notify{
		OutTradeNo:  outTradeNo,
		TradeNo:     tradeNo,
		TradeStatus: tradeStatus,
		TotalAmount: totalAmount,
		AppID:       strings.TrimSpace(flat["app_id"]),
		SellerID:    strings.TrimSpace(flat["seller_id"]),
		NotifyID:    strings.TrimSpace(flat["notify_id"]),
	}, nil
}

// signedParams builds one signed request parameter map. extras are added before
// signing so the signature always covers every non-empty value that is sent.
func (c *Client) signedParams(method, bizContent string, extras map[string]string) (map[string]string, error) {
	params := map[string]string{
		"app_id":      c.settings.AppID,
		"method":      method,
		"format":      formatJSON,
		"charset":     charsetUTF8,
		"sign_type":   SignTypeRSA2,
		"timestamp":   c.currentTime().In(alipayLocation).Format(TimestampLayout),
		"version":     versionV1,
		"biz_content": bizContent,
	}
	for key, value := range extras {
		if value != "" {
			params[key] = value
		}
	}
	signature, err := SignValues(params, c.settings.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	params["sign"] = signature
	return params, nil
}

// call posts one signed request and returns the verified response node.
func (c *Client) call(ctx context.Context, method, nodeName, bizContent string) (json.RawMessage, error) {
	params, err := c.signedParams(method, bizContent, nil)
	if err != nil {
		return nil, err
	}
	body, err := c.postForm(ctx, params)
	if err != nil {
		return nil, err
	}
	return verifyGatewayResponse(body, nodeName, c.settings.PublicKeyPEM)
}

// postForm sends the signed parameters as an application/x-www-form-urlencoded
// POST and returns a bounded response body.
func (c *Client) postForm(ctx context.Context, params map[string]string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	form := make(url.Values, len(params))
	for key, value := range params {
		form.Set(key, value)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.settings.Gateway, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w：无法构造支付宝网关请求", ErrInvalidRequest)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	request.Header.Set("Accept", "application/json")
	client := c.httpClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	response, err := client.Do(request)
	if err != nil {
		// The transport error may name the gateway URL; only a safe category is
		// reported so nothing about the request or signature leaks.
		return nil, fmt.Errorf("%w：%s", ErrGatewayUnavailable, transportReason(err))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w：读取支付宝返回内容失败", ErrGatewayUnavailable)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w：支付宝返回内容超过 1 MiB", ErrInvalidResponse)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w：支付宝网关返回 HTTP %d", ErrGatewayUnavailable, response.StatusCode)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("%w：支付宝返回内容为空", ErrInvalidResponse)
	}
	return body, nil
}

// currentTime returns the client clock, defaulting to time.Now.
func (c *Client) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// pagePayBiz is the biz_content of alipay.trade.page.pay.
type pagePayBiz struct {
	OutTradeNo     string `json:"out_trade_no"`
	ProductCode    string `json:"product_code"`
	TotalAmount    string `json:"total_amount"`
	Subject        string `json:"subject"`
	Body           string `json:"body,omitempty"`
	TimeoutExpress string `json:"timeout_express,omitempty"`
}

// tradeBiz is the biz_content of alipay.trade.query and alipay.trade.close.
type tradeBiz struct {
	OutTradeNo string `json:"out_trade_no"`
}

// refundBiz is the biz_content of alipay.trade.refund.
type refundBiz struct {
	OutTradeNo   string `json:"out_trade_no"`
	RefundAmount string `json:"refund_amount"`
	OutRequestNo string `json:"out_request_no"`
	RefundReason string `json:"refund_reason,omitempty"`
}

// queryRefundBiz is the biz_content of the refund query.
type queryRefundBiz struct {
	OutTradeNo   string `json:"out_trade_no"`
	OutRequestNo string `json:"out_request_no"`
}

// responseHead carries the common gateway result fields.
type responseHead struct {
	Code    string `json:"code"`
	Msg     string `json:"msg"`
	SubCode string `json:"sub_code"`
	SubMsg  string `json:"sub_msg"`
}

// failure converts a non-success gateway result into *APIError.
func (h responseHead) failure() error {
	if h.Code == apiSuccessCode && h.SubCode == "" {
		return nil
	}
	return newAPIError(h)
}

// newAPIError builds one safe gateway failure with a Chinese message.
func newAPIError(head responseHead) *APIError {
	detail := safeText(head.SubMsg)
	if detail == "" {
		detail = safeText(head.Msg)
	}
	if detail == "" {
		detail = "接口调用失败"
	}
	message := "支付宝接口返回错误：" + detail
	switch {
	case head.SubCode != "":
		message += "（" + head.SubCode + "）"
	case head.Code != "" && head.Code != apiSuccessCode:
		message += "（code=" + head.Code + "）"
	case head.Code == "":
		message = "支付宝接口返回错误：响应缺少业务代码"
	}
	return &APIError{Code: head.Code, SubCode: head.SubCode, Message: message}
}

// safeText collapses whitespace and truncates one externally supplied text so an
// error message stays short and single-line.
func safeText(value string) string {
	trimmed := strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(trimmed) <= maxErrorTextRunes {
		return trimmed
	}
	runes := []rune(trimmed)
	return string(runes[:maxErrorTextRunes]) + "…"
}

// marshalBiz encodes one biz_content object.
func marshalBiz(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w：无法构造支付宝请求参数", ErrInvalidRequest)
	}
	return string(encoded), nil
}

// timeoutExpress renders timeout_express as "<minutes>m".
func timeoutExpress(minutes int) string {
	if minutes < 1 {
		minutes = DefaultTimeoutMinutes
	}
	return fmt.Sprintf("%dm", minutes)
}

// validateOrderID checks the merchant order number.
func validateOrderID(orderID string) error {
	if strings.TrimSpace(orderID) == "" {
		return fmt.Errorf("%w：订单号不能为空", ErrInvalidRequest)
	}
	if len(orderID) > maxOrderIDLength {
		return fmt.Errorf("%w：订单号超过 %d 个字符", ErrInvalidRequest, maxOrderIDLength)
	}
	if strings.ContainsAny(orderID, " \t\r\n") {
		return fmt.Errorf("%w：订单号不能包含空白字符", ErrInvalidRequest)
	}
	return nil
}

// validateRequestNo checks the merchant refund request number.
func validateRequestNo(requestNo string) error {
	if strings.TrimSpace(requestNo) == "" {
		return fmt.Errorf("%w：退款请求号不能为空", ErrInvalidRequest)
	}
	if len(requestNo) > maxRequestNoLength {
		return fmt.Errorf("%w：退款请求号超过 %d 个字符", ErrInvalidRequest, maxRequestNoLength)
	}
	if strings.ContainsAny(requestNo, " \t\r\n") {
		return fmt.Errorf("%w：退款请求号不能包含空白字符", ErrInvalidRequest)
	}
	return nil
}

// transportReason classifies one transport failure without echoing the request
// URL, the form body or the signature.
func transportReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时"
	case errors.Is(err, context.Canceled):
		return "请求已取消"
	default:
		return "网络请求失败"
	}
}
