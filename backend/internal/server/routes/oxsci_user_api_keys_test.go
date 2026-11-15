//go:build unit

package routes

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type userAPIKeyRouteGroups struct{ service.AdminService }

func (*userAPIKeyRouteGroups) GetGroup(_ context.Context, id int64) (*service.Group, error) {
	return &service.Group{ID: id, Platform: service.PlatformAnthropic}, nil
}

// A scoped key reaches POST /users/:id/api-keys only through users.api_keys.create;
// once there, the handler holds it to the groups of its rule.
func TestScopedAdminKeyReachesCreateUserAPIKeyOnlyWithThePermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name    string
		policy  adminaccess.Policy
		message string
	}{
		{"site-wide user and subscription rights are not enough",
			adminaccess.Policy{GlobalPermissions: []string{"users.read", "subscriptions.assign"}},
			"管理密钥未获此操作权限"},
		{"the permission on another group lets it in, the handler refuses the group",
			adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"users.api_keys.create"}, GroupIDs: []int64{16}}}},
			"此分组未授权给受限管理密钥创建用户密钥"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := &scopedRouteKeys{}
			settings := service.NewSettingService(nil, nil)
			settings.SetScopedAdminKeyRepository(keys)
			key, _, err := settings.CreateScopedAdminKey(context.Background(), service.ScopedAdminKeyInput{Name: "ds-token-usage", Enabled: true, Policy: tc.policy}, 9)
			require.NoError(t, err)
			users := service.NewUserService(&scopedRouteUsers{}, nil, nil, nil)
			auth := middleware.NewAdminAuthMiddleware(nil, users, settings, nil)
			r := gin.New()
			RegisterAdminRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{
				Account: adminhandler.NewAccountHandler(&scopedRouteAccounts{t: t}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil),
				APIKey:  adminhandler.NewAdminAPIKeyHandler(&userAPIKeyRouteGroups{}, nil),
			}}, auth, middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }), middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)

			req := httptest.NewRequest("POST", "/api/v1/admin/users/42/api-keys", bytes.NewBufferString(`{"name":"k","group_id":17}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-api-key", key)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, 403, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), tc.message)
		})
	}
}

func TestCreateUserAPIKeyRouteIsRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAdminRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{}}, middleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() }), middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }), middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)
	paths := map[string]bool{}
	for _, route := range r.Routes() {
		paths[route.Method+" "+route.Path] = true
	}
	require.True(t, paths["POST /api/v1/admin/users/:id/api-keys"])
	require.True(t, paths["GET /api/v1/admin/users/:id/api-keys"])
}
