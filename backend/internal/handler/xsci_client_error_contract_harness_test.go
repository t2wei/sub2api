//go:build unit

package handler

// [OXSCI] Harness for the client error contract test
// (docs/xsci-client-error-contract.md, xsci_client_error_contract_test.go).
//
// It builds the /v1 chain that routes/gateway.go registers for the Anthropic
// surface (same middleware, same order, starting with the contract layer's
// XSciClientErrorContract; the composite-group rewrite is left out because it
// is a no-op for anthropic groups) and drives one request
// through it with a scripted upstream, so each contract row is observed the
// way a client sees it: status, headers and body bytes.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	ceGroupID   = int64(9700)
	ceUserID    = int64(9701)
	ceAPIKeyID  = int64(9702)
	ceAPIKey    = "sk-xsci-contract-0000000000000000"
	ceModel     = "claude-sonnet-5"
	ceUpstreamR = "req_contract_upstream"
)

// ceReply is one scripted upstream answer. Replies are consumed in order; the
// last one repeats.
type ceReply struct {
	status       int
	header       map[string]string
	body         string // JSON body (status >= 400) or SSE text (status 200)
	readErr      error  // after body, the stream read fails with this error
	hang         bool   // after body, the stream stays open and silent
	transportErr error  // DoWithTLS fails before any response
}

type ceUpstream struct {
	service.HTTPUpstream
	mu      sync.Mutex
	replies []ceReply
	calls   int
	closers []io.Closer
}

func (u *ceUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	if req != nil && req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	if len(u.replies) == 0 {
		return nil, errors.New("contract upstream: no reply scripted")
	}
	r := u.replies[0]
	if len(u.replies) > 1 {
		u.replies = u.replies[1:]
	}
	if r.transportErr != nil {
		return nil, r.transportErr
	}
	h := http.Header{"X-Request-Id": []string{ceUpstreamR}}
	if r.status < 400 {
		h.Set("Content-Type", "text/event-stream")
	} else {
		h.Set("Content-Type", "application/json")
	}
	for k, v := range r.header {
		h.Set(k, v)
	}
	var body io.ReadCloser
	switch {
	case r.hang:
		pr, pw := io.Pipe()
		go func() { _, _ = io.WriteString(pw, r.body) }()
		u.closers = append(u.closers, pr, pw)
		body = pr
	case r.readErr != nil:
		body = io.NopCloser(io.MultiReader(strings.NewReader(r.body), ceErrReader{r.readErr}))
	default:
		body = io.NopCloser(strings.NewReader(r.body))
	}
	return &http.Response{StatusCode: r.status, Header: h, Body: body}, nil
}

func (u *ceUpstream) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.closers {
		_ = c.Close()
	}
}

type ceErrReader struct{ err error }

func (r ceErrReader) Read([]byte) (int, error) { return 0, r.err }

// ceAPIKeyRepo answers the API-key lookup of the auth middleware.
type ceAPIKeyRepo struct {
	service.APIKeyRepository
	key     *service.APIKey
	lookErr error
}

func (r *ceAPIKeyRepo) GetByKeyForAuth(_ context.Context, key string) (*service.APIKey, error) {
	if r.lookErr != nil {
		return nil, r.lookErr
	}
	if r.key == nil || key != r.key.Key {
		return nil, service.ErrAPIKeyNotFound
	}
	clone := *r.key
	return &clone, nil
}

func (r *ceAPIKeyRepo) UpdateLastUsed(context.Context, int64, time.Time) error { return nil }

// ceAccountRepo records the account-state writes the rate-limit handling makes
// and serves the persistent candidate list the no-account classifier reads.
type ceAccountRepo struct {
	service.AccountRepository
	mu       sync.Mutex
	accounts []*service.Account
	writes   []string
}

func (r *ceAccountRepo) note(s string) {
	r.mu.Lock()
	r.writes = append(r.writes, s)
	r.mu.Unlock()
}

func (r *ceAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for _, a := range r.accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, service.ErrAccountNotFound
}

func (r *ceAccountRepo) ListModelAvailabilityCandidates(_ context.Context, groupID *int64, _ []string, _ bool) ([]service.Account, error) {
	var out []service.Account
	for _, a := range r.accounts {
		if a.Status != service.StatusActive || !a.Schedulable {
			continue
		}
		if groupID != nil {
			in := false
			for _, ag := range a.AccountGroups {
				if ag.GroupID == *groupID {
					in = true
				}
			}
			if !in {
				continue
			}
		}
		out = append(out, *a)
	}
	return out, nil
}

// SetRateLimited and SetModelRateLimit also update the in-memory account, as
// the database write does on prod, so later lookups in the same request (the
// Retry-After estimate, the model-unsupported check) see them.
func (r *ceAccountRepo) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.note("rate_limited")
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if a.ID == id {
			at := resetAt
			a.RateLimitResetAt = &at
		}
	}
	return nil
}

func (r *ceAccountRepo) SetModelRateLimit(_ context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	r.note("model_rate_limited")
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if a.ID == id {
			a.Extra = ceWithModelRateLimit(a.Extra, scope, resetAt, strings.Join(reason, ""))
		}
	}
	return nil
}

// ceWithModelRateLimit returns a copy of extra with one model cooldown set,
// in the shape the account row stores (extra.model_rate_limits[scope]).
func ceWithModelRateLimit(extra map[string]any, scope string, resetAt time.Time, reason string) map[string]any {
	out := map[string]any{}
	for k, v := range extra {
		out[k] = v
	}
	limits := map[string]any{}
	if old, ok := extra["model_rate_limits"].(map[string]any); ok {
		for k, v := range old {
			limits[k] = v
		}
	}
	entry := map[string]any{
		"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
		"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339),
	}
	if reason != "" {
		entry["reason"] = reason
	}
	limits[scope] = entry
	out["model_rate_limits"] = limits
	return out
}

func (r *ceAccountRepo) SetOverloaded(context.Context, int64, time.Time) error {
	r.note("overloaded")
	return nil
}

func (r *ceAccountRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.note("temp_unschedulable")
	return nil
}

func (r *ceAccountRepo) SetError(context.Context, int64, string) error {
	r.note("error")
	return nil
}

func (r *ceAccountRepo) UpdateSessionWindow(context.Context, int64, *time.Time, *time.Time, string) error {
	return nil
}

func (r *ceAccountRepo) UpdateExtra(context.Context, int64, map[string]any) error { return nil }

// ceSettingRepo is an in-memory settings table (nothing set = upstream
// defaults, which is also what prod runs for every setting the contract
// depends on; see the doc).
type ceSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func (r *ceSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.values[key]; ok {
		return &service.Setting{Key: key, Value: v}, nil
	}
	return nil, service.ErrSettingNotFound
}

func (r *ceSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.values[key]; ok {
		return v, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *ceSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}

func (r *ceSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := r.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (r *ceSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range settings {
		r.values[k] = v
	}
	return nil
}

func (r *ceSettingRepo) GetAll(context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

func (r *ceSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

// ceRuleRepo serves error-passthrough rules.
type ceRuleRepo struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (r *ceRuleRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}

// ceConcurrencyCache lets a case deny user / account slots or the wait queues.
type ceConcurrencyCache struct {
	fakeConcurrencyCache
	userFree      func() bool // nil = always free
	userWaitFull  bool
	accountFree   func() bool // nil = always free
	accountFull   bool        // account wait queue full
	userSlotError error
}

func (f *ceConcurrencyCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	if f.userSlotError != nil {
		return false, f.userSlotError
	}
	if f.userFree == nil {
		return true, nil
	}
	return f.userFree(), nil
}

func (f *ceConcurrencyCache) IncrementWaitCount(context.Context, int64, int) (bool, error) {
	return !f.userWaitFull, nil
}

func (f *ceConcurrencyCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	if f.accountFree == nil {
		return true, nil
	}
	return f.accountFree(), nil
}

func (f *ceConcurrencyCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	return !f.accountFull, nil
}

func (f *ceConcurrencyCache) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	out := map[int64]*service.AccountLoadInfo{}
	if f.accountFree == nil {
		return out, nil
	}
	for _, a := range accounts {
		out[a.ID] = &service.AccountLoadInfo{AccountID: a.ID, CurrentConcurrency: a.MaxConcurrency, LoadRate: 100}
	}
	return out, nil
}

// ceBillingCache is the in-memory balance cache with the async write path
// (usage settlement of partial streams) accepted and dropped.
type ceBillingCache struct {
	*handlerInflightCache
	balanceErr error
}

func newCEBillingCache(balance float64) *ceBillingCache {
	return &ceBillingCache{handlerInflightCache: newHandlerInflightCache(balance)}
}

func (c *ceBillingCache) GetUserBalance(ctx context.Context, userID int64) (float64, error) {
	if c.balanceErr != nil {
		return 0, c.balanceErr
	}
	return c.handlerInflightCache.GetUserBalance(ctx, userID)
}

func (c *ceBillingCache) DeductUserBalance(context.Context, int64, float64) error { return nil }
func (c *ceBillingCache) SetUserBalance(context.Context, int64, float64) error    { return nil }
func (c *ceBillingCache) InvalidateUserBalance(context.Context, int64) error      { return nil }

// ceUserSubRepo serves the subscription lookups of the auth middleware.
type ceUserSubRepo struct {
	service.UserSubscriptionRepository
	sub *service.UserSubscription
}

// ResetDailyUsage is reached only when a usage window has to be advanced; the
// maintenance drive makes that write fail.
func (r *ceUserSubRepo) ResetDailyUsage(context.Context, int64, *time.Time, time.Time) error {
	return errors.New("contract: subscription window write failed")
}

func (r *ceUserSubRepo) GetActiveByUserIDAndGroupID(context.Context, int64, int64) (*service.UserSubscription, error) {
	if r.sub == nil {
		return nil, service.ErrSubscriptionNotFound
	}
	clone := *r.sub
	return &clone, nil
}

// ceEnv is one request's world: key, user, group, accounts, settings, rules
// and the scripted upstream.
type ceEnv struct {
	t           *testing.T
	cfg         *config.Config
	group       *service.Group
	user        *service.User
	key         *service.APIKey
	accounts    []*service.Account
	rules       []*model.ErrorPassthroughRule
	settings    map[string]string
	upstream    *ceUpstream
	conc        *ceConcurrencyCache
	billing     *ceBillingCache
	keyLookErr  error
	sub         *service.UserSubscription
	pingEvery   time.Duration
	maxSwitches int
	accountRepo *ceAccountRepo
}

func newCEEnv(t *testing.T) *ceEnv {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxBodySize = 1 << 20
	cfg.Gateway.MaxLineSize = 64 << 10
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.Scheduling.LoadBatchEnabled = true // config default (gateway.scheduling.load_batch_enabled)
	group := &service.Group{
		ID: ceGroupID, Name: "ANT-CONTRACT", Hydrated: true, Platform: service.PlatformAnthropic,
		Status: service.StatusActive, RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard,
	}
	user := &service.User{ID: ceUserID, Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 5}
	groupID := group.ID
	key := &service.APIKey{
		ID: ceAPIKeyID, UserID: user.ID, Key: ceAPIKey, Name: "contract", Status: service.StatusActive,
		GroupID: &groupID, Group: group, User: user,
	}
	e := &ceEnv{
		t: t, cfg: cfg, group: group, user: user, key: key,
		settings:    map[string]string{},
		upstream:    &ceUpstream{},
		conc:        &ceConcurrencyCache{},
		billing:     newCEBillingCache(100),
		maxSwitches: 2,
	}
	e.accounts = []*service.Account{e.setupTokenAccount(9711)}
	t.Cleanup(e.upstream.close)
	return e
}

// setupTokenAccount is an Anthropic subscription account (the type behind
// ANT-team / ANT-PROD-ONLY on prod).
func (e *ceEnv) setupTokenAccount(id int64) *service.Account {
	return &service.Account{
		ID: id, Name: "ANT-CONTRACT-" + string(rune('A'+id%26)), Platform: service.PlatformAnthropic,
		Type: service.AccountTypeSetupToken, Status: service.StatusActive, Schedulable: true, Concurrency: 5,
		Credentials:   map[string]any{"access_token": "tok", "refresh_token": "refresh"},
		AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: ceGroupID}},
	}
}

func (e *ceEnv) apiKeyAccount(id int64) *service.Account {
	return &service.Account{
		ID: id, Name: "ANT-API-CONTRACT", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 5,
		Credentials:   map[string]any{"api_key": "sk-ant-upstream"},
		AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: ceGroupID}},
	}
}

func (e *ceEnv) bedrockAccount(id int64) *service.Account {
	return &service.Account{
		ID: id, Name: "Bedrock-CONTRACT", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeBedrock, Status: service.StatusActive, Schedulable: true, Concurrency: 5,
		Credentials: map[string]any{
			"auth_mode": "apikey", "api_key": "bedrock-key", "aws_region": "us-east-1",
		},
		AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: ceGroupID}},
	}
}

func (e *ceEnv) reply(r ...ceReply) *ceEnv {
	e.upstream.replies = append(e.upstream.replies, r...)
	return e
}

// prodRule1 is prod error-passthrough rule id 1 (P160 T3 Q2, created
// 2026-11-19), field for field.
func prodRule1() *model.ErrorPassthroughRule {
	code := 502
	return &model.ErrorPassthroughRule{
		ID: 1, Name: "anthropic: pass through mid-stream upstream errors", Enabled: true, Priority: 10,
		ErrorCodes: []int{403}, Keywords: []string{"content filtering", "Unable to complete this request", "Overloaded"},
		MatchMode: model.MatchModeAll, Platforms: []string{"anthropic"},
		PassthroughCode: false, ResponseCode: &code, PassthroughBody: true,
	}
}

func (e *ceEnv) router() (*gin.Engine, *GatewayHandler) {
	cfg := e.cfg
	e.accountRepo = &ceAccountRepo{accounts: e.accounts}
	settingSvc := service.NewSettingService(&ceSettingRepo{values: e.settings}, cfg)
	apiKeySvc := service.NewAPIKeyService(&ceAPIKeyRepo{key: e.key, lookErr: e.keyLookErr}, nil, nil, nil, nil, nil, cfg)
	billingCache := service.NewBillingCacheService(e.billing, nil, nil, nil, nil, nil, cfg, nil)
	e.t.Cleanup(billingCache.Stop)
	var subSvc *service.SubscriptionService
	if e.group.IsSubscriptionType() {
		subSvc = service.NewSubscriptionService(&groupMapRepo{fakeGroupRepo: &fakeGroupRepo{group: e.group}, groups: map[int64]*service.Group{e.group.ID: e.group}},
			&ceUserSubRepo{sub: e.sub}, billingCache, nil, cfg)
	}
	concurrencySvc := service.NewConcurrencyService(e.conc)
	rateLimitSvc := service.NewRateLimitService(e.accountRepo, nil, cfg, nil, nil)
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: e.accounts}, nil, nil, nil, nil)
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
	gw := service.NewGatewayService(
		e.accountRepo, &groupMapRepo{fakeGroupRepo: &fakeGroupRepo{group: e.group}, groups: map[int64]*service.Group{e.group.ID: e.group}},
		usageRepo, &compatStreamUsageBillingRepo{}, nil, nil, nil, nil, cfg,
		snapshot, concurrencySvc, service.NewBillingService(cfg, nil), rateLimitSvc, billingCache, nil, e.upstream,
		&service.DeferredService{}, nil, nil, nil, nil, settingSvc, nil, nil, nil, nil, nil, nil,
	)
	passthrough := service.NewErrorPassthroughService(&ceRuleRepo{rules: e.rules}, nil)
	cfg.Gateway.MaxAccountSwitches = e.maxSwitches
	h := NewGatewayHandler(gw, nil, nil, nil, nil, concurrencySvc, billingCache, nil, apiKeySvc, nil, passthrough, nil, nil, cfg, settingSvc)
	if e.pingEvery > 0 {
		h.concurrencyHelper = NewConcurrencyHelper(concurrencySvc, SSEPingFormatClaude, e.pingEvery)
	}

	r := gin.New()
	r.Use(middleware.RequestLogger())
	v1 := r.Group("/v1")
	v1.Use(XSciClientErrorContract())
	v1.Use(middleware.RequestBodyLimit(cfg.Gateway.MaxBodySize, pkghttputil.NewBodyMemoryBudget(cfg.Gateway.RequestBodyMemoryBudgetBytes)))
	v1.Use(middleware.ClientRequestID())
	v1.Use(OpsErrorLoggerMiddleware(nil))
	v1.Use(InboundEndpointMiddleware())
	v1.Use(gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(apiKeySvc, subSvc, cfg)))
	v1.Use(middleware.GroupModelAllowlist())
	v1.Use(middleware.RequireGroupAssignment(settingSvc, middleware.AnthropicErrorWriter))
	v1.POST("/messages", h.Messages)
	v1.POST("/messages/count_tokens", h.CountTokens)
	v1.GET("/sub2api/availability", h.KeyAvailability)
	return r, h
}

// ceRequest is what the client sends.
type ceRequest struct {
	method  string    // default POST
	path    string    // default /v1/messages
	body    string    // default: a small messages request for ceModel
	empty   bool      // send an empty body
	reader  io.Reader // body reader (overrides body), e.g. one that fails mid-read
	stream  bool
	headers map[string]string
	noKey   bool
	key     string // default ceAPIKey
	query   string
	ctx     context.Context
}

func ceMessagesBody(model string, stream bool) string {
	s := "false"
	if stream {
		s = "true"
	}
	return `{"model":"` + model + `","max_tokens":32,"stream":` + s + `,"messages":[{"role":"user","content":"hello"}]}`
}

func (e *ceEnv) do(req ceRequest) *httptest.ResponseRecorder {
	e.t.Helper()
	r, _ := e.router()
	path := req.path
	if path == "" {
		path = "/v1/messages"
	}
	if req.query != "" {
		path += "?" + req.query
	}
	body := req.body
	if body == "" && !req.empty {
		body = ceMessagesBody(ceModel, req.stream)
	}
	var bodyReader io.Reader = strings.NewReader(body)
	if req.reader != nil {
		bodyReader = req.reader
	}
	method := req.method
	if method == "" {
		method = http.MethodPost
	}
	if method == http.MethodGet {
		bodyReader = http.NoBody
	}
	httpReq := httptest.NewRequest(method, path, bodyReader)
	if req.ctx != nil {
		httpReq = httpReq.WithContext(req.ctx)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if !req.noKey {
		key := req.key
		if key == "" {
			key = ceAPIKey
		}
		httpReq.Header.Set("x-api-key", key)
	}
	for k, v := range req.headers {
		httpReq.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httpReq)
	return rec
}
