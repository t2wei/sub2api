package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOIDCParseGroups(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"absent":         {`{"sub":"s"}`, nil},
		"null":           {`{"groups":null}`, nil},
		"not an array":   {`{"groups":"staff.basic"}`, nil},
		"empty":          {`{"groups":[]}`, []string{}},
		"values trimmed": {`{"groups":[" staff.basic ","staff.dist"]}`, []string{"staff.basic", "staff.dist"}},
		"junk dropped":   {`{"groups":["collaborator.basic","",7,null,{"a":1}]}`, []string{"collaborator.basic"}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, oidcParseGroups(tc.body))
		})
	}
}

func TestOIDCParseUserInfoReadsGroupsBesideTheOrgClaim(t *testing.T) {
	claims := oidcParseUserInfo(`{"sub":"s","email":"a@xsci.com","in_oxsci_org":true,"groups":["staff.basic"]}`, config.OIDCConnectConfig{})
	require.NotNil(t, claims.InOxSciOrg)
	require.True(t, *claims.InOxSciOrg)
	require.Equal(t, []string{"staff.basic"}, claims.Groups)

	without := oidcParseUserInfo(`{"sub":"s","email":"a@xsci.com","in_oxsci_org":true}`, config.OIDCConnectConfig{})
	require.Nil(t, without.Groups)
}

func TestOIDCOAuthCallbackKeepsGroupsInIdentityMetadata(t *testing.T) {
	cfg, cleanup := newOIDCTestProvider(t, oidcProviderFixture{
		Subject:           "oidc-subject-groups-metadata",
		PreferredUsername: "groups_metadata",
		DisplayName:       "Groups Metadata",
		Email:             "groups-metadata@xsci.com",
		EmailVerified:     true,
		InOxSciOrg:        boolPtr(true),
		Groups:            []string{"staff.basic", "staff.dist"},
	})
	defer cleanup()

	handler, client := newOIDCOAuthHandlerAndClientWithSettings(t, false, cfg, nil)
	t.Cleanup(func() { _ = client.Close() })

	recorder := runOIDCCallbackForTest(t, handler, "oidc-subject-groups-metadata")
	require.Equal(t, http.StatusFound, recorder.Code)
	require.Contains(t, recorder.Header().Get("Location"), "access_token=")

	identity := oidcIdentityForTest(t, client, cfg.IssuerURL, "oidc-subject-groups-metadata", "groups-metadata@xsci.com")
	require.Equal(t, []any{"staff.basic", "staff.dist"}, identity.Metadata["groups"])
	require.Equal(t, true, identity.Metadata["in_oxsci_org"])
}

func TestOIDCOAuthCallbackStillGatesOnTheOrgClaimWhateverTheGroups(t *testing.T) {
	cfg, cleanup := newOIDCTestProvider(t, oidcProviderFixture{
		Subject:           "oidc-subject-groups-not-in-org",
		PreferredUsername: "groups_not_in_org",
		Email:             "groups-not-in-org@xsci.com",
		EmailVerified:     true,
		InOxSciOrg:        boolPtr(false),
		Groups:            []string{"staff.basic"},
	})
	defer cleanup()

	handler, client := newOIDCOAuthHandlerAndClientWithSettings(t, false, cfg, nil)
	t.Cleanup(func() { _ = client.Close() })

	recorder := runOIDCCallbackForTest(t, handler, "oidc-subject-groups-not-in-org")
	require.Equal(t, http.StatusFound, recorder.Code)
	require.Contains(t, recorder.Header().Get("Location"), "org_not_allowed")

	count, err := client.User.Query().Where(dbuser.EmailEQ("groups-not-in-org@xsci.com")).Count(context.Background())
	require.NoError(t, err)
	require.Zero(t, count)
}

// runOIDCCallbackForTest drives OIDCOAuthCallback the way the browser does after the
// provider redirects back, with the state, PKCE and nonce cookies set by start.
func runOIDCCallbackForTest(t *testing.T, handler *AuthHandler, subject string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	state := "state-" + subject
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/oidc/callback?code=oidc-code&state="+state, nil)
	req.AddCookie(encodedCookie(oidcOAuthStateCookieName, state))
	req.AddCookie(encodedCookie(oidcOAuthRedirectCookie, "/dashboard"))
	req.AddCookie(encodedCookie(oidcOAuthVerifierCookie, "verifier-"+subject))
	req.AddCookie(encodedCookie(oidcOAuthNonceCookie, "nonce-"+subject))
	req.AddCookie(encodedCookie(oidcOAuthIntentCookieName, oauthIntentLogin))
	req.AddCookie(encodedCookie(oauthPendingBrowserCookieName, "browser-"+subject))
	c.Request = req
	handler.OIDCOAuthCallback(c)
	return recorder
}

func oidcIdentityForTest(t *testing.T, client *dbent.Client, issuer, subject, email string) *dbent.AuthIdentity {
	t.Helper()
	ctx := context.Background()
	user, err := client.User.Query().Where(dbuser.EmailEQ(email)).Only(ctx)
	require.NoError(t, err)
	identity, err := client.AuthIdentity.Query().
		Where(
			authidentity.ProviderTypeEQ("oidc"),
			authidentity.ProviderKeyEQ(issuer),
			authidentity.ProviderSubjectEQ(subject),
			authidentity.UserIDEQ(user.ID),
		).
		Only(ctx)
	require.NoError(t, err)
	return identity
}
