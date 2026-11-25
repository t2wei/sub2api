package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// userAccountStub answers GetUser, UpdateUser and the identity lookup; every other
// AdminService method is unused here.
type userAccountStub struct {
	service.AdminService
	users     map[int64]*service.User
	updateErr error
	updates   []userAccountUpdate
	lookups   [][2]string
	byIssuer  map[[2]string]int64
}

type userAccountUpdate struct {
	id    int64
	input service.UpdateUserInput
}

func (s *userAccountStub) GetUser(_ context.Context, id int64) (*service.User, error) {
	if u, ok := s.users[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, service.ErrUserNotFound
}

func (s *userAccountStub) UpdateUser(_ context.Context, id int64, input *service.UpdateUserInput) (*service.User, error) {
	s.updates = append(s.updates, userAccountUpdate{id: id, input: *input})
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	u := s.users[id]
	u.Status = input.Status
	cp := *u
	return &cp, nil
}

func (s *userAccountStub) FindUserByOIDCIdentity(ctx context.Context, issuer, subject string) (*service.User, error) {
	s.lookups = append(s.lookups, [2]string{issuer, subject})
	id, ok := s.byIssuer[[2]string{issuer, subject}]
	if !ok {
		return nil, service.ErrUserNotFound
	}
	return s.GetUser(ctx, id)
}

func newUserAccountStub() *userAccountStub {
	return &userAccountStub{
		users: map[int64]*service.User{
			42: {ID: 42, Email: "staff@xsci.com", Username: "staff", Role: service.RoleUser, Status: service.StatusActive, Balance: 12.5},
			43: {ID: 43, Email: "former@xsci.com", Role: service.RoleUser, Status: service.StatusDisabled},
		},
		byIssuer: map[[2]string]int64{
			{"https://data.xsci.ai", "user-7"}: 42,
			{"https://data.xsci.ai", "user-8"}: 43,
		},
	}
}

func newUserAccountRouter(h *UserHandler, principal *adminaccess.Principal, audit *string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9})
		if principal != nil {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), principal))
		}
		c.Next()
		if audit != nil {
			*audit = c.GetString("audit_action")
		}
	})
	r.GET("/api/v1/admin/users/by-identity", h.FindByIdentity)
	r.GET("/api/v1/admin/users/:id", h.GetByID)
	r.POST("/api/v1/admin/users/:id/status", h.SetStatus)
	return r
}

func serve(r *gin.Engine, method, target, body string) (*httptest.ResponseRecorder, map[string]any) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	return rec, envelope.Data
}

func TestSetStatusDisablesThroughTheConsoleStatusSwitchAndNothingElse(t *testing.T) {
	stub := newUserAccountStub()
	var audit string
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, &audit)

	// Fields beyond status are not part of the request and never reach the service.
	rec, data := serve(r, http.MethodPost, "/api/v1/admin/users/42/status", `{"status":"disabled","role":"admin","concurrency":99,"balance":1000}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, map[string]any{"id": float64(42), "status": "disabled", "changed": true}, data)
	require.Len(t, stub.updates, 1)
	require.Equal(t, int64(42), stub.updates[0].id)
	require.Equal(t, service.UpdateUserInput{Status: service.StatusDisabled, ActorAdminID: 9}, stub.updates[0].input)
	require.Equal(t, "admin.users.status.update", audit)
}

func TestSetStatusRestoresADisabledUser(t *testing.T) {
	stub := newUserAccountStub()
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, nil)

	rec, data := serve(r, http.MethodPost, "/api/v1/admin/users/43/status", `{"status":"active"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, map[string]any{"id": float64(43), "status": "active", "changed": true}, data)
	require.Equal(t, service.UpdateUserInput{Status: service.StatusActive, ActorAdminID: 9}, stub.updates[0].input)
}

func TestSetStatusToTheCurrentStatusWritesNothing(t *testing.T) {
	stub := newUserAccountStub()
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, nil)

	for _, tc := range []struct {
		id, body, status string
	}{
		{"42", `{"status":"active"}`, "active"},
		{"43", `{"status":"disabled"}`, "disabled"},
	} {
		rec, data := serve(r, http.MethodPost, "/api/v1/admin/users/"+tc.id+"/status", tc.body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, tc.status, data["status"])
		require.Equal(t, false, data["changed"])
	}
	require.Empty(t, stub.updates)
}

func TestSetStatusRejectsAnythingButActiveOrDisabled(t *testing.T) {
	stub := newUserAccountStub()
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, nil)

	for _, tc := range []struct{ id, body string }{
		{"42", `{}`},
		{"42", `{"status":""}`},
		{"42", `{"status":"deleted"}`},
		{"42", `{"status":"DISABLED"}`},
		{"42", `not json`},
		{"0", `{"status":"disabled"}`},
		{"x", `{"status":"disabled"}`},
	} {
		rec, _ := serve(r, http.MethodPost, "/api/v1/admin/users/"+tc.id+"/status", tc.body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s %s: %s", tc.id, tc.body, rec.Body.String())
	}
	require.Empty(t, stub.updates)
}

func TestSetStatusAnswersTheServiceRefusal(t *testing.T) {
	stub := newUserAccountStub()
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, nil)

	rec, _ := serve(r, http.MethodPost, "/api/v1/admin/users/404/status", `{"status":"disabled"}`)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Empty(t, stub.updates)

	stub.updateErr = infraerrors.BadRequest("ADMIN_DISABLE_FORBIDDEN", "cannot disable admin user; change the role to user first")
	rec, _ = serve(r, http.MethodPost, "/api/v1/admin/users/42/status", `{"status":"disabled"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "ADMIN_DISABLE_FORBIDDEN")
}

func TestFindByIdentityAnswersTheUserWhateverItsStatus(t *testing.T) {
	stub := newUserAccountStub()
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, nil)

	for subject, want := range map[string]struct {
		id     float64
		status string
	}{"user-7": {42, "active"}, "user-8": {43, "disabled"}} {
		q := url.Values{"issuer": {"https://data.xsci.ai"}, "subject": {subject}}
		rec, data := serve(r, http.MethodGet, "/api/v1/admin/users/by-identity?"+q.Encode(), "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, want.id, data["id"])
		require.Equal(t, want.status, data["status"])
	}
	// The static segment is routed to the lookup, not read as a user id.
	require.Len(t, stub.lookups, 2)
}

func TestFindByIdentityPassesTheKeyAsGiven(t *testing.T) {
	stub := newUserAccountStub()
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, nil)

	q := url.Values{"issuer": {"  https://data.xsci.ai "}, "subject": {" user-7"}}
	rec, _ := serve(r, http.MethodGet, "/api/v1/admin/users/by-identity?"+q.Encode(), "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, [][2]string{{"https://data.xsci.ai", "user-7"}}, stub.lookups)
}

func TestFindByIdentityNeedsBothHalvesOfTheKey(t *testing.T) {
	stub := newUserAccountStub()
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, nil)

	for _, q := range []string{"", "issuer=https%3A%2F%2Fdata.xsci.ai", "subject=user-7", "issuer=+&subject=user-7"} {
		rec, _ := serve(r, http.MethodGet, "/api/v1/admin/users/by-identity?"+q, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "%q: %s", q, rec.Body.String())
	}
	require.Empty(t, stub.lookups)
}

func TestFindByIdentityAnswers404WhenNoUserHasTheIdentity(t *testing.T) {
	stub := newUserAccountStub()
	r := newUserAccountRouter(&UserHandler{adminService: stub}, nil, nil)

	q := url.Values{"issuer": {"https://data.xsci.ai"}, "subject": {"nobody"}}
	rec, _ := serve(r, http.MethodGet, "/api/v1/admin/users/by-identity?"+q.Encode(), "")

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "USER_NOT_FOUND")
}

// A scoped key sees what GET /users/:id would show it: the id always, the profile
// fields only with users.read, the balance only with users.balance.read.
func TestFindByIdentityTrimsTheAnswerLikeGetUser(t *testing.T) {
	q := url.Values{"issuer": {"https://data.xsci.ai"}, "subject": {"user-7"}}
	for _, tc := range []struct {
		name        string
		permissions []string
		want        map[string]any
	}{
		{"identity lookup alone", []string{"users.identity.read"}, map[string]any{"id": float64(42)}},
		{"with users.read", []string{"users.identity.read", "users.read"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			principal := &adminaccess.Principal{KeyID: "ds", Policy: adminaccess.Policy{GlobalPermissions: tc.permissions}}
			r := newUserAccountRouter(&UserHandler{adminService: newUserAccountStub()}, principal, nil)

			rec, data := serve(r, http.MethodGet, "/api/v1/admin/users/by-identity?"+q.Encode(), "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			_, byID := serve(r, http.MethodGet, "/api/v1/admin/users/42", "")
			require.Equal(t, byID, data)
			if tc.want != nil {
				require.Equal(t, tc.want, data)
				return
			}
			require.Equal(t, "staff@xsci.com", data["email"])
			require.Equal(t, "active", data["status"])
			require.NotContains(t, data, "balance")
		})
	}
}

func TestFindByIdentityWithoutTheLookupServiceIs503(t *testing.T) {
	r := newUserAccountRouter(&UserHandler{adminService: &groupLookupStub{}}, nil, nil)

	q := url.Values{"issuer": {"https://data.xsci.ai"}, "subject": {"user-7"}}
	rec, _ := serve(r, http.MethodGet, "/api/v1/admin/users/by-identity?"+q.Encode(), "")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
}
