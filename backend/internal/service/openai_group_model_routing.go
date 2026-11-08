package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type openAIGroupModelRoutingContextKey struct{}

// The route is resolved once before selecting accounts. A nil account set means
// routing is disabled or no rule matched; a matched rule is a strict allowlist.
type openAIGroupModelRouting struct {
	groupID    int64
	platform   string
	model      string
	accountIDs map[int64]struct{}
}

func (s *OpenAIGatewayService) withOpenAIGroupModelRouting(ctx context.Context, groupID *int64, platform, model string) (context.Context, error) {
	platform = NormalizeOpenAICompatiblePlatform(platform)
	if route, ok := ctx.Value(openAIGroupModelRoutingContextKey{}).(*openAIGroupModelRouting); ok && route != nil &&
		route.groupID == derefGroupID(groupID) && route.platform == platform && route.model == model {
		return ctx, nil
	}
	route := &openAIGroupModelRouting{groupID: derefGroupID(groupID), platform: platform, model: model}
	// Always replace a route from another scheduling group/model, including when
	// the new request has no route. Composite routing must not inherit its parent.
	ctx = context.WithValue(ctx, openAIGroupModelRoutingContextKey{}, route)
	if platform != PlatformOpenAI || groupID == nil || *groupID <= 0 || model == "" {
		return ctx, nil
	}

	var group *Group
	if authGroup, ok := ctx.Value(ctxkey.Group).(*Group); ok && IsGroupContextValid(authGroup) && authGroup.ID == *groupID {
		group = authGroup
	} else if s != nil && s.schedulerSnapshot != nil && s.schedulerSnapshot.groupRepo != nil {
		loaded, err := s.schedulerSnapshot.GetGroupByIDLite(ctx, *groupID)
		if err != nil {
			return ctx, fmt.Errorf("%w: model routing group lookup failed: %w", ErrNoAvailableAccounts, err)
		}
		if loaded == nil || loaded.ID != *groupID {
			return ctx, fmt.Errorf("%w: model routing group lookup returned no matching group", ErrNoAvailableAccounts)
		}
		group = loaded
	}
	// Internal callers without a group repository retain their unconfigured
	// behavior. Production uses the authenticated group or the snapshot service.
	if group == nil {
		return ctx, nil
	}
	if ids, matched := openAIGroupRoutingAccountIDs(group, model); matched {
		route.accountIDs = make(map[int64]struct{}, len(ids))
		for _, id := range ids {
			route.accountIDs[id] = struct{}{}
		}
	}
	return ctx, nil
}

// OpenAI routes are strict: an explicitly empty rule denies all accounts.
// Exact names win, then the longest matching prefix. Keep the shared group's
// priority-routing semantics unchanged for other platform schedulers.
func openAIGroupRoutingAccountIDs(group *Group, model string) ([]int64, bool) {
	if !group.ModelRoutingEnabled {
		return nil, false
	}
	if ids, exact := group.ModelRouting[model]; exact {
		return ids, true
	}
	var selected []int64
	longest := -1
	for pattern, ids := range group.ModelRouting {
		if len(pattern) > longest && matchModelPattern(pattern, model) {
			selected, longest = ids, len(pattern)
		}
	}
	return selected, longest >= 0
}

func openAIGroupModelRoutingAllows(ctx context.Context, account *Account) bool {
	route, _ := ctx.Value(openAIGroupModelRoutingContextKey{}).(*openAIGroupModelRouting)
	if route == nil || route.accountIDs == nil {
		return true
	}
	if account == nil {
		return false
	}
	_, allowed := route.accountIDs[account.ID]
	return allowed
}

// ValidateAccountModelRouting rechecks a fixed account when a later WebSocket
// turn changes the model without running account selection again.
func (s *OpenAIGatewayService) ValidateAccountModelRouting(ctx context.Context, groupID *int64, platform, model string, account *Account) error {
	if account == nil {
		return fmt.Errorf("%w: account_nil", ErrNoAvailableAccounts)
	}
	ctx, err := s.withOpenAIGroupModelRouting(ctx, groupID, platform, model)
	if err != nil {
		return err
	}
	if !openAIGroupModelRoutingAllows(ctx, account) {
		return fmt.Errorf("%w: model_routing_mismatch", ErrNoAvailableAccounts)
	}
	return nil
}

// ValidateModelRoutingCandidates rejects conflicting model fields when one is
// routed. Different JSON decoders must not select an unrestricted account while
// the upstream sees a restricted model in the same passthrough request.
func (s *OpenAIGatewayService) ValidateModelRoutingCandidates(ctx context.Context, groupID *int64, platform string, models []string) error {
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			seen[model] = struct{}{}
		}
	}
	if len(seen) < 2 {
		return nil
	}
	for model := range seen {
		routeCtx, err := s.withOpenAIGroupModelRouting(ctx, groupID, platform, model)
		if err != nil {
			return err
		}
		route, _ := routeCtx.Value(openAIGroupModelRoutingContextKey{}).(*openAIGroupModelRouting)
		if route != nil && route.accountIDs != nil {
			return fmt.Errorf("%w: ambiguous model routing", ErrNoAvailableAccounts)
		}
	}
	return nil
}
