package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const oidcRequiredGroupsPath = "/api/v1/admin/settings/oidc-required-groups"

func setupOIDCRequiredGroupsRouter(repo *emailBlacklistSettingRepoStub) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewSettingHandler(service.NewSettingService(repo, &config.Config{}), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET(oidcRequiredGroupsPath, handler.GetOIDCRequiredGroups)
	router.PUT(oidcRequiredGroupsPath, handler.UpdateOIDCRequiredGroups)
	return router
}

type oidcRequiredGroupsEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Groups []string `json:"groups"`
		Max    int      `json:"max"`
	} `json:"data"`
}

func decodeOIDCRequiredGroups(t *testing.T, body []byte) oidcRequiredGroupsEnvelope {
	t.Helper()
	var out oidcRequiredGroupsEnvelope
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func TestGetOIDCRequiredGroups_DefaultsToEmpty(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{}}}
	rec := serveEmailBlacklistJSON(t, setupOIDCRequiredGroupsRouter(repo), http.MethodGet, oidcRequiredGroupsPath, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeOIDCRequiredGroups(t, rec.Body.Bytes())
	require.Empty(t, got.Data.Groups)
	require.NotNil(t, got.Data.Groups, "answers [] rather than null")
	require.Equal(t, service.MaxOIDCRequiredGroups, got.Data.Max)
}

func TestUpdateOIDCRequiredGroups_RequiresGroupsField(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyOIDCConnectRequiredGroups: `["staff.*"]`,
	}}}
	rec := serveEmailBlacklistJSON(t, setupOIDCRequiredGroupsRouter(repo), http.MethodPut, oidcRequiredGroupsPath, `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, repo.setCalls)
	require.Equal(t, `["staff.*"]`, repo.values[service.SettingKeyOIDCConnectRequiredGroups])
}

func TestUpdateOIDCRequiredGroups_SavesNormalizedAndReadsBack(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{}}}
	router := setupOIDCRequiredGroupsRouter(repo)

	rec := serveEmailBlacklistJSON(t, router, http.MethodPut, oidcRequiredGroupsPath, map[string]any{
		"groups": []string{" staff.* ", "staff.*", ""},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"staff.*"}, decodeOIDCRequiredGroups(t, rec.Body.Bytes()).Data.Groups)

	rec = serveEmailBlacklistJSON(t, router, http.MethodGet, oidcRequiredGroupsPath, `{}`)
	require.Equal(t, []string{"staff.*"}, decodeOIDCRequiredGroups(t, rec.Body.Bytes()).Data.Groups)

	rec = serveEmailBlacklistJSON(t, router, http.MethodPut, oidcRequiredGroupsPath, map[string]any{"groups": []string{}})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, decodeOIDCRequiredGroups(t, rec.Body.Bytes()).Data.Groups)
	require.Equal(t, `[]`, repo.values[service.SettingKeyOIDCConnectRequiredGroups])
}

func TestUpdateOIDCRequiredGroups_RefusesInvalidEntries(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyOIDCConnectRequiredGroups: `["staff.*"]`,
	}}}
	rec := serveEmailBlacklistJSON(t, setupOIDCRequiredGroupsRouter(repo), http.MethodPut, oidcRequiredGroupsPath, map[string]any{
		"groups": []string{"staff.*", "st*aff"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "OIDC_REQUIRED_GROUPS_INVALID_ENTRY")
	require.Zero(t, repo.setCalls)
	require.Equal(t, `["staff.*"]`, repo.values[service.SettingKeyOIDCConnectRequiredGroups])
}
