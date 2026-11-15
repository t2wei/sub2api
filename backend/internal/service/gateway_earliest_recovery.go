package service

// [OXSCI] Earliest recovery of a group's account pool for one model.
//
// When account selection finds nothing, every reason an account is blocked is
// already stored on the account row: rate_limit_reset_at, overload_until,
// temp_unschedulable_until and extra.model_rate_limits[*].rate_limit_reset_at.
// IsSchedulable compares them with the clock on every selection, so the pool
// recovers by itself once the earliest of them passes. This file turns those
// fields into one time so the gateway can say when to come back:
//
//   - the "No available accounts" 503 carries Retry-After;
//   - GET /v1/sub2api/availability reports retry_after_s.
//
// It lives in its own file so rebasing onto upstream only touches the call
// sites, not this logic.

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// BusyAccountRecoveryDelay is the recovery estimate for an account that is
// schedulable right now and was only passed over for a runtime limit with no
// stored end time (concurrency slots, wait queue, RPM, window cost).
const BusyAccountRecoveryDelay = 5 * time.Second

// EarliestRecoveryForPlatform returns when the first account in the group that
// serves requestedModel is expected to become selectable again.
//
// known is false when no account counts: the group has no persistently
// enabled account for the model, or every one of them is blocked by something
// that does not lift by itself on a clock (expired credentials, exhausted
// account quota). Callers then omit the hint instead of guessing.
//
// The candidate list and platform scoping are the same as
// DiagnoseModelAvailabilityForPlatform: persistently enabled accounts
// (active + schedulable), ignoring transient state.
func (s *GatewayService) EarliestRecoveryForPlatform(
	ctx context.Context,
	groupID *int64,
	requestedModel string,
	platform string,
) (at time.Time, known bool, err error) {
	if s == nil || s.accountRepo == nil {
		return time.Time{}, false, nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	platform = strings.TrimSpace(platform)
	if platform == "" {
		return time.Time{}, false, nil
	}

	useMixed := platform == PlatformAnthropic || platform == PlatformGemini
	platforms := []string{platform}
	if useMixed {
		platforms = append(platforms, PlatformAntigravity)
	}
	queryGroupID := groupID
	includeGrouped := false
	if useMixed {
		if groupID == nil && s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
			includeGrouped = true
		}
	} else if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		queryGroupID = nil
		includeGrouped = true
	}

	accounts, err := s.accountRepo.ListModelAvailabilityCandidates(ctx, queryGroupID, platforms, includeGrouped)
	if err != nil {
		return time.Time{}, false, err
	}

	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if useMixed && acc.Platform == PlatformAntigravity && !acc.IsMixedSchedulingEnabled() {
			continue
		}
		if requestedModel != "" && !s.isModelSupportedByAccountWithContext(ctx, acc, requestedModel) {
			continue
		}
		candidates = append(candidates, acc)
	}
	at, known = earliestRecoveryAt(ctx, candidates, requestedModel, time.Now())
	return at, known, nil
}

// earliestRecoveryAt is the minimum of accountRecoveryAt over the candidates.
func earliestRecoveryAt(ctx context.Context, candidates []*Account, requestedModel string, now time.Time) (time.Time, bool) {
	var earliest time.Time
	known := false
	for _, acc := range candidates {
		at, ok := accountRecoveryAt(ctx, acc, requestedModel, now)
		if !ok {
			continue
		}
		if !known || at.Before(earliest) {
			earliest = at
			known = true
		}
	}
	return earliest, known
}

// accountRecoveryAt estimates when one account can be selected again:
//
//   - disabled, errored or switched off by hand: not counted;
//   - expired credentials or exhausted account quota: not counted (no stored
//     time lifts them);
//   - blocked by stored times: the latest of those still in the future, since
//     every one of them has to pass;
//   - otherwise it is schedulable now and was only passed over for a runtime
//     limit (concurrency, RPM, window cost): now + BusyAccountRecoveryDelay.
func accountRecoveryAt(ctx context.Context, a *Account, requestedModel string, now time.Time) (time.Time, bool) {
	if a == nil || !a.IsActive() || !a.Schedulable {
		return time.Time{}, false
	}
	if a.AutoPauseOnExpired && a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return time.Time{}, false
	}
	if a.SupportsAccountQuota() && a.IsQuotaExceeded() {
		return time.Time{}, false
	}

	var latest time.Time
	consider := func(t *time.Time) {
		if t != nil && t.After(now) && t.After(latest) {
			latest = *t
		}
	}
	consider(a.RateLimitResetAt)
	consider(a.OverloadUntil)
	consider(a.TempUnschedulableUntil)
	// Same exemption as IsSchedulableForModelWithContext: Antigravity accounts
	// with overages enabled and credits left ignore model-level limits.
	overagesExempt := a.Platform == PlatformAntigravity && a.IsOveragesEnabled() && !a.isCreditsExhausted()
	if !overagesExempt {
		for _, key := range a.modelRateLimitKeysForRequest(ctx, requestedModel) {
			consider(a.modelRateLimitResetAt(key))
		}
	}

	if latest.IsZero() {
		return now.Add(BusyAccountRecoveryDelay), true
	}
	return latest, true
}

// RetryAfterSeconds turns a recovery time into a Retry-After value: whole
// seconds, rounded up, at least 1.
func RetryAfterSeconds(at, now time.Time) int {
	seconds := int(math.Ceil(at.Sub(now).Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}
