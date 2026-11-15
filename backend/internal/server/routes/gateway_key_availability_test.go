package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGatewayRoutesKeyAvailabilityPathIsRegistered(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	for _, route := range router.Routes() {
		if route.Method == http.MethodGet && route.Path == "/v1/sub2api/availability" {
			return
		}
	}

	t.Fatal("GET /v1/sub2api/availability should be registered")
}

func TestGatewayRoutesKeyAvailabilityRequiresAPIKey(t *testing.T) {
	router, _, _ := newKeyBillingRouteTestRouter(config.RunModeStandard)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/sub2api/availability?model=claude-opus-5", nil))

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "application/json")
	require.NotContains(t, strings.ToLower(w.Body.String()), "<!doctype html>")
}
