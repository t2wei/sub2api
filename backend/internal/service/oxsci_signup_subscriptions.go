package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// signupSubscriptionsWithheldKey marks a context whose sign-up gets no default
// subscriptions (OxSci: OIDC sign-ups that are not staff).
type signupSubscriptionsWithheldKey struct{}

// WithSignupSubscriptionsWithheld returns a context under which an account created by
// sign-up gets none of the default subscriptions. Balance, concurrency, RPM and
// platform-quota defaults still apply. Who is withheld is the caller's decision (the
// OIDC handler: groups without a staff entry); the service only honours the mark.
func WithSignupSubscriptionsWithheld(ctx context.Context) context.Context {
	return context.WithValue(ctx, signupSubscriptionsWithheldKey{}, true)
}

// signupSubscriptionsWithheld reports whether ctx carries the mark, and logs the
// subscriptions it holds back so an operator can see why a new account has none.
func signupSubscriptionsWithheld(ctx context.Context, userID int64, items []DefaultSubscriptionSetting) bool {
	withheld, _ := ctx.Value(signupSubscriptionsWithheldKey{}).(bool)
	if withheld && len(items) > 0 {
		logger.LegacyPrintf("service.auth", "[Auth] signup default subscriptions withheld: user_id=%d count=%d", userID, len(items))
	}
	return withheld
}
