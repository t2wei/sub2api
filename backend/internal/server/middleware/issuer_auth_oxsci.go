package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// IssuerAuth guards /api/v1/issuer/* ([OXSCI] P135 T13 K1). It is its own gate, not
// the admin key nor a user JWT: the issuer API is off (404 ISSUER_DISABLED) unless
// all three SUB2API_ISSUER_* variables are set and valid, and a request must carry
// `Authorization: Bearer <SUB2API_ISSUER_SECRET>` (401 ISSUER_UNAUTHORIZED otherwise).
// The comparison is constant-time over SHA-256 digests, so neither the secret's
// content nor its length leaks through timing.
func IssuerAuth(cfg config.IssuerConfig) gin.HandlerFunc {
	want := sha256.Sum256([]byte(cfg.Secret))
	return func(c *gin.Context) {
		if !cfg.Enabled() {
			response.ErrorFrom(c, infraerrors.NotFound(service.IssuerReasonDisabled, "the issuer API is not enabled on this server"))
			c.Abort()
			return
		}
		header := strings.TrimSpace(c.GetHeader("Authorization"))
		const prefix = "bearer "
		if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
			issuerUnauthorized(c)
			return
		}
		got := sha256.Sum256([]byte(strings.TrimSpace(header[len(prefix):])))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			issuerUnauthorized(c)
			return
		}
		c.Next()
	}
}

func issuerUnauthorized(c *gin.Context) {
	response.ErrorFrom(c, infraerrors.New(http.StatusUnauthorized, service.IssuerReasonUnauthorized, "a valid issuer secret is required"))
	c.Abort()
}
