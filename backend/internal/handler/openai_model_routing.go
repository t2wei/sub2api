package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const openAIModelRoutingAmbiguousMessage = "Conflicting model fields are not allowed for routed OpenAI requests"
const openAIModelRoutingAccountMessage = "Model is not available for this connection; please reconnect"

// Preserve request bytes (including identical duplicate fields), but reject
// ambiguous model identities that could select one route and execute another.
func (h *OpenAIGatewayHandler) rejectAmbiguousModelRouting(c *gin.Context, apiKey *service.APIKey, body []byte, writeError func(*gin.Context, int, string, string)) bool {
	models := requestmodel.FromBodyCandidates(c.FullPath(), c.GetHeader("Content-Type"), body)
	if err := h.gatewayService.ValidateModelRoutingCandidates(c.Request.Context(), apiKey.GroupID, openAICompatibleRequestPlatform(c.Request.Context(), apiKey), models); err != nil {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
		middleware.MarkIngressRejected(c, middleware.IngressRejectModelNotAllowed)
		writeError(c, http.StatusBadRequest, "invalid_request_error", openAIModelRoutingAmbiguousMessage)
		return true
	}
	return false
}
