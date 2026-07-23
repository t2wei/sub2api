//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildUpstreamRequest_Context1MWithToolChangeBetasRespectsPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dropped := range []string{"", claude.BetaContext1M, claude.BetaMidConversationToolChanges, claude.BetaInlineTools} {
		name := "preserve all"
		if dropped != "" {
			name = "drop " + dropped
		}
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("anthropic-beta", strings.Join([]string{
				claude.BetaMidConversationToolChanges, claude.BetaInlineTools, "unrelated-beta",
			}, ","))
			if dropped != "" {
				c.Set(betaPolicyFilterSetKey, map[string]struct{}{dropped: {}})
			}
			account := &Account{ID: 702, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token"}, Status: StatusActive, Schedulable: true}
			body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"tool_addition","tool":{"type":"tool_reference","tool_name":"lookup"}},{"type":"text","text":"Use lookup"}]}]}`)
			svc := &GatewayService{cfg: &config.Config{}}
			req, _, err := svc.buildUpstreamRequest(WithAnthropicContext1M(context.Background()), c, account, body,
				"test-token", "oauth", "claude-opus-4-8", false, true)
			require.NoError(t, err)
			require.JSONEq(t, gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(readUpstreamBodyForTest(t, req), "messages").Raw)
			header := getHeaderRaw(req.Header, "anthropic-beta")
			for _, token := range []string{claude.BetaContext1M, claude.BetaMidConversationToolChanges, claude.BetaInlineTools} {
				require.Equal(t, token != dropped, containsBetaToken(header, token), "beta %s in %s", token, header)
			}
			require.True(t, containsBetaToken(header, claude.BetaOAuth))
			require.False(t, containsBetaToken(header, "unrelated-beta"))
		})
	}
}
