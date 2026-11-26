package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// DefaultSignupGrantSource is the sign-up source the admin read answers for when the
// caller names none: the one XSci staff sign in with.
const DefaultSignupGrantSource = "oidc"

// ErrSignupSourceUnknown is a sign-up source no auth source default exists for.
var ErrSignupSourceUnknown = infraerrors.BadRequest(
	"SIGNUP_SOURCE_UNKNOWN",
	"source must be one of: email, linuxdo, oidc, wechat, github, google, dingtalk",
)

// SignupGrantSubscriptions answers the subscriptions an account created by a first
// sign-in through source is given: the same resolution the sign-up itself makes
// (AuthService's sign-up grant plan) — the global default subscriptions, replaced by
// the source's own when its grant-on-signup is on and it names any. It reads
// settings only and writes nothing. A sign-in the source's own rules withhold
// subscriptions from (an OIDC sign-in whose groups name no staff admission) gets
// none; that is decided per sign-in, not here. Never nil.
func (s *SettingService) SignupGrantSubscriptions(ctx context.Context, source string) ([]DefaultSubscriptionSetting, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	if _, known := authSourceSignupSettings(&AuthSourceDefaultSettings{}, source); !known {
		return nil, ErrSignupSourceUnknown
	}
	resolved, _, err := s.ResolveAuthSourceGrantSettings(ctx, source, false)
	if err != nil {
		return nil, err
	}
	out := make([]DefaultSubscriptionSetting, 0, len(resolved.Subscriptions))
	return append(out, resolved.Subscriptions...), nil
}
