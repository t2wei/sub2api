//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func recoveryTestAccount(id int64, groupID int64) Account {
	return Account{
		ID:            id,
		Platform:      PlatformAnthropic,
		Type:          AccountTypeOAuth,
		Status:        StatusActive,
		Schedulable:   true,
		AccountGroups: []AccountGroup{{AccountID: id, GroupID: groupID}},
	}
}

func recoveryTimePtr(t time.Time) *time.Time { return &t }

func withModelRateLimit(a Account, model string, resetAt time.Time) Account {
	a.Extra = map[string]any{
		modelRateLimitsKey: map[string]any{
			model: map[string]any{"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339)},
		},
	}
	return a
}

func TestAccountRecoveryAt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	const model = "claude-opus-5"

	t.Run("schedulable now counts as busy: now + 5s", func(t *testing.T) {
		a := recoveryTestAccount(1, 7)
		at, ok := accountRecoveryAt(ctx, &a, model, now)
		require.True(t, ok)
		require.Equal(t, now.Add(BusyAccountRecoveryDelay), at)
	})

	t.Run("blocked by several stored times: the latest future one", func(t *testing.T) {
		a := recoveryTestAccount(1, 7)
		a.RateLimitResetAt = recoveryTimePtr(now.Add(20 * time.Minute))
		a.OverloadUntil = recoveryTimePtr(now.Add(10 * time.Minute))
		a.TempUnschedulableUntil = recoveryTimePtr(now.Add(-time.Minute)) // already passed
		at, ok := accountRecoveryAt(ctx, &a, model, now)
		require.True(t, ok)
		require.Equal(t, now.Add(20*time.Minute), at)
	})

	t.Run("times already passed are ignored", func(t *testing.T) {
		a := recoveryTestAccount(1, 7)
		a.RateLimitResetAt = recoveryTimePtr(now.Add(-time.Second))
		a.OverloadUntil = recoveryTimePtr(now.Add(-time.Hour))
		at, ok := accountRecoveryAt(ctx, &a, model, now)
		require.True(t, ok)
		require.Equal(t, now.Add(BusyAccountRecoveryDelay), at)
	})

	t.Run("model rate limit applies only to that model", func(t *testing.T) {
		a := withModelRateLimit(recoveryTestAccount(1, 7), model, now.Add(30*time.Minute))
		a.OverloadUntil = recoveryTimePtr(now.Add(5 * time.Minute))

		at, ok := accountRecoveryAt(ctx, &a, model, now)
		require.True(t, ok)
		require.Equal(t, now.Add(30*time.Minute), at)

		at, ok = accountRecoveryAt(ctx, &a, "claude-sonnet-5", now)
		require.True(t, ok)
		require.Equal(t, now.Add(5*time.Minute), at)
	})

	t.Run("states that do not lift on a clock are not counted", func(t *testing.T) {
		inactive := recoveryTestAccount(1, 7)
		inactive.Status = StatusError
		off := recoveryTestAccount(2, 7)
		off.Schedulable = false
		expired := recoveryTestAccount(3, 7)
		expired.AutoPauseOnExpired = true
		expired.ExpiresAt = recoveryTimePtr(now.Add(-time.Minute))
		quota := recoveryTestAccount(4, 7)
		quota.Type = AccountTypeAPIKey
		quota.Extra = map[string]any{"quota_limit": 10.0, "quota_used": 10.0}
		require.True(t, quota.IsQuotaExceeded(), "fixture must exceed its account quota")

		for _, a := range []Account{inactive, off, expired, quota} {
			_, ok := accountRecoveryAt(ctx, &a, model, now)
			require.False(t, ok, "account %d must not be counted", a.ID)
		}
	})
}

func TestEarliestRecoveryAt_MinimumOverAccounts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	a := recoveryTestAccount(1, 7)
	a.RateLimitResetAt = recoveryTimePtr(now.Add(3*time.Hour + 40*time.Minute))
	b := recoveryTestAccount(2, 7)
	b.OverloadUntil = recoveryTimePtr(now.Add(19 * time.Minute))
	c := recoveryTestAccount(3, 7)
	c.Status = StatusDisabled

	at, ok := earliestRecoveryAt(ctx, []*Account{&a, &b, &c}, "claude-opus-5", now)
	require.True(t, ok)
	require.Equal(t, now.Add(19*time.Minute), at)

	_, ok = earliestRecoveryAt(ctx, []*Account{&c}, "claude-opus-5", now)
	require.False(t, ok, "only uncounted accounts: unknown")

	_, ok = earliestRecoveryAt(ctx, nil, "claude-opus-5", now)
	require.False(t, ok, "no accounts: unknown")
}

func TestEarliestRecoveryForPlatform_ScopesToGroupAndModel(t *testing.T) {
	now := time.Now()
	inGroupLimited := recoveryTestAccount(1, 7)
	inGroupLimited.RateLimitResetAt = recoveryTimePtr(now.Add(40 * time.Minute))

	// Same group, earlier reset, but its mapping does not serve the model.
	otherModel := recoveryTestAccount(2, 7)
	otherModel.OverloadUntil = recoveryTimePtr(now.Add(2 * time.Minute))
	otherModel.Credentials = map[string]any{"model_mapping": map[string]any{"claude-haiku-5-5": "claude-haiku-5-5"}}

	// Serves the model and recovers earlier, but belongs to another group.
	otherGroup := recoveryTestAccount(3, 8)
	otherGroup.OverloadUntil = recoveryTimePtr(now.Add(time.Minute))

	repo := &mockAccountRepoForPlatform{
		accounts:     []Account{inGroupLimited, otherModel, otherGroup},
		accountsByID: map[int64]*Account{},
	}
	svc := &GatewayService{accountRepo: repo, cfg: testConfig()}
	groupID := int64(7)

	at, known, err := svc.EarliestRecoveryForPlatform(context.Background(), &groupID, "claude-opus-5", PlatformAnthropic)
	require.NoError(t, err)
	require.True(t, known)
	require.WithinDuration(t, now.Add(40*time.Minute), at, time.Second)

	// Without a model every account in the group counts.
	at, known, err = svc.EarliestRecoveryForPlatform(context.Background(), &groupID, "", PlatformAnthropic)
	require.NoError(t, err)
	require.True(t, known)
	require.WithinDuration(t, now.Add(2*time.Minute), at, time.Second)

	emptyGroup := int64(99)
	_, known, err = svc.EarliestRecoveryForPlatform(context.Background(), &emptyGroup, "claude-opus-5", PlatformAnthropic)
	require.NoError(t, err)
	require.False(t, known)
}

type failingCandidatesRepo struct {
	mockAccountRepoForPlatform
	err error
}

func (r *failingCandidatesRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	return nil, r.err
}

func TestEarliestRecoveryForPlatform_ReturnsRepositoryError(t *testing.T) {
	boom := errors.New("db down")
	svc := &GatewayService{accountRepo: &failingCandidatesRepo{err: boom}, cfg: testConfig()}
	groupID := int64(7)

	_, known, err := svc.EarliestRecoveryForPlatform(context.Background(), &groupID, "claude-opus-5", PlatformAnthropic)
	require.ErrorIs(t, err, boom)
	require.False(t, known)
}

func TestRetryAfterSeconds_RoundsUpAndFloorsAtOne(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	require.Equal(t, 1140, RetryAfterSeconds(now.Add(1140*time.Second), now))
	require.Equal(t, 1141, RetryAfterSeconds(now.Add(1140*time.Second+200*time.Millisecond), now))
	require.Equal(t, 5, RetryAfterSeconds(now.Add(BusyAccountRecoveryDelay), now))
	require.Equal(t, 1, RetryAfterSeconds(now, now))
	require.Equal(t, 1, RetryAfterSeconds(now.Add(-time.Minute), now))
}
