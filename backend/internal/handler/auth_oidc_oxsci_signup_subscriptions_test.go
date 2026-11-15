package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The site's default subscriptions in these tests: two groups, as on token.xsci.ai.
const oxsciTestDefaultSubscriptions = `[{"group_id":15,"validity_days":180},{"group_id":5,"validity_days":180}]`

type oxsciSignupCase struct {
	name     string
	groups   any // as the upstream claims hold it; nil = claim absent
	wantSubs bool
}

var oxsciSignupCases = []oxsciSignupCase{
	{"staff", []string{"staff.basic", "staff.dist"}, true},
	{"collaborator", []string{"collaborator.basic", "collaborator.assistant-owner"}, false},
	{"no groups claim", nil, false},
	{"empty groups", []string{}, false},
}

func newOxSciSignupTestHandler(t *testing.T, assigner *oauthPendingFlowDefaultSubAssignerStub, emailCache service.EmailCache) (*AuthHandler, *dbent.Client) {
	t.Helper()
	return newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{
		emailVerifyEnabled: emailCache != nil,
		emailCache:         emailCache,
		settingValues: map[string]string{
			service.SettingKeyDefaultSubscriptions: oxsciTestDefaultSubscriptions,
			service.SettingKeyDefaultBalance:       "3",
			service.SettingKeyDefaultConcurrency:   "4",
		},
		defaultSubAssigner: assigner,
	})
}

func withGroups(claims map[string]any, groups any) map[string]any {
	if groups != nil {
		claims["groups"] = groups
	}
	return claims
}

func requireOxSciSignupGrant(t *testing.T, client *dbent.Client, assigner *oauthPendingFlowDefaultSubAssignerStub, email string, wantSubs bool) {
	t.Helper()
	user, err := client.User.Query().Where(dbuser.EmailEQ(email)).Only(context.Background())
	require.NoError(t, err)
	require.Equal(t, "oidc", user.SignupSource)
	// Balance and concurrency defaults apply to everyone; only subscriptions differ.
	require.Equal(t, 3.0, user.Balance)
	require.Equal(t, 4, user.Concurrency)
	if !wantSubs {
		require.Empty(t, assigner.calls)
		return
	}
	require.Len(t, assigner.calls, 2)
	require.Equal(t, user.ID, assigner.calls[0].UserID)
	require.Equal(t, int64(15), assigner.calls[0].GroupID)
	require.Equal(t, int64(5), assigner.calls[1].GroupID)
	require.Equal(t, 180, assigner.calls[0].ValidityDays)
}

func TestOIDCFastPathSignupGrantsDefaultSubscriptionsOnlyToStaff(t *testing.T) {
	for _, tc := range oxsciSignupCases {
		t.Run(tc.name, func(t *testing.T) {
			assigner := &oauthPendingFlowDefaultSubAssignerStub{}
			handler, client := newOxSciSignupTestHandler(t, assigner, nil)
			email := "fastpath-" + oxsciTestSuffix(tc.name) + "@xsci.com"

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/oidc/callback", nil)
			completed := handler.tryOIDCVerifiedEmailFastPath(
				c,
				"/auth/oidc/callback",
				"/dashboard",
				service.PendingAuthIdentityKey{ProviderType: "oidc", ProviderKey: "https://issuer.example.com", ProviderSubject: "fast-" + email},
				email,
				"fast_user",
				withGroups(map[string]any{"in_oxsci_org": true, "trusted_org": true}, tc.groups),
				true,
			)

			require.True(t, completed)
			require.Contains(t, recorder.Header().Get("Location"), "access_token=")
			requireOxSciSignupGrant(t, client, assigner, email, tc.wantSubs)
		})
	}
}

func TestOIDCFastPathLoginOfAnExistingUserAssignsNothing(t *testing.T) {
	assigner := &oauthPendingFlowDefaultSubAssignerStub{}
	handler, client := newOxSciSignupTestHandler(t, assigner, nil)
	email := "fastpath-existing-" + oxsciTestSuffix("existing") + "@xsci.com"
	_, err := client.User.Create().
		SetEmail(email).
		SetUsername("existing").
		SetPasswordHash("x").
		SetRole(service.RoleUser).
		SetStatus(service.StatusActive).
		Save(context.Background())
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/oidc/callback", nil)
	completed := handler.tryOIDCVerifiedEmailFastPath(
		c, "/auth/oidc/callback", "/dashboard",
		service.PendingAuthIdentityKey{ProviderType: "oidc", ProviderKey: "https://issuer.example.com", ProviderSubject: "existing-" + email},
		email, "existing",
		map[string]any{"in_oxsci_org": true, "groups": []string{"staff.basic"}},
		true,
	)

	require.True(t, completed)
	require.Empty(t, assigner.calls)
}

func TestCompleteOIDCOAuthRegistrationGrantsDefaultSubscriptionsOnlyToStaff(t *testing.T) {
	for _, tc := range oxsciSignupCases {
		t.Run(tc.name, func(t *testing.T) {
			assigner := &oauthPendingFlowDefaultSubAssignerStub{}
			handler, client := newOxSciSignupTestHandler(t, assigner, nil)
			suffix := oxsciTestSuffix(tc.name)
			email := "complete-" + suffix + "@xsci.com"

			// Groups go through the pending session's JSON column, so they come back as []any.
			session, err := client.PendingAuthSession.Create().
				SetSessionToken("complete-" + suffix).
				SetIntent("login").
				SetProviderType("oidc").
				SetProviderKey("https://issuer.example.com").
				SetProviderSubject("complete-" + suffix).
				SetResolvedEmail(email).
				SetBrowserSessionKey("browser-" + suffix).
				SetUpstreamIdentityClaims(withGroups(map[string]any{"username": "complete_user"}, tc.groups)).
				SetExpiresAt(time.Now().UTC().Add(10 * time.Minute)).
				Save(context.Background())
			require.NoError(t, err)

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oauth/oidc/complete-registration", bytes.NewBufferString(`{"invitation_code":"unused"}`))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: oauthPendingSessionCookieName, Value: encodeCookieValue(session.SessionToken)})
			req.AddCookie(&http.Cookie{Name: oauthPendingBrowserCookieName, Value: encodeCookieValue("browser-" + suffix)})
			c.Request = req

			handler.CompleteOIDCOAuthRegistration(c)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			requireOxSciSignupGrant(t, client, assigner, email, tc.wantSubs)
		})
	}
}

func TestCreateOIDCOAuthAccountGrantsDefaultSubscriptionsOnlyToStaff(t *testing.T) {
	for _, tc := range oxsciSignupCases {
		t.Run(tc.name, func(t *testing.T) {
			suffix := oxsciTestSuffix(tc.name)
			email := "create-" + suffix + "@example.com"
			cache := &oauthPendingFlowEmailCacheStub{
				verificationCodes: map[string]*service.VerificationCodeData{
					email: {Code: "135790", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(15 * time.Minute)},
				},
			}
			assigner := &oauthPendingFlowDefaultSubAssignerStub{}
			handler, client := newOxSciSignupTestHandler(t, assigner, cache)

			session, err := client.PendingAuthSession.Create().
				SetSessionToken("create-" + suffix).
				SetIntent("login").
				SetProviderType("oidc").
				SetProviderKey("https://issuer.example").
				SetProviderSubject("create-" + suffix).
				SetBrowserSessionKey("browser-" + suffix).
				SetUpstreamIdentityClaims(withGroups(map[string]any{"username": "create_user"}, tc.groups)).
				SetRedirectTo("/profile").
				SetExpiresAt(time.Now().UTC().Add(10 * time.Minute)).
				Save(context.Background())
			require.NoError(t, err)

			body := bytes.NewBufferString(`{"email":"` + email + `","verify_code":"135790","password":"secret-123","adopt_display_name":false,"adopt_avatar":false}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oauth/oidc/create-account", body)
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: oauthPendingSessionCookieName, Value: encodeCookieValue(session.SessionToken)})
			req.AddCookie(&http.Cookie{Name: oauthPendingBrowserCookieName, Value: encodeCookieValue("browser-" + suffix)})
			c.Request = req

			handler.CreateOIDCOAuthAccount(c)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			user, err := client.User.Query().Where(dbuser.EmailEQ(email)).Only(context.Background())
			require.NoError(t, err)
			if !tc.wantSubs {
				require.Empty(t, assigner.calls)
				return
			}
			require.Len(t, assigner.calls, 2)
			require.Equal(t, user.ID, assigner.calls[0].UserID)
		})
	}
}

func TestOIDCSignupContextLeavesOtherProvidersAlone(t *testing.T) {
	ctx := context.Background()
	require.Equal(t, ctx, oidcSignupContext(ctx, "linuxdo", map[string]any{}))
	require.Equal(t, ctx, oidcSignupContext(ctx, "OIDC", map[string]any{"groups": []any{"collaborator.basic", "staff.basic"}}))
	require.NotEqual(t, ctx, oidcSignupContext(ctx, "oidc", map[string]any{"groups": []any{"collaborator.basic"}}))
	// A prefix match on "staff." only: "staffing.x" or a bare "staff" is not a staff admission.
	require.NotEqual(t, ctx, oidcSignupContext(ctx, "oidc", map[string]any{"groups": []string{"staff", "staffing.basic"}}))
}

// oxsciTestSuffix keeps rows apart: every test in this package shares one in-memory DB.
func oxsciTestSuffix(label string) string {
	clean := make([]rune, 0, len(label))
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			clean = append(clean, r)
		}
	}
	return fmt.Sprintf("%s-%d-%d", string(clean), time.Now().UnixNano(), oxsciTestSeq.Add(1))
}

var oxsciTestSeq atomic.Int64
