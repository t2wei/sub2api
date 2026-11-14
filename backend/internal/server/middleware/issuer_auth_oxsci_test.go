//go:build unit

package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const issuerTestSecret = "0123456789abcdef0123456789abcdef-test"

func issuerTestRouter(cfg config.IssuerConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/issuer")
	g.Use(IssuerAuth(cfg))
	g.GET("/users/:id", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"reached": true}) })
	return r
}

func enabledIssuerConfig() config.IssuerConfig {
	return config.LoadIssuerConfig(func(name string) string {
		return map[string]string{
			config.IssuerSecretEnv:      issuerTestSecret,
			config.IssuerEmailDomainEnv: "interview.xsci.invalid",
			config.IssuerGroupIDsEnv:    "41",
		}[name]
	})
}

func issuerCall(t *testing.T, r *gin.Engine, auth string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issuer/users/1", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Code, body
}

func TestIssuerAuth(t *testing.T) {
	r := issuerTestRouter(enabledIssuerConfig())

	code, body := issuerCall(t, r, "")
	require.Equal(t, http.StatusUnauthorized, code)
	require.Equal(t, "ISSUER_UNAUTHORIZED", body["reason"])

	code, body = issuerCall(t, r, "Bearer "+issuerTestSecret+"x")
	require.Equal(t, http.StatusUnauthorized, code)
	require.Equal(t, "ISSUER_UNAUTHORIZED", body["reason"])

	code, _ = issuerCall(t, r, "Basic "+issuerTestSecret)
	require.Equal(t, http.StatusUnauthorized, code)

	code, body = issuerCall(t, r, "Bearer "+issuerTestSecret)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, true, body["reached"])

	code, _ = issuerCall(t, r, "bearer "+issuerTestSecret)
	require.Equal(t, http.StatusOK, code)
}

func TestIssuerAuthDisabledAnswers404(t *testing.T) {
	for name, cfg := range map[string]config.IssuerConfig{
		"not configured": config.LoadIssuerConfig(func(string) string { return "" }),
		"short secret": config.LoadIssuerConfig(func(name string) string {
			return map[string]string{
				config.IssuerSecretEnv:      "short",
				config.IssuerEmailDomainEnv: "interview.xsci.invalid",
				config.IssuerGroupIDsEnv:    "41",
			}[name]
		}),
	} {
		t.Run(name, func(t *testing.T) {
			r := issuerTestRouter(cfg)
			// even the would-be secret does not get in
			code, body := issuerCall(t, r, "Bearer short")
			require.Equal(t, http.StatusNotFound, code)
			require.Equal(t, "ISSUER_DISABLED", body["reason"])
			code, _ = issuerCall(t, r, "")
			require.Equal(t, http.StatusNotFound, code)
		})
	}
}
