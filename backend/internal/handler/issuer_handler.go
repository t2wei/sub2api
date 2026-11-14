package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// IssuerHandler serves /api/v1/issuer/* ([OXSCI] P135 T13 K1): the data service's
// narrow issuance API. Authentication and the enabled check are the route group's
// middleware (middleware.IssuerAuth); every bound (reserved email domain, configured
// groups) is enforced in service.IssuerService.
type IssuerHandler struct {
	issuer *service.IssuerService
	now    func() time.Time
}

// NewIssuerHandler creates the issuer handler.
func NewIssuerHandler(issuer *service.IssuerService) *IssuerHandler {
	return &IssuerHandler{issuer: issuer, now: time.Now}
}

func issuerBadRequest(c *gin.Context, message string) {
	response.ErrorFrom(c, infraerrors.New(http.StatusUnprocessableEntity, service.IssuerReasonInvalid, message))
}

func (h *IssuerHandler) userID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorFrom(c, infraerrors.NotFound(service.IssuerReasonUserNotFound, "no such user"))
		return 0, false
	}
	return id, true
}

type issuerCreateUserRequest struct {
	Email         string   `json:"email"`
	Balance       *float64 `json:"balance"`
	AllowedGroups []int64  `json:"allowed_groups"`
	Notes         string   `json:"notes"`
}

type issuerUserResponse struct {
	service.IssuerUser
	Created bool `json:"created"`
}

// CreateUser handles POST /api/v1/issuer/users: 201 when created, 200 with
// created=false when the reserved-domain user already exists (untouched).
func (h *IssuerHandler) CreateUser(c *gin.Context) {
	var req issuerCreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		issuerBadRequest(c, "invalid request body: "+err.Error())
		return
	}
	if req.Balance == nil {
		issuerBadRequest(c, "balance is required")
		return
	}
	user, created, err := h.issuer.CreateUser(c.Request.Context(), service.IssuerCreateUserInput{
		Email:         req.Email,
		Balance:       *req.Balance,
		AllowedGroups: req.AllowedGroups,
		Notes:         req.Notes,
	})
	if response.ErrorFrom(c, err) {
		return
	}
	out := issuerUserResponse{IssuerUser: user, Created: created}
	if created {
		response.Created(c, out)
		return
	}
	response.Success(c, out)
}

// GetUser handles GET /api/v1/issuer/users/:id.
func (h *IssuerHandler) GetUser(c *gin.Context) {
	id, ok := h.userID(c)
	if !ok {
		return
	}
	user, err := h.issuer.GetUser(c.Request.Context(), id)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, user)
}

type issuerIssueKeyRequest struct {
	Name      string `json:"name"`
	GroupID   int64  `json:"group_id"`
	ExpiresAt string `json:"expires_at"`
}

// IssueKey handles POST /api/v1/issuer/users/:id/keys (201). The key value is in this
// answer only; it is never listed again.
func (h *IssuerHandler) IssueKey(c *gin.Context) {
	id, ok := h.userID(c)
	if !ok {
		return
	}
	var req issuerIssueKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		issuerBadRequest(c, "invalid request body: "+err.Error())
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(req.ExpiresAt))
	if err != nil {
		issuerBadRequest(c, "expires_at must be an RFC 3339 time")
		return
	}
	key, err := h.issuer.IssueKey(c.Request.Context(), id, req.Name, req.GroupID, expiresAt, h.now())
	if response.ErrorFrom(c, err) {
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Created(c, key)
}

// DisableUser handles POST /api/v1/issuer/users/:id/disable (idempotent).
func (h *IssuerHandler) DisableUser(c *gin.Context) {
	id, ok := h.userID(c)
	if !ok {
		return
	}
	user, err := h.issuer.DisableUser(c.Request.Context(), id)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, gin.H{"id": user.ID, "status": user.Status})
}

// Usage handles GET /api/v1/issuer/users/:id/usage?page&page_size[&start_time&end_time].
func (h *IssuerHandler) Usage(c *gin.Context) {
	id, ok := h.userID(c)
	if !ok {
		return
	}
	q := service.IssuerUsageQuery{Page: 1, PageSize: 500}
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			issuerBadRequest(c, "page must be an integer")
			return
		}
		q.Page = v
	}
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			issuerBadRequest(c, "page_size must be an integer")
			return
		}
		q.PageSize = v
	}
	for name, dst := range map[string]**time.Time{"start_time": &q.StartTime, "end_time": &q.EndTime} {
		raw := strings.TrimSpace(c.Query(name))
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			issuerBadRequest(c, name+" must be an RFC 3339 time")
			return
		}
		*dst = &t
	}
	rows, result, err := h.issuer.Usage(c.Request.Context(), id, q)
	if response.ErrorFrom(c, err) {
		return
	}
	total := int64(len(rows))
	if result != nil {
		total = result.Total
	}
	response.Paginated(c, rows, total, q.Page, q.PageSize)
}
