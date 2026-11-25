package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// AdminSetUserStatusRequest names the one field POST /users/:id/status may change.
type AdminSetUserStatusRequest struct {
	Status string `json:"status" binding:"required,oneof=active disabled"`
}

// SetStatus sets a user's status to active or disabled, and nothing else. It is the
// console's status switch (PUT /users/:id with only status) as its own catalogued
// route, so a scoped admin key can be given exactly this: the same service call, so
// an admin cannot be disabled, disabling revokes the user's MCP grants and drops the
// auth cache (every key of the user answers 401 USER_INACTIVE at once), and
// restoring brings the keys back.
// POST /api/v1/admin/users/:id/status
//
// Setting the status a user already has writes nothing and answers changed: false,
// so the call is safe to repeat.
func (h *UserHandler) SetStatus(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "Invalid user ID")
		return
	}
	var req AdminSetUserStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	middleware.SetAuditAction(c, "admin.users.status.update")

	ctx := c.Request.Context()
	user, err := h.adminService.GetUser(ctx, userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if user.Status == req.Status {
		response.Success(c, userStatusResponse(user, false))
		return
	}
	updated, err := h.adminService.UpdateUser(ctx, userID, &service.UpdateUserInput{
		Status:       req.Status,
		ActorAdminID: getAdminIDFromContext(c),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, userStatusResponse(updated, true))
}

// userStatusResponse lists its fields on purpose: users.status.write alone reads
// nothing else about the user.
func userStatusResponse(user *service.User, changed bool) gin.H {
	return gin.H{"id": user.ID, "status": user.Status, "changed": changed}
}

// FindByIdentity answers the user whose OIDC login identity is (issuer, subject),
// whatever the user's status, in the shape of GET /users/:id (a scoped key sees the
// fields its permissions allow there: always the id, profile fields with
// users.read). It is the identity lookup an OIDC login makes. No such identity, or
// a deleted owner, answers 404 USER_NOT_FOUND.
// GET /api/v1/admin/users/by-identity?issuer=&subject=
func (h *UserHandler) FindByIdentity(c *gin.Context) {
	issuer := strings.TrimSpace(c.Query("issuer"))
	subject := strings.TrimSpace(c.Query("subject"))
	if issuer == "" || subject == "" {
		response.BadRequest(c, "issuer and subject are required")
		return
	}
	lookup, ok := h.adminService.(service.AdminUserIdentityLookup)
	if !ok {
		response.Error(c, http.StatusServiceUnavailable, "User lookup by identity is unavailable")
		return
	}
	user, err := lookup.FindUserByOIDCIdentity(c.Request.Context(), issuer, subject)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, adminUserResponse(c, user))
}
