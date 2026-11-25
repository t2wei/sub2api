package adminaccess

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsersStatusWriteOpensOnlyTheStatusRoute(t *testing.T) {
	p := Policy{GlobalPermissions: []string{"users.status.write"}}
	require.NoError(t, ValidatePolicy(p))
	require.True(t, RouteAllowed(p, "POST", "/api/v1/admin/users/:id/status"))

	// Not the full-record update, the batch disable, or anything that reads the user.
	require.False(t, RouteAllowed(p, "PUT", "/api/v1/admin/users/:id"))
	require.False(t, RouteAllowed(p, "POST", "/api/v1/admin/users/batch-disable"))
	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/users/:id"))
	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/users/by-identity"))

	// A site-wide permission, never a group rule; other user permissions do not reach it.
	require.Error(t, ValidatePolicy(Policy{Rules: []Rule{{Permissions: []string{"users.status.write"}, AllGroups: true}}}))
	require.False(t, RouteAllowed(Policy{GlobalPermissions: []string{"users.read", "users.quotas.write", "subscriptions.revoke"}}, "POST", "/api/v1/admin/users/:id/status"))
}

func TestUsersIdentityReadOpensOnlyTheLookupRoute(t *testing.T) {
	p := Policy{GlobalPermissions: []string{"users.identity.read"}}
	require.NoError(t, ValidatePolicy(p))
	require.True(t, RouteAllowed(p, "GET", "/api/v1/admin/users/by-identity"))

	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/users"))
	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/users/:id"))
	require.False(t, RouteAllowed(p, "POST", "/api/v1/admin/users/:id/auth-identities"))
	require.False(t, RouteAllowed(p, "POST", "/api/v1/admin/users/:id/status"))

	require.Error(t, ValidatePolicy(Policy{Rules: []Rule{{Permissions: []string{"users.identity.read"}, AllGroups: true}}}))
	// users.read searches by e-mail; it does not open the identity lookup.
	require.False(t, RouteAllowed(Policy{GlobalPermissions: []string{"users.read"}}, "GET", "/api/v1/admin/users/by-identity"))
}

func TestStatusAndIdentityPermissionsAreCatalogued(t *testing.T) {
	want := map[string]bool{"users.status.write": false, "users.identity.read": false}
	for _, entry := range PermissionCatalog() {
		if _, ok := want[entry.ID]; ok {
			require.Equal(t, "global", entry.Scope, entry.ID)
			require.Equal(t, "用户", entry.Category, entry.ID)
			want[entry.ID] = true
		}
	}
	for id, found := range want {
		require.True(t, found, id)
	}
	// An existing key's policy gains neither route.
	require.False(t, RouteAllowed(Policy{}, "POST", "/api/v1/admin/users/:id/status"))
	require.False(t, RouteAllowed(Policy{}, "GET", "/api/v1/admin/users/by-identity"))
}
