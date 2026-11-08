//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Exercise the real Messages entry point so a disconnected lifecycle hook cannot
// regress while the hook's isolated HTTP tests still pass.
func TestGatewayMessagesEmitsLLMLifecycle(t *testing.T) {
	for _, scenario := range []string{"json", "stream", "no-account"} {
		t.Run(scenario, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			groupID := int64(2001)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
			accounts := []*service.Account{{
				ID: 1001, Name: "logging-test", Platform: service.PlatformAntigravity,
				Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
				Concurrency: 1, Priority: 1,
				Credentials:   map[string]any{"access_token": "account-secret", "intercept_warmup_requests": true},
				Extra:         map[string]any{"mixed_scheduling": true},
				AccountGroups: []service.AccountGroup{{AccountID: 1001, GroupID: groupID}},
			}}
			if scenario == "no-account" {
				accounts = nil
			}
			h, cleanup := newTestGatewayHandler(t, group, accounts)
			defer cleanup()
			hook, calls := newTestLLMLogSink(t)
			h.lifecycleHook = NewCompositeHook(hook)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			stream := "false"
			if scenario == "stream" {
				stream = "true"
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":`+stream+`,"max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"Warmup"}]}]}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
			apiKey := &service.APIKey{ID: 3001, UserID: 4001, GroupID: &groupID, Status: service.StatusActive,
				User: &service.User{ID: 4001, Concurrency: 10, Balance: 100}, Group: group}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
			h.Messages(c)
			create := receiveLLMCall(t, calls)
			update := receiveLLMCall(t, calls)
			require.Equal(t, "claude-sonnet-4-5", create.body["model"])
			if scenario == "no-account" {
				require.GreaterOrEqual(t, rec.Code, 400)
				require.Equal(t, "error", update.body["status"])
			} else {
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, "success", update.body["status"])
				require.NotEmpty(t, update.body["response_messages"])
				require.Equal(t, float64(1001), requireLLMLogValue[map[string]any](t, update.body["meta"])["account_id"])
			}
		})
	}
}
