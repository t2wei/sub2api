package handler

// [OXSCI] Pool availability for agents, and Retry-After on "No available
// accounts". Kept in its own file so upstream rebases only meet the one-line
// call sites in the existing handlers.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// keyAvailabilityResponse is the body of GET /v1/sub2api/availability.
// When available is false, Code says why, with the same error.code a request
// would get (docs/xsci-client-error-contract.md): no_available_accounts (come
// back later; RetryAfterSeconds when the gateway knows when the first account
// comes back) or model_unsupported (no account of the group can serve the
// model; waiting does not help).
type keyAvailabilityResponse struct {
	Available         bool   `json:"available"`
	Code              string `json:"code,omitempty"`
	RetryAfterSeconds *int   `json:"retry_after_s,omitempty"`
}

// KeyAvailability answers "would a request with this key and model find an
// account right now?" for the key's group.
// GET /v1/sub2api/availability?model=<model>
//
// It runs the same account filter as count_tokens (SelectAccountForModel):
// no concurrency slot is taken, nothing is sent upstream and no usage is
// recorded. model may be omitted. The check is made as a Claude Code client
// would see the group, because the callers are agents that call the model
// through the Claude CLI; a claude_code_only group is therefore checked
// itself, not its fallback group.
func (h *GatewayHandler) KeyAvailability(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if apiKey.GroupID == nil || apiKey.Group == nil {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key is not assigned to a group")
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	reqLog := requestLogger(
		c,
		"handler.gateway.availability",
		zap.Int64("api_key_id", apiKey.ID),
		zap.Int64p("group_id", apiKey.GroupID),
		zap.String("model", model),
	)

	ensureCompositeTargetPlatform(c, apiKey, model)
	platform := effectiveAPIKeyPlatform(c, apiKey)
	if platform == "" || platform == service.PlatformComposite || domain.UsesOpenAIGateway(platform) {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Availability is not supported for this group platform")
		return
	}

	ctx := service.SetClaudeCodeClient(c.Request.Context(), true)
	c.Header("Cache-Control", "no-store")
	_, err := h.gatewayService.SelectAccountForModel(ctx, apiKey.GroupID, "", model)
	if err == nil {
		c.JSON(http.StatusOK, keyAvailabilityResponse{Available: true})
		return
	}
	if !errors.Is(err, service.ErrNoAvailableAccounts) {
		reqLog.Error("gateway.availability_select_failed", zap.Error(err))
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Availability check failed")
		return
	}

	if model != "" {
		// Same verdicts as a request: no account configured for the model, or
		// every configured account answered "model not found" (P160 T9).
		cls := classifyNoAccountError(ctx, h.gatewayService, apiKey, model, model, platform)
		if cls.ModelNotFound || (platform == service.PlatformAnthropic &&
			h.gatewayService.XSciModelNotFoundOnEveryAccount(ctx, apiKey.GroupID, model, platform)) {
			c.JSON(http.StatusOK, keyAvailabilityResponse{Available: false, Code: service.XSciCodeModelUnsupported})
			return
		}
	}

	at, known, err := h.gatewayService.EarliestRecoveryForPlatform(ctx, apiKey.GroupID, model, platform)
	if err != nil {
		reqLog.Error("gateway.availability_recovery_failed", zap.Error(err))
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Availability check failed")
		return
	}
	resp := keyAvailabilityResponse{Available: false, Code: service.XSciCodeNoAvailableAccounts}
	if known {
		seconds := service.RetryAfterSeconds(at, time.Now())
		resp.RetryAfterSeconds = &seconds
	}
	c.JSON(http.StatusOK, resp)
}

// setNoAccountRetryAfter adds Retry-After (whole seconds, rounded up) to a
// "No available accounts" response, from the earliest time an account in the
// group that serves the model becomes selectable again. When that time is
// unknown, or the response has already started (an SSE stream after ping),
// the header is left out; the response body is never changed.
//
// A failed lookup is logged and the header left out: the 503 itself must
// still go out unchanged.
func (h *GatewayHandler) setNoAccountRetryAfter(c *gin.Context, groupID *int64, model, platform string) {
	if c == nil || c.Request == nil || c.Writer.Written() {
		return
	}
	at, known, err := h.gatewayService.EarliestRecoveryForPlatform(c.Request.Context(), groupID, model, platform)
	if err != nil {
		requestLogger(c, "handler.gateway.retry_after").Warn(
			"gateway.no_account_retry_after_lookup_failed",
			zap.Int64p("group_id", groupID),
			zap.String("model", model),
			zap.String("platform", platform),
			zap.Error(err),
		)
		return
	}
	if !known {
		return
	}
	c.Header("Retry-After", strconv.Itoa(service.RetryAfterSeconds(at, time.Now())))
}
