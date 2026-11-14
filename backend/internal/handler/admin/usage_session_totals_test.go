package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type sessionTotalsRepoCapture struct {
	service.UsageLogRepository
	query *usagestats.SessionTotalsQuery
	page  *usagestats.SessionTotalsPage
}

func (s *sessionTotalsRepoCapture) GetSessionTotals(ctx context.Context, q usagestats.SessionTotalsQuery) (*usagestats.SessionTotalsPage, error) {
	s.query = &q
	if s.page != nil {
		return s.page, nil
	}
	return &usagestats.SessionTotalsPage{Items: []usagestats.SessionTotalsRow{}}, nil
}

func newSessionTotalsRouter(repo *sessionTotalsRepoCapture, principal *adminaccess.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewUsageHandler(service.NewUsageService(repo, nil, nil, nil), nil, nil, nil)
	router := gin.New()
	if principal != nil {
		router.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), principal))
			c.Next()
		})
	}
	router.GET("/admin/usage/session-totals", handler.SessionTotals)
	return router
}

type sessionTotalsBody struct {
	Code int `json:"code"`
	Data struct {
		Items      []usagestats.SessionTotalsRow `json:"items"`
		NextCursor *string                       `json:"next_cursor"`
		Timezone   string                        `json:"timezone"`
		Start      time.Time                     `json:"start"`
		End        time.Time                     `json:"end"`
	} `json:"data"`
}

func getSessionTotals(t *testing.T, router *gin.Engine, query string) (*httptest.ResponseRecorder, sessionTotalsBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/usage/session-totals?"+query, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var body sessionTotalsBody
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return rec, body
}

func TestAdminUsageSessionTotalsRejectsBadParams(t *testing.T) {
	ok := "start_date=2026-03-09&end_date=2026-03-10&timezone=Asia/Shanghai"
	for name, query := range map[string]string{
		"no timezone":       "start_date=2026-03-09&end_date=2026-03-10",
		"local timezone":    "start_date=2026-03-09&end_date=2026-03-10&timezone=Local",
		"unknown timezone":  "start_date=2026-03-09&end_date=2026-03-10&timezone=Mars/Base",
		"no start":          "end_date=2026-03-10&timezone=UTC",
		"no end":            "start_date=2026-03-10&timezone=UTC",
		"bad date":          "start_date=2026/03/09&end_date=2026-03-10&timezone=UTC",
		"end before start":  "start_date=2026-03-10&end_date=2026-03-09&timezone=UTC",
		"range too long":    "start_date=2026-01-01&end_date=2026-04-30&timezone=UTC",
		"bad user":          ok + "&user_id=abc",
		"zero user":         ok + "&user_id=0",
		"page size zero":    ok + "&page_size=0",
		"page size too big": ok + "&page_size=5001",
		"cursor not base64": ok + "&cursor=***",
		"cursor bad json":   ok + "&cursor=bm90LWpzb24",
	} {
		repo := &sessionTotalsRepoCapture{}
		rec, _ := getSessionTotals(t, newSessionTotalsRouter(repo, nil), query)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		require.Nil(t, repo.query, name)
	}
}

func TestAdminUsageSessionTotalsPassesWindowAndPages(t *testing.T) {
	gid := int64(7)
	sid := "sess-1"
	next := usagestats.SessionTotalsKey{Day: "2026-03-09", UserID: 3, APIKeyID: 4, GroupID: 7, SessionID: "sess-1"}
	repo := &sessionTotalsRepoCapture{page: &usagestats.SessionTotalsPage{
		Items: []usagestats.SessionTotalsRow{{Day: "2026-03-09", UserID: 3, UserEmail: "a@x", APIKeyID: 4, GroupID: &gid, SessionID: &sid, Calls: 2}},
		Next:  &next,
	}}
	router := newSessionTotalsRouter(repo, nil)

	rec, body := getSessionTotals(t, router, "start_date=2026-03-09&end_date=2026-03-15&timezone=Asia/Shanghai&user_id=3&page_size=10")
	require.Equal(t, http.StatusOK, rec.Code)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	require.True(t, repo.query.StartTime.Equal(time.Date(2026, 3, 9, 0, 0, 0, 0, shanghai)))
	require.True(t, repo.query.EndTime.Equal(time.Date(2026, 3, 16, 0, 0, 0, 0, shanghai)), "end_date is inclusive")
	require.Equal(t, "Asia/Shanghai", repo.query.Timezone)
	require.Equal(t, int64(3), repo.query.UserID)
	require.Equal(t, 10, repo.query.Limit)
	require.Nil(t, repo.query.After)
	require.Equal(t, "a@x", body.Data.Items[0].UserEmail)
	require.NotNil(t, body.Data.NextCursor)
	require.Equal(t, "Asia/Shanghai", body.Data.Timezone)

	// The cursor round-trips into the next query.
	repo.page = nil
	rec, body = getSessionTotals(t, router, "start_date=2026-03-09&end_date=2026-03-15&timezone=Asia/Shanghai&cursor="+url.QueryEscape(*body.Data.NextCursor))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, &next, repo.query.After)
	require.Equal(t, usagestats.SessionTotalsMaxPageSize, repo.query.Limit, "default page size")
	require.Nil(t, body.Data.NextCursor)
}

func TestAdminUsageSessionTotalsWithholdsEmailFromScopedKeyWithoutUsersRead(t *testing.T) {
	page := func() *usagestats.SessionTotalsPage {
		return &usagestats.SessionTotalsPage{Items: []usagestats.SessionTotalsRow{{Day: "2026-03-09", UserID: 3, UserEmail: "a@x", APIKeyID: 4}}}
	}
	query := "start_date=2026-03-09&end_date=2026-03-09&timezone=UTC"

	usageOnly := &adminaccess.Principal{KeyID: "k1", Policy: adminaccess.Policy{GlobalPermissions: []string{"usage.read"}}}
	_, body := getSessionTotals(t, newSessionTotalsRouter(&sessionTotalsRepoCapture{page: page()}, usageOnly), query)
	require.Equal(t, "", body.Data.Items[0].UserEmail)

	withUsers := &adminaccess.Principal{KeyID: "k2", Policy: adminaccess.Policy{GlobalPermissions: []string{"usage.read", "users.read"}}}
	_, body = getSessionTotals(t, newSessionTotalsRouter(&sessionTotalsRepoCapture{page: page()}, withUsers), query)
	require.Equal(t, "a@x", body.Data.Items[0].UserEmail)

	full := &adminaccess.Principal{KeyID: "k3", Policy: adminaccess.Policy{FullAccess: true}}
	_, body = getSessionTotals(t, newSessionTotalsRouter(&sessionTotalsRepoCapture{page: page()}, full), query)
	require.Equal(t, "a@x", body.Data.Items[0].UserEmail)
}
