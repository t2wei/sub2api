//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

const issuerTestDomain = "interview.xsci.invalid"

func issuerTestConfig() *config.Config {
	return &config.Config{Issuer: config.LoadIssuerConfig(func(name string) string {
		switch name {
		case config.IssuerSecretEnv:
			return "0123456789abcdef0123456789abcdef"
		case config.IssuerEmailDomainEnv:
			return issuerTestDomain
		case config.IssuerGroupIDsEnv:
			return "41, 42"
		}
		return ""
	})}
}

type issuerGroupRepoStub struct {
	*groupRepoStub
	groups map[int64]*Group
}

func (s *issuerGroupRepoStub) GetByID(_ context.Context, id int64) (*Group, error) {
	if g, ok := s.groups[id]; ok {
		return g, nil
	}
	return nil, ErrGroupNotFound
}

type issuerAPIKeyRepoStub struct {
	*apiKeyRepoStub
	created  []*APIKey
	userKeys map[int64][]string
}

func (s *issuerAPIKeyRepoStub) Create(_ context.Context, key *APIKey) error {
	key.ID = int64(len(s.created) + 1)
	s.created = append(s.created, key)
	if s.userKeys == nil {
		s.userKeys = map[int64][]string{}
	}
	s.userKeys[key.UserID] = append(s.userKeys[key.UserID], key.Key)
	return nil
}

func (s *issuerAPIKeyRepoStub) ListKeysByUserID(_ context.Context, userID int64) ([]string, error) {
	return s.userKeys[userID], nil
}

type issuerUsageRepoStub struct {
	UsageLogRepository
	logs    []UsageLog
	filters usagestats.UsageLogFilters
	params  pagination.PaginationParams
}

func (s *issuerUsageRepoStub) ListWithFilters(_ context.Context, params pagination.PaginationParams, filters usagestats.UsageLogFilters) ([]UsageLog, *pagination.PaginationResult, error) {
	s.params = params
	s.filters = filters
	return s.logs, &pagination.PaginationResult{Total: int64(len(s.logs)), Page: params.Page, PageSize: params.PageSize}, nil
}

type issuerFixture struct {
	svc    *IssuerService
	users  *userRepoStub
	keys   *issuerAPIKeyRepoStub
	cache  *apiKeyCacheStub
	usage  *issuerUsageRepoStub
	apiKey *APIKeyService
}

func newIssuerFixture(t *testing.T, users ...*User) *issuerFixture {
	t.Helper()
	repo := &userRepoStub{usersByID: map[int64]*User{}, usersByEmail: map[string]*User{}, nextID: 100}
	for _, u := range users {
		repo.usersByID[u.ID] = u
		repo.usersByEmail[u.Email] = u
	}
	cfg := issuerTestConfig()
	require.True(t, cfg.Issuer.Enabled(), cfg.Issuer.DisabledReason)
	keys := &issuerAPIKeyRepoStub{apiKeyRepoStub: &apiKeyRepoStub{}}
	cache := &apiKeyCacheStub{}
	groups := &issuerGroupRepoStub{groupRepoStub: &groupRepoStub{}, groups: map[int64]*Group{
		41: {ID: 41, Name: "interview-anthropic", IsExclusive: true, Status: StatusActive},
		42: {ID: 42, Name: "interview-openai", IsExclusive: true, Status: StatusActive},
		7:  {ID: 7, Name: "staff", IsExclusive: true, Status: StatusActive},
	}}
	apiKeySvc := &APIKeyService{apiKeyRepo: keys, userRepo: repo, groupRepo: groups, cache: cache, cfg: cfg}
	usage := &issuerUsageRepoStub{}
	admin := &adminServiceImpl{userRepo: repo, authCacheInvalidator: apiKeySvc}
	svc := NewIssuerService(cfg, repo, admin, apiKeySvc, &UsageService{usageRepo: usage}, nil)
	return &issuerFixture{svc: svc, users: repo, keys: keys, cache: cache, usage: usage, apiKey: apiKeySvc}
}

func requireIssuerError(t *testing.T, err error, status int, reason string) {
	t.Helper()
	require.Error(t, err)
	code, body := infraerrors.ToHTTP(err)
	require.Equal(t, status, code, err.Error())
	require.Equal(t, reason, body.Reason)
}

func TestIssuerCreateUserIsIdempotent(t *testing.T) {
	f := newIssuerFixture(t)
	ctx := context.Background()
	in := IssuerCreateUserInput{Email: "P201.prod@Interview.XSci.invalid", Balance: 30, AllowedGroups: []int64{41, 41}, Notes: "P201"}

	first, created, err := f.svc.CreateUser(ctx, in)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "p201.prod@interview.xsci.invalid", first.Email)
	require.Equal(t, 30.0, first.Balance)
	require.Equal(t, []int64{41}, first.AllowedGroups)
	require.Equal(t, StatusActive, first.Status)
	stored := f.users.created[0]
	require.Equal(t, RoleUser, stored.Role)
	require.True(t, stored.RestrictPublicGroups)
	require.NotEmpty(t, stored.PasswordHash)

	again, created, err := f.svc.CreateUser(ctx, IssuerCreateUserInput{Email: in.Email, Balance: 99, AllowedGroups: []int64{41, 42}})
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.ID, again.ID)
	require.Equal(t, 30.0, again.Balance, "an existing user is answered untouched")
	require.Equal(t, []int64{41}, again.AllowedGroups)
	require.Len(t, f.users.created, 1)
}

func TestIssuerCreateUserBounds(t *testing.T) {
	f := newIssuerFixture(t)
	ctx := context.Background()

	_, _, err := f.svc.CreateUser(ctx, IssuerCreateUserInput{Email: "someone@xsci.com", Balance: 5, AllowedGroups: []int64{41}})
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)

	_, _, err = f.svc.CreateUser(ctx, IssuerCreateUserInput{Email: "a@b@" + issuerTestDomain, Balance: 5, AllowedGroups: []int64{41}})
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)

	_, _, err = f.svc.CreateUser(ctx, IssuerCreateUserInput{Email: "p1@" + issuerTestDomain, Balance: 5, AllowedGroups: []int64{7}})
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)

	_, _, err = f.svc.CreateUser(ctx, IssuerCreateUserInput{Email: "p1@" + issuerTestDomain, Balance: 0, AllowedGroups: []int64{41}})
	requireIssuerError(t, err, http.StatusUnprocessableEntity, IssuerReasonInvalid)

	_, _, err = f.svc.CreateUser(ctx, IssuerCreateUserInput{Email: "p1@" + issuerTestDomain, Balance: 5})
	requireIssuerError(t, err, http.StatusUnprocessableEntity, IssuerReasonInvalid)
	require.Empty(t, f.users.created)
}

func TestIssuerSeesOnlyReservedDomainUsers(t *testing.T) {
	staff := &User{ID: 5, Email: "staff@xsci.com", Status: StatusActive, AllowedGroups: []int64{41}}
	f := newIssuerFixture(t, staff)
	ctx := context.Background()
	now := time.Now()

	_, err := f.svc.GetUser(ctx, 5)
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)
	_, err = f.svc.IssueKey(ctx, 5, "k", 41, now.Add(15*time.Minute), now)
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)
	_, err = f.svc.DisableUser(ctx, 5)
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)
	_, _, err = f.svc.Usage(ctx, 5, IssuerUsageQuery{Page: 1, PageSize: 10})
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)
	require.Equal(t, StatusActive, staff.Status)
	require.Empty(t, f.keys.created)

	_, err = f.svc.GetUser(ctx, 999)
	requireIssuerError(t, err, http.StatusNotFound, IssuerReasonUserNotFound)

	// An existing staff address handed to create is refused, not answered.
	_, _, err = f.svc.CreateUser(ctx, IssuerCreateUserInput{Email: "staff@xsci.com", Balance: 5, AllowedGroups: []int64{41}})
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)
}

func TestIssuerIssueKey(t *testing.T) {
	candidate := &User{ID: 9, Email: "p201@" + issuerTestDomain, Status: StatusActive, AllowedGroups: []int64{41}}
	f := newIssuerFixture(t, candidate)
	ctx := context.Background()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	expires := now.Add(15 * time.Minute)

	key, err := f.svc.IssueKey(ctx, 9, "P201 s-3f2a", 41, expires, now)
	require.NoError(t, err)
	require.NotEmpty(t, key.Key)
	require.Equal(t, int64(41), key.GroupID)
	require.True(t, key.ExpiresAt.Equal(expires))
	require.Len(t, f.keys.created, 1)
	stored := f.keys.created[0]
	require.Equal(t, int64(9), stored.UserID)
	require.Equal(t, int64(41), *stored.GroupID)
	require.True(t, stored.ExpiresAt.Equal(expires))
	require.Equal(t, StatusActive, stored.Status)
	stored.ExpiresAt = ptrTime(now.Add(-time.Second))
	require.True(t, stored.IsExpired(), "past expires_at the gateway answers API_KEY_EXPIRED")

	_, err = f.svc.IssueKey(ctx, 9, "k", 41, now.Add(IssuerKeyMaxLifetime+time.Second), now)
	requireIssuerError(t, err, http.StatusUnprocessableEntity, IssuerReasonInvalid)
	_, err = f.svc.IssueKey(ctx, 9, "k", 41, now, now)
	requireIssuerError(t, err, http.StatusUnprocessableEntity, IssuerReasonInvalid)
	_, err = f.svc.IssueKey(ctx, 9, "", 41, expires, now)
	requireIssuerError(t, err, http.StatusUnprocessableEntity, IssuerReasonInvalid)
	// configured but not the user's group
	_, err = f.svc.IssueKey(ctx, 9, "k", 42, expires, now)
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)
	// not configured at all
	_, err = f.svc.IssueKey(ctx, 9, "k", 7, expires, now)
	requireIssuerError(t, err, http.StatusForbidden, IssuerReasonForbidden)
	require.Len(t, f.keys.created, 1)
}

func TestIssuerDisableRefusesKeys(t *testing.T) {
	candidate := &User{ID: 9, Email: "p201@" + issuerTestDomain, Status: StatusActive, AllowedGroups: []int64{41}}
	f := newIssuerFixture(t, candidate)
	ctx := context.Background()
	now := time.Now()
	key, err := f.svc.IssueKey(ctx, 9, "k", 41, now.Add(15*time.Minute), now)
	require.NoError(t, err)
	f.cache.deleteAuthKeys = nil

	out, err := f.svc.DisableUser(ctx, 9)
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, out.Status)
	require.Equal(t, StatusDisabled, f.users.usersByID[9].Status)
	require.Contains(t, f.cache.deleteAuthKeys, f.apiKey.authCacheKey(key.Key), "the key's auth cache entry is dropped at once")
	// The gateway re-reads the key with its user: a disabled user is USER_INACTIVE (401).
	require.False(t, f.users.usersByID[9].IsActive())

	// idempotent: a retry answers the same and drops the cache again
	f.cache.deleteAuthKeys = nil
	out, err = f.svc.DisableUser(ctx, 9)
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, out.Status)
	require.Contains(t, f.cache.deleteAuthKeys, f.apiKey.authCacheKey(key.Key))

	_, err = f.svc.IssueKey(ctx, 9, "k2", 41, now.Add(15*time.Minute), now)
	requireIssuerError(t, err, http.StatusConflict, IssuerReasonUserDisabled)
}

func TestIssuerUsagePassesFieldsThrough(t *testing.T) {
	candidate := &User{ID: 9, Email: "p201@" + issuerTestDomain, Status: StatusActive, AllowedGroups: []int64{41}}
	f := newIssuerFixture(t, candidate)
	session, ua, ip := "3f2a0000-0000-4000-8000-000000000000", "claude-cli/2.1", "203.0.113.7"
	at := time.Date(2026, 11, 14, 6, 5, 12, 0, time.UTC)
	f.usage.logs = []UsageLog{{
		ID: 77, UserID: 9, APIKeyID: 3, RequestID: "client:abc", Model: "claude-sonnet-4-5",
		InputTokens: 12400, OutputTokens: 800, CacheCreationTokens: 10, CacheReadTokens: 2000,
		TotalCost: 0.30, ActualCost: 0.21, SessionID: &session, UserAgent: &ua, IPAddress: &ip, CreatedAt: at,
	}}
	start := at.Add(-time.Hour)

	rows, result, err := f.svc.Usage(context.Background(), 9, IssuerUsageQuery{Page: 2, PageSize: 500, StartTime: &start})
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Total)
	require.Equal(t, int64(9), f.usage.filters.UserID)
	require.Equal(t, &start, f.usage.filters.StartTime)
	require.True(t, f.usage.filters.ExactTotal)
	require.Equal(t, 2, f.usage.params.Page)
	require.Equal(t, "id", f.usage.params.SortBy)
	require.Equal(t, pagination.SortOrderDesc, f.usage.params.SortOrder)
	require.Equal(t, []IssuerUsageRow{{
		ID: 77, CreatedAt: at, Model: "claude-sonnet-4-5", InputTokens: 12400, OutputTokens: 800,
		CacheCreationTokens: 10, CacheReadTokens: 2000, TotalCost: 0.30, ActualCost: 0.21,
		RequestID: "client:abc", SessionID: &session, APIKeyID: 3, UserAgent: &ua, IPAddress: &ip,
	}}, rows)

	_, _, err = f.svc.Usage(context.Background(), 9, IssuerUsageQuery{Page: 1, PageSize: IssuerUsageMaxPageSize + 1})
	requireIssuerError(t, err, http.StatusUnprocessableEntity, IssuerReasonInvalid)
}

func TestIssuerConfigLoading(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	none := config.LoadIssuerConfig(env(nil))
	require.False(t, none.Enabled())
	require.False(t, none.Misconfigured)

	short := config.LoadIssuerConfig(env(map[string]string{
		config.IssuerSecretEnv: "short", config.IssuerEmailDomainEnv: issuerTestDomain, config.IssuerGroupIDsEnv: "1",
	}))
	require.False(t, short.Enabled())
	require.True(t, short.Misconfigured)

	missing := config.LoadIssuerConfig(env(map[string]string{config.IssuerSecretEnv: "0123456789abcdef0123456789abcdef"}))
	require.False(t, missing.Enabled())
	require.Contains(t, missing.DisabledReason, config.IssuerEmailDomainEnv)

	badGroup := config.LoadIssuerConfig(env(map[string]string{
		config.IssuerSecretEnv: "0123456789abcdef0123456789abcdef", config.IssuerEmailDomainEnv: issuerTestDomain, config.IssuerGroupIDsEnv: "1,x",
	}))
	require.False(t, badGroup.Enabled())

	ok := issuerTestConfig().Issuer
	require.True(t, ok.Enabled())
	require.Equal(t, []int64{41, 42}, ok.GroupIDs)
	require.True(t, ok.InDomain("X@INTERVIEW.xsci.invalid"))
	require.False(t, ok.InDomain("x@sub.interview.xsci.invalid"))
	require.False(t, ok.InDomain("@interview.xsci.invalid"))
}
