package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type userAPIKeyCreatorStub struct {
	calls []userAPIKeyCreateCall
	err   error
}

type userAPIKeyCreateCall struct {
	userID int64
	req    service.CreateAPIKeyRequest
}

func (s *userAPIKeyCreatorStub) Create(_ context.Context, userID int64, req service.CreateAPIKeyRequest) (*service.APIKey, error) {
	s.calls = append(s.calls, userAPIKeyCreateCall{userID: userID, req: req})
	if s.err != nil {
		return nil, s.err
	}
	expires := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	return &service.APIKey{
		ID: 501, UserID: userID, Key: "sk-created-once", Name: req.Name, GroupID: req.GroupID,
		Status: service.StatusActive, IPWhitelist: req.IPWhitelist, Quota: req.Quota, ExpiresAt: &expires,
		CreatedAt: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC),
	}, nil
}

// groupLookupStub answers GetGroup only; every other AdminService method is unused here.
type groupLookupStub struct {
	service.AdminService
	groups map[int64]*service.Group
	err    error
	asked  []int64
}

func (s *groupLookupStub) GetGroup(_ context.Context, id int64) (*service.Group, error) {
	s.asked = append(s.asked, id)
	if s.err != nil {
		return nil, s.err
	}
	if g, ok := s.groups[id]; ok {
		return g, nil
	}
	return nil, service.ErrGroupNotFound
}

func newCreateForUserRouter(creator *userAPIKeyCreatorStub, groups *groupLookupStub, principal *adminaccess.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := &AdminAPIKeyHandler{adminService: groups, apiKeyCreator: creator}
	r := gin.New()
	if principal != nil {
		r.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), principal))
			c.Next()
		})
	}
	r.POST("/api/v1/admin/users/:id/api-keys", h.CreateForUser)
	return r
}

func postCreateForUser(r *gin.Engine, userID string, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/"+userID+"/api-keys", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

func scopedCreatePrincipal(groupIDs ...int64) *adminaccess.Principal {
	return &adminaccess.Principal{KeyID: "ds-token-usage", Policy: adminaccess.Policy{
		Rules:             []adminaccess.Rule{{Permissions: []string{permissionUsersAPIKeysCreate}, GroupIDs: groupIDs}},
		GlobalPermissions: []string{"users.read", "subscriptions.assign"},
	}}
}

func interviewGroups() *groupLookupStub {
	return &groupLookupStub{groups: map[int64]*service.Group{
		16: {ID: 16, Name: "ANT-interview", Platform: service.PlatformAnthropic},
		15: {ID: 15, Name: "ANT-team", Platform: service.PlatformAnthropic},
	}}
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Data
}

func TestCreateForUserFullAccessCreatesThroughTheUserSideService(t *testing.T) {
	creator := &userAPIKeyCreatorStub{}
	r := newCreateForUserRouter(creator, interviewGroups(), nil)

	rec := postCreateForUser(r, "42", `{"name":"interview","group_id":16,"expires_in_days":7,"ip_whitelist":["127.0.0.1/32"],"quota":50,"custom_key":"sk-chosen-by-caller"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, creator.calls, 1)
	call := creator.calls[0]
	require.Equal(t, int64(42), call.userID)
	require.Equal(t, "interview", call.req.Name)
	require.Equal(t, int64(16), *call.req.GroupID)
	require.Equal(t, 7, *call.req.ExpiresInDays)
	require.Equal(t, []string{"127.0.0.1/32"}, call.req.IPWhitelist)
	require.Equal(t, 50.0, call.req.Quota)
	require.Nil(t, call.req.CustomKey, "the key value is always generated")

	data := decodeData(t, rec)
	require.Equal(t, "sk-created-once", data["key"])
	require.EqualValues(t, 501, data["id"])
	require.EqualValues(t, 42, data["user_id"])
	require.EqualValues(t, 16, data["group_id"])
	require.NotContains(t, data, "user")
	require.NotContains(t, data, "group")
}

func TestCreateForUserFullAccessMayCreateAnUngroupedKey(t *testing.T) {
	creator := &userAPIKeyCreatorStub{}
	groups := interviewGroups()
	r := newCreateForUserRouter(creator, groups, nil)

	rec := postCreateForUser(r, "42", `{"name":"plain"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, creator.calls, 1)
	require.Nil(t, creator.calls[0].req.GroupID)
	require.Empty(t, groups.asked)
}

func TestCreateForUserScopedKeyIsHeldToItsGroups(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantCode int
	}{
		"group in its rule":           {`{"name":"k","group_id":16}`, http.StatusOK},
		"group outside its rule":      {`{"name":"k","group_id":15}`, http.StatusForbidden},
		"group that does not exist":   {`{"name":"k","group_id":99}`, http.StatusForbidden},
		"no group":                    {`{"name":"k"}`, http.StatusForbidden},
		"zero group":                  {`{"name":"k","group_id":0}`, http.StatusForbidden},
		"rule covers every group too": {`{"name":"k","group_id":15}`, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			creator := &userAPIKeyCreatorStub{}
			principal := scopedCreatePrincipal(16)
			if name == "rule covers every group too" {
				principal.Policy.Rules[0] = adminaccess.Rule{Permissions: []string{permissionUsersAPIKeysCreate}, AllGroups: true}
			}
			r := newCreateForUserRouter(creator, interviewGroups(), principal)

			rec := postCreateForUser(r, "42", tc.body)

			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
			if tc.wantCode == http.StatusOK {
				require.Len(t, creator.calls, 1)
			} else {
				require.Empty(t, creator.calls)
			}
		})
	}
}

func TestCreateForUserScopedKeyWithoutThePermissionIsRefused(t *testing.T) {
	creator := &userAPIKeyCreatorStub{}
	principal := &adminaccess.Principal{KeyID: "other", Policy: adminaccess.Policy{
		Rules: []adminaccess.Rule{{Permissions: []string{"groups.list.read"}, GroupIDs: []int64{16}}},
	}}
	r := newCreateForUserRouter(creator, interviewGroups(), principal)

	rec := postCreateForUser(r, "42", `{"name":"k","group_id":16}`)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Empty(t, creator.calls)
}

func TestCreateForUserGroupReadFailureIsAnErrorNotARefusal(t *testing.T) {
	creator := &userAPIKeyCreatorStub{}
	groups := &groupLookupStub{err: errors.New("connection reset")}
	r := newCreateForUserRouter(creator, groups, scopedCreatePrincipal(16))

	rec := postCreateForUser(r, "42", `{"name":"k","group_id":16}`)

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Empty(t, creator.calls)
}

func TestCreateForUserPassesTheServiceAnswersThrough(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		wantCode int
		reason   string
	}{
		"user does not exist":                 {fmt.Errorf("get user: %w", service.ErrUserNotFound), http.StatusNotFound, "USER_NOT_FOUND"},
		"subscription group, no subscription": {service.ErrGroupNotAllowed, http.StatusForbidden, "GROUP_NOT_ALLOWED"},
		"bad ip pattern":                      {fmt.Errorf("%w: [nope]", service.ErrInvalidIPPattern), http.StatusBadRequest, "INVALID_IP_PATTERN"},
	} {
		t.Run(name, func(t *testing.T) {
			creator := &userAPIKeyCreatorStub{err: tc.err}
			r := newCreateForUserRouter(creator, interviewGroups(), scopedCreatePrincipal(16))

			rec := postCreateForUser(r, "42", `{"name":"k","group_id":16}`)

			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.reason)
			require.NotContains(t, rec.Body.String(), "sk-")
		})
	}
}

func TestCreateForUserRejectsMalformedRequests(t *testing.T) {
	for name, tc := range map[string]struct{ userID, body string }{
		"user id not a number": {"abc", `{"name":"k"}`},
		"user id zero":         {"0", `{"name":"k"}`},
		"name missing":         {"42", `{"group_id":16}`},
		"negative quota":       {"42", `{"name":"k","quota":-1}`},
		"zero expiry":          {"42", `{"name":"k","expires_in_days":0}`},
		"negative rate limit":  {"42", `{"name":"k","rate_limit_1d":-5}`},
	} {
		t.Run(name, func(t *testing.T) {
			creator := &userAPIKeyCreatorStub{}
			r := newCreateForUserRouter(creator, interviewGroups(), nil)

			rec := postCreateForUser(r, tc.userID, tc.body)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Empty(t, creator.calls)
		})
	}
}
