//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const availabilityTestGroupID = int64(9400)

// availabilityAccountRepo serves the persistent candidate list the recovery
// estimate reads (active + schedulable, any transient state).
type availabilityAccountRepo struct {
	service.AccountRepository
	accounts   []*service.Account
	listCalls  int
	listGroups []int64
}

func (r *availabilityAccountRepo) ListModelAvailabilityCandidates(_ context.Context, groupID *int64, _ []string, _ bool) ([]service.Account, error) {
	r.listCalls++
	var out []service.Account
	for _, acc := range r.accounts {
		if acc.Status != service.StatusActive || !acc.Schedulable {
			continue
		}
		if groupID != nil {
			r.listGroups = append(r.listGroups, *groupID)
			inGroup := false
			for _, ag := range acc.AccountGroups {
				if ag.GroupID == *groupID {
					inGroup = true
				}
			}
			if !inGroup {
				continue
			}
		}
		out = append(out, *acc)
	}
	return out, nil
}

type availabilityFixture struct {
	h      *GatewayHandler
	repo   *availabilityAccountRepo
	group  *service.Group
	apiKey *service.APIKey
}

func newAvailabilityFixture(t *testing.T, group *service.Group, accounts ...*service.Account) *availabilityFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &availabilityAccountRepo{accounts: accounts}
	schedulerCache := &groupScopedSchedulerCache{fakeSchedulerCache: &fakeSchedulerCache{accounts: accounts}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewGatewayService(
		repo, &groupMapRepo{fakeGroupRepo: &fakeGroupRepo{}, groups: map[int64]*service.Group{group.ID: group}},
		nil, nil, nil, nil, nil, nil, nil,
		service.NewSchedulerSnapshotService(schedulerCache, nil, nil, nil, nil),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingCacheService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheService.Stop)
	groupID := group.ID
	return &availabilityFixture{
		h: &GatewayHandler{
			gatewayService:      gatewayService,
			billingCacheService: billingCacheService,
			concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
			maxAccountSwitches:  1,
			cfg:                 cfg,
		},
		repo:  repo,
		group: group,
		apiKey: &service.APIKey{
			ID: 9420, UserID: 9430, GroupID: &groupID, Group: group, Status: service.StatusActive,
			User: &service.User{ID: 9430, Concurrency: 10, Balance: 100},
		},
	}
}

func (f *availabilityFixture) context(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), ctxkey.Group, f.group)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil).WithContext(ctx)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
	}
	c.Request = req
	c.Set(string(middleware.ContextKeyAPIKey), f.apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: f.apiKey.UserID, Concurrency: 10})
	return c, recorder
}

func availabilityTestGroup() *service.Group {
	return &service.Group{
		ID: availabilityTestGroupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive,
	}
}

func availabilityTestAccount(id int64) *service.Account {
	return &service.Account{
		ID: id, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: availabilityTestGroupID}},
	}
}

func availabilityTimePtr(t time.Time) *time.Time { return &t }

// exhaustedPool: one account rate-limited for 20 min, one overloaded for 10
// min; the pool recovers when the overload ends.
func exhaustedPool(now time.Time) []*service.Account {
	limited := availabilityTestAccount(9410)
	limited.RateLimitResetAt = availabilityTimePtr(now.Add(20 * time.Minute))
	overloaded := availabilityTestAccount(9411)
	overloaded.OverloadUntil = availabilityTimePtr(now.Add(10 * time.Minute))
	return []*service.Account{limited, overloaded}
}

func decodeAvailability(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	return body
}

func TestKeyAvailability_AvailableWhenAnAccountIsSelectable(t *testing.T) {
	f := newAvailabilityFixture(t, availabilityTestGroup(), availabilityTestAccount(9410))
	c, recorder := f.context(http.MethodGet, "/v1/sub2api/availability?model=claude-opus-5", "")

	f.h.KeyAvailability(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Equal(t, map[string]any{"available": true}, decodeAvailability(t, recorder))
	require.Zero(t, f.repo.listCalls, "an available pool needs no recovery estimate")
}

func TestKeyAvailability_UnavailableCarriesEarliestRecovery(t *testing.T) {
	f := newAvailabilityFixture(t, availabilityTestGroup(), exhaustedPool(time.Now())...)
	c, recorder := f.context(http.MethodGet, "/v1/sub2api/availability?model=claude-opus-5", "")

	f.h.KeyAvailability(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	body := decodeAvailability(t, recorder)
	require.Equal(t, false, body["available"])
	retryAfter, ok := body["retry_after_s"].(float64)
	require.True(t, ok, "retry_after_s must be present when the recovery time is known: %v", body)
	require.InDelta(t, 600, retryAfter, 2)
	require.Equal(t, "no_available_accounts", body["code"])
	require.NotEmpty(t, f.repo.listGroups)
	for _, g := range f.repo.listGroups {
		require.Equal(t, availabilityTestGroupID, g, "recovery is read for the key's own group")
	}
}

func TestKeyAvailability_UnknownRecoveryOmitsRetryAfter(t *testing.T) {
	disabled := availabilityTestAccount(9410)
	disabled.Schedulable = false // switched off by hand: never lifts by itself
	f := newAvailabilityFixture(t, availabilityTestGroup(), disabled)
	c, recorder := f.context(http.MethodGet, "/v1/sub2api/availability", "")

	f.h.KeyAvailability(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, map[string]any{"available": false, "code": "no_available_accounts"}, decodeAvailability(t, recorder))
}

func TestKeyAvailability_ClaudeCodeOnlyGroupIsCheckedItself(t *testing.T) {
	group := availabilityTestGroup()
	group.ClaudeCodeOnly = true // no fallback group: a non-Claude-Code caller would be refused
	f := newAvailabilityFixture(t, group, availabilityTestAccount(9410))
	c, recorder := f.context(http.MethodGet, "/v1/sub2api/availability?model=claude-opus-5", "")

	f.h.KeyAvailability(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, map[string]any{"available": true}, decodeAvailability(t, recorder))
}

func TestKeyAvailability_RejectsKeysWithoutGroupAndOpenAIGroups(t *testing.T) {
	f := newAvailabilityFixture(t, availabilityTestGroup(), availabilityTestAccount(9410))
	f.apiKey.GroupID, f.apiKey.Group = nil, nil
	c, recorder := f.context(http.MethodGet, "/v1/sub2api/availability", "")
	f.h.KeyAvailability(c)
	require.Equal(t, http.StatusForbidden, recorder.Code)

	openAIGroup := availabilityTestGroup()
	openAIGroup.Platform = service.PlatformOpenAI
	f = newAvailabilityFixture(t, openAIGroup)
	c, recorder = f.context(http.MethodGet, "/v1/sub2api/availability", "")
	f.h.KeyAvailability(c)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func requireRetryAfterAbout(t *testing.T, recorder *httptest.ResponseRecorder, want int) {
	t.Helper()
	raw := recorder.Header().Get("Retry-After")
	require.NotEmpty(t, raw, "Retry-After must be set; body: %s", recorder.Body.String())
	got, err := strconv.Atoi(raw)
	require.NoError(t, err)
	require.InDelta(t, want, got, 2)
}

func TestNoAvailableAccounts503CarriesRetryAfter(t *testing.T) {
	endpoints := []struct {
		name string
		path string
		body string
		call func(*GatewayHandler, *gin.Context)
		// the body each endpoint already returned before this change
		wantBody string
	}{
		{
			name: "messages", path: "/v1/messages",
			body:     `{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`,
			call:     (*GatewayHandler).Messages,
			wantBody: "No available accounts",
		},
		{
			name: "chat completions", path: "/v1/chat/completions",
			body:     `{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],"stream":false}`,
			call:     (*GatewayHandler).ChatCompletions,
			wantBody: "No available accounts",
		},
		{
			name: "count_tokens", path: "/v1/messages/count_tokens",
			body:     `{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`,
			call:     (*GatewayHandler).CountTokens,
			wantBody: "Service temporarily unavailable",
		},
	}
	for _, ep := range endpoints {
		t.Run(ep.name, func(t *testing.T) {
			f := newAvailabilityFixture(t, availabilityTestGroup(), exhaustedPool(time.Now())...)
			c, recorder := f.context(http.MethodPost, ep.path, ep.body)

			ep.call(f.h, c)

			require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
			require.Contains(t, recorder.Body.String(), ep.wantBody)
			requireRetryAfterAbout(t, recorder, 600)
		})
	}
}

func TestNoAvailableAccounts503WithoutKnownRecoveryHasNoRetryAfter(t *testing.T) {
	disabled := availabilityTestAccount(9410)
	disabled.Schedulable = false
	f := newAvailabilityFixture(t, availabilityTestGroup(), disabled)
	c, recorder := f.context(http.MethodPost, "/v1/chat/completions",
		`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	f.h.ChatCompletions(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.Empty(t, recorder.Header().Get("Retry-After"))
}

func TestSetNoAccountRetryAfter_BusyAccountIsFiveSeconds(t *testing.T) {
	// Schedulable but passed over (e.g. no free slot): Retry-After 5.
	f := newAvailabilityFixture(t, availabilityTestGroup(), availabilityTestAccount(9410))
	c, recorder := f.context(http.MethodPost, "/v1/messages", `{}`)

	f.h.setNoAccountRetryAfter(c, f.apiKey.GroupID, "claude-opus-5", service.PlatformAnthropic)
	c.Status(http.StatusServiceUnavailable)

	require.Equal(t, "5", recorder.Header().Get("Retry-After"))
}
