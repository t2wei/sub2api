package adminaccess

import "testing"

// The session-totals read sits under usage.read and nowhere else.
func TestSessionTotalsRouteNeedsUsageRead(t *testing.T) {
	path := "/api/v1/admin/usage/session-totals"
	if !RouteAllowed(Policy{GlobalPermissions: []string{"usage.read"}}, "GET", path) {
		t.Fatalf("usage.read cannot read %s", path)
	}
	if RouteAllowed(Policy{GlobalPermissions: []string{"dashboard.read", "users.read"}}, "GET", path) {
		t.Fatalf("%s is reachable without usage.read", path)
	}
	if RouteAllowed(Policy{GlobalPermissions: []string{"usage.read"}}, "POST", path) {
		t.Fatalf("POST %s must not be allowed", path)
	}
}
