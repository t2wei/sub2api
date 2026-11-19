//go:build unit

package handler

// [OXSCI] Client error contract (P160 T9).
//
// docs/xsci-client-error-contract.md lists every error sub2api returns to a
// client on the Anthropic surface (/v1/messages, /v1/messages/count_tokens).
// This file holds the same rows as Go (ceContract) and
//
//   - TestXSciClientErrorContract drives every row through the real code path
//     (the /v1 middleware chain + GatewayHandler + GatewayService with a
//     scripted upstream; a few rows at the closest function, see "direct")
//     and checks what the client sees: status, body envelope, error.type,
//     error.code, message, stream frame and Retry-After;
//   - TestXSciClientErrorContractMatchesDoc parses the doc table and requires
//     it to equal ceContract cell for cell, so the doc and the code cannot
//     drift apart: changing either alone fails a test.
//
// To change the contract, change the code, ceContract and the doc together.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Body envelopes (doc §1).
const (
	ceBodyAnthropic = "anthropic" // {"type":"error","error":{"type","message"[,"code"]}}
	ceBodyFlat      = "flat"      // {"code","message"}
	ceBodyUpstream  = "upstream"  // upstream's JSON body, byte for byte
	ceBodyNone      = "none"      // no body
)

// Stream forms (doc §1): what a stream:true request receives.
const (
	ceStreamJSON         = "JSON"
	ceStreamJSONData     = "JSON; `data:` frame after ping"
	ceStreamJSONGinEvent = "JSON; `event:error` frame after ping"
	ceStreamJSONEither   = "JSON; `event:error` or `data:` frame after ping"
	ceStreamData         = "`data:` frame"
	ceStreamEventData    = "`event: error` frame, then `data:` frame"
	ceStreamNone         = "none"
)

// Observed wire forms a single drive asserts.
const (
	ceFormJSON       = "json"
	ceFormVerbatim   = "verbatim"
	ceFormData       = "data-frame"
	ceFormEvent      = "event-frame+data-frame"
	ceFormGinEvent   = "gin-event-frame"
	ceFormEmpty      = "empty"
	ceStreamFallback = `data: {"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`
)

// Other doc cell values.
const (
	ceUpstreamText = "(upstream text)"
	ceUpstreamType = "(upstream type)"
	ceNo           = "no"
	ceYes          = "yes"
	ceWhenKnown    = "when known"
	ceDash         = "—"
)

type ceRow struct {
	ID         string
	HTTP       string
	Body       string
	Type       string
	Code       string
	Message    string
	Stream     string
	RetryAfter string
	drives     []ceDrive
}

type ceDrive struct {
	name   string
	env    func(e *ceEnv)
	req    ceRequest
	direct func(t *testing.T) *httptest.ResponseRecorder // closest-function drive
	want   ceWant
	check  func(t *testing.T, e *ceEnv, rec *httptest.ResponseRecorder) // extra row-specific facts
}

type ceWant struct {
	status     int
	form       string
	message    string // concrete message ("" = row.Message as is)
	errType    string // concrete error.type ("" = row.Type)
	raw        string // verbatim body
	retryAfter string // "" = absent, "any" = a positive integer, else exact
}

// Upstream bodies and streams used by the drives.
const (
	ceUp400          = `{"type":"error","error":{"type":"invalid_request_error","message":"The provided model identifier is invalid."},"request_id":"req_011CUp400"}`
	ceUpRequest429   = `{"type":"error","error":{"type":"rate_limit_error","message":"Error"},"request_id":"req_011CUpReq429"}`
	ceUpAccount429   = `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`
	ceUp529          = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	ceUp500          = `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`
	ceUp401          = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	ceUp403          = `{"type":"error","error":{"type":"permission_error","message":"Request not allowed"}}`
	ceUp404          = `{"type":"error","error":{"type":"not_found_error","message":"model: claude-sonnet-5"}}`
	ceUp413          = `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`
	ceUp402          = `{"error":{"message":"OpenRouter could not verify available credits","code":402}}`
	ceBedrockFilter  = `{"message":"The model response was blocked by content filtering policy."}`
	ceContentFilter  = "Output blocked by content filtering policy"
	ceUnableComplete = "Unable to complete this request right now. Please try again."
)

const ceStreamStart = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":12,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n"

func ceSSEError(errType, message string) string {
	return "event: error\n" + `data: {"type":"error","error":{"type":"` + errType + `","message":"` + message + `"}}` + "\n\n"
}

func ceJSON(status int, body string) ceReply { return ceReply{status: status, body: body} }

// cePingFirst makes a stream:true request wait for a user slot long enough for
// sub2api to send keepalive pings (HTTP 200 + text/event-stream) first.
func cePingFirst(e *ceEnv) {
	e.pingEvery = 20 * time.Millisecond
	calls := 0
	e.conc.userFree = func() bool {
		calls++
		return calls > 2
	}
}

func ceWithRule1(e *ceEnv) { e.rules = []*model.ErrorPassthroughRule{prodRule1()} }

func ceSecondAccount(e *ceEnv) { e.accounts = append(e.accounts, e.setupTokenAccount(9712)) }

func ceAPIKeyAccountOnly(e *ceEnv) { e.accounts = []*service.Account{e.apiKeyAccount(9721)} }

func ceStream() ceRequest { return ceRequest{stream: true} }

func ceCountTokens() ceRequest {
	return ceRequest{path: "/v1/messages/count_tokens", body: `{"model":"` + ceModel + `","messages":[{"role":"user","content":"hello"}]}`}
}

// ceDirectBilling drives the handler's billing-error path at the closest
// function: billingErrorDetails maps the error, the handler writes it.
func ceDirectBilling(err error) func(t *testing.T) *httptest.ResponseRecorder {
	return func(t *testing.T) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		(&GatewayHandler{}).handleStreamingAwareError(c, status, code, message, false)
		return rec
	}
}

func ceQuotaErr(base *infraerrors.ApplicationError) error {
	return base.WithMetadata(map[string]string{"window_resets_at": time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)})
}

// ceDirectUserSlotTimeout drives the user-slot wait at the closest function:
// the handler's 30 s wait (maxConcurrencyWait) is shortened to 60 ms.
func ceDirectUserSlotTimeout(stream bool) func(t *testing.T) *httptest.ResponseRecorder {
	return func(t *testing.T) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		conc := &ceConcurrencyCache{userFree: func() bool { return false }}
		h := &GatewayHandler{concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(conc), SSEPingFormatClaude, 15*time.Millisecond)}
		streamStarted := false
		_, err := h.concurrencyHelper.acquireUserSlotWithWaitTimeout(c, ceUserID, 1, 60*time.Millisecond, stream, &streamStarted)
		require.Error(t, err)
		h.handleConcurrencyError(c, err, "user", streamStarted)
		return rec
	}
}

var ceContract = []ceRow{
	// ---- API key (auth middleware): flat {"code","message"}, never SSE ----
	{ID: "E-AUTH-MISSING-KEY", HTTP: "401", Body: ceBodyFlat, Type: ceDash, Code: "API_KEY_REQUIRED",
		Message: "API key is required in Authorization header (Bearer scheme), x-api-key header, or x-goog-api-key header",
		Stream:  ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{req: ceRequest{noKey: true, stream: true}, want: ceWant{status: 401, form: ceFormJSON}}}},
	{ID: "E-AUTH-INVALID-KEY", HTTP: "401", Body: ceBodyFlat, Type: ceDash, Code: "INVALID_API_KEY",
		Message: "Invalid API key", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "unknown key", req: ceRequest{key: "sk-unknown"}, want: ceWant{status: 401, form: ceFormJSON}},
			{name: "oversized credential", req: ceRequest{key: strings.Repeat("k", service.MaxAPIKeyCredentialBytes+1)}, want: ceWant{status: 401, form: ceFormJSON}},
		}},
	{ID: "E-AUTH-KEY-IN-QUERY", HTTP: "400", Body: ceBodyFlat, Type: ceDash, Code: "api_key_in_query_deprecated",
		Message: "API key in query parameter is deprecated. Please use Authorization header instead.", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{req: ceRequest{query: "key=" + ceAPIKey}, want: ceWant{status: 400, form: ceFormJSON}}}},
	{ID: "E-AUTH-TOO-MANY-INVALID", HTTP: "429", Body: ceBodyFlat, Type: ceDash, Code: "INVALID_AUTH_RATE_LIMITED",
		Message: "Too many invalid authentication attempts; retry later", Stream: ceStreamJSON, RetryAfter: ceYes,
		drives: []ceDrive{{name: "after repeated invalid keys from one IP", direct: ceDirectInvalidAuthLimiter, want: ceWant{status: 429, form: ceFormJSON, retryAfter: "any"}}}},
	{ID: "E-AUTH-LOOKUP-OVERLOADED", HTTP: "503", Body: ceBodyFlat, Type: ceDash, Code: "API_KEY_AUTH_OVERLOADED",
		Message: "API key authentication is temporarily unavailable", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.keyLookErr = service.ErrAPIKeyAuthOverloaded }, want: ceWant{status: 503, form: ceFormJSON}}}},
	{ID: "E-AUTH-LOOKUP-FAILED", HTTP: "500", Body: ceBodyFlat, Type: ceDash, Code: "INTERNAL_ERROR",
		Message: "Failed to validate API key", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.keyLookErr = errors.New("db down") }, want: ceWant{status: 500, form: ceFormJSON}}}},
	{ID: "E-AUTH-KEY-DISABLED", HTTP: "401", Body: ceBodyFlat, Type: ceDash, Code: "API_KEY_DISABLED",
		Message: "API key is disabled", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.key.Status = service.StatusDisabled }, want: ceWant{status: 401, form: ceFormJSON}}}},
	{ID: "E-AUTH-IP-DENIED", HTTP: "403", Body: ceBodyFlat, Type: ceDash, Code: "ACCESS_DENIED",
		Message: "Access denied. Your IP is {ip}", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.key.IPWhitelist = []string{"10.9.9.9"} },
			want: ceWant{status: 403, form: ceFormJSON, message: "Access denied. Your IP is 192.0.2.1"}}}},
	{ID: "E-AUTH-USER-INACTIVE", HTTP: "401", Body: ceBodyFlat, Type: ceDash, Code: "USER_INACTIVE",
		Message: "User account is not active", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.user.Status = service.StatusDisabled }, want: ceWant{status: 401, form: ceFormJSON}}}},
	{ID: "E-AUTH-GROUP-DELETED", HTTP: "403", Body: ceBodyFlat, Type: ceDash, Code: "GROUP_DELETED",
		Message: "API Key 所属分组已删除", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.group.Status = "deleted" }, want: ceWant{status: 403, form: ceFormJSON}}}},
	{ID: "E-AUTH-GROUP-DISABLED", HTTP: "403", Body: ceBodyFlat, Type: ceDash, Code: "GROUP_DISABLED",
		Message: "API Key 所属分组已停用", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.group.Status = service.StatusDisabled }, want: ceWant{status: 403, form: ceFormJSON}}}},
	{ID: "E-AUTH-GROUP-NOT-ALLOWED", HTTP: "403", Body: ceBodyFlat, Type: ceDash, Code: "GROUP_NOT_ALLOWED",
		Message: "API Key 所属专属分组不再允许当前用户使用", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.group.IsExclusive = true }, want: ceWant{status: 403, form: ceFormJSON}}}},
	{ID: "E-AUTH-NO-SUBSCRIPTION", HTTP: "403", Body: ceBodyFlat, Type: ceDash, Code: "SUBSCRIPTION_NOT_FOUND",
		Message: "No active subscription found for this group", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.group.SubscriptionType = service.SubscriptionTypeSubscription }, want: ceWant{status: 403, form: ceFormJSON}}}},
	{ID: "E-AUTH-KEY-QUOTA-EXHAUSTED", HTTP: "429", Body: ceBodyFlat, Type: ceDash, Code: "API_KEY_QUOTA_EXHAUSTED",
		Message: "API key 额度已用完", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.key.Status = service.StatusAPIKeyQuotaExhausted }, want: ceWant{status: 429, form: ceFormJSON}}}},
	{ID: "E-AUTH-KEY-EXPIRED", HTTP: "403", Body: ceBodyFlat, Type: ceDash, Code: "API_KEY_EXPIRED",
		Message: "API key 已过期", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { past := time.Now().Add(-time.Hour); e.key.ExpiresAt = &past }, want: ceWant{status: 403, form: ceFormJSON}}}},
	{ID: "E-AUTH-SUBSCRIPTION-INVALID", HTTP: "403", Body: ceBodyFlat, Type: ceDash, Code: "SUBSCRIPTION_INVALID",
		Message: "{subscription error}", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.group.SubscriptionType = service.SubscriptionTypeSubscription
			e.sub = ceSubscription(e)
			e.sub.Status = service.SubscriptionStatusSuspended
		}, want: ceWant{status: 403, form: ceFormJSON, message: service.ErrSubscriptionSuspended.Error()}}}},
	{ID: "E-AUTH-SUBSCRIPTION-LIMIT", HTTP: "429", Body: ceBodyFlat, Type: ceDash, Code: "USAGE_LIMIT_EXCEEDED",
		Message: "{subscription error}", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.group.SubscriptionType = service.SubscriptionTypeSubscription
			limit := 5.0
			e.group.DailyLimitUSD = &limit
			e.sub = ceSubscription(e)
			e.sub.DailyUsageUSD = 6
		}, want: ceWant{status: 429, form: ceFormJSON, message: service.ErrDailyLimitExceeded.Error()}}}},
	{ID: "E-AUTH-SUBSCRIPTION-MAINTENANCE", HTTP: "500", Body: ceBodyFlat, Type: ceDash, Code: "SUBSCRIPTION_MAINTENANCE_FAILED",
		Message: "Failed to maintain subscription usage windows", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.group.SubscriptionType = service.SubscriptionTypeSubscription
			e.sub = ceSubscription(e)
			old := time.Now().Add(-72 * time.Hour)
			e.sub.DailyWindowStart = &old
		}, want: ceWant{status: 500, form: ceFormJSON}}}},
	{ID: "E-AUTH-NO-BALANCE", HTTP: "403", Body: ceBodyFlat, Type: ceDash, Code: "INSUFFICIENT_BALANCE",
		Message: "Insufficient account balance", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.user.Balance = 0 }, want: ceWant{status: 403, form: ceFormJSON}}}},

	// ---- Group gate: Anthropic envelope, never SSE ----
	{ID: "E-GROUP-UNASSIGNED", HTTP: "403", Body: ceBodyAnthropic, Type: "permission_error", Code: ceDash,
		Message: "API Key is not assigned to any group and cannot be used. Please contact the administrator to assign it to a group.",
		Stream:  ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.key.GroupID = nil; e.key.Group = nil }, req: ceStream(), want: ceWant{status: 403, form: ceFormJSON}}}},
	{ID: "E-GROUP-MODEL-NOT-ALLOWED", HTTP: "404", Body: ceBodyAnthropic, Type: "not_found_error", Code: ceDash,
		Message: `Model "{model}" is not available for this group`, Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"claude-opus-5"}}
		}, req: ceStream(), want: ceWant{status: 404, form: ceFormJSON, message: `Model "claude-sonnet-5" is not available for this group`}}}},

	// ---- Request checks: Anthropic envelope, never SSE ----
	{ID: "E-REQ-BODY-TOO-LARGE", HTTP: "413", Body: ceBodyAnthropic, Type: "invalid_request_error", Code: ceDash,
		Message: "Request body too large, limit is {limit}", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.cfg.Gateway.MaxBodySize = 1 << 10 },
			req:  ceRequest{body: `{"model":"` + ceModel + `","max_tokens":32,"messages":[{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}]}`},
			want: ceWant{status: 413, form: ceFormJSON, message: "Request body too large, limit is 1024B"}}}},
	{ID: "E-REQ-BODY-READ-TIMEOUT", HTTP: "408", Body: ceBodyAnthropic, Type: "invalid_request_error", Code: ceDash,
		Message: "Timed out reading request body", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{req: ceRequest{reader: io.MultiReader(strings.NewReader(`{"model":`), ceErrReader{os.ErrDeadlineExceeded})}, want: ceWant{status: 408, form: ceFormJSON}}}},
	{ID: "E-REQ-BODY-READ-FAILED", HTTP: "400", Body: ceBodyAnthropic, Type: "invalid_request_error", Code: ceDash,
		Message: "Failed to read request body", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{req: ceRequest{reader: io.MultiReader(strings.NewReader(`{"model":`), ceErrReader{io.ErrUnexpectedEOF})}, want: ceWant{status: 400, form: ceFormJSON}}}},
	{ID: "E-REQ-BODY-BUSY", HTTP: "503", Body: ceBodyAnthropic, Type: "api_error", Code: ceDash,
		Message: "Request body buffering capacity is exhausted; retry later", Stream: ceStreamJSON, RetryAfter: ceYes,
		drives: []ceDrive{{env: func(e *ceEnv) { e.cfg.Gateway.RequestBodyMemoryBudgetBytes = 64 }, want: ceWant{status: 503, form: ceFormJSON, retryAfter: "1"}}}},
	{ID: "E-REQ-BODY-EMPTY", HTTP: "400", Body: ceBodyAnthropic, Type: "invalid_request_error", Code: ceDash,
		Message: "Request body is empty", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{req: ceRequest{empty: true}, want: ceWant{status: 400, form: ceFormJSON}}}},
	{ID: "E-REQ-BODY-INVALID", HTTP: "400", Body: ceBodyAnthropic, Type: "invalid_request_error", Code: ceDash,
		Message: "Failed to parse request body", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{req: ceRequest{body: `{"model":`}, want: ceWant{status: 400, form: ceFormJSON}}}},
	{ID: "E-REQ-MODEL-MISSING", HTTP: "400", Body: ceBodyAnthropic, Type: "invalid_request_error", Code: ceDash,
		Message: "model is required", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{req: ceRequest{body: `{"max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`}, want: ceWant{status: 400, form: ceFormJSON}}}},
	{ID: "E-REQ-MODEL-RULE", HTTP: "400", Body: ceBodyAnthropic, Type: "invalid_request_error", Code: ceDash,
		Message: "{model rule}", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{req: ceRequest{body: `{"model":"claude-sonnet-5-5","max_tokens":32,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hello"}]}`},
			want: ceWant{status: 400, form: ceFormJSON, message: "claude-sonnet-5-5 requires adaptive thinking or thinking.type=between_tools; omit thinking or use one of those modes"},
			check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
				require.Zero(t, e.upstream.calls, "rejected before any upstream call")
			}}}},

	// ---- Capacity and limits before any upstream call ----
	{ID: "E-LIMIT-USER-CONCURRENCY", HTTP: "429", Body: ceBodyAnthropic, Type: "rate_limit_error", Code: "gateway_concurrency_limit",
		Message: "Concurrency limit exceeded for user, please retry later", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "non-stream, waited for a slot", direct: ceDirectUserSlotTimeout(false), want: ceWant{status: 429, form: ceFormJSON}},
			{name: "stream, pings sent while waiting", direct: ceDirectUserSlotTimeout(true), want: ceWant{status: 200, form: ceFormData}},
		}},
	{ID: "E-LIMIT-ACCOUNT-CONCURRENCY", HTTP: "429", Body: ceBodyAnthropic, Type: "rate_limit_error", Code: "gateway_concurrency_limit",
		Message: "Concurrency limit exceeded for account, please retry later", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.conc.accountFree = func() bool { return false }
			e.cfg.Gateway.Scheduling.FallbackWaitTimeout = 60 * time.Millisecond
			e.cfg.Gateway.Scheduling.FallbackMaxWaiting = 10
		}, want: ceWant{status: 429, form: ceFormJSON}}}},
	{ID: "E-LIMIT-QUEUE-FULL", HTTP: "429", Body: ceBodyAnthropic, Type: "rate_limit_error", Code: "gateway_queue_full",
		Message: "Too many pending requests, please retry later", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "user wait queue full", env: func(e *ceEnv) {
				e.conc.userFree = func() bool { return false }
				e.conc.userWaitFull = true
			}, req: ceStream(), want: ceWant{status: 429, form: ceFormJSON}},
			{name: "account wait queue full", env: func(e *ceEnv) {
				e.conc.accountFree = func() bool { return false }
				e.conc.accountFull = true
				e.cfg.Gateway.Scheduling.FallbackWaitTimeout = time.Second
				e.cfg.Gateway.Scheduling.FallbackMaxWaiting = 1
			}, want: ceWant{status: 429, form: ceFormJSON}},
		}},
	{ID: "E-LIMIT-SLOT-ERROR", HTTP: "503", Body: ceBodyAnthropic, Type: "api_error", Code: ceDash,
		Message: "Service temporarily unavailable, please retry later", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.conc.userSlotError = errors.New("redis: connection refused") }, req: ceStream(), want: ceWant{status: 503, form: ceFormJSON}}}},

	// ---- Billing checks after the slot wait: error.type carries a sub2api code ----
	{ID: "E-BILLING-UNAVAILABLE", HTTP: "503", Body: ceBodyAnthropic, Type: "billing_service_error", Code: ceDash,
		Message: "Billing service temporarily unavailable. Please retry later.", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{{direct: ceDirectBilling(service.ErrBillingServiceUnavailable.WithCause(errors.New("redis down"))), want: ceWant{status: 503, form: ceFormJSON}}}},
	{ID: "E-BILLING-DENIED", HTTP: "403", Body: ceBodyAnthropic, Type: "billing_error", Code: ceDash,
		Message: "{billing error}", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "balance used up while waiting", env: func(e *ceEnv) { e.billing.balance = 0 }, req: ceStream(), want: ceWant{status: 403, form: ceFormJSON, message: service.ErrInsufficientBalance.Message}},
			{name: "balance used up, after ping", env: func(e *ceEnv) { e.billing.balance = 0; cePingFirst(e) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormData, message: service.ErrInsufficientBalance.Message}},
			{name: "subscription limit reached while waiting", direct: ceDirectBilling(service.ErrDailyLimitExceeded), want: ceWant{status: 403, form: ceFormJSON, message: service.ErrDailyLimitExceeded.Message}},
			{name: "subscription invalid", direct: ceDirectBilling(service.ErrSubscriptionInvalid), want: ceWant{status: 403, form: ceFormJSON, message: service.ErrSubscriptionInvalid.Message}},
		}},
	{ID: "E-BILLING-KEY-WINDOW", HTTP: "429", Body: ceBodyAnthropic, Type: "rate_limit_exceeded", Code: ceDash,
		Message: "{key window}", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "5h", direct: ceDirectBilling(service.ErrAPIKeyRateLimit5hExceeded), want: ceWant{status: 429, form: ceFormJSON, message: "api key 5小时限额已用完"}},
			{name: "1d", direct: ceDirectBilling(service.ErrAPIKeyRateLimit1dExceeded), want: ceWant{status: 429, form: ceFormJSON, message: "api key 日限额已用完"}},
			{name: "7d", direct: ceDirectBilling(service.ErrAPIKeyRateLimit7dExceeded), want: ceWant{status: 429, form: ceFormJSON, message: "api key 7天限额已用完"}},
		}},
	{ID: "E-BILLING-RPM", HTTP: "429", Body: ceBodyAnthropic, Type: "rate_limit_exceeded", Code: ceDash,
		Message: "{rpm scope} requests-per-minute limit exceeded", Stream: ceStreamJSONData, RetryAfter: ceYes,
		drives: []ceDrive{
			{name: "user", direct: ceDirectBilling(service.ErrUserRPMExceeded), want: ceWant{status: 429, form: ceFormJSON, message: "user requests-per-minute limit exceeded", retryAfter: "any"}},
			{name: "group", direct: ceDirectBilling(service.ErrGroupRPMExceeded), want: ceWant{status: 429, form: ceFormJSON, message: "group requests-per-minute limit exceeded", retryAfter: "any"}},
		}},
	{ID: "E-BILLING-PLATFORM-QUOTA", HTTP: "429", Body: ceBodyAnthropic, Type: "rate_limit_exceeded", Code: ceDash,
		Message: "{Daily/Weekly/Monthly} usage quota exhausted for this platform.", Stream: ceStreamJSONData, RetryAfter: ceYes,
		drives: []ceDrive{
			{name: "daily", direct: ceDirectBilling(ceQuotaErr(service.ErrUserPlatformDailyQuotaExhausted)), want: ceWant{status: 429, form: ceFormJSON, message: "Daily usage quota exhausted for this platform.", retryAfter: "any"}},
			{name: "weekly", direct: ceDirectBilling(ceQuotaErr(service.ErrUserPlatformWeeklyQuotaExhausted)), want: ceWant{status: 429, form: ceFormJSON, message: "Weekly usage quota exhausted for this platform.", retryAfter: "any"}},
			{name: "monthly", direct: ceDirectBilling(ceQuotaErr(service.ErrUserPlatformMonthlyQuotaExhausted)), want: ceWant{status: 429, form: ceFormJSON, message: "Monthly usage quota exhausted for this platform.", retryAfter: "any"}},
		}},

	// ---- Account pool ----
	{ID: "E-POOL-EXHAUSTED", HTTP: "503", Body: ceBodyAnthropic, Type: "api_error", Code: ceDash,
		Message: "No available accounts: {reason}", Stream: ceStreamJSONData, RetryAfter: ceWhenKnown,
		drives: []ceDrive{
			{name: "every account rate-limited", env: func(e *ceEnv) {
				reset := time.Now().Add(10 * time.Minute)
				e.accounts[0].RateLimitResetAt = &reset
			}, req: ceStream(), want: ceWant{status: 503, form: ceFormJSON, message: "No available accounts: no available accounts", retryAfter: "any"}},
			{name: "no schedulable account", env: func(e *ceEnv) { e.accounts[0].Schedulable = false },
				want: ceWant{status: 503, form: ceFormJSON, message: "No available accounts: no available accounts"}},
			{name: "after ping", env: func(e *ceEnv) {
				reset := time.Now().Add(10 * time.Minute)
				e.accounts[0].RateLimitResetAt = &reset
				cePingFirst(e)
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormData, message: "No available accounts: no available accounts"}},
		}},
	{ID: "E-POOL-MODEL-UNSUPPORTED", HTTP: "404", Body: ceBodyAnthropic, Type: "model_not_found", Code: ceDash,
		Message: `Model "{model}" is not supported by any configured account in this group`, Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.accounts[0].Credentials["model_mapping"] = map[string]any{"claude-opus-5": "claude-opus-5"}
		}, req: ceStream(), want: ceWant{status: 404, form: ceFormJSON, message: `Model "claude-sonnet-5" is not supported by any configured account in this group`}}}},

	// ---- Upstream answered with an error before any output ----
	{ID: "E-UP-400", HTTP: "400", Body: ceBodyUpstream, Type: ceUpstreamType, Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSONGinEvent, RetryAfter: ceNo,
		drives: []ceDrive{
			{env: func(e *ceEnv) { e.reply(ceJSON(400, ceUp400)) }, req: ceStream(), want: ceWant{status: 400, form: ceFormVerbatim, raw: ceUp400}},
			{name: "after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(400, ceUp400)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormGinEvent, errType: "invalid_request_error", message: "The provided model identifier is invalid."}},
		}},
	{ID: "E-UP-REQUEST-429", HTTP: "429", Body: ceBodyAnthropic, Type: "rate_limit_error", Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSONGinEvent, RetryAfter: ceNo,
		drives: []ceDrive{
			{env: func(e *ceEnv) {
				ceSecondAccount(e)
				e.reply(ceReply{status: 429, body: ceUpRequest429, header: map[string]string{"Retry-After": "30"}})
			}, req: ceStream(), want: ceWant{status: 429, form: ceFormJSON, message: "Error"},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 1, e.upstream.calls, "no failover")
					require.Empty(t, e.accountRepo.writes, "account not marked")
				}},
			{name: "after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(429, ceUpRequest429)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormGinEvent, message: "Error"}},
		}},
	{ID: "E-UP-429", HTTP: "429", Body: ceBodyAnthropic, Type: "rate_limit_error", Code: ceDash,
		Message: "Upstream rate limit exceeded, please retry later", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{env: func(e *ceEnv) {
				ceSecondAccount(e)
				e.reply(ceReply{status: 429, body: ceUpAccount429, header: map[string]string{"Retry-After": "30"}})
			}, req: ceStream(), want: ceWant{status: 429, form: ceFormJSON},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "failed over to the second account first")
				}},
			{name: "after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(429, ceUpAccount429)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormData}},
		}},
	{ID: "E-UP-529", HTTP: "503", Body: ceBodyAnthropic, Type: "overloaded_error", Code: ceDash,
		Message: "Upstream service overloaded, please retry later", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "HTTP 529", env: func(e *ceEnv) { e.reply(ceJSON(529, ceUp529)) }, want: ceWant{status: 503, form: ceFormJSON}},
			{name: "overloaded_error event before any output", env: func(e *ceEnv) { e.reply(ceReply{status: 200, body: ceSSEError("overloaded_error", "Overloaded")}) },
				req: ceStream(), want: ceWant{status: 503, form: ceFormJSON},
				check: func(t *testing.T, _ *ceEnv, rec *httptest.ResponseRecorder) {
					require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"), "the upstream's 200 already set the stream content type")
				}},
		}},
	{ID: "E-UP-5XX", HTTP: "502", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Upstream service temporarily unavailable", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "HTTP 500", env: func(e *ceEnv) { e.reply(ceJSON(500, ceUp500)) }, want: ceWant{status: 502, form: ceFormJSON}},
			{name: "connection refused", env: func(e *ceEnv) { e.reply(ceReply{transportErr: syscall.ECONNREFUSED}) }, want: ceWant{status: 502, form: ceFormJSON}},
			{name: "stream broke before the first byte", env: func(e *ceEnv) { e.reply(ceReply{status: 200, readErr: syscall.ECONNRESET}) }, req: ceStream(), want: ceWant{status: 502, form: ceFormJSON}},
		}},
	{ID: "E-UP-401", HTTP: "502", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Upstream authentication failed, please contact administrator", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { ceAPIKeyAccountOnly(e); e.reply(ceJSON(401, ceUp401)) }, want: ceWant{status: 502, form: ceFormJSON}}}},
	{ID: "E-UP-403", HTTP: "502", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Upstream access forbidden, please contact administrator", Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "HTTP 403", env: func(e *ceEnv) { ceAPIKeyAccountOnly(e); e.reply(ceJSON(403, ceUp403)) }, want: ceWant{status: 502, form: ceFormJSON}},
			{name: "error event before any output", env: func(e *ceEnv) {
				e.reply(ceReply{status: 200, body: ceSSEError("invalid_request_error", ceContentFilter)})
			}, req: ceStream(), want: ceWant{status: 502, form: ceFormJSON}},
			{name: "prod rule 1 loaded, no keyword", env: func(e *ceEnv) {
				ceWithRule1(e)
				e.reply(ceReply{status: 200, body: ceSSEError("api_error", "Internal server error")})
			}, req: ceStream(), want: ceWant{status: 502, form: ceFormJSON}},
		}},
	{ID: "E-UP-403-RULE1", HTTP: "502", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSONData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "content filter event before any output", env: func(e *ceEnv) {
				ceWithRule1(e)
				e.reply(ceReply{status: 200, body: ceSSEError("invalid_request_error", ceContentFilter)})
			}, req: ceStream(), want: ceWant{status: 502, form: ceFormJSON, message: ceContentFilter}},
			{name: "unable-to-complete event before any output", env: func(e *ceEnv) {
				ceWithRule1(e)
				e.reply(ceReply{status: 200, body: ceSSEError("api_error", ceUnableComplete)})
			}, req: ceStream(), want: ceWant{status: 502, form: ceFormJSON, message: ceUnableComplete}},
		}},
	{ID: "E-UP-OTHER", HTTP: "502", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Upstream request failed", Stream: ceStreamJSONEither, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "HTTP 413, answered by the first account", env: func(e *ceEnv) { ceSecondAccount(e); e.reply(ceJSON(413, ceUp413)) },
				want: ceWant{status: 502, form: ceFormJSON},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 1, e.upstream.calls, "no failover")
				}},
			{name: "HTTP 413 after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(413, ceUp413)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormGinEvent}},
			{name: "HTTP 402 on every account", env: func(e *ceEnv) {
				ceAPIKeyAccountOnly(e)
				e.accounts = append(e.accounts, e.apiKeyAccount(9722))
				e.reply(ceJSON(402, ceUp402))
			}, want: ceWant{status: 502, form: ceFormJSON},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "402 takes the account out and fails over")
				}},
			{name: "HTTP 404 model not found on every account", env: func(e *ceEnv) { ceSecondAccount(e); e.reply(ceJSON(404, ceUp404)) }, want: ceWant{status: 502, form: ceFormJSON},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "model-not-found fails over")
				}},
			{name: "HTTP 404 model not found after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(404, ceUp404)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormData}},
			{name: "local failure before forwarding", env: func(e *ceEnv) {
				e.accounts[0].Credentials = map[string]any{}
			}, want: ceWant{status: 502, form: ceFormJSON},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Zero(t, e.upstream.calls)
				}},
		}},
	{ID: "E-UP-RETRIES-EXHAUSTED", HTTP: "502", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Upstream request failed after retries", Stream: ceStreamJSONGinEvent, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			acc := e.apiKeyAccount(9721)
			acc.Credentials["custom_error_codes_enabled"] = true
			acc.Credentials["custom_error_codes"] = []any{float64(429)}
			e.accounts = []*service.Account{acc}
			e.reply(ceJSON(404, ceUp404))
		}, want: ceWant{status: 502, form: ceFormJSON},
			check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
				require.Greater(t, e.upstream.calls, 1, "retried on the same account")
			}}}},
	{ID: "E-UP-BEDROCK-CONTENT-FILTER", HTTP: "400", Body: ceBodyAnthropic, Type: "invalid_request_error", Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSONGinEvent, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.accounts = []*service.Account{e.bedrockAccount(9731)}
			e.reply(ceJSON(503, ceBedrockFilter))
		}, want: ceWant{status: 400, form: ceFormJSON, message: "The model response was blocked by content filtering policy."},
			check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
				require.Equal(t, 1, e.upstream.calls, "no failover")
			}}}},

	// ---- Errors after output started (HTTP 200 already sent) ----
	{ID: "E-STREAM-UPSTREAM-ERROR", HTTP: "200", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Upstream access forbidden, please contact administrator", Stream: ceStreamData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "Overloaded mid-stream", env: func(e *ceEnv) {
				ceSecondAccount(e)
				e.reply(ceReply{status: 200, body: ceStreamStart + ceSSEError("overloaded_error", "Overloaded")})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormData},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 1, e.upstream.calls, "no failover after output")
				}},
			{name: "prod rule 1 loaded, no keyword", env: func(e *ceEnv) {
				ceWithRule1(e)
				e.reply(ceReply{status: 200, body: ceStreamStart + ceSSEError("api_error", "Internal server error")})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormData}},
		}},
	{ID: "E-STREAM-UPSTREAM-ERROR-RULE1", HTTP: "200", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamData, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "Overloaded", env: func(e *ceEnv) {
				ceWithRule1(e)
				e.reply(ceReply{status: 200, body: ceStreamStart + ceSSEError("overloaded_error", "Overloaded")})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormData, message: "Overloaded"}},
			{name: "content filter", env: func(e *ceEnv) {
				ceWithRule1(e)
				e.reply(ceReply{status: 200, body: ceStreamStart + ceSSEError("invalid_request_error", ceContentFilter)})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormData, message: ceContentFilter}},
		}},
	{ID: "E-STREAM-DISCONNECTED", HTTP: "200", Body: ceBodyAnthropic, Type: "stream_read_error", Code: ceDash,
		Message: "upstream stream disconnected: {reason}", Stream: ceStreamEventData, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.reply(ceReply{status: 200, body: ceStreamStart, readErr: syscall.ECONNRESET}) }, req: ceStream(),
			want: ceWant{status: 200, form: ceFormEvent, message: "upstream stream disconnected: connection reset by peer"}}}},
	{ID: "E-STREAM-IDLE", HTTP: "200", Body: ceBodyAnthropic, Type: "stream_timeout", Code: ceDash,
		Message: "upstream stream idle for {interval}", Stream: ceStreamEventData, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.cfg.Gateway.StreamDataIntervalTimeout = 1
			e.reply(ceReply{status: 200, body: ceStreamStart, hang: true})
		}, req: ceStream(), want: ceWant{status: 200, form: ceFormEvent, message: "upstream stream idle for 1s"}}}},
	{ID: "E-STREAM-LINE-TOO-LONG", HTTP: "200", Body: ceBodyAnthropic, Type: "response_too_large", Code: ceDash,
		Message: "upstream SSE line exceeded {n} bytes", Stream: ceStreamEventData, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.cfg.Gateway.MaxLineSize = 4096
			e.reply(ceReply{status: 200, body: ceStreamStart + "event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("x", 8192) + `"}}` + "\n\n"})
		}, req: ceStream(), want: ceWant{status: 200, form: ceFormEvent, message: "upstream SSE line exceeded 4096 bytes"}}}},
	{ID: "E-STREAM-INCOMPLETE", HTTP: "200", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Upstream request failed", Stream: ceStreamData, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.reply(ceReply{status: 200, body: ceStreamStart}) }, req: ceStream(), want: ceWant{status: 200, form: ceFormData},
			check: func(t *testing.T, _ *ceEnv, rec *httptest.ResponseRecorder) {
				require.Equal(t, ceUpstreamR, rec.Header().Get("X-Request-ID"), "a stream that reached the upstream carries the upstream request id")
			}}}},

	// ---- Client went away ----
	{ID: "E-CLIENT-GONE", HTTP: "499", Body: ceBodyNone, Type: ceDash, Code: ceDash,
		Message: ceDash, Stream: ceStreamNone, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.keyLookErr = context.Canceled }, req: ceRequest{ctx: ceCanceledCtx()}, want: ceWant{status: 499, form: ceFormEmpty}}}},

	// ---- /v1/messages/count_tokens (never SSE) ----
	{ID: "E-CT-POOL-EXHAUSTED", HTTP: "503", Body: ceBodyAnthropic, Type: "api_error", Code: ceDash,
		Message: "Service temporarily unavailable", Stream: ceStreamJSON, RetryAfter: ceWhenKnown,
		drives: []ceDrive{{env: func(e *ceEnv) {
			reset := time.Now().Add(10 * time.Minute)
			e.accounts[0].RateLimitResetAt = &reset
		}, req: ceCountTokens(), want: ceWant{status: 503, form: ceFormJSON, retryAfter: "any"}}}},
	{ID: "E-CT-BEDROCK", HTTP: "404", Body: ceBodyAnthropic, Type: "not_found_error", Code: ceDash,
		Message: "count_tokens endpoint is not supported for Bedrock", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.accounts = []*service.Account{e.bedrockAccount(9731)} }, req: ceCountTokens(), want: ceWant{status: 404, form: ceFormJSON}}}},
	{ID: "E-CT-UP-429", HTTP: "429", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Rate limit exceeded", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.reply(ceJSON(429, ceUpAccount429)) }, req: ceCountTokens(), want: ceWant{status: 429, form: ceFormJSON}}}},
	{ID: "E-CT-UP-529", HTTP: "529", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Service overloaded", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.reply(ceJSON(529, ceUp529)) }, req: ceCountTokens(), want: ceWant{status: 529, form: ceFormJSON}}}},
	{ID: "E-CT-UP-ERROR", HTTP: "= upstream", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Upstream request failed", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "404", env: func(e *ceEnv) { e.reply(ceJSON(404, ceUp404)) }, req: ceCountTokens(), want: ceWant{status: 404, form: ceFormJSON}},
			{name: "400", env: func(e *ceEnv) { e.reply(ceJSON(400, ceUp400)) }, req: ceCountTokens(), want: ceWant{status: 400, form: ceFormJSON}},
			{name: "503", env: func(e *ceEnv) { e.reply(ceJSON(503, ceUp500)) }, req: ceCountTokens(), want: ceWant{status: 503, form: ceFormJSON}},
		}},
	{ID: "E-CT-REQUEST-FAILED", HTTP: "502", Body: ceBodyAnthropic, Type: "upstream_error", Code: ceDash,
		Message: "Request failed", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.reply(ceReply{transportErr: syscall.ECONNREFUSED}) }, req: ceCountTokens(), want: ceWant{status: 502, form: ceFormJSON}}}},
}

func ceSubscription(e *ceEnv) *service.UserSubscription {
	now := time.Now()
	start := now
	return &service.UserSubscription{
		ID: 9741, UserID: e.user.ID, GroupID: e.group.ID, Status: service.SubscriptionStatusActive,
		StartsAt: now.Add(-24 * time.Hour), ExpiresAt: now.Add(30 * 24 * time.Hour),
		DailyWindowStart: &start, WeeklyWindowStart: &start, MonthlyWindowStart: &start,
	}
}

func ceCanceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// ceDirectInvalidAuthLimiter sends invalid keys from one client until the
// invalid-auth limiter blocks it, and returns the blocked response.
func ceDirectInvalidAuthLimiter(t *testing.T) *httptest.ResponseRecorder {
	e := newCEEnv(t)
	// config defaults are threshold 120 / 60 s window / 60 s block; 10 keeps the drive short
	e.cfg.APIKeyAuth.InvalidAbuse = config.InvalidAuthAbuseConfig{Enabled: true, Threshold: 10, WindowSeconds: 60, BlockSeconds: 60, Capacity: 256}
	r, _ := e.router()
	var last *httptest.ResponseRecorder
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(ceMessagesBody(ceModel, false)))
		req.Header.Set("x-api-key", fmt.Sprintf("sk-invalid-%d", i))
		req.RemoteAddr = "198.51.100.7:4000"
		last = httptest.NewRecorder()
		r.ServeHTTP(last, req)
		if last.Code == http.StatusTooManyRequests {
			return last
		}
	}
	return last
}

func TestXSciClientErrorContract(t *testing.T) {
	gin.SetMode(gin.TestMode) // once, before the parallel rows
	seen := map[string]bool{}
	for _, row := range ceContract {
		row := row
		require.False(t, seen[row.ID], "duplicate row %s", row.ID)
		seen[row.ID] = true
		require.NotEmpty(t, row.drives, "row %s has no drive", row.ID)
		t.Run(row.ID, func(t *testing.T) {
			t.Parallel()
			for i, d := range row.drives {
				d := d
				name := d.name
				if name == "" {
					name = strconv.Itoa(i)
				}
				t.Run(name, func(t *testing.T) {
					var rec *httptest.ResponseRecorder
					var e *ceEnv
					if d.direct != nil {
						rec = d.direct(t)
					} else {
						e = newCEEnv(t)
						if d.env != nil {
							d.env(e)
						}
						rec = e.do(d.req)
						require.NotEmpty(t, rec.Header().Get("X-Request-ID"), "X-Request-ID on every response")
						if row.Body != ceBodyNone {
							require.NotEmpty(t, rec.Header().Get("X-Client-Request-ID"), "X-Client-Request-ID on every response")
						}
					}
					ceAssert(t, row, d.want, rec)
					if d.check != nil {
						d.check(t, e, rec)
					}
				})
			}
		})
	}
}

func ceAssert(t *testing.T, row ceRow, want ceWant, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	require.Equal(t, want.status, rec.Code, "status; body: %s", body)
	if row.HTTP != "= upstream" {
		if want.form == ceFormData || want.form == ceFormEvent || want.form == ceFormGinEvent {
			if row.HTTP != "200" {
				// a row whose own status is not 200 seen after a ping: the 200 was already sent
				require.Equal(t, http.StatusOK, rec.Code)
			}
		} else {
			require.Equal(t, row.HTTP, strconv.Itoa(rec.Code), "row HTTP column")
		}
	}

	switch want.retryAfter {
	case "":
		require.Empty(t, rec.Header().Get("Retry-After"), "no Retry-After")
	case "any":
		v, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		require.NoError(t, err, "Retry-After must be whole seconds")
		require.Positive(t, v)
	default:
		require.Equal(t, want.retryAfter, rec.Header().Get("Retry-After"))
	}
	if row.RetryAfter == ceNo {
		require.Empty(t, want.retryAfter, "row says no Retry-After")
	}

	message := want.message
	if message == "" {
		message = row.Message
	}
	errType := want.errType
	if errType == "" {
		errType = row.Type
	}

	switch want.form {
	case ceFormEmpty:
		require.Empty(t, body)
		return
	case ceFormVerbatim:
		require.Equal(t, row.Body, ceBodyUpstream)
		require.Equal(t, want.raw, body, "upstream body byte for byte")
		return
	case ceFormJSON:
		require.NotContains(t, body, "data:", "a JSON error, not a frame")
		ceAssertEnvelope(t, row, []byte(body), errType, message)
		return
	}

	blocks := ceSSEBlocks(body)
	require.NotEmpty(t, blocks, "SSE frames; body: %s", body)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	last := blocks[len(blocks)-1]
	switch want.form {
	case ceFormData:
		require.True(t, strings.HasPrefix(last, "data: "), "a data-only frame; got %q", last)
		require.NotContains(t, last, "\n", "no event: line; got %q", last)
		ceAssertEnvelope(t, row, []byte(strings.TrimPrefix(last, "data: ")), errType, message)
		require.Equal(t, 1, ceCountErrorFrames(blocks), "one error frame")
	case ceFormGinEvent:
		require.True(t, strings.HasPrefix(last, "event:error\ndata:"), "gin event frame; got %q", last)
		ceAssertEnvelope(t, row, []byte(strings.TrimPrefix(last, "event:error\ndata:")), errType, message)
		require.Equal(t, 1, ceCountErrorFrames(blocks), "one error frame")
	case ceFormEvent:
		require.GreaterOrEqual(t, len(blocks), 2)
		require.Equal(t, ceStreamFallback, last, "the handler's fallback frame closes the stream")
		ev := blocks[len(blocks)-2]
		require.True(t, strings.HasPrefix(ev, "event: error\ndata: "), "event: error frame; got %q", ev)
		ceAssertEnvelope(t, row, []byte(strings.TrimPrefix(ev, "event: error\ndata: ")), errType, message)
		require.Equal(t, 2, ceCountErrorFrames(blocks), "two error frames")
	default:
		t.Fatalf("unknown form %q", want.form)
	}
}

func ceAssertEnvelope(t *testing.T, row ceRow, raw []byte, errType, message string) {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload), "JSON body: %s", raw)
	switch row.Body {
	case ceBodyFlat:
		require.Len(t, payload, 2, "flat body is {code,message}: %s", raw)
		require.Equal(t, row.Code, payload["code"])
		require.Equal(t, message, payload["message"])
	case ceBodyAnthropic:
		require.Len(t, payload, 2, "anthropic envelope is {type,error}: %s", raw)
		require.Equal(t, "error", payload["type"])
		inner, ok := payload["error"].(map[string]any)
		require.True(t, ok, "error object: %s", raw)
		require.Equal(t, errType, inner["type"])
		require.Equal(t, message, inner["message"])
		if row.Code == ceDash {
			require.NotContains(t, inner, "code")
			require.Len(t, inner, 2, "error has type and message only: %s", raw)
		} else {
			require.Equal(t, row.Code, inner["code"])
			require.Len(t, inner, 3, "error has type, code and message: %s", raw)
		}
	case ceBodyUpstream:
		inner, _ := payload["error"].(map[string]any)
		require.Equal(t, errType, inner["type"])
		require.Equal(t, message, inner["message"])
	default:
		t.Fatalf("row %s: body %q has no JSON", row.ID, row.Body)
	}
}

// ceSSEBlocks splits an SSE body into its non-empty blocks.
func ceSSEBlocks(body string) []string {
	var out []string
	for _, b := range strings.Split(body, "\n\n") {
		b = strings.Trim(b, "\n")
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}

func ceCountErrorFrames(blocks []string) int {
	n := 0
	for _, b := range blocks {
		if strings.Contains(b, `"type":"error"`) {
			n++
		}
	}
	return n
}

// ---- doc lock ----

const ceDocPath = "docs/xsci-client-error-contract.md"

func ceRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(file)
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, ceDocPath)); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("%s not found above %s", ceDocPath, filepath.Dir(file))
	return ""
}

// ceDocTable returns the rows of the markdown table that follows the given
// heading prefix, as cells (backticks around a whole cell removed).
func ceDocTable(t *testing.T, doc, heading string) [][]string {
	t.Helper()
	var rows [][]string
	in := false
	sc := bufio.NewScanner(strings.NewReader(doc))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			if in && len(rows) > 0 {
				break
			}
			in = strings.HasPrefix(line, heading)
			continue
		}
		if !in || !strings.HasPrefix(line, "| E-") {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|"), " | ")
		cells := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if len(p) >= 2 && strings.HasPrefix(p, "`") && strings.HasSuffix(p, "`") && strings.Count(p, "`") == 2 {
				p = p[1 : len(p)-1]
			}
			cells = append(cells, p)
		}
		rows = append(rows, cells)
	}
	require.NotEmpty(t, rows, "no table rows under %q in %s", heading, ceDocPath)
	return rows
}

func TestXSciClientErrorContractMatchesDoc(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(ceRepoRoot(t), ceDocPath))
	require.NoError(t, err)
	doc := string(raw)

	contract := ceDocTable(t, doc, "## 2.")
	var docIDs, goIDs []string
	byID := map[string][]string{}
	for _, cells := range contract {
		require.Len(t, cells, 8, "contract row %v", cells)
		docIDs = append(docIDs, cells[0])
		byID[cells[0]] = cells
	}
	for _, row := range ceContract {
		goIDs = append(goIDs, row.ID)
		cells, ok := byID[row.ID]
		if !ok {
			continue
		}
		want := []string{row.ID, row.HTTP, row.Body, row.Type, row.Code, row.Message, row.Stream, row.RetryAfter}
		require.Equal(t, want, cells, "doc row %s differs from ceContract", row.ID)
	}
	require.Equal(t, goIDs, docIDs, "doc §2 rows and ceContract rows (same ids, same order)")

	semantics := ceDocTable(t, doc, "## 3.")
	var semIDs []string
	for _, cells := range semantics {
		semIDs = append(semIDs, cells[0])
	}
	sortedGo := append([]string(nil), goIDs...)
	sort.Strings(sortedGo)
	sort.Strings(semIDs)
	require.Equal(t, sortedGo, semIDs, "doc §3 explains every row of §2")
}
