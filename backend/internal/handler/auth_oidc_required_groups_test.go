package handler

import (
	"context"
	"net/http"
	"testing"

	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// A login the site's "required groups" refuses: no account, no pending session, the
// error code the login page explains.
func requireOIDCLoginRefused(t *testing.T, recorderLocation string, hasPendingCookie bool, code string) {
	t.Helper()
	require.Contains(t, recorderLocation, "error="+code)
	require.NotContains(t, recorderLocation, "access_token=")
	require.False(t, hasPendingCookie, "a refused login opens no pending session")
}

func TestOIDCOAuthCallbackRequiredGroups(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required string // stored value; "" = setting absent
		groups   []string
		allowed  bool
	}{
		{"no requirement lets a collaborator in, as before", "", []string{"collaborator.assistant-owner"}, true},
		{"empty list is no requirement", `[]`, []string{"collaborator.basic"}, true},
		{"prefix lets staff in", `["staff.*"]`, []string{"collaborator.basic", "staff.basic"}, true},
		{"exact entry lets the named group in", `["staff.basic"]`, []string{"staff.basic"}, true},
		{"prefix refuses a collaborator", `["staff.*"]`, []string{"collaborator.assistant-owner"}, false},
		{"requirement refuses a userinfo without groups", `["staff.*"]`, nil, false},
		{"requirement refuses empty groups", `["staff.*"]`, []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject := "oidc-required-groups-" + oxsciTestSuffix(tc.name)
			email := subject + "@xsci.com"
			cfg, cleanup := newOIDCTestProvider(t, oidcProviderFixture{
				Subject:           subject,
				PreferredUsername: "required_groups",
				Email:             email,
				EmailVerified:     true,
				InOxSciOrg:        boolPtr(true),
				Groups:            tc.groups,
			})
			defer cleanup()

			settings := map[string]string{}
			if tc.required != "" {
				settings[service.SettingKeyOIDCConnectRequiredGroups] = tc.required
			}
			handler, client := newOIDCOAuthHandlerAndClientWithSettings(t, false, cfg, settings)
			t.Cleanup(func() { _ = client.Close() })

			recorder := runOIDCCallbackForTest(t, handler, subject)
			require.Equal(t, http.StatusFound, recorder.Code)
			location := recorder.Header().Get("Location")

			count, err := client.User.Query().Where(dbuser.EmailEQ(email)).Count(context.Background())
			require.NoError(t, err)
			if tc.allowed {
				require.Contains(t, location, "access_token=")
				require.Equal(t, 1, count)
				return
			}
			requireOIDCLoginRefused(t, location, findCookie(recorder.Result().Cookies(), oauthPendingSessionCookieName) != nil, "group_not_allowed")
			require.Contains(t, location, "error_description=userinfo+groups+must+match+oidc_connect_required_groups")
			require.Zero(t, count, "a refused sign-up creates no account")
		})
	}
}

func TestOIDCOAuthCallbackRequiredGroupsAlsoRefusesAnExistingAccount(t *testing.T) {
	const subject = "oidc-required-groups-existing"
	cfg, cleanup := newOIDCTestProvider(t, oidcProviderFixture{
		Subject:           subject,
		PreferredUsername: "required_groups_existing",
		Email:             "required-groups-existing@xsci.com",
		EmailVerified:     true,
		InOxSciOrg:        boolPtr(true),
		Groups:            []string{"collaborator.assistant-owner"},
	})
	defer cleanup()

	handler, client := newOIDCOAuthHandlerAndClientWithSettings(t, false, cfg, map[string]string{
		service.SettingKeyOIDCConnectRequiredGroups: `["staff.*"]`,
	})
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	existing, err := client.User.Create().
		SetEmail("required-groups-existing@xsci.com").
		SetUsername("required_groups_existing").
		SetPasswordHash("hash").
		SetRole(service.RoleUser).
		SetStatus(service.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	_, err = client.AuthIdentity.Create().
		SetUserID(existing.ID).
		SetProviderType("oidc").
		SetProviderKey(cfg.IssuerURL).
		SetProviderSubject(subject).
		Save(ctx)
	require.NoError(t, err)

	recorder := runOIDCCallbackForTest(t, handler, subject)
	require.Equal(t, http.StatusFound, recorder.Code)
	requireOIDCLoginRefused(t, recorder.Header().Get("Location"), findCookie(recorder.Result().Cookies(), oauthPendingSessionCookieName) != nil, "group_not_allowed")
}

func TestOIDCOAuthCallbackRequiredGroupsUnreadableRefuses(t *testing.T) {
	const subject = "oidc-required-groups-corrupt"
	cfg, cleanup := newOIDCTestProvider(t, oidcProviderFixture{
		Subject:           subject,
		PreferredUsername: "required_groups_corrupt",
		Email:             "required-groups-corrupt@xsci.com",
		EmailVerified:     true,
		InOxSciOrg:        boolPtr(true),
		Groups:            []string{"staff.basic"},
	})
	defer cleanup()

	handler, client := newOIDCOAuthHandlerAndClientWithSettings(t, false, cfg, map[string]string{
		service.SettingKeyOIDCConnectRequiredGroups: `staff.*`,
	})
	t.Cleanup(func() { _ = client.Close() })

	recorder := runOIDCCallbackForTest(t, handler, subject)
	require.Equal(t, http.StatusFound, recorder.Code)
	requireOIDCLoginRefused(t, recorder.Header().Get("Location"), findCookie(recorder.Result().Cookies(), oauthPendingSessionCookieName) != nil, "config_error")

	count, err := client.User.Query().Where(dbuser.EmailEQ("required-groups-corrupt@xsci.com")).Count(context.Background())
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestOIDCOAuthCallbackOrgGateStillComesFirst(t *testing.T) {
	const subject = "oidc-required-groups-not-in-org"
	cfg, cleanup := newOIDCTestProvider(t, oidcProviderFixture{
		Subject:           subject,
		PreferredUsername: "required_groups_not_in_org",
		Email:             "required-groups-not-in-org@xsci.com",
		EmailVerified:     true,
		InOxSciOrg:        boolPtr(false),
		Groups:            []string{"staff.basic"},
	})
	defer cleanup()

	handler, client := newOIDCOAuthHandlerAndClientWithSettings(t, false, cfg, map[string]string{
		service.SettingKeyOIDCConnectRequiredGroups: `["staff.*"]`,
	})
	t.Cleanup(func() { _ = client.Close() })

	recorder := runOIDCCallbackForTest(t, handler, subject)
	requireOIDCLoginRefused(t, recorder.Header().Get("Location"), findCookie(recorder.Result().Cookies(), oauthPendingSessionCookieName) != nil, "org_not_allowed")
}
