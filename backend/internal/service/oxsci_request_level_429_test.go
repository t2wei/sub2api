//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	requestLevel429Body  = `{"type":"error","error":{"type":"rate_limit_error","message":"Error"},"request_id":"req_011ChAeiDxAcAf4n5KfRu7RX"}`
	accountLevel429Body  = `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`
	creditsRequired429   = `{"type":"error","error":{"type":"rate_limit_error","message":"Error","details":{"error_code":"credits_required","model":"claude-fable-5"}}}`
	requestLevelTestUser = "hello"
)

func TestIsAnthropicRequestLevel429(t *testing.T) {
	oauth := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	setupToken := &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeSetupToken}
	apiKey := &Account{ID: 3, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	bedrock := &Account{ID: 4, Platform: PlatformAnthropic, Type: AccountTypeBedrock}
	openAIOAuth := &Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	future := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}

	cases := []struct {
		name    string
		account *Account
		status  int
		headers http.Header
		body    string
		want    bool
	}{
		{name: "oauth bare Error, no headers", account: oauth, status: 429, body: requestLevel429Body, want: true},
		{name: "setup-token bare Error, no headers", account: setupToken, status: 429, body: requestLevel429Body, want: true},
		{name: "message trimmed and case-insensitive", account: oauth, status: 429, body: `{"type":"error","error":{"type":"rate_limit_error","message":"  error "}}`, want: true},
		{name: "retry-after is not an account signal", account: oauth, status: 429, headers: hdr("Retry-After", "30"), body: requestLevel429Body, want: true},
		{name: "utilization below the limit is not an account signal", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-5h-utilization", "0.42"), body: requestLevel429Body, want: true},

		{name: "api-key account unchanged", account: apiKey, status: 429, body: requestLevel429Body},
		{name: "bedrock account unchanged", account: bedrock, status: 429, body: requestLevel429Body},
		{name: "openai oauth unchanged", account: openAIOAuth, status: 429, body: requestLevel429Body},
		{name: "nil account", account: nil, status: 429, body: requestLevel429Body},
		{name: "not 429", account: oauth, status: 403, body: requestLevel429Body},
		{name: "account rate-limit message", account: oauth, status: 429, body: accountLevel429Body},
		{name: "message merely contains Error", account: oauth, status: 429, body: `{"type":"error","error":{"type":"rate_limit_error","message":"Error: rate limited"}}`},
		{name: "other error type", account: oauth, status: 429, body: `{"type":"error","error":{"type":"api_error","message":"Error"}}`},
		{name: "structured details", account: oauth, status: 429, body: creditsRequired429},
		{name: "empty body", account: oauth, status: 429, body: ``},

		{name: "aggregated reset header", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-reset", future), body: requestLevel429Body},
		{name: "5h reset header", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-5h-reset", future), body: requestLevel429Body},
		{name: "7d reset header", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-7d-reset", future), body: requestLevel429Body},
		{name: "7d_oi reset header", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-7d_oi-reset", future), body: requestLevel429Body},
		{name: "aggregated status rejected", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-status", "rejected"), body: requestLevel429Body},
		{name: "5h status rejected", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-5h-status", "rejected"), body: requestLevel429Body},
		{name: "7d utilization exhausted", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-7d-utilization", "1.0"), body: requestLevel429Body},
		{name: "5h surpassed threshold", account: oauth, status: 429, headers: hdr("anthropic-ratelimit-unified-5h-surpassed-threshold", "true"), body: requestLevel429Body},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isAnthropicRequestLevel429(tc.account, tc.status, tc.headers, []byte(tc.body)))
		})
	}
}

type requestLevel429Repo struct {
	AccountRepository
	account            *Account
	rateLimitedCalls   int
	modelLimitCalls    int
	sessionWindowCalls int
	extraCalls         int
	tempUnschedCalls   int
	errorCalls         int
}

func (r *requestLevel429Repo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *requestLevel429Repo) SetRateLimited(context.Context, int64, time.Time) error {
	r.rateLimitedCalls++
	return nil
}

func (r *requestLevel429Repo) SetModelRateLimit(context.Context, int64, string, time.Time, ...string) error {
	r.modelLimitCalls++
	return nil
}

func (r *requestLevel429Repo) UpdateSessionWindow(context.Context, int64, *time.Time, *time.Time, string) error {
	r.sessionWindowCalls++
	return nil
}

func (r *requestLevel429Repo) UpdateExtra(context.Context, int64, map[string]any) error {
	r.extraCalls++
	return nil
}

func (r *requestLevel429Repo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.tempUnschedCalls++
	return nil
}

func (r *requestLevel429Repo) SetError(context.Context, int64, string) error {
	r.errorCalls++
	return nil
}

func (r *requestLevel429Repo) accountStateWrites() int {
	return r.rateLimitedCalls + r.modelLimitCalls + r.sessionWindowCalls + r.tempUnschedCalls + r.errorCalls
}

// TestGatewayForward_RequestLevel429 drives GatewayService.Forward with one
// upstream 429 and checks the failover / account-state decision the handler's
// failover loop acts on: an UpstreamFailoverError means "switch account".
func TestGatewayForward_RequestLevel429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	future := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)

	cases := []struct {
		name         string
		accountType  string
		headers      map[string]string
		body         string
		requestLevel bool
	}{
		{name: "oauth bare Error answers the caller", accountType: AccountTypeOAuth, body: requestLevel429Body, requestLevel: true},
		{name: "setup-token bare Error answers the caller", accountType: AccountTypeSetupToken, body: requestLevel429Body, requestLevel: true},
		{name: "oauth account rate limit still fails over", accountType: AccountTypeOAuth, body: accountLevel429Body},
		{name: "oauth Error with window reset still fails over", accountType: AccountTypeOAuth, body: requestLevel429Body,
			headers: map[string]string{"anthropic-ratelimit-unified-5h-status": "rejected", "anthropic-ratelimit-unified-5h-reset": future}},
		{name: "api-key bare Error still fails over", accountType: AccountTypeAPIKey, body: requestLevel429Body},
	}

	for _, stream := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				account := &Account{
					ID: 25, Name: "ANT-TEAM-1", Platform: PlatformAnthropic, Type: tc.accountType,
					Status: StatusActive, Schedulable: true, Concurrency: 1,
					Credentials: map[string]any{"access_token": "tok", "refresh_token": "refresh", "api_key": "tok"},
				}
				repo := &requestLevel429Repo{account: account}
				cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
				calls := 0
				upstream := &systemOneHTTPUpstream{do: func(req *http.Request) (*http.Response, error) {
					calls++
					_, _ = io.Copy(io.Discard, req.Body)
					_ = req.Body.Close()
					h := http.Header{"Content-Type": []string{"application/json"}}
					for k, v := range tc.headers {
						h.Set(k, v)
					}
					return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				}}
				svc := &GatewayService{
					cfg: cfg, accountRepo: repo, httpUpstream: upstream,
					claudeTokenProvider: NewClaudeTokenProvider(repo, newClaudeTokenCacheStub(), nil),
					rateLimitService:    &RateLimitService{cfg: cfg, accountRepo: repo},
					deferredService:     &DeferredService{},
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				body := []byte(fmt.Sprintf(`{"model":"claude-opus-5-5","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":%q}]}`, stream, requestLevelTestUser))
				parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				require.NoError(t, err)

				result, err := svc.Forward(context.Background(), c, account, parsed)
				require.Nil(t, result)
				require.Error(t, err)
				require.Equal(t, 1, calls, "no same-account retry")

				var failover *UpstreamFailoverError
				if tc.requestLevel {
					require.NotErrorAs(t, err, &failover, "request-level 429 must not switch accounts")
					require.Zero(t, repo.accountStateWrites(), "request-level 429 must not mark the account")
					require.True(t, IsResponseCommitted(c))

					require.Equal(t, http.StatusTooManyRequests, rec.Code)
					require.Empty(t, rec.Header().Get("Retry-After"))
					var payload struct {
						Type  string `json:"type"`
						Error struct {
							Type    string `json:"type"`
							Message string `json:"message"`
						} `json:"error"`
					}
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
					require.Equal(t, "error", payload.Type)
					require.Equal(t, "rate_limit_error", payload.Error.Type)
					require.Equal(t, "Error", payload.Error.Message)

					v, ok := c.Get(OpsUpstreamErrorsKey)
					require.True(t, ok)
					events, _ := v.([]*OpsUpstreamErrorEvent)
					require.Len(t, events, 1)
					require.Equal(t, opsUpstreamKindRequestLevel429, events[0].Kind)
					require.Equal(t, "Error", events[0].Message)
					require.Equal(t, http.StatusTooManyRequests, events[0].UpstreamStatusCode)
					require.Equal(t, account.ID, events[0].AccountID)
					status, ok := c.Get(OpsUpstreamStatusCodeKey)
					require.True(t, ok)
					require.Equal(t, http.StatusTooManyRequests, status)
					return
				}

				require.ErrorAs(t, err, &failover, "account-level 429 keeps failing over")
				require.Equal(t, http.StatusTooManyRequests, failover.StatusCode)
				require.Equal(t, tc.body, string(failover.ResponseBody))
				require.Equal(t, 1, repo.rateLimitedCalls, "account-level 429 keeps marking the account")
				require.False(t, c.Writer.Written(), "leave the response to the failover loop")
				v, _ := c.Get(OpsUpstreamErrorsKey)
				events, _ := v.([]*OpsUpstreamErrorEvent)
				require.Len(t, events, 1)
				require.Equal(t, "failover", events[0].Kind)
			})
		}
	}
}
