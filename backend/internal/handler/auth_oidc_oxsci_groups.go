package handler

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
)

// oidcStaffGroupPrefix marks a groups entry that names a staff admission.
const oidcStaffGroupPrefix = "staff."

// oidcParseGroups reads the userinfo "groups" claim: the person's groups at the
// identity provider, as strings. XSci's IdP writes them as
// "<admission kind>.<capability group>" (staff.basic, collaborator.model).
// It answers nil when the claim is absent or is not an array, so "no claim" and
// "an empty list" stay distinguishable; blank and non-string entries are dropped.
func oidcParseGroups(body string) []string {
	res := gjson.Get(body, "groups")
	if !res.Exists() || !res.IsArray() {
		return nil
	}
	groups := []string{}
	for _, item := range res.Array() {
		if item.Type != gjson.String {
			continue
		}
		if group := strings.TrimSpace(item.String()); group != "" {
			groups = append(groups, group)
		}
	}
	return groups
}

// oidcClaimGroups reads the groups back out of the upstream claims. Straight from the
// callback they are []string; after a pending session has round-tripped through its
// JSON column they are []any.
func oidcClaimGroups(claims map[string]any) []string {
	switch raw := claims["groups"].(type) {
	case []string:
		return raw
	case []any:
		groups := make([]string, 0, len(raw))
		for _, item := range raw {
			if group, ok := item.(string); ok {
				groups = append(groups, group)
			}
		}
		return groups
	default:
		return nil
	}
}

func oidcGroupsIncludeStaff(groups []string) bool {
	for _, group := range groups {
		if strings.HasPrefix(group, oidcStaffGroupPrefix) {
			return true
		}
	}
	return false
}

// oidcSignupContext is the context an account may be created under for this identity.
// An OIDC sign-up whose groups name no staff admission gets no default subscriptions:
// staff keep the site defaults, collaborators are given theirs explicitly by an
// operator. Missing groups count as not staff. Other providers are left alone.
func oidcSignupContext(ctx context.Context, providerType string, claims map[string]any) context.Context {
	if !strings.EqualFold(strings.TrimSpace(providerType), "oidc") || oidcGroupsIncludeStaff(oidcClaimGroups(claims)) {
		return ctx
	}
	return service.WithSignupSubscriptionsWithheld(ctx)
}
