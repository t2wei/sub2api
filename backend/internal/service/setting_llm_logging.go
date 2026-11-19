package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// [OXSCI] Call logging to data-service: the runtime switch.
//
// llm_logging_enabled decides whether the gateway's call-logging hook delivers calls
// to the configured sink. It defaults to off: an absent, empty or unreadable value
// means off. The hook is only wired when a sink URL is configured (llm_logging.url);
// with no URL this switch has no effect. The old llm_logging.enabled config flag is
// gone — this setting is the only switch.
//
// The gateway reads the switch on every request, so the read is served from an
// in-process cache (60s TTL) that is refreshed in the background and never makes a
// request wait on the DB. Saving the switch refreshes this node's cache at once;
// other nodes pick it up within a minute. It has its own admin endpoint and is not
// part of SystemSettings.

// SettingKeyLLMLoggingEnabled turns call logging to data-service on ("true") or off.
const SettingKeyLLMLoggingEnabled = "llm_logging_enabled"

const (
	llmLoggingEnabledCacheTTL   = 60 * time.Second
	llmLoggingEnabledErrorTTL   = 5 * time.Second
	llmLoggingEnabledDBTimeout  = 5 * time.Second
	llmLoggingEnabledRefreshKey = "llm_logging_enabled"
)

type cachedLLMLoggingEnabled struct {
	enabled   bool
	expiresAt int64 // unix nano
}

// parseLLMLoggingEnabled reads a stored value; anything but a true boolean is off.
func parseLLMLoggingEnabled(raw string) bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	return err == nil && enabled
}

// GetLLMLoggingEnabled reads the switch from the DB (admin read path, no cache).
func (s *SettingService) GetLLMLoggingEnabled(ctx context.Context) (bool, error) {
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyLLMLoggingEnabled)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get llm logging enabled: %w", err)
	}
	return parseLLMLoggingEnabled(raw), nil
}

// SetLLMLoggingEnabled saves the switch and refreshes this node's cache, so the next
// request on this node already follows it.
func (s *SettingService) SetLLMLoggingEnabled(ctx context.Context, enabled bool) error {
	s.llmLoggingEnabledMu.Lock()
	defer s.llmLoggingEnabledMu.Unlock()
	if err := s.settingRepo.Set(ctx, SettingKeyLLMLoggingEnabled, strconv.FormatBool(enabled)); err != nil {
		return fmt.Errorf("save llm logging enabled: %w", err)
	}
	s.storeLLMLoggingEnabled(enabled, llmLoggingEnabledCacheTTL)
	return nil
}

// LLMLoggingEnabled is the gateway's per-request read. It never blocks on the DB:
// a fresh cached value is returned as is; a stale one is returned while a background
// refresh runs; with nothing cached yet it answers off and starts the refresh.
func (s *SettingService) LLMLoggingEnabled(_ context.Context) bool {
	if s == nil || s.settingRepo == nil {
		return false
	}
	cached, _ := s.llmLoggingEnabledCache.Load().(*cachedLLMLoggingEnabled)
	if cached != nil && time.Now().UnixNano() < cached.expiresAt {
		return cached.enabled
	}
	s.llmLoggingEnabledSF.DoChan(llmLoggingEnabledRefreshKey, func() (any, error) {
		s.refreshLLMLoggingEnabled()
		return nil, nil
	})
	if cached != nil {
		return cached.enabled
	}
	return false
}

// refreshLLMLoggingEnabled reloads the cache from the DB. On a DB error it keeps the
// last known value (off if none) and retries sooner. The lock keeps a refresh that
// read the old value from overwriting a value saved meanwhile.
func (s *SettingService) refreshLLMLoggingEnabled() {
	s.llmLoggingEnabledMu.Lock()
	defer s.llmLoggingEnabledMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), llmLoggingEnabledDBTimeout)
	defer cancel()
	enabled, err := s.GetLLMLoggingEnabled(ctx)
	if err != nil {
		slog.Warn("failed to refresh llm logging switch; keeping the last known value", "error", err)
		prior := false
		if cached, _ := s.llmLoggingEnabledCache.Load().(*cachedLLMLoggingEnabled); cached != nil {
			prior = cached.enabled
		}
		s.storeLLMLoggingEnabled(prior, llmLoggingEnabledErrorTTL)
		return
	}
	s.storeLLMLoggingEnabled(enabled, llmLoggingEnabledCacheTTL)
}

func (s *SettingService) storeLLMLoggingEnabled(enabled bool, ttl time.Duration) {
	s.llmLoggingEnabledCache.Store(&cachedLLMLoggingEnabled{
		enabled:   enabled,
		expiresAt: time.Now().Add(ttl).UnixNano(),
	})
}

// LLMLoggingSinkConfigured reports whether a sink URL is configured; without one the
// switch has no effect.
func (s *SettingService) LLMLoggingSinkConfigured() bool {
	return s != nil && s.cfg != nil && strings.TrimSpace(s.cfg.LLMLogging.URL) != ""
}
