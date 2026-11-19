package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLLMLoggingEnabledDefaultsToOff(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"no row":     {},
		"empty":      {SettingKeyLLMLoggingEnabled: "  "},
		"unreadable": {SettingKeyLLMLoggingEnabled: "yes please"},
		"false":      {SettingKeyLLMLoggingEnabled: "false"},
	} {
		t.Run(name, func(t *testing.T) {
			svc := NewSettingService(&panelRateLimitSettingRepo{values: values}, nil)
			enabled, err := svc.GetLLMLoggingEnabled(context.Background())
			require.NoError(t, err)
			require.False(t, enabled)
			require.False(t, svc.LLMLoggingEnabled(context.Background()))
			svc.refreshLLMLoggingEnabled()
			require.False(t, svc.LLMLoggingEnabled(context.Background()))
		})
	}

	var nilSvc *SettingService
	require.False(t, nilSvc.LLMLoggingEnabled(context.Background()))
}

func TestLLMLoggingEnabledRoundTrip(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{}}
	svc := NewSettingService(repo, nil)

	require.NoError(t, svc.SetLLMLoggingEnabled(context.Background(), true))
	require.Equal(t, "true", repo.values[SettingKeyLLMLoggingEnabled])
	enabled, err := svc.GetLLMLoggingEnabled(context.Background())
	require.NoError(t, err)
	require.True(t, enabled)
	calls := repo.getValueCalls
	require.True(t, svc.LLMLoggingEnabled(context.Background()), "saving refreshes this node's cache at once")
	require.Equal(t, calls, repo.getValueCalls, "the per-request read is served from the cache")

	require.NoError(t, svc.SetLLMLoggingEnabled(context.Background(), false))
	require.Equal(t, "false", repo.values[SettingKeyLLMLoggingEnabled])
	require.False(t, svc.LLMLoggingEnabled(context.Background()))
}

func TestLLMLoggingEnabledLoadsInTheBackground(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{SettingKeyLLMLoggingEnabled: "true"}}
	svc := NewSettingService(repo, nil)

	// Nothing cached yet: the request is not held up; it reads off and starts a refresh.
	require.False(t, svc.LLMLoggingEnabled(context.Background()))
	require.Eventually(t, func() bool { return svc.LLMLoggingEnabled(context.Background()) }, 5*time.Second, 10*time.Millisecond)

	// Another node turned it off: an expired entry is served once more while it refreshes.
	repo.mu.Lock()
	repo.values[SettingKeyLLMLoggingEnabled] = "false"
	repo.mu.Unlock()
	svc.storeLLMLoggingEnabled(true, -time.Second)
	require.True(t, svc.LLMLoggingEnabled(context.Background()))
	require.Eventually(t, func() bool { return !svc.LLMLoggingEnabled(context.Background()) }, 5*time.Second, 10*time.Millisecond)
}

func TestLLMLoggingEnabledKeepsLastValueOnDBError(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{SettingKeyLLMLoggingEnabled: "true"}}
	svc := NewSettingService(repo, nil)
	svc.refreshLLMLoggingEnabled()
	require.True(t, svc.LLMLoggingEnabled(context.Background()))

	repo.mu.Lock()
	repo.getValueErr = errors.New("db down")
	repo.mu.Unlock()
	svc.refreshLLMLoggingEnabled()
	require.True(t, svc.LLMLoggingEnabled(context.Background()))
	_, err := svc.GetLLMLoggingEnabled(context.Background())
	require.Error(t, err)

	fresh := NewSettingService(&panelRateLimitSettingRepo{getValueErr: errors.New("db down")}, nil)
	fresh.refreshLLMLoggingEnabled()
	require.False(t, fresh.LLMLoggingEnabled(context.Background()), "no known value: off")
}

func TestLLMLoggingSinkConfigured(t *testing.T) {
	require.False(t, NewSettingService(&panelRateLimitSettingRepo{}, nil).LLMLoggingSinkConfigured())
	require.False(t, NewSettingService(&panelRateLimitSettingRepo{}, &config.Config{}).LLMLoggingSinkConfigured())
	require.True(t, NewSettingService(&panelRateLimitSettingRepo{}, &config.Config{
		LLMLogging: config.LLMLoggingConfig{URL: "http://localhost:8008/api/database/v1/llm_calls"},
	}).LLMLoggingSinkConfigured())
}
