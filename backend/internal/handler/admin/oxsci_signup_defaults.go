package admin

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// SignupDefaultsResponse is what a first sign-in through source subscribes a new
// account to: each group and its number of days.
type SignupDefaultsResponse struct {
	Source        string                               `json:"source"`
	Subscriptions []service.DefaultSubscriptionSetting `json:"subscriptions"`
}

// GetSignupDefaults answers the subscriptions a first sign-in grants a new account,
// as the sign-up resolves them (SettingService.SignupGrantSubscriptions), and
// nothing else of the settings — so a scoped admin key can be given exactly this
// read (settings.signup_defaults.read) without the whole settings record. source
// defaults to oidc; an unknown one is 400 SIGNUP_SOURCE_UNKNOWN.
// GET /api/v1/admin/settings/signup-defaults?source=
func (h *SettingHandler) GetSignupDefaults(c *gin.Context) {
	source := strings.ToLower(strings.TrimSpace(c.Query("source")))
	if source == "" {
		source = service.DefaultSignupGrantSource
	}
	subscriptions, err := h.settingService.SignupGrantSubscriptions(c.Request.Context(), source)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, SignupDefaultsResponse{Source: source, Subscriptions: subscriptions})
}
