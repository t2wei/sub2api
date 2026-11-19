package service

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// opsUpstreamKindRequestLevel429 marks, in the ops upstream-error trail, an
// Anthropic 429 that was answered to the caller without failover or cooldown.
const opsUpstreamKindRequestLevel429 = "request_level_429"

// anthropicRateLimitWindows are the Anthropic unified rate-limit windows whose
// headers the 429 handling reads (HandleUpstreamError / handle429).
var anthropicRateLimitWindows = []string{"5h", "7d", "7d_oi"}

// isAnthropicRequestLevel429 reports whether an upstream 429 rejects this
// request rather than the account (OxSci, P160 T3).
//
// Anthropic answers some Claude Code requests on subscription (OAuth /
// setup-token) accounts with
//
//	429 {"type":"error","error":{"type":"rate_limit_error","message":"Error"}}
//
// and none of the rate-limit window headers. Every account in the group gives
// the same answer for the same request (P160 T3: 283 such 429s over 4 weeks,
// 0 recovered by failover), while other requests on those accounts succeed.
// Treated as an account limit, each one walked every account in the group and
// put each on the no-reset fallback cooldown, so for ~10 s other users of the
// group got "No available accounts".
//
// Kept narrow: Anthropic OAuth / setup-token accounts only (API-key accounts
// keep today's handling), status 429, error type rate_limit_error with the
// bare message "Error" and no error details, and none of the account-limit
// signals the 429 handling reads (see hasAnthropicAccountRateLimitSignal).
// Anything else keeps the account-limit path.
func isAnthropicRequestLevel429(account *Account, statusCode int, headers http.Header, body []byte) bool {
	if account == nil || !account.IsAnthropicOAuthOrSetupToken() || statusCode != http.StatusTooManyRequests {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.message").String()), "Error") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.type").String()), "rate_limit_error") {
		return false
	}
	// Structured details (e.g. Fable credits_required) have their own handling.
	if gjson.GetBytes(body, "error.details").Exists() {
		return false
	}
	return !hasAnthropicAccountRateLimitSignal(headers)
}

// hasAnthropicAccountRateLimitSignal reports whether a 429 carries any of the
// Anthropic headers that the account rate-limit handling acts on: a window
// reset time (aggregated, 5h, 7d, 7d_oi), a rejected window, or an exhausted
// window. Retry-After is not among them: the Anthropic 429 path does not read
// it either.
func hasAnthropicAccountRateLimitSignal(headers http.Header) bool {
	if headers == nil {
		return false
	}
	if strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-reset")) != "" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-status")), "rejected") {
		return true
	}
	for _, window := range anthropicRateLimitWindows {
		prefix := "anthropic-ratelimit-unified-" + window + "-"
		if strings.TrimSpace(headers.Get(prefix+"reset")) != "" {
			return true
		}
		if isAnthropicWindowRejected(headers, window) || isAnthropicWindowExceeded(headers, window) {
			return true
		}
	}
	return false
}

// handleAnthropicRequestLevel429 answers a request-level 429 (see
// isAnthropicRequestLevel429) to the caller: no failover and no account state
// change, so the account stays schedulable for everyone else. The upstream
// error is still recorded in the ops upstream-error trail, and the caller gets
// Anthropic's own answer (429 rate_limit_error "Error") rather than the
// gateway's generic "please retry later", without Retry-After: the same
// request is rejected again on any account.
func (s *GatewayService) handleAnthropicRequestLevel429(c *gin.Context, resp *http.Response, account *Account, body []byte) (*ForwardResult, error) {
	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	if upstreamMsg == "" {
		upstreamMsg = "Error"
	}

	logger.LegacyPrintf("service.gateway", "[Forward] Upstream request-level 429 (no failover, account not marked): Account=%d(%s) Status=%d RequestID=%s Body=%s",
		account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(body), 1000))

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(body), maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               opsUpstreamKindRequestLevel429,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})

	MarkResponseCommitted(c)

	status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		account.Platform,
		resp.StatusCode,
		body,
		http.StatusTooManyRequests,
		"rate_limit_error",
		upstreamMsg,
	)
	writeGatewayErrorResponse(c, status, errType, errMsg)

	note := "request-level, no failover"
	if matched {
		note += ", passthrough rule matched"
	}
	return nil, fmt.Errorf("upstream error: %d (%s) message=%s", resp.StatusCode, note, upstreamMsg)
}
