package adminaccess

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsersAPIKeysCreateIsAGroupScopedPermission(t *testing.T) {
	p := Policy{Rules: []Rule{{Permissions: []string{"users.api_keys.create"}, GroupIDs: []int64{16}}}}
	require.NoError(t, ValidatePolicy(p))
	require.True(t, RouteAllowed(p, "POST", "/api/v1/admin/users/:id/api-keys"))
	require.True(t, p.AllowsGroupConfig("users.api_keys.create", 16, "anthropic"))
	require.False(t, p.AllowsGroupConfig("users.api_keys.create", 15, "anthropic"))

	// It opens exactly the create route: not the key list, not the user record.
	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/users/:id/api-keys"))
	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/users/:id"))
	require.False(t, RouteAllowed(p, "PUT", "/api/v1/admin/api-keys/:id"))

	// Never a site-wide grant, and other permissions do not reach the route.
	require.Error(t, ValidatePolicy(Policy{GlobalPermissions: []string{"users.api_keys.create"}}))
	require.False(t, RouteAllowed(Policy{GlobalPermissions: []string{"users.read", "subscriptions.assign"}}, "POST", "/api/v1/admin/users/:id/api-keys"))
	require.False(t, RouteAllowed(Policy{Rules: []Rule{{Permissions: []string{"groups.list.read"}, AllGroups: true}}}, "POST", "/api/v1/admin/users/:id/api-keys"))
}
