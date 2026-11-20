package service

// [OXSCI] Client error contract of the Anthropic Messages surface (P160 T9).
//
// Every error a client receives on POST /v1/messages and
// POST /v1/messages/count_tokens is one of two kinds
// (docs/xsci-client-error-contract.md):
//
//   - (a) the upstream's own standard answer to this request: its status and
//     body are returned unchanged (in a stream: its `event: error` frame), with
//     no error.code;
//   - (b) a conclusion sub2api computed (no account, failover exhausted, limits,
//     billing, the caller's key, local failures): the standard Anthropic
//     envelope with the standard error.type for its status, plus a stable
//     error.code, Retry-After when known and, when upstream answers led to
//     it, the last upstream answer in error.upstream.
//
// The shaping lives in this file and its handler counterpart; upstream
// functions only call into it (one-line hooks), so an upstream rebase meets
// few conflict points. The layer is switched on per request by
// MarkXSciAnthropicSurface (a route middleware); requests without the mark
// keep upstream's behaviour unchanged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Stable error.code values of gateway answers (b). The API-key middleware's
// codes (API_KEY_REQUIRED, INVALID_API_KEY, …) and sub2api's billing reasons
// (USER_RPM_EXCEEDED, …) are kept as they are and listed in the contract.
const (
	XSciCodeModelUnsupported          = "model_unsupported"
	XSciCodeModelNotAllowed           = "model_not_allowed"
	XSciCodeNoAvailableAccounts       = "no_available_accounts"
	XSciCodeUpstreamRateLimited       = "upstream_rate_limited"
	XSciCodeUpstreamOverloaded        = "upstream_overloaded"
	XSciCodeUpstreamUnavailable       = "upstream_unavailable"
	XSciCodeUpstreamAccountError      = "upstream_account_error"
	XSciCodeUpstreamRequestFailed     = "upstream_request_failed"
	XSciCodeUpstreamStreamIncomplete  = "upstream_stream_incomplete"
	XSciCodeStreamReadError           = "stream_read_error"
	XSciCodeStreamTimeout             = "stream_timeout"
	XSciCodeResponseTooLarge          = "response_too_large"
	XSciCodeErrorPassthroughRule      = "error_passthrough_rule"
	XSciCodeGroupUnassigned           = "group_unassigned"
	XSciCodeCountTokensUnsupported    = "count_tokens_unsupported"
	XSciCodeRequestBodyTooLarge       = "request_body_too_large"
	XSciCodeRequestBodyTimeout        = "request_body_timeout"
	XSciCodeRequestBodyUnreadable     = "request_body_unreadable"
	XSciCodeRequestBodyBufferFull     = "request_body_buffer_full"
	XSciCodeRequestBodyEmpty          = "request_body_empty"
	XSciCodeRequestBodyInvalid        = "request_body_invalid"
	XSciCodeModelRequired             = "model_required"
	XSciCodeModelParameters           = "model_parameters_unsupported"
	XSciCodeGatewayConcurrencyLimit   = "gateway_concurrency_limit"
	XSciCodeGatewayQueueFull          = "gateway_queue_full"
	XSciCodeGatewayConcurrencyFailure = "gateway_concurrency_unavailable"
	XSciCodeGatewayError              = "gateway_error"

	// Billing / quota codes shared by the API-key middleware and the billing
	// re-check after the slot wait (one situation, one answer).
	XSciCodeInsufficientBalance = "INSUFFICIENT_BALANCE"
	XSciCodeSubscriptionInvalid = "SUBSCRIPTION_INVALID"
	XSciCodeUsageLimitExceeded  = "USAGE_LIMIT_EXCEEDED"
	XSciCodeAPIKeyQuotaExhaused = "API_KEY_QUOTA_EXHAUSTED"
	XSciCodeBillingError        = "BILLING_ERROR"
)

// Messages of gateway answers that the contract fixes.
const (
	xsciMsgUpstreamRateLimited  = "Upstream rate limit exceeded, please retry later"
	xsciMsgUpstreamOverloaded   = "Upstream service overloaded, please retry later"
	xsciMsgUpstreamUnavailable  = "Upstream service temporarily unavailable"
	xsciMsgUpstreamAccountError = "Upstream account unavailable, please contact administrator"
	xsciMsgUpstreamFailed       = "Upstream request failed"
	xsciMsgStreamIncomplete     = "Upstream stream ended unexpectedly"
)

const (
	xsciSurfaceKey          = "xsci_anthropic_surface"
	xsciStreamErrorEventKey = "xsci_upstream_stream_error_event"
)

// xsciSurfacePaths are the request paths the contract covers.
var xsciSurfacePaths = map[string]struct{}{
	"/v1/messages":              {},
	"/v1/messages/count_tokens": {},
	"/messages/count_tokens":    {},
}

// MarkXSciAnthropicSurface turns the contract layer on for this request when
// its path is on the Anthropic Messages surface. Called by the route
// middleware before any other middleware can answer.
func MarkXSciAnthropicSurface(c *gin.Context) {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return
	}
	if _, ok := xsciSurfacePaths[strings.TrimRight(c.Request.URL.Path, "/")]; ok {
		c.Set(xsciSurfaceKey, true)
	}
}

// XSciAnthropicSurface reports whether the contract layer shapes this
// request's errors.
func XSciAnthropicSurface(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(xsciSurfaceKey)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// XSciStandardErrorType is the Anthropic error.type of an HTTP status
// (Anthropic's error table; 408 and other 4xx read as invalid_request_error,
// 502 / 503 / other 5xx as api_error).
func XSciStandardErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// XSciUpstreamAnswer is the last upstream answer attached to a gateway answer
// as error.upstream. Status is the upstream status as sub2api classified it
// (an `event: error` with overloaded_error before any output counts as 529).
// Message is left out when the answer is about the account itself.
type XSciUpstreamAnswer struct {
	Status    int    `json:"status"`
	Type      string `json:"type,omitempty"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// XSciGatewayError is a gateway answer (b).
type XSciGatewayError struct {
	Status     int
	Code       string
	Message    string
	Upstream   *XSciUpstreamAnswer
	RetryAfter int // whole seconds; 0 = no header
}

type xsciErrorObject struct {
	Type     string              `json:"type"`
	Code     string              `json:"code,omitempty"`
	Message  string              `json:"message"`
	Upstream *XSciUpstreamAnswer `json:"upstream,omitempty"`
}

type xsciErrorBody struct {
	Type      string          `json:"type"`
	Error     xsciErrorObject `json:"error"`
	RequestID string          `json:"request_id,omitempty"`
}

func xsciRequestID(c *gin.Context) string {
	if c == nil || c.Writer == nil {
		return ""
	}
	return strings.TrimSpace(c.Writer.Header().Get("X-Request-ID"))
}

func xsciMarshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","code":"gateway_error","message":"Internal error"}}`)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// xsciStreamStarted reports whether the response is already an SSE stream
// (a keepalive ping or upstream output went out): the status is fixed and an
// error can only be a frame.
func xsciStreamStarted(c *gin.Context) bool {
	return c != nil && c.Writer != nil && c.Writer.Written() &&
		strings.HasPrefix(strings.ToLower(c.Writer.Header().Get("Content-Type")), "text/event-stream")
}

func xsciWriteFrame(c *gin.Context, data []byte) {
	frame := make([]byte, 0, len(data)+24)
	frame = append(frame, "event: error\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	if _, err := c.Writer.Write(frame); err != nil {
		_ = c.Error(err)
	}
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

func xsciWriteJSON(c *gin.Context, status int, body []byte) {
	h := c.Writer.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Del("Content-Length")
	c.Status(status)
	if _, err := c.Writer.Write(body); err != nil {
		_ = c.Error(err)
	}
}

// XSciWriteGatewayError writes a gateway answer (b): JSON with its status, or
// one `event: error` frame when the stream already started.
func XSciWriteGatewayError(c *gin.Context, e XSciGatewayError, streamStarted bool) {
	if c == nil || c.Writer == nil {
		return
	}
	if e.Code == "" {
		e.Code = XSciCodeGatewayError
	}
	errType := XSciStandardErrorType(e.Status)
	payload := xsciMarshal(xsciErrorBody{
		Type:      "error",
		Error:     xsciErrorObject{Type: errType, Code: e.Code, Message: e.Message, Upstream: e.Upstream},
		RequestID: xsciRequestID(c),
	})
	if streamStarted || xsciStreamStarted(c) {
		MarkOpsStreamError(c, errType, e.Message, e.Status)
		xsciWriteFrame(c, payload)
		MarkResponseCommitted(c)
		return
	}
	if e.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	xsciWriteJSON(c, e.Status, payload)
	MarkResponseCommitted(c)
}

// ---- (b) classification of the messages the existing writers pass in ----

var xsciBillingByMessage = func() map[string]*infraerrors.ApplicationError {
	out := map[string]*infraerrors.ApplicationError{}
	for _, e := range []*infraerrors.ApplicationError{
		ErrInsufficientBalance, ErrSubscriptionInvalid, ErrBillingServiceUnavailable,
		ErrDailyLimitExceeded, ErrWeeklyLimitExceeded, ErrMonthlyLimitExceeded,
		ErrAPIKeyRateLimit5hExceeded, ErrAPIKeyRateLimit1dExceeded, ErrAPIKeyRateLimit7dExceeded,
		ErrUserRPMExceeded, ErrGroupRPMExceeded,
		ErrUserPlatformDailyQuotaExhausted, ErrUserPlatformWeeklyQuotaExhausted, ErrUserPlatformMonthlyQuotaExhausted,
	} {
		out[e.Message] = e
	}
	return out
}()

// xsciBillingAnswer maps a billing / quota reason to the contract's status
// and code. Window and rate limits are 429 (waiting helps); spent-out money
// is 402 billing_error; a missing or invalid subscription is 403.
func xsciBillingAnswer(reason string, status int) (int, string) {
	switch reason {
	case "INSUFFICIENT_BALANCE":
		return http.StatusPaymentRequired, XSciCodeInsufficientBalance
	case "SUBSCRIPTION_INVALID", "SUBSCRIPTION_EXPIRED", "SUBSCRIPTION_SUSPENDED":
		return http.StatusForbidden, XSciCodeSubscriptionInvalid
	case "DAILY_LIMIT_EXCEEDED", "WEEKLY_LIMIT_EXCEEDED", "MONTHLY_LIMIT_EXCEEDED", "USAGE_LIMIT_EXCEEDED":
		return http.StatusTooManyRequests, XSciCodeUsageLimitExceeded
	case "BILLING_SERVICE_ERROR":
		return http.StatusServiceUnavailable, reason
	case "API_KEY_QUOTA_EXHAUSTED":
		return http.StatusPaymentRequired, reason
	}
	return status, reason
}

// XSciClassifyGatewayError turns what an existing writer passes (status, its
// error type, an optional code, the message) into the contract's answer.
func XSciClassifyGatewayError(status int, errType, code, message string) XSciGatewayError {
	e := XSciGatewayError{Status: status, Code: code, Message: message}
	if code != "" {
		return e
	}
	if appErr, ok := xsciBillingByMessage[message]; ok {
		e.Status, e.Code = xsciBillingAnswer(appErr.Reason, status)
		return e
	}
	switch {
	case message == "Request body is empty":
		e.Code = XSciCodeRequestBodyEmpty
	case message == "Failed to parse request body":
		e.Code = XSciCodeRequestBodyInvalid
	case message == "model is required":
		e.Code = XSciCodeModelRequired
	case strings.HasPrefix(message, "Request body too large") || message == "Request body is too large":
		e.Code = XSciCodeRequestBodyTooLarge
	case message == "Timed out reading request body":
		e.Code = XSciCodeRequestBodyTimeout
	case message == "Failed to read request body":
		e.Code = XSciCodeRequestBodyUnreadable
	case message == "Request body buffering capacity is exhausted; retry later":
		e.Code = XSciCodeRequestBodyBufferFull
	case message == "Service temporarily unavailable, please retry later":
		e.Code = XSciCodeGatewayConcurrencyFailure
	case strings.HasPrefix(message, "No available accounts"), status == http.StatusServiceUnavailable && message == "Service temporarily unavailable":
		e.Code = XSciCodeNoAvailableAccounts
	case errType == "model_not_found" || (status == http.StatusNotFound && strings.Contains(message, "is not supported by any")):
		e.Status, e.Code = http.StatusNotFound, XSciCodeModelUnsupported
	case status == http.StatusNotFound && strings.HasSuffix(message, "is not available for this group"):
		e.Code = XSciCodeModelNotAllowed
	case strings.HasPrefix(message, "API Key is not assigned to any group"):
		e.Code = XSciCodeGroupUnassigned
	case strings.HasPrefix(message, "count_tokens endpoint is not supported"):
		e.Code = XSciCodeCountTokensUnsupported
	case message == "Request failed" || message == "Failed to read response":
		e.Code = XSciCodeUpstreamUnavailable
	case message == xsciMsgUpstreamFailed || message == "Failed to build request":
		e.Code = XSciCodeUpstreamRequestFailed
	case errType == "billing_error":
		e.Status, e.Code = http.StatusPaymentRequired, XSciCodeBillingError
	default:
		e.Code = XSciCodeGatewayError
	}
	return e
}

var xsciAppErrorString = regexp.MustCompile(`^error: code=\d+ reason="([^"]*)" message=("(?:[^"\\]|\\.)*")`)

// XSciWriteMiddlewareError writes the API-key middleware's answers in the
// envelope: their code moves to error.code unchanged; a Go error string
// (subscription checks) is reduced to its message.
func XSciWriteMiddlewareError(c *gin.Context, status int, code, message string) {
	if m := xsciAppErrorString.FindStringSubmatch(message); m != nil {
		if unquoted, err := strconv.Unquote(m[2]); err == nil {
			message = unquoted
		}
	}
	switch code {
	case XSciCodeInsufficientBalance, XSciCodeAPIKeyQuotaExhaused:
		status = http.StatusPaymentRequired
	}
	XSciWriteGatewayError(c, XSciGatewayError{Status: status, Code: code, Message: message}, false)
}

// ---- (a) the upstream's own answer ----

// XSciIsAccountScopedUpstreamError reports whether an upstream answer is about
// the account's own credential or standing (bad credential, permission,
// payment, credit balance, disabled organization). Such answers never reach
// the client as they are: the caller's key is fine and the text can describe
// the account.
func XSciIsAccountScopedUpstreamError(status int, body []byte) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		msg := strings.ToLower(extractUpstreamErrorMessage(body))
		return strings.Contains(msg, "credit balance") ||
			strings.Contains(msg, "organization has been disabled") ||
			strings.Contains(msg, "identity verification is required")
	}
	return false
}

func xsciIsStandardErrorBody(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	r := gjson.ParseBytes(body)
	return r.Get("type").String() == "error" &&
		r.Get("error.type").Type == gjson.String &&
		r.Get("error.message").Type == gjson.String
}

func xsciUpstreamRequestID(header http.Header, body []byte) string {
	if id := strings.TrimSpace(gjson.GetBytes(body, "request_id").String()); id != "" {
		return id
	}
	if header == nil {
		return ""
	}
	for _, k := range []string{"request-id", "x-request-id", "x-amzn-requestid"} {
		if id := strings.TrimSpace(header.Get(k)); id != "" {
			return id
		}
	}
	return ""
}

// xsciUpstreamAnswerOf summarises an upstream answer for error.upstream.
func xsciUpstreamAnswerOf(status int, header http.Header, body []byte) *XSciUpstreamAnswer {
	a := &XSciUpstreamAnswer{
		Status:    status,
		Type:      strings.TrimSpace(gjson.GetBytes(body, "error.type").String()),
		RequestID: xsciUpstreamRequestID(header, body),
	}
	if !XSciIsAccountScopedUpstreamError(status, body) {
		a.Message = sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	}
	return a
}

// XSciUpstreamAnswerFromFailover is the upstream answer behind a failover
// error, or nil when no upstream answered (transport failure, stream broken
// before its first byte: sub2api's own placeholders).
func XSciUpstreamAnswerFromFailover(fe *UpstreamFailoverError) *XSciUpstreamAnswer {
	if fe == nil || len(fe.ResponseBody) == 0 || bytes.Equal(fe.ResponseBody, gatewayTransportFailoverBody) {
		return nil
	}
	if gjson.GetBytes(fe.ResponseBody, "error.type").String() == "upstream_disconnected" {
		return nil
	}
	return xsciUpstreamAnswerOf(fe.StatusCode, fe.ResponseHeaders, fe.ResponseBody)
}

// xsciWriteUpstreamAnswer answers with the upstream's own non-failover answer:
// (a) as it is, or — when it is about the account — the gateway answer
// upstream_account_error. It returns false (nothing written) off the surface.
func xsciWriteUpstreamAnswer(c *gin.Context, account *Account, status int, header http.Header, body []byte) bool {
	if !XSciAnthropicSurface(c) {
		return false
	}
	MarkResponseCommitted(c)
	if XSciIsAccountScopedUpstreamError(status, body) {
		XSciWriteGatewayError(c, XSciGatewayError{
			Status: http.StatusBadGateway, Code: XSciCodeUpstreamAccountError, Message: xsciMsgUpstreamAccountError,
			Upstream: xsciUpstreamAnswerOf(status, header, body),
		}, false)
		return true
	}
	streamStarted := xsciStreamStarted(c)
	if xsciIsStandardErrorBody(body) {
		if streamStarted {
			var compact bytes.Buffer
			if err := json.Compact(&compact, body); err == nil {
				MarkOpsStreamError(c, gjson.GetBytes(body, "error.type").String(), extractUpstreamErrorMessage(body), status)
				xsciWriteFrame(c, compact.Bytes())
				return true
			}
		} else {
			xsciWriteJSON(c, status, body)
			return true
		}
	}
	// Not an Anthropic envelope (Bedrock, a relay): the upstream's status and
	// message in the standard envelope, still without a code.
	outStatus := status
	if account != nil && account.IsBedrock() && isBedrockContentFilterError(status, body) {
		outStatus = http.StatusBadRequest // a content-filter verdict is about the request
	}
	msg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if msg == "" {
		msg = http.StatusText(status)
	}
	errType := XSciStandardErrorType(outStatus)
	payload := xsciMarshal(xsciErrorBody{
		Type:      "error",
		Error:     xsciErrorObject{Type: errType, Message: msg},
		RequestID: xsciUpstreamRequestID(header, body),
	})
	if streamStarted {
		MarkOpsStreamError(c, errType, msg, outStatus)
		xsciWriteFrame(c, payload)
		return true
	}
	xsciWriteJSON(c, outStatus, payload)
	return true
}

// xsciUpstreamAnswerError is what Forward returns after an upstream answer
// was written (for logs; the handler does not write again).
func xsciUpstreamAnswerError(status int, body []byte) error {
	msg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if msg == "" {
		return errors.New("upstream error: " + strconv.Itoa(status) + " (answered as is)")
	}
	return errors.New("upstream error: " + strconv.Itoa(status) + " (answered as is) message=" + msg)
}

// ---- streams ----

// xsciNoteUpstreamStreamErrorEvent records the upstream's `event: error` data
// and, once output has started, forwards that frame to the client as it is.
func xsciNoteUpstreamStreamErrorEvent(c *gin.Context, raw string, outputStarted bool) {
	if !XSciAnthropicSurface(c) {
		return
	}
	c.Set(xsciStreamErrorEventKey, raw)
	if outputStarted && !IsResponseCommitted(c) {
		XSciForwardUpstreamStreamErrorEvent(c, []byte(raw))
	}
}

// XSciUpstreamStreamErrorEvent returns the data of the last upstream
// `event: error` seen in this request.
func XSciUpstreamStreamErrorEvent(c *gin.Context) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.Get(xsciStreamErrorEventKey)
	if !ok {
		return nil, false
	}
	s, _ := v.(string)
	return []byte(s), s != ""
}

// XSciForwardUpstreamStreamErrorEvent writes the upstream's `event: error`
// frame to the client unchanged (a), in a 200 text/event-stream response.
func XSciForwardUpstreamStreamErrorEvent(c *gin.Context, data []byte) {
	if !c.Writer.Written() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
	}
	MarkOpsStreamError(c, gjson.GetBytes(data, "error.type").String(), extractUpstreamErrorMessage(data), http.StatusBadGateway)
	xsciWriteFrame(c, data)
	MarkResponseCommitted(c)
}

// xsciWriteStreamFailure is the one error frame of a stream that sub2api ends
// after output started (read error, idle, line too long). reason is the code.
func xsciWriteStreamFailure(c *gin.Context, reason, message string) {
	XSciWriteGatewayError(c, XSciGatewayError{Status: http.StatusInternalServerError, Code: reason, Message: message}, true)
}

// XSciUpstreamOutputStarted reports whether upstream output (message_start)
// already went to the client in this request.
func XSciUpstreamOutputStarted(c *gin.Context) bool {
	return observedUpstreamResponseModel(c) != ""
}

// XSciWriteForwardFailed is the answer when Forward failed without writing
// one: the stream ended early once output started, else the request could not
// be completed.
func XSciWriteForwardFailed(c *gin.Context, streamStarted bool) {
	if XSciUpstreamOutputStarted(c) && (streamStarted || xsciStreamStarted(c)) {
		XSciWriteGatewayError(c, XSciGatewayError{Status: http.StatusBadGateway, Code: XSciCodeUpstreamStreamIncomplete, Message: xsciMsgStreamIncomplete}, true)
		return
	}
	XSciWriteGatewayError(c, XSciGatewayError{Status: http.StatusBadGateway, Code: XSciCodeUpstreamRequestFailed, Message: xsciMsgUpstreamFailed}, streamStarted)
}

// xsciWriteRequestRuleError answers a request sub2api rejects before sending
// because the model does not take its parameters.
func xsciWriteRequestRuleError(c *gin.Context, err error) {
	XSciWriteGatewayError(c, XSciGatewayError{Status: http.StatusBadRequest, Code: XSciCodeModelParameters, Message: err.Error()}, false)
}

// ---- failover exhausted ----

// XSciFailoverAnswer is the gateway answer once failover ran out, from the
// last upstream answer. model is the requested model (for the message).
func XSciFailoverAnswer(fe *UpstreamFailoverError, modelUnsupported bool, model string) XSciGatewayError {
	up := XSciUpstreamAnswerFromFailover(fe)
	status := 0
	var body []byte
	if fe != nil {
		status, body = fe.StatusCode, fe.ResponseBody
	}
	e := XSciGatewayError{Status: http.StatusBadGateway, Code: XSciCodeUpstreamRequestFailed, Message: xsciMsgUpstreamFailed, Upstream: up}
	switch {
	case status == http.StatusNotFound && isUpstreamModelNotFoundError(status, body) && modelUnsupported:
		e.Status, e.Code = http.StatusNotFound, XSciCodeModelUnsupported
		e.Message = XSciModelNotFoundMessage(model)
	case status == http.StatusTooManyRequests:
		e.Status, e.Code, e.Message = http.StatusTooManyRequests, XSciCodeUpstreamRateLimited, xsciMsgUpstreamRateLimited
	case status == 529:
		e.Status, e.Code, e.Message = 529, XSciCodeUpstreamOverloaded, xsciMsgUpstreamOverloaded
	case XSciIsAccountScopedUpstreamError(status, body):
		e.Code, e.Message = XSciCodeUpstreamAccountError, xsciMsgUpstreamAccountError
	case status >= 500:
		e.Code, e.Message = XSciCodeUpstreamUnavailable, xsciMsgUpstreamUnavailable
	}
	return e
}

// XSciModelNotFoundMessage is the message of model_unsupported when the
// upstream reported the model as not found on every account: the same text as
// when no account is configured for the model (classifyNoAccountError), so a
// model_unsupported answer reads the same whichever way it was found.
func XSciModelNotFoundMessage(model string) string {
	return "Model " + strconv.Quote(model) + " is not supported by any configured account in this group"
}

// XSciIsUpstreamModelNotFound reports whether a failover error is an upstream
// "model not found".
func XSciIsUpstreamModelNotFound(fe *UpstreamFailoverError) bool {
	return fe != nil && isUpstreamModelNotFoundError(fe.StatusCode, fe.ResponseBody)
}

// ---- model unsupported across the group ----

// XSciModelNotFoundOnEveryAccount reports whether every persistently enabled
// account of the group that is configured for requestedModel is cooling the
// model down because the upstream answered "model not found" for it
// (upstreamModelNotFoundReason, HandleUpstreamModelNotFound). The scheduler
// skips such an account for the model until the cooldown ends; when all of
// them are, the model is unusable through this group, not just busy.
func (s *GatewayService) XSciModelNotFoundOnEveryAccount(ctx context.Context, groupID *int64, requestedModel, platform string) bool {
	requestedModel = strings.TrimSpace(requestedModel)
	if s == nil || requestedModel == "" {
		return false
	}
	candidates, err := s.xsciModelCandidates(ctx, groupID, requestedModel, platform)
	if err != nil || len(candidates) == 0 {
		return false
	}
	now := time.Now()
	for _, acc := range candidates {
		if !xsciModelNotFoundCooling(ctx, acc, requestedModel, now) {
			return false
		}
	}
	return true
}

func xsciModelNotFoundCooling(ctx context.Context, a *Account, requestedModel string, now time.Time) bool {
	for _, key := range a.modelRateLimitKeysForRequest(ctx, requestedModel) {
		resetAt := a.modelRateLimitResetAt(key)
		if resetAt != nil && now.Before(*resetAt) && a.modelRateLimitReason(key) == upstreamModelNotFoundReason {
			return true
		}
	}
	return false
}
