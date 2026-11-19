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

const llmLoggingSettingPath = "/api/v1/admin/settings/llm-logging"

func setupLLMLoggingSettingRouter(repo *emailBlacklistSettingRepoStub, cfg *config.Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewSettingHandler(service.NewSettingService(repo, cfg), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET(llmLoggingSettingPath, handler.GetLLMLoggingSetting)
	router.PUT(llmLoggingSettingPath, handler.UpdateLLMLoggingSetting)
	return router
}

type llmLoggingSettingEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Enabled        bool `json:"enabled"`
		SinkConfigured bool `json:"sink_configured"`
	} `json:"data"`
}

func decodeLLMLoggingSetting(t *testing.T, body []byte) llmLoggingSettingEnvelope {
	t.Helper()
	var out llmLoggingSettingEnvelope
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func TestGetLLMLoggingSetting_DefaultsToOff(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{}}}
	rec := serveEmailBlacklistJSON(t, setupLLMLoggingSettingRouter(repo, &config.Config{}), http.MethodGet, llmLoggingSettingPath, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeLLMLoggingSetting(t, rec.Body.Bytes())
	require.False(t, got.Data.Enabled)
	require.False(t, got.Data.SinkConfigured)
}

func TestUpdateLLMLoggingSetting_RequiresEnabledField(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{}}}
	rec := serveEmailBlacklistJSON(t, setupLLMLoggingSettingRouter(repo, &config.Config{}), http.MethodPut, llmLoggingSettingPath, `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, repo.setCalls)
}

func TestUpdateLLMLoggingSetting_SavesAndReadsBack(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{}}}
	cfg := &config.Config{LLMLogging: config.LLMLoggingConfig{URL: "http://localhost:8008/api/database/v1/llm_calls"}}
	router := setupLLMLoggingSettingRouter(repo, cfg)

	rec := serveEmailBlacklistJSON(t, router, http.MethodPut, llmLoggingSettingPath, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, decodeLLMLoggingSetting(t, rec.Body.Bytes()).Data.Enabled)
	require.Equal(t, "true", repo.values[service.SettingKeyLLMLoggingEnabled])

	rec = serveEmailBlacklistJSON(t, router, http.MethodGet, llmLoggingSettingPath, `{}`)
	got := decodeLLMLoggingSetting(t, rec.Body.Bytes())
	require.True(t, got.Data.Enabled)
	require.True(t, got.Data.SinkConfigured)

	rec = serveEmailBlacklistJSON(t, router, http.MethodPut, llmLoggingSettingPath, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, decodeLLMLoggingSetting(t, rec.Body.Bytes()).Data.Enabled)
	require.Equal(t, "false", repo.values[service.SettingKeyLLMLoggingEnabled])
}
