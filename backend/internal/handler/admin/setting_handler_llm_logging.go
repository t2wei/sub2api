package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

// LLMLoggingSettingResponse [OXSCI] call logging to data-service. Enabled is the
// runtime switch (default off); SinkConfigured says whether a sink URL is configured,
// without which the switch has no effect.
type LLMLoggingSettingResponse struct {
	Enabled        bool `json:"enabled"`
	SinkConfigured bool `json:"sink_configured"`
}

// UpdateLLMLoggingSettingRequest turns call logging on or off; enabled must be sent.
type UpdateLLMLoggingSettingRequest struct {
	Enabled *bool `json:"enabled"`
}

// GetLLMLoggingSetting 获取调用日志开关
// GET /api/v1/admin/settings/llm-logging
func (h *SettingHandler) GetLLMLoggingSetting(c *gin.Context) {
	enabled, err := h.settingService.GetLLMLoggingEnabled(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, LLMLoggingSettingResponse{
		Enabled:        enabled,
		SinkConfigured: h.settingService.LLMLoggingSinkConfigured(),
	})
}

// UpdateLLMLoggingSetting 打开或关闭调用日志
// PUT /api/v1/admin/settings/llm-logging
func (h *SettingHandler) UpdateLLMLoggingSetting(c *gin.Context) {
	var req UpdateLLMLoggingSettingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Enabled == nil {
		response.BadRequest(c, "enabled is required")
		return
	}
	if err := h.settingService.SetLLMLoggingEnabled(c.Request.Context(), *req.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, LLMLoggingSettingResponse{
		Enabled:        *req.Enabled,
		SinkConfigured: h.settingService.LLMLoggingSinkConfigured(),
	})
}
