//go:build unit

package routes

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newSignupDefaultsRouteEngine(t *testing.T, policy adminaccess.Policy) (*gin.Engine, string) {
	t.Helper()
	keys := &scopedRouteKeys{}
	auth := service.NewSettingService(nil, nil)
	auth.SetScopedAdminKeyRepository(keys)
	key, _, err := auth.CreateScopedAdminKey(context.Background(), service.ScopedAdminKeyInput{Name: "ds-token-service", Enabled: true, Policy: policy}, 9)
	require.NoError(t, err)
	settings := service.NewSettingService(&channelMonitorRouteSettingRepoStub{values: map[string]string{
		service.SettingKeyDefaultSubscriptions: `[{"group_id":15,"validity_days":180},{"group_id":5,"validity_days":180}]`,
	}}, &config.Config{})
	users := service.NewUserService(&scopedRouteUsers{}, nil, nil, nil)
	r := gin.New()
	RegisterAdminRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{
		Setting: adminhandler.NewSettingHandler(settings, nil, nil, nil, nil, nil, nil),
	}}, middleware.NewAdminAuthMiddleware(nil, users, auth, nil), middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }), middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)
	return r, key
}

// A scoped key reaches the signup defaults only through their own permission, and
// that permission opens nothing else of the settings.
func TestScopedAdminKeyReachesSignupDefaultsOnlyWithItsPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const path = "/api/v1/admin/settings/signup-defaults"
	for _, tc := range []struct {
		name        string
		permissions []string
		allowed     bool
	}{
		{"what DS holds is not enough", []string{"users.read", "users.identity.read", "subscriptions.read", "subscriptions.assign", "subscriptions.extend", "subscriptions.revoke", "usage.read"}, false},
		{"settings.signup_defaults.read", []string{"settings.signup_defaults.read"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, key := newSignupDefaultsRouteEngine(t, adminaccess.Policy{GlobalPermissions: tc.permissions})

			w := serveWithKey(r, key, "GET", path, "")
			if tc.allowed {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.JSONEq(t, `{"code":0,"message":"success","data":{"source":"oidc","subscriptions":[{"group_id":15,"validity_days":180},{"group_id":5,"validity_days":180}]}}`, w.Body.String())
			} else {
				require.Equal(t, 403, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "管理密钥未获此操作权限")
			}

			// The whole settings record stays closed either way.
			w = serveWithKey(r, key, "GET", "/api/v1/admin/settings", "")
			require.Equal(t, 403, w.Code, w.Body.String())
		})
	}
}

func TestSignupDefaultsRouteIsRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAdminRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{}}, middleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() }), middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }), middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)
	paths := map[string]bool{}
	for _, route := range r.Routes() {
		paths[route.Method+" "+route.Path] = true
	}
	require.True(t, paths["GET /api/v1/admin/settings/signup-defaults"])
	require.True(t, paths["GET /api/v1/admin/settings"])
}
