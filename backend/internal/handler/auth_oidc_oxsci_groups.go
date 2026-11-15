package handler

import (
	"strings"

	"github.com/tidwall/gjson"
)

// oidcParseGroups reads the userinfo "groups" claim: the person's groups at the
// identity provider, as strings. XSci's IdP writes them as
// "<admission kind>.<capability group>" (staff.basic, collaborator.assistant-owner).
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
