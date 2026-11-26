package adminaccess

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSignupDefaultsReadOpensOnlyItsRoute(t *testing.T) {
	p := Policy{GlobalPermissions: []string{"settings.signup_defaults.read"}}
	require.NoError(t, ValidatePolicy(p))
	require.True(t, RouteAllowed(p, "GET", "/api/v1/admin/settings/signup-defaults"))

	// Not the whole settings record, nor any other setting, nor a write.
	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/settings"))
	require.False(t, RouteAllowed(p, "PUT", "/api/v1/admin/settings"))
	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/settings/oidc-required-groups"))
	require.False(t, RouteAllowed(p, "GET", "/api/v1/admin/settings/admin-api-key"))

	// A site-wide permission, never a group rule; the subscription permissions do not reach it.
	require.Error(t, ValidatePolicy(Policy{Rules: []Rule{{Permissions: []string{"settings.signup_defaults.read"}, AllGroups: true}}}))
	require.False(t, RouteAllowed(Policy{GlobalPermissions: []string{"subscriptions.read", "subscriptions.assign", "users.read"}}, "GET", "/api/v1/admin/settings/signup-defaults"))
}

func TestSignupDefaultsReadIsCatalogued(t *testing.T) {
	found := false
	for _, entry := range PermissionCatalog() {
		if entry.ID == "settings.signup_defaults.read" {
			require.Equal(t, "global", entry.Scope)
			require.Equal(t, "订阅", entry.Category)
			found = true
		}
	}
	require.True(t, found)
	// An existing key's policy does not gain the route.
	require.False(t, RouteAllowed(Policy{}, "GET", "/api/v1/admin/settings/signup-defaults"))
}
