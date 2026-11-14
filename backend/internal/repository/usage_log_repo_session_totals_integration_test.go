//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestUsageLog_SessionTotals proves the session-totals read adds up to the
// per-row usage, cuts days in the requested timezone, keeps NULL group / NULL
// session id as their own buckets, and pages without losing or repeating rows.
func TestUsageLog_SessionTotals(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)

	user := mustCreateUser(t, client, &service.User{Email: "session-totals-" + uuid.NewString() + "@example.com"})
	keyA := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-st-a-" + uuid.NewString(), Name: "a"})
	keyB := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-st-b-" + uuid.NewString(), Name: "b"})
	account := mustCreateAccount(t, client, &service.Account{Name: "acc-st-" + uuid.NewString()})
	group := mustCreateGroup(t, client, &service.Group{Name: "grp-st-" + uuid.NewString()})

	shanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	s1 := "sess-" + uuid.NewString()
	s2 := "sess-" + uuid.NewString()

	type call struct {
		key     int64
		group   *int64
		session *string
		at      time.Time
		in      int
		cc      int
		cr      int
		out     int
		total   float64
		actual  float64
	}
	gid := group.ID
	calls := []call{
		// 23:30 and 23:50 on 03-09 in Shanghai (15:30Z / 15:50Z).
		{keyA.ID, &gid, &s1, time.Date(2026, 3, 9, 23, 30, 0, 0, shanghai), 100, 10, 1000, 50, 0.20, 0.10},
		{keyA.ID, &gid, &s1, time.Date(2026, 3, 9, 23, 50, 0, 0, shanghai), 200, 0, 2000, 60, 0.30, 0.15},
		// 00:30 on 03-10 in Shanghai, still 03-09 in UTC (16:30Z).
		{keyA.ID, &gid, &s1, time.Date(2026, 3, 10, 0, 30, 0, 0, shanghai), 300, 30, 0, 70, 0.40, 0.20},
		// Same key, no session id, no group.
		{keyA.ID, nil, nil, time.Date(2026, 3, 10, 9, 0, 0, 0, shanghai), 5, 0, 0, 5, 0.01, 0},
		// Other key, other session.
		{keyB.ID, &gid, &s2, time.Date(2026, 3, 10, 10, 0, 0, 0, shanghai), 7, 0, 70, 9, 0.05, 0.05},
		// Outside the window (03-11 Shanghai).
		{keyB.ID, &gid, &s2, time.Date(2026, 3, 11, 0, 1, 0, 0, shanghai), 1, 1, 1, 1, 9, 9},
	}
	for _, c := range calls {
		_, err := repo.Create(ctx, &service.UsageLog{
			UserID:              user.ID,
			APIKeyID:            c.key,
			AccountID:           account.ID,
			GroupID:             c.group,
			RequestID:           uuid.NewString(),
			Model:               "claude-sonnet",
			InputTokens:         c.in,
			CacheCreationTokens: c.cc,
			CacheReadTokens:     c.cr,
			OutputTokens:        c.out,
			TotalCost:           c.total,
			ActualCost:          c.actual,
			SessionID:           c.session,
			CreatedAt:           c.at.UTC(),
		})
		require.NoError(t, err)
	}

	start := time.Date(2026, 3, 9, 0, 0, 0, 0, shanghai)
	end := time.Date(2026, 3, 11, 0, 0, 0, 0, shanghai)
	page, err := repo.GetSessionTotals(ctx, usagestats.SessionTotalsQuery{
		StartTime: start, EndTime: end, Timezone: "Asia/Shanghai", UserID: user.ID, Limit: 5000,
	})
	require.NoError(t, err)
	require.Nil(t, page.Next)
	require.Len(t, page.Items, 4)

	// Key order: day, user, key, group (0 first), session.
	first := page.Items[0]
	require.Equal(t, "2026-03-09", first.Day)
	require.Equal(t, user.Email, first.UserEmail)
	require.Equal(t, keyA.ID, first.APIKeyID)
	require.Equal(t, gid, *first.GroupID)
	require.Equal(t, s1, *first.SessionID)
	require.Equal(t, int64(2), first.Calls)
	require.Equal(t, int64(300), first.InputTokens)
	require.Equal(t, int64(10), first.CacheCreationTokens)
	require.Equal(t, int64(3000), first.CacheReadTokens)
	require.Equal(t, int64(110), first.OutputTokens)
	require.InDelta(t, 0.50, first.TotalCost, 1e-9)
	require.InDelta(t, 0.25, first.ActualCost, 1e-9)
	require.True(t, first.FirstAt.Equal(calls[0].at))
	require.True(t, first.LastAt.Equal(calls[1].at))

	noSession := page.Items[1]
	require.Equal(t, "2026-03-10", noSession.Day)
	require.Equal(t, keyA.ID, noSession.APIKeyID)
	require.Nil(t, noSession.GroupID)
	require.Nil(t, noSession.SessionID)
	require.Equal(t, int64(1), noSession.Calls)

	require.Equal(t, "2026-03-10", page.Items[2].Day)
	require.Equal(t, s1, *page.Items[2].SessionID)
	require.Equal(t, int64(1), page.Items[2].Calls)
	require.Equal(t, keyB.ID, page.Items[3].APIKeyID)
	require.Equal(t, s2, *page.Items[3].SessionID)

	// The aggregate adds up to the per-row usage inside the window.
	var calls64, tokens int64
	var actual float64
	for _, it := range page.Items {
		calls64 += it.Calls
		tokens += it.InputTokens + it.CacheCreationTokens + it.CacheReadTokens + it.OutputTokens
		actual += it.ActualCost
	}
	var wantTokens int64
	var wantActual float64
	for _, c := range calls[:5] {
		wantTokens += int64(c.in + c.cc + c.cr + c.out)
		wantActual += c.actual
	}
	require.Equal(t, int64(5), calls64)
	require.Equal(t, wantTokens, tokens)
	require.InDelta(t, wantActual, actual, 1e-9)

	// Cut in UTC, the 00:30 Shanghai call joins the 03-09 bucket.
	utcPage, err := repo.GetSessionTotals(ctx, usagestats.SessionTotalsQuery{
		StartTime: start, EndTime: end, Timezone: "UTC", UserID: user.ID, Limit: 5000,
	})
	require.NoError(t, err)
	require.Equal(t, "2026-03-09", utcPage.Items[0].Day)
	require.Equal(t, s1, *utcPage.Items[0].SessionID)
	require.Equal(t, int64(3), utcPage.Items[0].Calls)

	// Paging one row at a time returns the same rows in the same order.
	var paged []usagestats.SessionTotalsRow
	var after *usagestats.SessionTotalsKey
	for i := 0; i < 10; i++ {
		p, err := repo.GetSessionTotals(ctx, usagestats.SessionTotalsQuery{
			StartTime: start, EndTime: end, Timezone: "Asia/Shanghai", UserID: user.ID, After: after, Limit: 1,
		})
		require.NoError(t, err)
		paged = append(paged, p.Items...)
		if p.Next == nil {
			break
		}
		after = p.Next
	}
	require.Equal(t, page.Items, paged)
}
