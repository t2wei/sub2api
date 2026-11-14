//go:build unit

package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

const issuerHandlerSecret = "0123456789abcdef0123456789abcdef-handler"

// The HTTP face of the issuer over a real (sqlite) user table: the route group's gate,
// the request shapes and the status codes DS's client relies on. Bounds and key / usage
// behaviour are covered at the service level (issuer_service_oxsci_test.go).
func newIssuerHTTPFixture(t *testing.T) (*gin.Engine, *dbent.Client) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	t.Cleanup(func() { _ = client.Close() })

	cfg := &config.Config{Issuer: config.LoadIssuerConfig(func(name string) string {
		return map[string]string{
			config.IssuerSecretEnv:      issuerHandlerSecret,
			config.IssuerEmailDomainEnv: "interview.xsci.invalid",
			config.IssuerGroupIDsEnv:    "41,42",
		}[name]
	})}
	require.True(t, cfg.Issuer.Enabled())
	svc := service.NewIssuerService(cfg, &oauthPendingFlowUserRepo{client: client}, nil, nil, nil, nil)
	h := NewIssuerHandler(svc)
	h.now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }

	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/issuer")
	g.Use(middleware.IssuerAuth(cfg.Issuer))
	g.POST("/users", h.CreateUser)
	g.GET("/users/:id", h.GetUser)
	g.POST("/users/:id/keys", h.IssueKey)
	return r, client
}

func issuerDo(t *testing.T, r *gin.Engine, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+issuerHandlerSecret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return rec.Code, out
}

func TestIssuerHandlerCreateUser201Then200(t *testing.T) {
	r, _ := newIssuerHTTPFixture(t)
	body := map[string]any{"email": "p201.prod@interview.xsci.invalid", "balance": 30, "allowed_groups": []int64{41}, "notes": "P201"}

	code, out := issuerDo(t, r, http.MethodPost, "/api/v1/issuer/users", body)
	require.Equal(t, http.StatusCreated, code, out)
	data := issuerData(t, out)
	require.Equal(t, true, data["created"])
	require.Equal(t, "p201.prod@interview.xsci.invalid", data["email"])
	require.Equal(t, 30.0, data["balance"])
	require.Equal(t, "active", data["status"])
	id := data["id"]

	code, out = issuerDo(t, r, http.MethodPost, "/api/v1/issuer/users", body)
	require.Equal(t, http.StatusOK, code, out)
	data = issuerData(t, out)
	require.Equal(t, false, data["created"])
	require.Equal(t, id, data["id"])
}

func TestIssuerHandlerRefusals(t *testing.T) {
	r, client := newIssuerHTTPFixture(t)
	staff, err := client.User.Create().SetEmail("staff@xsci.com").SetPasswordHash("x").SetRole(service.RoleUser).SetStatus(service.StatusActive).Save(t.Context())
	require.NoError(t, err)

	code, out := issuerDo(t, r, http.MethodPost, "/api/v1/issuer/users", map[string]any{"email": "someone@xsci.com", "balance": 5, "allowed_groups": []int64{41}})
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "ISSUER_FORBIDDEN", out["reason"])

	code, out = issuerDo(t, r, http.MethodPost, "/api/v1/issuer/users", map[string]any{"email": "p1@interview.xsci.invalid", "balance": 5, "allowed_groups": []int64{7}})
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "ISSUER_FORBIDDEN", out["reason"])

	code, out = issuerDo(t, r, http.MethodPost, "/api/v1/issuer/users", map[string]any{"email": "p1@interview.xsci.invalid", "allowed_groups": []int64{41}})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, "ISSUER_INVALID", out["reason"])

	code, out = issuerDo(t, r, http.MethodGet, "/api/v1/issuer/users/"+strconv.FormatInt(staff.ID, 10), nil)
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "ISSUER_FORBIDDEN", out["reason"])
	require.Nil(t, out["data"], "nothing about a non-reserved user is answered")

	code, out = issuerDo(t, r, http.MethodGet, "/api/v1/issuer/users/999999", nil)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, "ISSUER_USER_NOT_FOUND", out["reason"])

	code, out = issuerDo(t, r, http.MethodPost, "/api/v1/issuer/users/1/keys", map[string]any{"name": "k", "group_id": 41, "expires_at": "2026-11-15T12:00:01Z"})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, "ISSUER_INVALID", out["reason"])

	code, out = issuerDo(t, r, http.MethodPost, "/api/v1/issuer/users/1/keys", map[string]any{"name": "k", "group_id": 41, "expires_at": "tomorrow"})
	require.Equal(t, http.StatusUnprocessableEntity, code)
	require.Equal(t, "ISSUER_INVALID", out["reason"])
}

func issuerData(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	data, ok := out["data"].(map[string]any)
	require.True(t, ok, out)
	return data
}
