package handler

// [OXSCI] Client error contract of the Anthropic Messages surface (P160 T9),
// handler side. The shaping itself is service/xsci_client_errors.go; the
// GatewayHandler writers (errorResponseWithCode, handleStreamingAwareErrorWithCode,
// handleFailoverExhausted, handleFailoverExhaustedSimple,
// ensureForwardErrorResponse) and classifyNoAccountErrorFromGin hand over to
// the functions below with a one-line hook when the request is on the surface.

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// XSciClientErrorContract is the route middleware that switches the contract
// layer on for /v1/messages and /v1/messages/count_tokens. It goes first in
// the chain so that every later writer (API-key auth included) sees the mark.
func XSciClientErrorContract() gin.HandlerFunc {
	return func(c *gin.Context) {
		service.MarkXSciAnthropicSurface(c)
		c.Next()
	}
}

// XSciClientErrorContractWith marks the request, then runs next in the same
// chain slot (for route helpers whose middleware list is fixed).
func XSciClientErrorContractWith(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		service.MarkXSciAnthropicSurface(c)
		next(c)
	}
}

// xsciStreamingAwareError writes a gateway answer from what the existing
// writers pass in.
func xsciStreamingAwareError(c *gin.Context, status int, errType, code, message string, streamStarted bool) {
	service.XSciWriteGatewayError(c, service.XSciClassifyGatewayError(status, errType, code, message), streamStarted)
}

// xsciFailoverExhausted answers once failover ran out (fe is the last
// upstream failure; nil when no upstream answered).
func (h *GatewayHandler) xsciFailoverExhausted(c *gin.Context, fe *service.UpstreamFailoverError, platform string, streamStarted bool) {
	if service.IsResponseCommitted(c) {
		return // the upstream's frame already went out (error after output started)
	}
	if fe != nil {
		service.SetOpsUpstreamError(c, fe.StatusCode, service.SanitizeUpstreamErrorMessage(service.ExtractUpstreamErrorMessage(fe.ResponseBody)), "")
		// An upstream `event: error` before any output that is not an overload
		// (content filter, api_error, …) is the upstream's own answer to the
		// request: every account gave it, it goes back as the upstream sent it.
		if raw, ok := service.XSciUpstreamStreamErrorEvent(c); ok && bytes.Equal(raw, fe.ResponseBody) &&
			gjson.GetBytes(raw, "error.type").String() != "overloaded_error" {
			service.XSciForwardUpstreamStreamErrorEvent(c, raw)
			return
		}
	}

	model := xsciRequestModel(c)
	var groupID *int64
	if apiKey, ok := middleware2.GetAPIKeyFromContext(c); ok && apiKey != nil {
		groupID = apiKey.GroupID
	}
	modelUnsupported := false
	if service.XSciIsUpstreamModelNotFound(fe) && h.gatewayService != nil {
		modelUnsupported = h.gatewayService.XSciModelUnsupported(c.Request.Context(), groupID, model, platform, service.XSciTriedAnswersOf(c))
	}
	e := service.XSciFailoverAnswer(fe, modelUnsupported, model)

	// An admin error-passthrough rule still shapes gateway answers.
	if fe != nil && h.errorPassthroughService != nil && len(fe.ResponseBody) > 0 {
		if rule := h.errorPassthroughService.MatchRule(platform, fe.StatusCode, fe.ResponseBody); rule != nil {
			status := fe.StatusCode
			if !rule.PassthroughCode && rule.ResponseCode != nil {
				status = *rule.ResponseCode
			}
			msg := service.ExtractUpstreamErrorMessage(fe.ResponseBody)
			if !rule.PassthroughBody && rule.CustomMessage != nil {
				msg = *rule.CustomMessage
			}
			if rule.SkipMonitoring {
				c.Set(service.OpsSkipPassthroughKey, true)
			}
			e = service.XSciGatewayError{Status: status, Code: service.XSciCodeErrorPassthroughRule, Message: msg, Upstream: e.Upstream}
		}
	}
	if e.Code == service.XSciCodeUpstreamRateLimited {
		// Same estimate as "No available accounts": when the first account
		// whose rate limit was just recorded comes back.
		h.setNoAccountRetryAfter(c, groupID, model, platform)
	}
	service.XSciWriteGatewayError(c, e, streamStarted)
}

// xsciRequestModel is the model the client asked for (the one account
// selection compares against).
func xsciRequestModel(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(opsModelKey); ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// xsciModelNotFoundEverywhere turns "no account available" into
// model_unsupported (404) when the accounts of the group answered "model not
// found" for the model (service.XSciModelUnsupported: every configured account
// is cooling the model down for that reason, or — for a model the group did
// not serve in the last 7 days — the ones tried did and the rest are not
// selectable now). Without this the pool reads as busy (503 + Retry-After)
// although no account can serve the model.
func xsciModelNotFoundEverywhere(
	c *gin.Context,
	ctx context.Context,
	diag service.ModelAvailabilityDiagnoser,
	apiKey *service.APIKey,
	routingModel, displayModel, platform string,
	cls noAccountErrorClassification,
) noAccountErrorClassification {
	if cls.ModelNotFound || !service.XSciAnthropicSurface(c) || platform != service.PlatformAnthropic || apiKey == nil || apiKey.GroupID == nil {
		return cls
	}
	svc, ok := diag.(interface {
		XSciModelUnsupported(context.Context, *int64, string, string, service.XSciTriedAnswers) bool
	})
	if !ok || !svc.XSciModelUnsupported(ctx, apiKey.GroupID, routingModel, platform, service.XSciTriedAnswersOf(c)) {
		return cls
	}
	if strings.TrimSpace(displayModel) == "" {
		displayModel = routingModel
	}
	return noAccountErrorClassification{
		Status:        http.StatusNotFound,
		ErrType:       "model_not_found",
		Message:       service.XSciModelNotFoundMessage(strings.TrimSpace(displayModel)),
		ModelNotFound: true,
	}
}
