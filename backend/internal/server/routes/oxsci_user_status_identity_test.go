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

// userAccountRouteAdmin answers the calls behind the status and identity routes.
type userAccountRouteAdmin struct {
	service.AdminService
	updates int
}

func (*userAccountRouteAdmin) GetUser(_ context.Context, id int64) (*service.User, error) {
	return &service.User{ID: id, Email: "staff@xsci.com", Role: service.RoleUser, Status: service.StatusActive}, nil
}

func (s *userAccountRouteAdmin) UpdateUser(_ context.Context, id int64, input *service.UpdateUserInput) (*service.User, error) {
	s.updates++
	return &service.User{ID: id, Role: service.RoleUser, Status: input.Status}, nil
}

func (s *userAccountRouteAdmin) FindUserByOIDCIdentity(ctx context.Context, _, _ string) (*service.User, error) {
	return s.GetUser(ctx, 42)
}

func newUserAccountRouteEngine(t *testing.T, policy adminaccess.Policy, admin *userAccountRouteAdmin) (*gin.Engine, string) {
	t.Helper()
	keys := &scopedRouteKeys{}
	settings := service.NewSettingService(nil, nil)
	settings.SetScopedAdminKeyRepository(keys)
	key, _, err := settings.CreateScopedAdminKey(context.Background(), service.ScopedAdminKeyInput{Name: "ds-token-service", Enabled: true, Policy: policy}, 9)
	require.NoError(t, err)
	users := service.NewUserService(&scopedRouteUsers{}, nil, nil, nil)
	auth := middleware.NewAdminAuthMiddleware(nil, users, settings, nil)
	r := gin.New()
	RegisterAdminRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{
		Account: adminhandler.NewAccountHandler(&scopedRouteAccounts{t: t}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil),
		User:    adminhandler.NewUserHandler(admin, nil, nil, nil, nil, nil, nil),
	}}, auth, middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }), middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)
	return r, key
}

func serveWithKey(r *gin.Engine, key, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Each route is reached by a scoped key only through its own permission.
func TestScopedAdminKeyReachesStatusAndIdentityRoutesOnlyWithTheirPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const status = "/api/v1/admin/users/42/status"
	const lookup = "/api/v1/admin/users/by-identity?issuer=https%3A%2F%2Fdata.xsci.ai&subject=user-7"
	for _, tc := range []struct {
		name          string
		permissions   []string
		statusAllowed bool
		lookupAllowed bool
	}{
		{"what DS holds today is not enough", []string{"users.read", "subscriptions.read", "subscriptions.assign", "subscriptions.extend", "subscriptions.revoke"}, false, false},
		{"users.quotas.write does not change status", []string{"users.quotas.write"}, false, false},
		{"users.status.write", []string{"users.status.write"}, true, false},
		{"users.identity.read", []string{"users.identity.read"}, false, true},
		{"both", []string{"users.status.write", "users.identity.read"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := &userAccountRouteAdmin{}
			r, key := newUserAccountRouteEngine(t, adminaccess.Policy{GlobalPermissions: tc.permissions}, admin)

			w := serveWithKey(r, key, "POST", status, `{"status":"disabled"}`)
			if tc.statusAllowed {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"changed":true`)
				require.Equal(t, 1, admin.updates)
			} else {
				require.Equal(t, 403, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "管理密钥未获此操作权限")
				require.Zero(t, admin.updates)
			}

			w = serveWithKey(r, key, "GET", lookup, "")
			if tc.lookupAllowed {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"id":42`)
			} else {
				require.Equal(t, 403, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "管理密钥未获此操作权限")
			}

			// Neither permission opens the full-record update.
			w = serveWithKey(r, key, "PUT", "/api/v1/admin/users/42", `{"status":"disabled"}`)
			require.Equal(t, 403, w.Code, w.Body.String())
		})
	}
}

func TestStatusAndIdentityRoutesAreRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAdminRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{}}, middleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() }), middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }), middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }), nil, nil)
	paths := map[string]bool{}
	for _, route := range r.Routes() {
		paths[route.Method+" "+route.Path] = true
	}
	require.True(t, paths["POST /api/v1/admin/users/:id/status"])
	require.True(t, paths["GET /api/v1/admin/users/by-identity"])
	require.True(t, paths["GET /api/v1/admin/users/:id"])
	require.True(t, paths["PUT /api/v1/admin/users/:id"])
}
