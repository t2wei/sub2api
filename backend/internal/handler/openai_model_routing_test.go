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

func TestOpenAIHTTPModelRoutingRejectsAmbiguousModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []struct {
		path string
		call func(*OpenAIGatewayHandler, *gin.Context)
	}{
		{"/v1/responses", (*OpenAIGatewayHandler).Responses},
		{"/v1/messages", (*OpenAIGatewayHandler).Messages},
		{"/v1/chat/completions", (*OpenAIGatewayHandler).ChatCompletions},
		{"/v1/embeddings", (*OpenAIGatewayHandler).Embeddings},
		{"/v1/alpha/search", (*OpenAIGatewayHandler).AlphaSearch},
	} {
		for _, body := range []string{
			`{"model":"gpt-normal-routing-test","model":"gpt-restricted-routing-test","input":"hello"}`,
			`{"model":"gpt-normal-routing-test","Model":"gpt-restricted-routing-test","input":"hello"}`,
		} {
			t.Run(endpoint.path+body, func(t *testing.T) {
				group := wsModelRoutingGroup(true)
				apiKey := &service.APIKey{ID: 7, GroupID: &group.ID, Group: group, User: &service.User{ID: 8}}
				h := &OpenAIGatewayHandler{
					gatewayService: &service.OpenAIGatewayService{}, apiKeyService: &service.APIKeyService{},
					billingCacheService: &service.BillingCacheService{},
					concurrencyHelper:   NewConcurrencyHelper(&service.ConcurrencyService{}, SSEPingFormatNone, 0),
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
				c.Set(string(middleware.ContextKeyAPIKey), apiKey)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
				endpoint.call(h, c)
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Contains(t, rec.Body.String(), openAIModelRoutingAmbiguousMessage)
			})
		}
	}
}

func TestOpenAIHTTPModelRoutingLeavesUnambiguousAndUnroutedBodies(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		enabled    bool
	}{
		{"identical duplicates", `{"model":"gpt-normal-routing-test","model":"gpt-normal-routing-test"}`, true},
		{"disabled", `{"model":"gpt-normal-routing-test","model":"gpt-restricted-routing-test"}`, false},
		{"unmatched", `{"model":"other-routing-test","model":"another-routing-test"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := wsModelRoutingGroup(tc.enabled)
			apiKey := &service.APIKey{GroupID: &group.ID, Group: group}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
			h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
			body := []byte(tc.body)
			require.False(t, h.rejectAmbiguousModelRouting(c, apiKey, body, h.errorResponse))
			require.Equal(t, tc.body, string(body), "passthrough bytes must not be rewritten")
			require.False(t, c.Writer.Written())
		})
	}
}
