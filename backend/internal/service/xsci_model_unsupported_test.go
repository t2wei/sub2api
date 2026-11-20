//go:build unit

package service

// [OXSCI] P160 T9: the model_unsupported verdict (XSciModelUnsupported) and
// its served-in-7-days lookup. The client-visible rows are locked by the
// handler's contract test; this covers the verdict's inputs one by one.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const xmuModel = "claude-opus-9-9"

type xmuServedRepo struct {
	UsageLogRepository
	mu      sync.Mutex
	served  bool
	err     error
	calls   int
	groupID int64
	model   string
	since   time.Time
}

func (r *xmuServedRepo) XSciGroupServedModelSince(_ context.Context, groupID int64, model string, since time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.groupID, r.model, r.since = groupID, model, since
	return r.served, r.err
}

func xmuNotFoundCooling(a Account, until time.Time) Account {
	a.Extra = map[string]any{modelRateLimitsKey: map[string]any{
		xmuModel: map[string]any{"rate_limit_reset_at": until.UTC().Format(time.RFC3339), "reason": upstreamModelNotFoundReason},
	}}
	return a
}

func xmuService(usage UsageLogRepository, accounts ...Account) *GatewayService {
	return &GatewayService{
		accountRepo:  &mockAccountRepoForPlatform{accounts: accounts, accountsByID: map[int64]*Account{}},
		usageLogRepo: usage,
		cfg:          testConfig(),
	}
}

func TestXSciModelUnsupported(t *testing.T) {
	ctx := context.Background()
	gid := int64(7)
	now := time.Now()
	cooling := xmuNotFoundCooling(recoveryTestAccount(1, gid), now.Add(20*time.Minute))
	limited := recoveryTestAccount(2, gid)
	limited.RateLimitResetAt = recoveryTimePtr(now.Add(3 * time.Hour))
	otherReason := recoveryTestAccount(3, gid)
	otherReason.Extra = map[string]any{modelRateLimitsKey: map[string]any{
		xmuModel: map[string]any{"rate_limit_reset_at": now.Add(20 * time.Minute).UTC().Format(time.RFC3339), "reason": "upstream_429_model_rate_limited"},
	}}
	expired := xmuNotFoundCooling(recoveryTestAccount(4, gid), now.Add(-time.Minute))

	cases := []struct {
		name     string
		served   bool
		accounts []Account
		tried    XSciTriedAnswers
		want     bool
		lookups  int
	}{
		{"strict: every configured account cooling it", false, []Account{cooling, xmuNotFoundCooling(recoveryTestAccount(5, gid), now.Add(time.Minute))}, XSciTriedAnswers{}, true, 0},
		{"strict holds for a served model too", true, []Account{cooling}, XSciTriedAnswers{NotFound: 1}, true, 0},
		{"not served: one cooling, the rest rate-limited", false, []Account{cooling, limited}, XSciTriedAnswers{}, true, 1},
		{"served: one cooling, the rest rate-limited", true, []Account{cooling, limited}, XSciTriedAnswers{}, false, 1},
		{"not served: tried in this request, all model not found", false, []Account{limited, recoveryTestAccount(6, gid)}, XSciTriedAnswers{NotFound: 2}, true, 1},
		{"not served: one answer in this request was something else", false, []Account{cooling, limited}, XSciTriedAnswers{NotFound: 1, Other: 1}, false, 0},
		{"not served: nobody answered (all rate-limited)", false, []Account{limited}, XSciTriedAnswers{}, false, 0},
		{"not served: cooling for another reason is no answer", false, []Account{otherReason, limited}, XSciTriedAnswers{}, false, 0},
		{"not served: an expired cooldown is no answer", false, []Account{expired, limited}, XSciTriedAnswers{}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &xmuServedRepo{served: tc.served}
			svc := xmuService(repo, tc.accounts...)
			require.Equal(t, tc.want, svc.XSciModelUnsupported(ctx, &gid, xmuModel, PlatformAnthropic, tc.tried))
			require.Equal(t, tc.lookups, repo.calls)
			if repo.calls > 0 {
				require.Equal(t, gid, repo.groupID)
				require.Equal(t, xmuModel, repo.model)
				require.WithinDuration(t, time.Now().Add(-7*24*time.Hour), repo.since, time.Minute)
			}
		})
	}

	t.Run("no group: strict only", func(t *testing.T) {
		repo := &xmuServedRepo{}
		svc := xmuService(repo, cooling, limited)
		require.False(t, svc.XSciModelUnsupported(ctx, nil, xmuModel, PlatformAnthropic, XSciTriedAnswers{NotFound: 1}))
		require.Zero(t, repo.calls)
	})

	t.Run("usage repository without the lookup: strict only", func(t *testing.T) {
		svc := xmuService(&mockUsageLogRepoNoLookup{}, cooling, limited)
		require.False(t, svc.XSciModelUnsupported(ctx, &gid, xmuModel, PlatformAnthropic, XSciTriedAnswers{}))
	})

	t.Run("lookup failed: strict only, not cached", func(t *testing.T) {
		repo := &xmuServedRepo{err: errors.New("db down")}
		svc := xmuService(repo, cooling, limited)
		require.False(t, svc.XSciModelUnsupported(ctx, &gid, xmuModel, PlatformAnthropic, XSciTriedAnswers{}))
		require.False(t, svc.XSciModelUnsupported(ctx, &gid, xmuModel, PlatformAnthropic, XSciTriedAnswers{}))
		require.Equal(t, 2, repo.calls)
	})

	t.Run("lookup cached per group and model", func(t *testing.T) {
		repo := &xmuServedRepo{}
		svc := xmuService(repo, cooling, limited)
		for i := 0; i < 3; i++ {
			require.True(t, svc.XSciModelUnsupported(ctx, &gid, xmuModel, PlatformAnthropic, XSciTriedAnswers{}))
		}
		require.Equal(t, 1, repo.calls)
		other := int64(8)
		svc.XSciModelUnsupported(ctx, &other, xmuModel, PlatformAnthropic, XSciTriedAnswers{NotFound: 1})
		require.Equal(t, 2, repo.calls)
	})
}

type mockUsageLogRepoNoLookup struct{ UsageLogRepository }

func TestXSciServedModelCache(t *testing.T) {
	var c xsciServedModelCache // zero value is ready
	now := time.Now()
	_, ok := c.get("k", now)
	require.False(t, ok)

	c.put("k", false, now.Add(xsciModelNotServedTTL))
	served, ok := c.get("k", now.Add(30*time.Second))
	require.True(t, ok)
	require.False(t, served)
	_, ok = c.get("k", now.Add(xsciModelNotServedTTL))
	require.False(t, ok, "a \"not served\" answer is asked again after a minute")

	c.put("s", true, now.Add(xsciModelServedTTL))
	served, ok = c.get("s", now.Add(9*time.Minute))
	require.True(t, ok)
	require.True(t, served)

	// Bounded: a full map drops expired entries, then everything if still full.
	c = xsciServedModelCache{}
	for i := 0; i < xsciModelServedCacheLimit; i++ {
		c.put(string(rune(0x4e00+i)), true, now.Add(time.Hour))
	}
	c.put("new", true, now.Add(time.Hour))
	require.LessOrEqual(t, len(c.m), xsciModelServedCacheLimit)
	_, ok = c.get("new", now)
	require.True(t, ok)
}

func TestXSciTriedAnswers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	notFound := &UpstreamFailoverError{StatusCode: 404, ResponseBody: []byte(`{"type":"error","error":{"type":"not_found_error","message":"model: claude-opus-9-9"}}`)}
	other := &UpstreamFailoverError{StatusCode: 529, ResponseBody: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)}

	// Off the surface nothing is recorded.
	XSciNoteFailoverAnswer(c, notFound)
	require.Equal(t, XSciTriedAnswers{}, XSciTriedAnswersOf(c))

	c.Set(xsciSurfaceKey, true)
	XSciNoteFailoverAnswer(c, notFound)
	XSciNoteFailoverAnswer(c, notFound)
	XSciNoteFailoverAnswer(c, other)
	XSciNoteFailoverAnswer(c, nil)
	require.Equal(t, XSciTriedAnswers{NotFound: 2, Other: 1}, XSciTriedAnswersOf(c))
	require.Equal(t, XSciTriedAnswers{}, XSciTriedAnswersOf(nil))
}
