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
//     and checks what the client sees: status, body, error.type, error.code,
//     message, error.upstream, request_id, stream frame and Retry-After;
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

// Classes (doc §1).
const (
	ceClassUpstream = "a" // the upstream's own answer, unchanged
	ceClassGateway  = "b" // a conclusion sub2api computed: envelope + error.code
)

// Bodies (doc §1.1).
const (
	ceBodyEnvelope    = "envelope"            // {"type":"error","error":{"type","code","message"[,"upstream"]},"request_id"}
	ceBodyUpstream    = "upstream"            // the upstream's JSON body, byte for byte
	ceBodyUpstreamEnv = "upstream, enveloped" // upstream status + message in the envelope, no code
	ceBodyNone        = "none"
)

// Stream forms (doc §1.2): what a stream:true request receives.
const (
	ceStreamJSON         = "JSON"
	ceStreamJSONFrame    = "JSON; frame after ping"
	ceStreamJSONUpFrame  = "JSON; upstream frame after ping"
	ceStreamFrame        = "frame"
	ceStreamUpFrame      = "upstream frame"
	ceStreamNone         = "none"
	ceFormJSON           = "json"     // a JSON body with the row's status
	ceFormVerbatim       = "verbatim" // the upstream's JSON body, byte for byte
	ceFormFrame          = "frame"    // one `event: error` frame carrying the envelope
	ceFormUpFrame        = "upframe"  // one `event: error` frame carrying the upstream's data, as sent
	ceFormEmpty          = "empty"
	ceUpstreamText       = "(upstream text)"
	ceUpstreamType       = "(upstream type)"
	ceNo                 = "no"
	ceYes                = "yes"
	ceWhenKnown          = "when known"
	ceDash               = "—"
	ceHTTPUpstream       = "= upstream"
	ceHTTPRule           = "= rule"
	ceTypeOfStatus       = "(type of the status)"
	ceRuleMessage        = "(rule message)"
	ceModelNotFoundSonet = `Model "claude-sonnet-5" is not supported by any configured account in this group`
)

type ceRow struct {
	ID         string
	Class      string
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
	message    string    // concrete message ("" = row.Message as is)
	errType    string    // concrete error.type ("" = row.Type)
	code       string    // concrete error.code ("" = row.Code)
	raw        string    // verbatim upstream body / frame data
	retryAfter string    // "" = absent, "any" = a positive integer, else exact
	upstream   *ceUpWant // expected error.upstream (nil = absent)
}

// ceUpWant is the expected error.upstream; message "" means absent.
type ceUpWant struct {
	status    int
	errType   string
	message   string
	requestID string
}

// Upstream bodies and streams used by the drives.
const (
	ceUp400          = `{"type":"error","error":{"type":"invalid_request_error","message":"The provided model identifier is invalid."},"request_id":"req_011CUp400"}`
	ceUpRequest429   = `{"type":"error","error":{"type":"rate_limit_error","message":"Error"},"request_id":"req_011CUpReq429"}`
	ceUpAccount429   = `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_011CUp429"}`
	ceUp529          = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_011CUp529"}`
	ceUp500          = `{"type":"error","error":{"type":"api_error","message":"Internal server error"},"request_id":"req_011CUp500"}`
	ceUp401          = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"},"request_id":"req_011CUp401"}`
	ceUp403          = `{"type":"error","error":{"type":"permission_error","message":"Request not allowed"},"request_id":"req_011CUp403"}`
	ceUp403Keyword   = `{"type":"error","error":{"type":"permission_error","message":"Unable to complete this request right now. Please try again."},"request_id":"req_011CUp403k"}`
	ceUp404          = `{"type":"error","error":{"type":"not_found_error","message":"model: claude-sonnet-5"},"request_id":"req_011CUp404"}`
	ceUp409          = `{"type":"error","error":{"type":"invalid_request_error","message":"Conflict"},"request_id":"req_011CUp409"}`
	ceUp413          = `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."},"request_id":"req_011CUp413"}`
	ceUpCredit400    = `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."},"request_id":"req_011CUpCredit"}`
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

// ceSSEErrorData is the data of an upstream `event: error`.
func ceSSEErrorData(errType, message string) string {
	return `{"type":"error","error":{"type":"` + errType + `","message":"` + message + `"}}`
}

func ceSSEError(errType, message string) string {
	return "event: error\n" + "data: " + ceSSEErrorData(errType, message) + "\n\n"
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

// ceModelNotFoundCooling marks the model as cooling down on every account of
// the env because the upstream answered "model not found" (what
// HandleUpstreamModelNotFound stores).
func ceModelNotFoundCooling(e *ceEnv) {
	for _, a := range e.accounts {
		a.Extra = ceWithModelRateLimit(a.Extra, ceModel, time.Now().Add(20*time.Minute), "upstream_404_model_not_found")
	}
}

// ceDirectContext is a recorder + gin context for a closest-function drive,
// with the contract layer switched on as the route middleware does.
func ceDirectContext() (*httptest.ResponseRecorder, *gin.Context) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	service.MarkXSciAnthropicSurface(c)
	return rec, c
}

// ceDirectBilling drives the handler's billing-error path at the closest
// function: billingErrorDetails maps the error, the handler writes it.
func ceDirectBilling(err error) func(t *testing.T) *httptest.ResponseRecorder {
	return func(t *testing.T) *httptest.ResponseRecorder {
		rec, c := ceDirectContext()
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
		rec, c := ceDirectContext()
		conc := &ceConcurrencyCache{userFree: func() bool { return false }}
		h := &GatewayHandler{concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(conc), SSEPingFormatClaude, 15*time.Millisecond)}
		streamStarted := false
		_, err := h.concurrencyHelper.acquireUserSlotWithWaitTimeout(c, ceUserID, 1, 60*time.Millisecond, stream, &streamStarted)
		require.Error(t, err)
		h.handleConcurrencyError(c, err, "user", streamStarted)
		return rec
	}
}

// ceTestRule is an admin error-passthrough rule other than prod rule 1.
func ceTestRule(e *ceEnv) {
	code := 502
	e.rules = []*model.ErrorPassthroughRule{{
		ID: 7, Name: "test: pass 503 text", Enabled: true, Priority: 10,
		ErrorCodes: []int{503}, Keywords: []string{"Internal server error"}, MatchMode: model.MatchModeAll,
		Platforms: []string{"anthropic"}, PassthroughCode: false, ResponseCode: &code, PassthroughBody: true,
	}}
}

func ceAuthRow(id string, status int, errType, code, message string) ceRow {
	return ceRow{ID: id, Class: ceClassGateway, HTTP: strconv.Itoa(status), Body: ceBodyEnvelope, Type: errType, Code: code,
		Message: message, Stream: ceStreamJSON, RetryAfter: ceNo}
}

func withDrives(r ceRow, d ...ceDrive) ceRow { r.drives = d; return r }

var ceContract = []ceRow{
	// ---- API key (auth middleware): before any ping, so never a frame ----
	withDrives(ceAuthRow("E-AUTH-MISSING-KEY", 401, "authentication_error", "API_KEY_REQUIRED",
		"API key is required in Authorization header (Bearer scheme), x-api-key header, or x-goog-api-key header"),
		ceDrive{req: ceRequest{noKey: true, stream: true}, want: ceWant{status: 401, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-INVALID-KEY", 401, "authentication_error", "INVALID_API_KEY", "Invalid API key"),
		ceDrive{name: "unknown key", req: ceRequest{key: "sk-unknown"}, want: ceWant{status: 401, form: ceFormJSON}},
		ceDrive{name: "oversized credential", req: ceRequest{key: strings.Repeat("k", service.MaxAPIKeyCredentialBytes+1)}, want: ceWant{status: 401, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-KEY-IN-QUERY", 400, "invalid_request_error", "api_key_in_query_deprecated",
		"API key in query parameter is deprecated. Please use Authorization header instead."),
		ceDrive{req: ceRequest{query: "key=" + ceAPIKey}, want: ceWant{status: 400, form: ceFormJSON}}),
	func() ceRow {
		r := ceAuthRow("E-AUTH-TOO-MANY-INVALID", 429, "rate_limit_error", "INVALID_AUTH_RATE_LIMITED", "Too many invalid authentication attempts; retry later")
		r.RetryAfter = ceYes
		return withDrives(r, ceDrive{name: "after repeated invalid keys from one IP", direct: ceDirectInvalidAuthLimiter, want: ceWant{status: 429, form: ceFormJSON, retryAfter: "any"}})
	}(),
	withDrives(ceAuthRow("E-AUTH-LOOKUP-OVERLOADED", 503, "api_error", "API_KEY_AUTH_OVERLOADED", "API key authentication is temporarily unavailable"),
		ceDrive{env: func(e *ceEnv) { e.keyLookErr = service.ErrAPIKeyAuthOverloaded }, want: ceWant{status: 503, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-LOOKUP-FAILED", 500, "api_error", "INTERNAL_ERROR", "Failed to validate API key"),
		ceDrive{env: func(e *ceEnv) { e.keyLookErr = errors.New("db down") }, want: ceWant{status: 500, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-KEY-DISABLED", 401, "authentication_error", "API_KEY_DISABLED", "API key is disabled"),
		ceDrive{env: func(e *ceEnv) { e.key.Status = service.StatusDisabled }, want: ceWant{status: 401, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-IP-DENIED", 403, "permission_error", "ACCESS_DENIED", "Access denied. Your IP is {ip}"),
		ceDrive{env: func(e *ceEnv) { e.key.IPWhitelist = []string{"10.9.9.9"} }, want: ceWant{status: 403, form: ceFormJSON, message: "Access denied. Your IP is 192.0.2.1"}}),
	withDrives(ceAuthRow("E-AUTH-USER-INACTIVE", 401, "authentication_error", "USER_INACTIVE", "User account is not active"),
		ceDrive{env: func(e *ceEnv) { e.user.Status = service.StatusDisabled }, want: ceWant{status: 401, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-GROUP-DELETED", 403, "permission_error", "GROUP_DELETED", "API Key 所属分组已删除"),
		ceDrive{env: func(e *ceEnv) { e.group.Status = "deleted" }, want: ceWant{status: 403, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-GROUP-DISABLED", 403, "permission_error", "GROUP_DISABLED", "API Key 所属分组已停用"),
		ceDrive{env: func(e *ceEnv) { e.group.Status = service.StatusDisabled }, want: ceWant{status: 403, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-GROUP-NOT-ALLOWED", 403, "permission_error", "GROUP_NOT_ALLOWED", "API Key 所属专属分组不再允许当前用户使用"),
		ceDrive{env: func(e *ceEnv) { e.group.IsExclusive = true }, want: ceWant{status: 403, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-NO-SUBSCRIPTION", 403, "permission_error", "SUBSCRIPTION_NOT_FOUND", "No active subscription found for this group"),
		ceDrive{env: func(e *ceEnv) { e.group.SubscriptionType = service.SubscriptionTypeSubscription }, want: ceWant{status: 403, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-KEY-QUOTA-EXHAUSTED", 402, "billing_error", "API_KEY_QUOTA_EXHAUSTED", "API key 额度已用完"),
		ceDrive{env: func(e *ceEnv) { e.key.Status = service.StatusAPIKeyQuotaExhausted }, want: ceWant{status: 402, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-KEY-EXPIRED", 403, "permission_error", "API_KEY_EXPIRED", "API key 已过期"),
		ceDrive{env: func(e *ceEnv) { past := time.Now().Add(-time.Hour); e.key.ExpiresAt = &past }, want: ceWant{status: 403, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-AUTH-SUBSCRIPTION-MAINTENANCE", 500, "api_error", "SUBSCRIPTION_MAINTENANCE_FAILED", "Failed to maintain subscription usage windows"),
		ceDrive{env: func(e *ceEnv) {
			e.group.SubscriptionType = service.SubscriptionTypeSubscription
			e.sub = ceSubscription(e)
			old := time.Now().Add(-72 * time.Hour)
			e.sub.DailyWindowStart = &old
		}, want: ceWant{status: 500, form: ceFormJSON}}),

	// ---- Billing: the same situation answers the same at auth and after the slot wait ----
	{ID: "E-BILLING-NO-BALANCE", Class: ceClassGateway, HTTP: "402", Body: ceBodyEnvelope, Type: "billing_error", Code: "INSUFFICIENT_BALANCE",
		Message: "{balance message}", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "at auth", env: func(e *ceEnv) { e.user.Balance = 0 }, want: ceWant{status: 402, form: ceFormJSON, message: "Insufficient account balance"}},
			{name: "after the slot wait", env: func(e *ceEnv) { e.billing.balance = 0 }, req: ceStream(), want: ceWant{status: 402, form: ceFormJSON, message: service.ErrInsufficientBalance.Message}},
			{name: "after the slot wait, after ping", env: func(e *ceEnv) { e.billing.balance = 0; cePingFirst(e) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormFrame, message: service.ErrInsufficientBalance.Message}},
		}},
	{ID: "E-BILLING-SUBSCRIPTION-INVALID", Class: ceClassGateway, HTTP: "403", Body: ceBodyEnvelope, Type: "permission_error", Code: "SUBSCRIPTION_INVALID",
		Message: "{subscription message}", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "at auth", env: func(e *ceEnv) {
				e.group.SubscriptionType = service.SubscriptionTypeSubscription
				e.sub = ceSubscription(e)
				e.sub.Status = service.SubscriptionStatusSuspended
			}, want: ceWant{status: 403, form: ceFormJSON, message: service.ErrSubscriptionSuspended.Message}},
			{name: "after the slot wait", direct: ceDirectBilling(service.ErrSubscriptionInvalid), want: ceWant{status: 403, form: ceFormJSON, message: service.ErrSubscriptionInvalid.Message}},
		}},
	{ID: "E-BILLING-SUBSCRIPTION-LIMIT", Class: ceClassGateway, HTTP: "429", Body: ceBodyEnvelope, Type: "rate_limit_error", Code: "USAGE_LIMIT_EXCEEDED",
		Message: "{daily/weekly/monthly} usage limit exceeded", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "at auth", env: func(e *ceEnv) {
				e.group.SubscriptionType = service.SubscriptionTypeSubscription
				limit := 5.0
				e.group.DailyLimitUSD = &limit
				e.sub = ceSubscription(e)
				e.sub.DailyUsageUSD = 6
			}, want: ceWant{status: 429, form: ceFormJSON, message: service.ErrDailyLimitExceeded.Message}},
			{name: "after the slot wait", direct: ceDirectBilling(service.ErrDailyLimitExceeded), want: ceWant{status: 429, form: ceFormJSON, message: service.ErrDailyLimitExceeded.Message}},
			{name: "weekly, after the slot wait", direct: ceDirectBilling(service.ErrWeeklyLimitExceeded), want: ceWant{status: 429, form: ceFormJSON, message: service.ErrWeeklyLimitExceeded.Message}},
		}},
	{ID: "E-BILLING-UNAVAILABLE", Class: ceClassGateway, HTTP: "503", Body: ceBodyEnvelope, Type: "api_error", Code: "BILLING_SERVICE_ERROR",
		Message: "Billing service temporarily unavailable. Please retry later.", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{{direct: ceDirectBilling(service.ErrBillingServiceUnavailable.WithCause(errors.New("redis down"))), want: ceWant{status: 503, form: ceFormJSON}}}},
	{ID: "E-BILLING-KEY-WINDOW", Class: ceClassGateway, HTTP: "429", Body: ceBodyEnvelope, Type: "rate_limit_error", Code: "API_KEY_RATE_{5H,1D,7D}_EXCEEDED",
		Message: "{key window}", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "5h", direct: ceDirectBilling(service.ErrAPIKeyRateLimit5hExceeded), want: ceWant{status: 429, form: ceFormJSON, code: "API_KEY_RATE_5H_EXCEEDED", message: "api key 5小时限额已用完"}},
			{name: "1d", direct: ceDirectBilling(service.ErrAPIKeyRateLimit1dExceeded), want: ceWant{status: 429, form: ceFormJSON, code: "API_KEY_RATE_1D_EXCEEDED", message: "api key 日限额已用完"}},
			{name: "7d", direct: ceDirectBilling(service.ErrAPIKeyRateLimit7dExceeded), want: ceWant{status: 429, form: ceFormJSON, code: "API_KEY_RATE_7D_EXCEEDED", message: "api key 7天限额已用完"}},
		}},
	{ID: "E-BILLING-RPM", Class: ceClassGateway, HTTP: "429", Body: ceBodyEnvelope, Type: "rate_limit_error", Code: "{USER,GROUP}_RPM_EXCEEDED",
		Message: "{rpm scope} requests-per-minute limit exceeded", Stream: ceStreamJSONFrame, RetryAfter: ceYes,
		drives: []ceDrive{
			{name: "user", direct: ceDirectBilling(service.ErrUserRPMExceeded), want: ceWant{status: 429, form: ceFormJSON, code: "USER_RPM_EXCEEDED", message: "user requests-per-minute limit exceeded", retryAfter: "any"}},
			{name: "group", direct: ceDirectBilling(service.ErrGroupRPMExceeded), want: ceWant{status: 429, form: ceFormJSON, code: "GROUP_RPM_EXCEEDED", message: "group requests-per-minute limit exceeded", retryAfter: "any"}},
		}},
	{ID: "E-BILLING-PLATFORM-QUOTA", Class: ceClassGateway, HTTP: "429", Body: ceBodyEnvelope, Type: "rate_limit_error", Code: "USER_PLATFORM_{DAILY,WEEKLY,MONTHLY}_QUOTA_EXHAUSTED",
		Message: "{Daily/Weekly/Monthly} usage quota exhausted for this platform.", Stream: ceStreamJSONFrame, RetryAfter: ceYes,
		drives: []ceDrive{
			{name: "daily", direct: ceDirectBilling(ceQuotaErr(service.ErrUserPlatformDailyQuotaExhausted)), want: ceWant{status: 429, form: ceFormJSON, code: "USER_PLATFORM_DAILY_QUOTA_EXHAUSTED", message: "Daily usage quota exhausted for this platform.", retryAfter: "any"}},
			{name: "weekly", direct: ceDirectBilling(ceQuotaErr(service.ErrUserPlatformWeeklyQuotaExhausted)), want: ceWant{status: 429, form: ceFormJSON, code: "USER_PLATFORM_WEEKLY_QUOTA_EXHAUSTED", message: "Weekly usage quota exhausted for this platform.", retryAfter: "any"}},
			{name: "monthly", direct: ceDirectBilling(ceQuotaErr(service.ErrUserPlatformMonthlyQuotaExhausted)), want: ceWant{status: 429, form: ceFormJSON, code: "USER_PLATFORM_MONTHLY_QUOTA_EXHAUSTED", message: "Monthly usage quota exhausted for this platform.", retryAfter: "any"}},
		}},

	// ---- Group gate ----
	withDrives(ceAuthRow("E-GROUP-UNASSIGNED", 403, "permission_error", "group_unassigned",
		"API Key is not assigned to any group and cannot be used. Please contact the administrator to assign it to a group."),
		ceDrive{env: func(e *ceEnv) { e.key.GroupID = nil; e.key.Group = nil }, req: ceStream(), want: ceWant{status: 403, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-GROUP-MODEL-NOT-ALLOWED", 404, "not_found_error", "model_not_allowed", `Model "{model}" is not available for this group`),
		ceDrive{env: func(e *ceEnv) {
			e.group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"claude-opus-5"}}
		}, req: ceStream(), want: ceWant{status: 404, form: ceFormJSON, message: `Model "claude-sonnet-5" is not available for this group`}}),

	// ---- Request checks ----
	withDrives(ceAuthRow("E-REQ-BODY-TOO-LARGE", 413, "request_too_large", "request_body_too_large", "Request body too large, limit is {limit}"),
		ceDrive{env: func(e *ceEnv) { e.cfg.Gateway.MaxBodySize = 1 << 10 },
			req:  ceRequest{body: `{"model":"` + ceModel + `","max_tokens":32,"messages":[{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}]}`},
			want: ceWant{status: 413, form: ceFormJSON, message: "Request body too large, limit is 1024B"}}),
	withDrives(ceAuthRow("E-REQ-BODY-READ-TIMEOUT", 408, "invalid_request_error", "request_body_timeout", "Timed out reading request body"),
		ceDrive{req: ceRequest{reader: io.MultiReader(strings.NewReader(`{"model":`), ceErrReader{os.ErrDeadlineExceeded})}, want: ceWant{status: 408, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-REQ-BODY-READ-FAILED", 400, "invalid_request_error", "request_body_unreadable", "Failed to read request body"),
		ceDrive{req: ceRequest{reader: io.MultiReader(strings.NewReader(`{"model":`), ceErrReader{io.ErrUnexpectedEOF})}, want: ceWant{status: 400, form: ceFormJSON}}),
	func() ceRow {
		r := ceAuthRow("E-REQ-BODY-BUSY", 503, "api_error", "request_body_buffer_full", "Request body buffering capacity is exhausted; retry later")
		r.RetryAfter = ceYes
		return withDrives(r, ceDrive{env: func(e *ceEnv) { e.cfg.Gateway.RequestBodyMemoryBudgetBytes = 64 }, want: ceWant{status: 503, form: ceFormJSON, retryAfter: "1"}})
	}(),
	withDrives(ceAuthRow("E-REQ-BODY-EMPTY", 400, "invalid_request_error", "request_body_empty", "Request body is empty"),
		ceDrive{req: ceRequest{empty: true}, want: ceWant{status: 400, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-REQ-BODY-INVALID", 400, "invalid_request_error", "request_body_invalid", "Failed to parse request body"),
		ceDrive{req: ceRequest{body: `{"model":`}, want: ceWant{status: 400, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-REQ-MODEL-MISSING", 400, "invalid_request_error", "model_required", "model is required"),
		ceDrive{req: ceRequest{body: `{"max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`}, want: ceWant{status: 400, form: ceFormJSON}}),
	withDrives(ceAuthRow("E-REQ-MODEL-RULE", 400, "invalid_request_error", "model_parameters_unsupported", "{model rule}"),
		ceDrive{req: ceRequest{body: `{"model":"claude-sonnet-5-5","max_tokens":32,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hello"}]}`},
			want: ceWant{status: 400, form: ceFormJSON, message: "claude-sonnet-5-5 requires adaptive thinking or thinking.type=between_tools; omit thinking or use one of those modes"},
			check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
				require.Zero(t, e.upstream.calls, "rejected before any upstream call")
			}},
		ceDrive{name: "count_tokens", req: ceRequest{path: "/v1/messages/count_tokens", body: `{"model":"claude-sonnet-5-5","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hello"}]}`},
			want: ceWant{status: 400, form: ceFormJSON, message: "claude-sonnet-5-5 requires adaptive thinking or thinking.type=between_tools; omit thinking or use one of those modes"}}),

	// ---- Capacity and limits before any upstream call ----
	{ID: "E-LIMIT-USER-CONCURRENCY", Class: ceClassGateway, HTTP: "429", Body: ceBodyEnvelope, Type: "rate_limit_error", Code: "gateway_concurrency_limit",
		Message: "Concurrency limit exceeded for user, please retry later", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "non-stream, waited for a slot", direct: ceDirectUserSlotTimeout(false), want: ceWant{status: 429, form: ceFormJSON}},
			{name: "stream, pings sent while waiting", direct: ceDirectUserSlotTimeout(true), want: ceWant{status: 200, form: ceFormFrame}},
		}},
	{ID: "E-LIMIT-ACCOUNT-CONCURRENCY", Class: ceClassGateway, HTTP: "429", Body: ceBodyEnvelope, Type: "rate_limit_error", Code: "gateway_concurrency_limit",
		Message: "Concurrency limit exceeded for account, please retry later", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.conc.accountFree = func() bool { return false }
			e.cfg.Gateway.Scheduling.FallbackWaitTimeout = 60 * time.Millisecond
			e.cfg.Gateway.Scheduling.FallbackMaxWaiting = 10
		}, want: ceWant{status: 429, form: ceFormJSON}}}},
	{ID: "E-LIMIT-QUEUE-FULL", Class: ceClassGateway, HTTP: "429", Body: ceBodyEnvelope, Type: "rate_limit_error", Code: "gateway_queue_full",
		Message: "Too many pending requests, please retry later", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
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
	{ID: "E-LIMIT-SLOT-ERROR", Class: ceClassGateway, HTTP: "503", Body: ceBodyEnvelope, Type: "api_error", Code: "gateway_concurrency_unavailable",
		Message: "Service temporarily unavailable, please retry later", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.conc.userSlotError = errors.New("redis: connection refused") }, req: ceStream(), want: ceWant{status: 503, form: ceFormJSON}}}},

	// ---- Account pool ----
	{ID: "E-POOL-EXHAUSTED", Class: ceClassGateway, HTTP: "503", Body: ceBodyEnvelope, Type: "api_error", Code: "no_available_accounts",
		Message: "No available accounts: {reason}", Stream: ceStreamJSONFrame, RetryAfter: ceWhenKnown,
		drives: []ceDrive{
			{name: "every account rate-limited", env: func(e *ceEnv) {
				reset := time.Now().Add(10 * time.Minute)
				e.accounts[0].RateLimitResetAt = &reset
			}, req: ceStream(), want: ceWant{status: 503, form: ceFormJSON, message: "No available accounts: no available accounts", retryAfter: "any"}},
			{name: "model cooling down for another reason", env: func(e *ceEnv) {
				e.accounts[0].Extra = ceWithModelRateLimit(nil, ceModel, time.Now().Add(10*time.Minute), "upstream_429_model_rate_limited")
			}, want: ceWant{status: 503, form: ceFormJSON, message: "No available accounts: no available accounts", retryAfter: "any"}},
			{name: "no schedulable account", env: func(e *ceEnv) { e.accounts[0].Schedulable = false },
				want: ceWant{status: 503, form: ceFormJSON, message: "No available accounts: no available accounts"}},
			{name: "after ping", env: func(e *ceEnv) {
				reset := time.Now().Add(10 * time.Minute)
				e.accounts[0].RateLimitResetAt = &reset
				cePingFirst(e)
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormFrame, message: "No available accounts: no available accounts"}},
		}},
	{ID: "E-POOL-MODEL-UNSUPPORTED", Class: ceClassGateway, HTTP: "404", Body: ceBodyEnvelope, Type: "not_found_error", Code: "model_unsupported",
		Message: `Model "{model}" is not supported by any configured account in this group`, Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{env: func(e *ceEnv) {
				e.accounts[0].Credentials["model_mapping"] = map[string]any{"claude-opus-5": "claude-opus-5"}
			}, req: ceStream(),
				want: ceWant{status: 404, form: ceFormJSON, message: `Model "claude-sonnet-5" is not supported by any configured account in this group`}},
			{name: "after ping", env: func(e *ceEnv) {
				e.accounts[0].Credentials["model_mapping"] = map[string]any{"claude-opus-5": "claude-opus-5"}
				cePingFirst(e)
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormFrame, message: `Model "claude-sonnet-5" is not supported by any configured account in this group`}},
			{name: "count_tokens", env: func(e *ceEnv) {
				e.accounts[0].Credentials["model_mapping"] = map[string]any{"claude-opus-5": "claude-opus-5"}
			}, req: ceCountTokens(),
				want: ceWant{status: 404, form: ceFormJSON, message: `Model "claude-sonnet-5" is not supported by any configured account in this group`}},
		}},
	{ID: "E-UP-MODEL-NOT-FOUND", Class: ceClassGateway, HTTP: "404", Body: ceBodyEnvelope, Type: "not_found_error", Code: "model_unsupported",
		Message: `Model "{model}" is not supported by any configured account in this group`, Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "every account answered model not found", env: func(e *ceEnv) { ceSecondAccount(e); e.reply(ceJSON(404, ceUp404)) },
				want: ceWant{status: 404, form: ceFormJSON, message: ceModelNotFoundSonet,
					upstream: &ceUpWant{status: 404, errType: "not_found_error", message: "model: claude-sonnet-5", requestID: "req_011CUp404"}},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "every account was tried")
					// The next request, stream or not, does not reach the upstream and gives the same code.
					for _, stream := range []bool{false, true} {
						next := e.do(ceRequest{stream: stream})
						ceAssert(t, ceModelUnsupportedRow, ceWant{status: 404, form: ceFormJSON, message: ceModelNotFoundSonet}, next)
					}
					require.Equal(t, 2, e.upstream.calls, "the cooled model is not sent upstream again")
				}},
			{name: "stream request", env: func(e *ceEnv) { e.reply(ceJSON(404, ceUp404)) }, req: ceStream(),
				want: ceWant{status: 404, form: ceFormJSON, message: ceModelNotFoundSonet,
					upstream: &ceUpWant{status: 404, errType: "not_found_error", message: "model: claude-sonnet-5", requestID: "req_011CUp404"}}},
			{name: "after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(404, ceUp404)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormFrame, message: ceModelNotFoundSonet,
					upstream: &ceUpWant{status: 404, errType: "not_found_error", message: "model: claude-sonnet-5", requestID: "req_011CUp404"}}},
			{name: "model cooling on every account", env: func(e *ceEnv) { ceSecondAccount(e); ceModelNotFoundCooling(e) },
				want:  ceWant{status: 404, form: ceFormJSON, message: ceModelNotFoundSonet},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) { require.Zero(t, e.upstream.calls) }},
			{name: "model cooling on every account, stream", env: ceModelNotFoundCooling, req: ceStream(),
				want: ceWant{status: 404, form: ceFormJSON, message: ceModelNotFoundSonet}},
			{name: "model cooling on every account, count_tokens", env: ceModelNotFoundCooling, req: ceCountTokens(),
				want: ceWant{status: 404, form: ceFormJSON, message: ceModelNotFoundSonet}},
		}},

	// ---- The upstream's own answer, no failover: unchanged (a) ----
	{ID: "E-UP-400", Class: ceClassUpstream, HTTP: "400", Body: ceBodyUpstream, Type: ceUpstreamType, Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSONUpFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{env: func(e *ceEnv) { e.reply(ceJSON(400, ceUp400)) }, req: ceStream(), want: ceWant{status: 400, form: ceFormVerbatim, raw: ceUp400}},
			{name: "after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(400, ceUp400)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormUpFrame, raw: ceUp400}},
		}},
	{ID: "E-UP-4XX", Class: ceClassUpstream, HTTP: ceHTTPUpstream, Body: ceBodyUpstream, Type: ceUpstreamType, Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSONUpFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "413, answered by the first account", env: func(e *ceEnv) { ceSecondAccount(e); e.reply(ceJSON(413, ceUp413)) },
				want: ceWant{status: 413, form: ceFormVerbatim, raw: ceUp413},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 1, e.upstream.calls, "no failover")
				}},
			{name: "409", env: func(e *ceEnv) { e.reply(ceJSON(409, ceUp409)) }, want: ceWant{status: 409, form: ceFormVerbatim, raw: ceUp409}},
			{name: "413 after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(413, ceUp413)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormUpFrame, raw: ceUp413}},
			{name: "retried on the same account (custom error codes)", env: func(e *ceEnv) {
				acc := e.apiKeyAccount(9721)
				acc.Credentials["custom_error_codes_enabled"] = true
				acc.Credentials["custom_error_codes"] = []any{float64(429)}
				e.accounts = []*service.Account{acc}
				e.reply(ceJSON(404, ceUp404))
			}, want: ceWant{status: 404, form: ceFormVerbatim, raw: ceUp404},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Greater(t, e.upstream.calls, 1, "retried on the same account")
				}},
		}},
	{ID: "E-UP-REQUEST-429", Class: ceClassUpstream, HTTP: "429", Body: ceBodyUpstream, Type: "rate_limit_error", Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSONUpFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{env: func(e *ceEnv) {
				ceSecondAccount(e)
				e.reply(ceReply{status: 429, body: ceUpRequest429, header: map[string]string{"Retry-After": "30"}})
			}, req: ceStream(), want: ceWant{status: 429, form: ceFormVerbatim, raw: ceUpRequest429},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 1, e.upstream.calls, "no failover")
					require.Empty(t, e.accountRepo.writes, "account not marked")
				}},
			{name: "after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(429, ceUpRequest429)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormUpFrame, raw: ceUpRequest429}},
		}},
	{ID: "E-UP-BEDROCK-CONTENT-FILTER", Class: ceClassUpstream, HTTP: "400", Body: ceBodyUpstreamEnv, Type: "invalid_request_error", Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.accounts = []*service.Account{e.bedrockAccount(9731)}
			e.reply(ceJSON(503, ceBedrockFilter))
		}, want: ceWant{status: 400, form: ceFormJSON, message: "The model response was blocked by content filtering policy."},
			check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
				require.Equal(t, 1, e.upstream.calls, "no failover")
			}}}},

	// ---- Failover ran out: gateway conclusions with the last upstream answer (b) ----
	{ID: "E-UP-429", Class: ceClassGateway, HTTP: "429", Body: ceBodyEnvelope, Type: "rate_limit_error", Code: "upstream_rate_limited",
		Message: "Upstream rate limit exceeded, please retry later", Stream: ceStreamJSONFrame, RetryAfter: ceWhenKnown,
		drives: []ceDrive{
			{env: func(e *ceEnv) {
				ceSecondAccount(e)
				e.reply(ceReply{status: 429, body: ceUpAccount429, header: map[string]string{"Retry-After": "30"}})
			}, req: ceStream(), want: ceWant{status: 429, form: ceFormJSON, retryAfter: "any",
				upstream: &ceUpWant{status: 429, errType: "rate_limit_error", message: "This request would exceed your account's rate limit. Please try again later.", requestID: "req_011CUp429"}},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "failed over to the second account first")
				}},
			{name: "after ping", env: func(e *ceEnv) { cePingFirst(e); e.reply(ceJSON(429, ceUpAccount429)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormFrame,
					upstream: &ceUpWant{status: 429, errType: "rate_limit_error", message: "This request would exceed your account's rate limit. Please try again later.", requestID: "req_011CUp429"}}},
		}},
	{ID: "E-UP-529", Class: ceClassGateway, HTTP: "529", Body: ceBodyEnvelope, Type: "overloaded_error", Code: "upstream_overloaded",
		Message: "Upstream service overloaded, please retry later", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "HTTP 529", env: func(e *ceEnv) { e.reply(ceJSON(529, ceUp529)) },
				want: ceWant{status: 529, form: ceFormJSON, upstream: &ceUpWant{status: 529, errType: "overloaded_error", message: "Overloaded", requestID: "req_011CUp529"}}},
			{name: "overloaded_error event before any output", env: func(e *ceEnv) { e.reply(ceReply{status: 200, body: ceSSEError("overloaded_error", "Overloaded")}) },
				req: ceStream(), want: ceWant{status: 529, form: ceFormJSON, upstream: &ceUpWant{status: 529, errType: "overloaded_error", message: "Overloaded"}}},
		}},
	{ID: "E-UP-5XX", Class: ceClassGateway, HTTP: "502", Body: ceBodyEnvelope, Type: "api_error", Code: "upstream_unavailable",
		Message: "Upstream service temporarily unavailable", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "HTTP 500", env: func(e *ceEnv) { e.reply(ceJSON(500, ceUp500)) },
				want: ceWant{status: 502, form: ceFormJSON, upstream: &ceUpWant{status: 500, errType: "api_error", message: "Internal server error", requestID: "req_011CUp500"}}},
			{name: "connection refused", env: func(e *ceEnv) { e.reply(ceReply{transportErr: syscall.ECONNREFUSED}) }, want: ceWant{status: 502, form: ceFormJSON}},
			{name: "stream broke before the first byte", env: func(e *ceEnv) { e.reply(ceReply{status: 200, readErr: syscall.ECONNRESET}) }, req: ceStream(), want: ceWant{status: 502, form: ceFormJSON}},
		}},
	{ID: "E-UP-ACCOUNT", Class: ceClassGateway, HTTP: "502", Body: ceBodyEnvelope, Type: "api_error", Code: "upstream_account_error",
		Message: "Upstream account unavailable, please contact administrator", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "401 on every account", env: func(e *ceEnv) { ceAPIKeyAccountOnly(e); e.reply(ceJSON(401, ceUp401)) },
				want: ceWant{status: 502, form: ceFormJSON, upstream: &ceUpWant{status: 401, errType: "authentication_error", requestID: "req_011CUp401"}}},
			{name: "403 on every account", env: func(e *ceEnv) { ceAPIKeyAccountOnly(e); e.reply(ceJSON(403, ceUp403)) },
				want: ceWant{status: 502, form: ceFormJSON, upstream: &ceUpWant{status: 403, errType: "permission_error", requestID: "req_011CUp403"}}},
			{name: "402 on every account", env: func(e *ceEnv) {
				ceAPIKeyAccountOnly(e)
				e.accounts = append(e.accounts, e.apiKeyAccount(9722))
				e.reply(ceJSON(402, ceUp402))
			}, want: ceWant{status: 502, form: ceFormJSON, upstream: &ceUpWant{status: 402}},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "402 takes the account out and fails over")
				}},
			{name: "credit balance too low on every account", env: func(e *ceEnv) { ceSecondAccount(e); e.reply(ceJSON(400, ceUpCredit400)) },
				want: ceWant{status: 502, form: ceFormJSON, upstream: &ceUpWant{status: 400, errType: "invalid_request_error", requestID: "req_011CUpCredit"}},
				check: func(t *testing.T, e *ceEnv, rec *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "an account-level 400 fails over")
					require.NotContains(t, rec.Body.String(), "credit balance", "nothing about the account reaches the client")
				}},
			{name: "after ping", env: func(e *ceEnv) { ceAPIKeyAccountOnly(e); cePingFirst(e); e.reply(ceJSON(401, ceUp401)) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormFrame, upstream: &ceUpWant{status: 401, errType: "authentication_error", requestID: "req_011CUp401"}}},
			{name: "count_tokens, 401", env: func(e *ceEnv) { e.reply(ceJSON(401, ceUp401)) }, req: ceCountTokens(),
				want: ceWant{status: 502, form: ceFormJSON, upstream: &ceUpWant{status: 401, errType: "authentication_error", requestID: "req_011CUp401"}}},
		}},
	{ID: "E-UP-REQUEST-FAILED", Class: ceClassGateway, HTTP: "502", Body: ceBodyEnvelope, Type: "api_error", Code: "upstream_request_failed",
		Message: "Upstream request failed", Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "local failure before forwarding", env: func(e *ceEnv) { e.accounts[0].Credentials = map[string]any{} },
				want:  ceWant{status: 502, form: ceFormJSON},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) { require.Zero(t, e.upstream.calls) }},
			{name: "model not found on the accounts tried, others untried", env: func(e *ceEnv) {
				ceSecondAccount(e)
				e.accounts = append(e.accounts, e.setupTokenAccount(9713))
				e.maxSwitches = 1
				e.reply(ceJSON(404, ceUp404))
			}, want: ceWant{status: 502, form: ceFormJSON, upstream: &ceUpWant{status: 404, errType: "not_found_error", message: "model: claude-sonnet-5", requestID: "req_011CUp404"}},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "the switch budget ran out with one account untried")
				}},
			{name: "local failure, after ping", env: func(e *ceEnv) { e.accounts[0].Credentials = map[string]any{}; cePingFirst(e) }, req: ceStream(),
				want: ceWant{status: 200, form: ceFormFrame}},
		}},
	{ID: "E-UP-PASSTHROUGH-RULE", Class: ceClassGateway, HTTP: ceHTTPRule, Body: ceBodyEnvelope, Type: ceTypeOfStatus, Code: "error_passthrough_rule",
		Message: ceRuleMessage, Stream: ceStreamJSONFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "admin rule on a failover answer", env: func(e *ceEnv) { ceTestRule(e); e.reply(ceJSON(503, ceUp500)) },
				want: ceWant{status: 502, form: ceFormJSON, errType: "api_error", message: "Internal server error",
					upstream: &ceUpWant{status: 503, errType: "api_error", message: "Internal server error", requestID: "req_011CUp500"}}},
			{name: "prod rule 1, HTTP 403 with a keyword", env: func(e *ceEnv) { ceWithRule1(e); ceAPIKeyAccountOnly(e); e.reply(ceJSON(403, ceUp403Keyword)) },
				want: ceWant{status: 502, form: ceFormJSON, errType: "api_error", message: ceUnableComplete,
					upstream: &ceUpWant{status: 403, errType: "permission_error", requestID: "req_011CUp403k"}}},
		}},

	// ---- Errors after output started (HTTP 200 already sent) ----
	{ID: "E-STREAM-UPSTREAM-ERROR", Class: ceClassUpstream, HTTP: "200", Body: ceBodyUpstream, Type: ceUpstreamType, Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamUpFrame, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "Overloaded mid-stream", env: func(e *ceEnv) {
				ceSecondAccount(e)
				e.reply(ceReply{status: 200, body: ceStreamStart + ceSSEError("overloaded_error", "Overloaded")})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormUpFrame, raw: ceSSEErrorData("overloaded_error", "Overloaded")},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 1, e.upstream.calls, "no failover after output")
				}},
			{name: "content filter mid-stream", env: func(e *ceEnv) {
				e.reply(ceReply{status: 200, body: ceStreamStart + ceSSEError("invalid_request_error", ceContentFilter)})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormUpFrame, raw: ceSSEErrorData("invalid_request_error", ceContentFilter)}},
			{name: "prod rule 1 loaded, Overloaded mid-stream", env: func(e *ceEnv) {
				ceWithRule1(e)
				e.reply(ceReply{status: 200, body: ceStreamStart + ceSSEError("overloaded_error", "Overloaded")})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormUpFrame, raw: ceSSEErrorData("overloaded_error", "Overloaded")}},
			{name: "content filter before any output, every account", env: func(e *ceEnv) {
				ceSecondAccount(e)
				e.reply(ceReply{status: 200, body: ceSSEError("invalid_request_error", ceContentFilter)})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormUpFrame, raw: ceSSEErrorData("invalid_request_error", ceContentFilter)},
				check: func(t *testing.T, e *ceEnv, _ *httptest.ResponseRecorder) {
					require.Equal(t, 2, e.upstream.calls, "an error event before output fails over")
				}},
			{name: "api_error before any output", env: func(e *ceEnv) {
				e.reply(ceReply{status: 200, body: ceSSEError("api_error", ceUnableComplete)})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormUpFrame, raw: ceSSEErrorData("api_error", ceUnableComplete)}},
			{name: "prod rule 1 loaded, content filter before any output", env: func(e *ceEnv) {
				ceWithRule1(e)
				e.reply(ceReply{status: 200, body: ceSSEError("invalid_request_error", ceContentFilter)})
			}, req: ceStream(), want: ceWant{status: 200, form: ceFormUpFrame, raw: ceSSEErrorData("invalid_request_error", ceContentFilter)}},
		}},
	{ID: "E-STREAM-DISCONNECTED", Class: ceClassGateway, HTTP: "200", Body: ceBodyEnvelope, Type: "api_error", Code: "stream_read_error",
		Message: "upstream stream disconnected: {reason}", Stream: ceStreamFrame, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.reply(ceReply{status: 200, body: ceStreamStart, readErr: syscall.ECONNRESET}) }, req: ceStream(),
			want: ceWant{status: 200, form: ceFormFrame, message: "upstream stream disconnected: connection reset by peer"}}}},
	{ID: "E-STREAM-IDLE", Class: ceClassGateway, HTTP: "200", Body: ceBodyEnvelope, Type: "api_error", Code: "stream_timeout",
		Message: "upstream stream idle for {interval}", Stream: ceStreamFrame, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.cfg.Gateway.StreamDataIntervalTimeout = 1
			e.reply(ceReply{status: 200, body: ceStreamStart, hang: true})
		}, req: ceStream(), want: ceWant{status: 200, form: ceFormFrame, message: "upstream stream idle for 1s"}}}},
	{ID: "E-STREAM-LINE-TOO-LONG", Class: ceClassGateway, HTTP: "200", Body: ceBodyEnvelope, Type: "api_error", Code: "response_too_large",
		Message: "upstream SSE line exceeded {n} bytes", Stream: ceStreamFrame, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) {
			e.cfg.Gateway.MaxLineSize = 4096
			e.reply(ceReply{status: 200, body: ceStreamStart + "event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("x", 8192) + `"}}` + "\n\n"})
		}, req: ceStream(), want: ceWant{status: 200, form: ceFormFrame, message: "upstream SSE line exceeded 4096 bytes"}}}},
	{ID: "E-STREAM-INCOMPLETE", Class: ceClassGateway, HTTP: "200", Body: ceBodyEnvelope, Type: "api_error", Code: "upstream_stream_incomplete",
		Message: "Upstream stream ended unexpectedly", Stream: ceStreamFrame, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.reply(ceReply{status: 200, body: ceStreamStart}) }, req: ceStream(), want: ceWant{status: 200, form: ceFormFrame},
			check: func(t *testing.T, _ *ceEnv, rec *httptest.ResponseRecorder) {
				require.Equal(t, ceUpstreamR, rec.Header().Get("X-Request-ID"), "a stream that reached the upstream carries the upstream request id")
			}}}},

	// ---- Client went away ----
	{ID: "E-CLIENT-GONE", Class: ceDash, HTTP: "499", Body: ceBodyNone, Type: ceDash, Code: ceDash,
		Message: ceDash, Stream: ceStreamNone, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.keyLookErr = context.Canceled }, req: ceRequest{ctx: ceCanceledCtx()}, want: ceWant{status: 499, form: ceFormEmpty}}}},

	// ---- /v1/messages/count_tokens (never a stream, never fails over) ----
	{ID: "E-CT-POOL-EXHAUSTED", Class: ceClassGateway, HTTP: "503", Body: ceBodyEnvelope, Type: "api_error", Code: "no_available_accounts",
		Message: "Service temporarily unavailable", Stream: ceStreamJSON, RetryAfter: ceWhenKnown,
		drives: []ceDrive{{env: func(e *ceEnv) {
			reset := time.Now().Add(10 * time.Minute)
			e.accounts[0].RateLimitResetAt = &reset
		}, req: ceCountTokens(), want: ceWant{status: 503, form: ceFormJSON, retryAfter: "any"}}}},
	{ID: "E-CT-BEDROCK", Class: ceClassGateway, HTTP: "404", Body: ceBodyEnvelope, Type: "not_found_error", Code: "count_tokens_unsupported",
		Message: "count_tokens endpoint is not supported for Bedrock", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.accounts = []*service.Account{e.bedrockAccount(9731)} }, req: ceCountTokens(), want: ceWant{status: 404, form: ceFormJSON}}}},
	{ID: "E-CT-UP-ANSWER", Class: ceClassUpstream, HTTP: ceHTTPUpstream, Body: ceBodyUpstream, Type: ceUpstreamType, Code: ceDash,
		Message: ceUpstreamText, Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{
			{name: "429", env: func(e *ceEnv) { e.reply(ceJSON(429, ceUpAccount429)) }, req: ceCountTokens(), want: ceWant{status: 429, form: ceFormVerbatim, raw: ceUpAccount429}},
			{name: "529", env: func(e *ceEnv) { e.reply(ceJSON(529, ceUp529)) }, req: ceCountTokens(), want: ceWant{status: 529, form: ceFormVerbatim, raw: ceUp529}},
			{name: "404", env: func(e *ceEnv) { e.reply(ceJSON(404, ceUp404)) }, req: ceCountTokens(), want: ceWant{status: 404, form: ceFormVerbatim, raw: ceUp404}},
			{name: "400", env: func(e *ceEnv) { e.reply(ceJSON(400, ceUp400)) }, req: ceCountTokens(), want: ceWant{status: 400, form: ceFormVerbatim, raw: ceUp400}},
			{name: "503", env: func(e *ceEnv) { e.reply(ceJSON(503, ceUp500)) }, req: ceCountTokens(), want: ceWant{status: 503, form: ceFormVerbatim, raw: ceUp500}},
		}},
	{ID: "E-CT-REQUEST-FAILED", Class: ceClassGateway, HTTP: "502", Body: ceBodyEnvelope, Type: "api_error", Code: "upstream_unavailable",
		Message: "Request failed", Stream: ceStreamJSON, RetryAfter: ceNo,
		drives: []ceDrive{{env: func(e *ceEnv) { e.reply(ceReply{transportErr: syscall.ECONNREFUSED}) }, req: ceCountTokens(), want: ceWant{status: 502, form: ceFormJSON}}}},
}

// ceModelUnsupportedRow is the shape of E-UP-MODEL-NOT-FOUND, for asserting a
// follow-up request inside a drive.
var ceModelUnsupportedRow = ceRow{ID: "E-UP-MODEL-NOT-FOUND", Class: ceClassGateway, HTTP: "404", Body: ceBodyEnvelope,
	Type: "not_found_error", Code: "model_unsupported", Stream: ceStreamJSONFrame, RetryAfter: ceNo}

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
		ceCheckRowShape(t, row)
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

// ceCheckRowShape holds the rules of doc §1 on every row: (a) rows carry no
// code, (b) rows always do and use the standard type of their status.
func ceCheckRowShape(t *testing.T, row ceRow) {
	t.Helper()
	switch row.Class {
	case ceClassUpstream:
		require.Equal(t, ceDash, row.Code, "row %s: an upstream answer carries no error.code", row.ID)
		require.NotEqual(t, ceBodyEnvelope, row.Body, "row %s", row.ID)
	case ceClassGateway:
		require.NotEqual(t, ceDash, row.Code, "row %s: a gateway answer always carries error.code", row.ID)
		require.Equal(t, ceBodyEnvelope, row.Body, "row %s", row.ID)
		if status, err := strconv.Atoi(row.HTTP); err == nil && status != http.StatusOK {
			require.Equal(t, service.XSciStandardErrorType(status), row.Type, "row %s: error.type is the standard type of its status", row.ID)
		}
	default:
		require.Equal(t, ceBodyNone, row.Body, "row %s", row.ID)
	}
}

func ceAssert(t *testing.T, row ceRow, want ceWant, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	require.Equal(t, want.status, rec.Code, "status; body: %s", body)
	switch row.HTTP {
	case ceHTTPUpstream, ceHTTPRule:
	default:
		if want.form == ceFormFrame || want.form == ceFormUpFrame {
			require.Equal(t, http.StatusOK, rec.Code, "a frame rides on a 200 stream")
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

	switch want.form {
	case ceFormEmpty:
		require.Empty(t, body)
		return
	case ceFormVerbatim:
		require.Equal(t, ceBodyUpstream, row.Body)
		require.Equal(t, want.raw, body, "upstream body byte for byte")
		require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"))
		return
	case ceFormJSON:
		require.NotContains(t, body, "data:", "a JSON error, not a frame")
		require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json"), "JSON content type, got %q", rec.Header().Get("Content-Type"))
		ceAssertEnvelope(t, row, want, []byte(body), rec.Header().Get("X-Request-ID"))
		return
	}

	blocks := ceSSEBlocks(body)
	require.NotEmpty(t, blocks, "SSE frames; body: %s", body)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Equal(t, 1, ceCountErrorFrames(blocks), "one error frame; body: %s", body)
	last := blocks[len(blocks)-1]
	require.True(t, strings.HasPrefix(last, "event: error\ndata: "), "the stream ends with an `event: error` frame; got %q", last)
	data := strings.TrimPrefix(last, "event: error\ndata: ")
	require.NotContains(t, data, "\n")
	switch want.form {
	case ceFormUpFrame:
		require.Equal(t, want.raw, data, "the upstream's error data, as sent")
	case ceFormFrame:
		ceAssertEnvelope(t, row, want, []byte(data), rec.Header().Get("X-Request-ID"))
	default:
		t.Fatalf("unknown form %q", want.form)
	}
}

func ceAssertEnvelope(t *testing.T, row ceRow, want ceWant, raw []byte, requestID string) {
	t.Helper()
	message := want.message
	if message == "" {
		message = row.Message
	}
	errType := want.errType
	if errType == "" {
		errType = row.Type
	}
	code := want.code
	if code == "" {
		code = row.Code
	}
	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload), "JSON body: %s", raw)
	require.Equal(t, "error", payload["type"])
	wantTop := 2
	if requestID != "" && row.Body == ceBodyEnvelope {
		require.Equal(t, requestID, payload["request_id"], "request_id is the X-Request-ID of the response")
		wantTop = 3
	} else if _, ok := payload["request_id"]; ok {
		wantTop = 3
	}
	require.Len(t, payload, wantTop, "envelope is {type,error[,request_id]}: %s", raw)
	inner, ok := payload["error"].(map[string]any)
	require.True(t, ok, "error object: %s", raw)
	require.Equal(t, errType, inner["type"])
	require.Equal(t, message, inner["message"])
	keys := 2
	if row.Body == ceBodyEnvelope {
		require.Equal(t, code, inner["code"], "error.code")
		keys++
	} else {
		require.NotContains(t, inner, "code", "an upstream answer has no error.code: %s", raw)
	}
	if want.upstream == nil {
		require.NotContains(t, inner, "upstream", "no error.upstream: %s", raw)
	} else {
		keys++
		up, ok := inner["upstream"].(map[string]any)
		require.True(t, ok, "error.upstream: %s", raw)
		require.Equal(t, float64(want.upstream.status), up["status"])
		ceOptional(t, up, "type", want.upstream.errType)
		ceOptional(t, up, "message", want.upstream.message)
		ceOptional(t, up, "request_id", want.upstream.requestID)
	}
	require.Len(t, inner, keys, "error object fields: %s", raw)
}

func ceOptional(t *testing.T, m map[string]any, key, want string) {
	t.Helper()
	if want == "" {
		require.NotContains(t, m, key, "error.upstream.%s absent", key)
		return
	}
	require.Equal(t, want, m[key], "error.upstream.%s", key)
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

// TestXSciAvailabilityAgreesWithContract: GET /v1/sub2api/availability gives
// the code a request would get.
func TestXSciAvailabilityAgreesWithContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	probe := ceRequest{method: http.MethodGet, path: "/v1/sub2api/availability", query: "model=" + ceModel}
	cases := []struct {
		name string
		env  func(e *ceEnv)
		want map[string]any
	}{
		{"available", nil, map[string]any{"available": true}},
		{"model cooling on every account (upstream: model not found)", func(e *ceEnv) { ceSecondAccount(e); ceModelNotFoundCooling(e) },
			map[string]any{"available": false, "code": service.XSciCodeModelUnsupported}},
		{"no account configured for the model", func(e *ceEnv) {
			e.accounts[0].Credentials["model_mapping"] = map[string]any{"claude-opus-5": "claude-opus-5"}
		}, map[string]any{"available": false, "code": service.XSciCodeModelUnsupported}},
		{"every account rate-limited", func(e *ceEnv) {
			reset := time.Now().Add(10 * time.Minute)
			e.accounts[0].RateLimitResetAt = &reset
		}, map[string]any{"available": false, "code": service.XSciCodeNoAvailableAccounts, "retry_after_s": "any"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newCEEnv(t)
			if tc.env != nil {
				tc.env(e)
			}
			rec := e.do(probe)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			if tc.want["retry_after_s"] == "any" {
				v, ok := got["retry_after_s"].(float64)
				require.True(t, ok, "retry_after_s: %v", got)
				require.Positive(t, v)
				got["retry_after_s"] = "any"
			}
			require.Equal(t, tc.want, got)
		})
	}
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
		require.Len(t, cells, 9, "contract row %v", cells)
		docIDs = append(docIDs, cells[0])
		byID[cells[0]] = cells
	}
	for _, row := range ceContract {
		goIDs = append(goIDs, row.ID)
		cells, ok := byID[row.ID]
		if !ok {
			continue
		}
		want := []string{row.ID, row.Class, row.HTTP, row.Body, row.Type, row.Code, row.Message, row.Stream, row.RetryAfter}
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
