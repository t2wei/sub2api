package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// RegisterIssuerRoutes registers the data service's narrow issuance API ([OXSCI] P135
// T13 K1) under /api/v1/issuer. The group is always registered; IssuerAuth answers
// 404 ISSUER_DISABLED while the issuer is not configured.
func RegisterIssuerRoutes(v1 *gin.RouterGroup, h *handler.Handlers, cfg config.IssuerConfig) {
	issuer := v1.Group("/issuer")
	issuer.Use(middleware.IssuerAuth(cfg))
	issuer.POST("/users", h.Issuer.CreateUser)
	issuer.GET("/users/:id", h.Issuer.GetUser)
	issuer.POST("/users/:id/keys", h.Issuer.IssueKey)
	issuer.POST("/users/:id/disable", h.Issuer.DisableUser)
	issuer.GET("/users/:id/usage", h.Issuer.Usage)
}
