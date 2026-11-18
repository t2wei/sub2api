package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// OIDCRequiredGroupsResponse OIDC 登录要求的组；groups 为空表示不限。
type OIDCRequiredGroupsResponse struct {
	Groups []string `json:"groups"`
	Max    int      `json:"max"`
}

// UpdateOIDCRequiredGroupsRequest 整体替换登录要求的组；groups 必须显式提交，
// 传空数组表示取消限制。
type UpdateOIDCRequiredGroupsRequest struct {
	Groups *[]string `json:"groups"`
}

// GetOIDCRequiredGroups 获取 OIDC 登录要求的组
// GET /api/v1/admin/settings/oidc-required-groups
func (h *SettingHandler) GetOIDCRequiredGroups(c *gin.Context) {
	groups, err := h.settingService.GetOIDCRequiredGroups(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, OIDCRequiredGroupsResponse{
		Groups: groups,
		Max:    service.MaxOIDCRequiredGroups,
	})
}

// UpdateOIDCRequiredGroups 整体替换 OIDC 登录要求的组
// PUT /api/v1/admin/settings/oidc-required-groups
func (h *SettingHandler) UpdateOIDCRequiredGroups(c *gin.Context) {
	var req UpdateOIDCRequiredGroupsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Groups == nil {
		response.BadRequest(c, "groups is required")
		return
	}
	saved, err := h.settingService.SetOIDCRequiredGroups(c.Request.Context(), *req.Groups)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, OIDCRequiredGroupsResponse{
		Groups: saved,
		Max:    service.MaxOIDCRequiredGroups,
	})
}
