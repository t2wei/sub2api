package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const signupDefaultsPath = "/api/v1/admin/settings/signup-defaults"

func setupSignupDefaultsRouter(repo service.SettingRepository) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewSettingHandler(service.NewSettingService(repo, &config.Config{}), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET(signupDefaultsPath, handler.GetSignupDefaults)
	return router
}

type signupDefaultsEnvelope struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
	Data   struct {
		Source        string            `json:"source"`
		Subscriptions []json.RawMessage `json:"subscriptions"`
	} `json:"data"`
}

func getSignupDefaults(t *testing.T, repo service.SettingRepository, query string) (int, signupDefaultsEnvelope, string) {
	t.Helper()
	rec := serveEmailBlacklistJSON(t, setupSignupDefaultsRouter(repo), http.MethodGet, signupDefaultsPath+query, `{}`)
	var out signupDefaultsEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return rec.Code, out, rec.Body.String()
}

const (
	globalDefaultSubs = `[{"group_id":15,"validity_days":180},{"group_id":5,"validity_days":180}]`
	oidcOwnSubs       = `[{"group_id":20,"validity_days":30}]`
)

func signupRepo(values map[string]string) *settingHandlerRepoStub {
	return &settingHandlerRepoStub{values: values}
}

func TestGetSignupDefaults_ResolvesAsTheSignupDoes(t *testing.T) {
	for name, tc := range map[string]struct {
		values map[string]string
		query  string
		source string
		want   string
	}{
		"oidc without its own grant: the global defaults": {
			values: map[string]string{service.SettingKeyDefaultSubscriptions: globalDefaultSubs},
			source: "oidc",
			want:   globalDefaultSubs,
		},
		"oidc grant on with its own subscriptions: those": {
			values: map[string]string{
				service.SettingKeyDefaultSubscriptions:               globalDefaultSubs,
				service.SettingKeyAuthSourceDefaultOIDCGrantOnSignup: "true",
				service.SettingKeyAuthSourceDefaultOIDCSubscriptions: oidcOwnSubs,
			},
			source: "oidc",
			want:   oidcOwnSubs,
		},
		"oidc grant on naming none: the global defaults": {
			values: map[string]string{
				service.SettingKeyDefaultSubscriptions:               globalDefaultSubs,
				service.SettingKeyAuthSourceDefaultOIDCGrantOnSignup: "true",
				service.SettingKeyAuthSourceDefaultOIDCSubscriptions: `[]`,
			},
			source: "oidc",
			want:   globalDefaultSubs,
		},
		"oidc's own subscriptions with its grant off: the global defaults": {
			values: map[string]string{
				service.SettingKeyDefaultSubscriptions:               globalDefaultSubs,
				service.SettingKeyAuthSourceDefaultOIDCGrantOnSignup: "false",
				service.SettingKeyAuthSourceDefaultOIDCSubscriptions: oidcOwnSubs,
			},
			source: "oidc",
			want:   globalDefaultSubs,
		},
		"another source by name": {
			values: map[string]string{
				service.SettingKeyDefaultSubscriptions:                globalDefaultSubs,
				service.SettingKeyAuthSourceDefaultEmailGrantOnSignup: "true",
				service.SettingKeyAuthSourceDefaultEmailSubscriptions: oidcOwnSubs,
			},
			query:  "?source=Email",
			source: "email",
			want:   oidcOwnSubs,
		},
		"nothing configured: an empty list, not null": {
			values: map[string]string{},
			source: "oidc",
			want:   `[]`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			code, got, body := getSignupDefaults(t, signupRepo(tc.values), tc.query)
			require.Equal(t, http.StatusOK, code, body)
			require.Equal(t, tc.source, got.Data.Source)
			require.NotNil(t, got.Data.Subscriptions, body)
			raw, err := json.Marshal(got.Data.Subscriptions)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(raw))
		})
	}
}

func TestGetSignupDefaults_AnswersOnlyTheSubscriptions(t *testing.T) {
	repo := signupRepo(map[string]string{
		service.SettingKeyDefaultSubscriptions:               globalDefaultSubs,
		service.SettingKeyAuthSourceDefaultOIDCGrantOnSignup: "true",
		service.SettingKeyAuthSourceDefaultOIDCBalance:       "25",
		service.SettingKeyAuthSourceDefaultOIDCConcurrency:   "7",
	})
	code, _, body := getSignupDefaults(t, repo, "")
	require.Equal(t, http.StatusOK, code, body)
	var raw struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	keys := make([]string, 0, len(raw.Data))
	for key := range raw.Data {
		keys = append(keys, key)
	}
	require.ElementsMatch(t, []string{"source", "subscriptions"}, keys)
	require.Nil(t, repo.lastUpdates, "a read writes nothing")
}

func TestGetSignupDefaults_RefusesAnUnknownSource(t *testing.T) {
	code, got, body := getSignupDefaults(t, signupRepo(map[string]string{}), "?source=saml")
	require.Equal(t, http.StatusBadRequest, code, body)
	require.Equal(t, "SIGNUP_SOURCE_UNKNOWN", got.Reason)
}

type signupDefaultsBrokenRepo struct{ *settingHandlerRepoStub }

func (signupDefaultsBrokenRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, errors.New("settings table unreachable")
}

func TestGetSignupDefaults_SaysWhenTheSettingsCannotBeRead(t *testing.T) {
	repo := signupDefaultsBrokenRepo{signupRepo(map[string]string{service.SettingKeyDefaultSubscriptions: globalDefaultSubs})}
	code, _, body := getSignupDefaults(t, repo, "")
	require.Equal(t, http.StatusInternalServerError, code, body)
	require.NotContains(t, body, `"group_id"`, "never the global defaults in place of what could not be read")
}
