package admin

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// permissionUsersAPIKeysCreate lets a scoped admin key create keys for users, bound to
// the groups its rule selects.
const permissionUsersAPIKeysCreate = "users.api_keys.create"

// userAPIKeyCreator is APIKeyService.Create: the same path a user's own "create key"
// takes, so group binding, subscription and per-user limits are checked identically.
type userAPIKeyCreator interface {
	Create(ctx context.Context, userID int64, req service.CreateAPIKeyRequest) (*service.APIKey, error)
}

// AdminCreateUserAPIKeyRequest is the user-side create request without custom_key:
// the key value is always generated, so it never appears in a request (or its audit
// row) and is answered exactly once.
type AdminCreateUserAPIKeyRequest struct {
	Name          string   `json:"name" binding:"required"`
	GroupID       *int64   `json:"group_id"`
	IPWhitelist   []string `json:"ip_whitelist"`
	IPBlacklist   []string `json:"ip_blacklist"`
	Quota         *float64 `json:"quota"`
	ExpiresInDays *int     `json:"expires_in_days"`
	RateLimit5h   *float64 `json:"rate_limit_5h"`
	RateLimit1d   *float64 `json:"rate_limit_1d"`
	RateLimit7d   *float64 `json:"rate_limit_7d"`
}

func (r AdminCreateUserAPIKeyRequest) validate() error {
	for _, limit := range []*float64{r.Quota, r.RateLimit5h, r.RateLimit1d, r.RateLimit7d} {
		if limit != nil && (math.IsNaN(*limit) || math.IsInf(*limit, 0) || *limit < 0) {
			return errors.New("numeric limits must be finite and non-negative")
		}
	}
	if r.ExpiresInDays != nil && *r.ExpiresInDays <= 0 {
		return errors.New("expires_in_days must be greater than zero")
	}
	return nil
}

func (r AdminCreateUserAPIKeyRequest) serviceRequest() service.CreateAPIKeyRequest {
	out := service.CreateAPIKeyRequest{
		Name:          r.Name,
		GroupID:       r.GroupID,
		IPWhitelist:   r.IPWhitelist,
		IPBlacklist:   r.IPBlacklist,
		ExpiresInDays: r.ExpiresInDays,
	}
	if r.Quota != nil {
		out.Quota = *r.Quota
	}
	if r.RateLimit5h != nil {
		out.RateLimit5h = *r.RateLimit5h
	}
	if r.RateLimit1d != nil {
		out.RateLimit1d = *r.RateLimit1d
	}
	if r.RateLimit7d != nil {
		out.RateLimit7d = *r.RateLimit7d
	}
	return out
}

// createdUserAPIKeyResponse lists its fields on purpose (scoped keys get no more than
// this, whatever the full DTO grows). It is the only answer that carries the key value.
func createdUserAPIKeyResponse(key *service.APIKey) gin.H {
	return gin.H{
		"id":            key.ID,
		"user_id":       key.UserID,
		"key":           key.Key,
		"name":          key.Name,
		"group_id":      key.GroupID,
		"status":        key.Status,
		"ip_whitelist":  key.IPWhitelist,
		"ip_blacklist":  key.IPBlacklist,
		"quota":         key.Quota,
		"rate_limit_5h": key.RateLimit5h,
		"rate_limit_1d": key.RateLimit1d,
		"rate_limit_7d": key.RateLimit7d,
		"expires_at":    key.ExpiresAt,
		"created_at":    key.CreatedAt,
	}
}

// CreateForUser creates an API key owned by the user in the path, through the same
// service method the user's own "create key" uses.
// POST /api/v1/admin/users/:id/api-keys
//
// A scoped admin key needs users.api_keys.create on a rule that selects the key's
// group; it cannot create an ungrouped key, and a group outside its rules answers
// 403 whether or not it exists. Subscription groups still need an active
// subscription first (the service's GROUP_NOT_ALLOWED).
func (h *AdminAPIKeyHandler) CreateForUser(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "Invalid user ID")
		return
	}
	var req AdminCreateUserAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := req.validate(); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if principal := adminaccess.RestrictedFromContext(c.Request.Context()); principal != nil {
		allowed, err := h.scopedKeyMayCreateIn(c.Request.Context(), principal.Policy, req.GroupID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if !allowed {
			response.Error(c, http.StatusForbidden, "此分组未授权给受限管理密钥创建用户密钥")
			return
		}
	}

	payload := struct {
		UserID  int64                        `json:"user_id"`
		Request AdminCreateUserAPIKeyRequest `json:"request"`
	}{userID, req}
	executeAdminIdempotentJSON(c, "admin.users.api_keys.create", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		key, err := h.apiKeyCreator.Create(ctx, userID, req.serviceRequest())
		if err != nil {
			return nil, err
		}
		return createdUserAPIKeyResponse(key), nil
	})
}

// scopedKeyMayCreateIn answers whether a scoped key's rules select this group. An
// unknown group is simply "not selected" (a scoped key learns nothing about groups
// outside its rules); any other failure to read the group is an error.
func (h *AdminAPIKeyHandler) scopedKeyMayCreateIn(ctx context.Context, policy adminaccess.Policy, groupID *int64) (bool, error) {
	if groupID == nil || *groupID <= 0 {
		return false, nil
	}
	group, err := h.adminService.GetGroup(ctx, *groupID)
	if errors.Is(err, service.ErrGroupNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return policy.AllowsGroupConfig(permissionUsersAPIKeysCreate, group.ID, group.Platform), nil
}
