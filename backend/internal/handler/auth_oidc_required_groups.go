package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// oidcLoginGroupsAllowed applies the site's "required groups" setting
// (oidc_connect_required_groups) to the IdP's groups claim. It answers the configured
// requirement alongside the verdict so the caller can log both. An empty requirement
// lets everyone through; a failure to read it is returned as err and the caller
// refuses the login (fail closed): an unreadable gate must not open.
func (h *AuthHandler) oidcLoginGroupsAllowed(ctx context.Context, groups []string) (allowed bool, required []string, err error) {
	if h == nil || h.settingSvc == nil {
		return true, nil, nil
	}
	required, err = h.settingSvc.GetOIDCRequiredGroups(ctx)
	if err != nil {
		return false, nil, err
	}
	return service.OIDCGroupsSatisfy(required, groups), required, nil
}
