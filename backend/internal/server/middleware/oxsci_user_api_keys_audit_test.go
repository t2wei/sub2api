package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Creating a key for a user is audited as its own action, and the audit row never
// holds a key value: the request has none (the key is generated) and the response,
// which does, is not recorded.
func TestAdminCreateUserAPIKeyIsAuditedWithoutTheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 77})
		c.Set(string(ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	router.POST("/api/v1/admin/users/:id/api-keys", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": 501, "key": "sk-audit-canary-value"}})
	})

	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/42/api-keys", bytes.NewBufferString(`{"name":"interview","group_id":16,"custom_key":"sk-audit-canary-custom"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), request)
	auditService.Stop()

	repository.mu.Lock()
	logs := append([]*service.AuditLog(nil), repository.logs...)
	repository.mu.Unlock()
	require.Len(t, logs, 1)
	entry := logs[0]
	require.Equal(t, "admin.users.api_keys.create", entry.Action)
	require.Equal(t, "/api/v1/admin/users/:id/api-keys", entry.Path)
	require.Contains(t, entry.RequestBody, `"group_id":16`)
	require.NotContains(t, entry.RequestBody, "sk-audit-canary")
	require.NotContains(t, entry.Extra, "key")
}
