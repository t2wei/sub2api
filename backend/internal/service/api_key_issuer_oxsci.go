package service

import (
	"context"
	"fmt"
	"html"
	"time"
)

// IssueExpiringKey creates an API key for userID bound to one group and expiring at
// an exact instant ([OXSCI] the issuer path, P135 T13 K1).
//
// It is the self-service Create minus what does not apply to it: no custom key, no IP
// lists, no quota, and no per-user create limits (api_key_create.*). The caller (the
// data service, through IssuerService) has already bounded the user and the group, and
// issues at most one key per board session per few minutes, gated by a live lease and
// capped by the account's balance; a self-service rate limit would turn an interview's
// later keys into failures. The group check itself is the same one Create applies.
func (s *APIKeyService) IssueExpiringKey(ctx context.Context, userID, groupID int64, name string, expiresAt time.Time) (*APIKey, error) {
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	group, err := s.groupRepo.GetByID(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("get group: %w", err)
	}
	if !s.canUserBindGroup(ctx, user, group) {
		return nil, ErrGroupNotAllowed
	}
	key, err := s.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	expires := expiresAt
	gid := groupID
	apiKey := &APIKey{
		UserID:    userID,
		Key:       key,
		Name:      html.EscapeString(name),
		GroupID:   &gid,
		Status:    StatusActive,
		ExpiresAt: &expires,
	}
	if err := s.apiKeyRepo.Create(ctx, apiKey); err != nil {
		return nil, fmt.Errorf("create api key: %w", err)
	}
	s.InvalidateAuthCacheByKey(ctx, apiKey.Key)
	s.compileAPIKeyIPRules(apiKey)
	return apiKey, nil
}
