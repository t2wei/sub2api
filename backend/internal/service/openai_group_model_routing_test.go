package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type modelRoutingGroupRepo struct {
	GroupRepository
	group *Group
	err   error
	calls int
}

func (r *modelRoutingGroupRepo) GetByIDLite(context.Context, int64) (*Group, error) {
	r.calls++
	return r.group, r.err
}
func (r *modelRoutingGroupRepo) GetByID(ctx context.Context, id int64) (*Group, error) {
	return r.GetByIDLite(ctx, id)
}

type modelRoutingMode struct {
	name      string
	advanced  bool
	loadBatch bool
	weighted  bool
}

var modelRoutingModes = []modelRoutingMode{
	{name: "legacy", loadBatch: false},
	{name: "legacy batch", loadBatch: true},
	{name: "advanced", advanced: true},
	{name: "advanced weighted", advanced: true, weighted: true},
}

func modelRoutingTestGroup(id int64) *Group {
	return &Group{
		ID: id, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true,
		ModelRoutingEnabled: true,
		ModelRouting: map[string][]int64{
			"gpt-restricted-test": {30},
			"gpt-*":               {27, 28},
			"codex-*":             {27, 28},
		},
	}
}

func modelRoutingTestService(groupID int64, mode modelRoutingMode, cache *schedulerTestGatewayCache, concurrency schedulerTestConcurrencyCache) (*OpenAIGatewayService, *schedulerGroupAwareOpenAIAccountRepo) {
	repo := &schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{
		{ID: 27, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
		{ID: 28, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10, GroupIDs: []int64{groupID}},
		{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 50, GroupIDs: []int64{groupID}},
	}}}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = mode.loadBatch
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600
	svc := &OpenAIGatewayService{
		accountRepo: repo, cache: cache, cfg: cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(strconv.FormatBool(mode.advanced), strconv.FormatBool(mode.weighted)),
		concurrencyService: NewConcurrencyService(concurrency),
	}
	return svc, repo
}

func TestOpenAIGroupModelRouting_AllSchedulerModes(t *testing.T) {
	const groupID int64 = 43001
	for _, mode := range modelRoutingModes {
		for _, tc := range []struct {
			name, model      string
			sticky, previous int64
			disabled, busy   bool
			excluded         map[int64]struct{}
			want             int64
		}{
			{name: "exact overrides wildcard before top K", model: "gpt-restricted-test", want: 30},
			{name: "gpt wildcard", model: "gpt-normal-test", excluded: map[int64]struct{}{27: {}}, want: 28},
			{name: "codex wildcard", model: "codex-normal-test", excluded: map[int64]struct{}{27: {}}, want: 28},
			{name: "wrong sticky cannot bypass exact route", model: "gpt-restricted-test", sticky: 27, want: 30},
			{name: "wrong previous response cannot bypass exact route", model: "gpt-restricted-test", previous: 27, want: 30},
			{name: "wildcard excludes restricted sticky", model: "gpt-normal-test", sticky: 30, excluded: map[int64]struct{}{27: {}}, want: 28},
			{name: "disabled keeps sticky", model: "gpt-restricted-test", disabled: true, sticky: 27, want: 27},
			{name: "unmatched permits remaining account", model: "other-test", excluded: map[int64]struct{}{27: {}, 28: {}}, want: 30},
			{name: "wait plan stays on route", model: "gpt-restricted-test", busy: true, want: 30},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				group := modelRoutingTestGroup(groupID)
				group.ModelRoutingEnabled = !tc.disabled
				ctx := context.WithValue(context.Background(), ctxkey.Group, group)
				cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:routing-session": tc.sticky}}
				var acquired []int64
				concurrency := schedulerTestConcurrencyCache{acquiredIDs: &acquired}
				if tc.busy {
					concurrency.acquireResults = map[int64]bool{27: false, 28: false, 30: false}
				}
				svc, _ := modelRoutingTestService(groupID, mode, cache, concurrency)
				previousID := ""
				if tc.previous != 0 {
					previousID = "resp_routing_test"
					require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, previousID, tc.previous, time.Hour))
				}
				excludedBefore := cloneExcludedAccountIDs(tc.excluded)
				selection, _, err := svc.SelectAccountWithScheduler(ctx, ptrInt64(groupID), previousID, "routing-session", tc.model, tc.excluded, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, tc.want, selection.Account.ID)
				require.Equal(t, tc.model, selection.Account.GetMappedModel(tc.model), "routing must keep passthrough model names")
				require.Equal(t, excludedBefore, tc.excluded, "caller exclusions must not be mutated")
				if tc.busy {
					require.NotNil(t, selection.WaitPlan)
					require.Equal(t, tc.want, selection.WaitPlan.AccountID)
				}
				if !tc.disabled && tc.model == "gpt-restricted-test" {
					require.NotContains(t, acquired, int64(27))
					require.NotContains(t, acquired, int64(28))
				}
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}
}

func TestOpenAIGroupModelRouting_UnavailableTargetsNeverFallBack(t *testing.T) {
	const groupID int64 = 43002
	for _, mode := range modelRoutingModes {
		for _, state := range []string{"excluded", "unschedulable", "other group", "missing", "model unsupported"} {
			t.Run(mode.name+"/"+state, func(t *testing.T) {
				ctx := context.WithValue(context.Background(), ctxkey.Group, modelRoutingTestGroup(groupID))
				svc, repo := modelRoutingTestService(groupID, mode, &schedulerTestGatewayCache{}, schedulerTestConcurrencyCache{})
				excluded := map[int64]struct{}{999: {}}
				switch state {
				case "excluded":
					excluded[30] = struct{}{}
				case "unschedulable":
					repo.accounts[2].Schedulable = false
				case "other group":
					repo.accounts[2].GroupIDs = []int64{groupID + 1}
				case "missing":
					repo.accounts = repo.accounts[:2]
				case "model unsupported":
					repo.accounts[2].Credentials = map[string]any{"model_mapping": map[string]any{"other-test": "other-test"}}
				}
				before := cloneExcludedAccountIDs(excluded)
				selection, _, err := svc.SelectAccountWithScheduler(ctx, ptrInt64(groupID), "", "", "gpt-restricted-test", excluded, OpenAIUpstreamTransportAny, false)
				require.ErrorIs(t, err, ErrNoAvailableAccounts)
				require.Nil(t, selection)
				require.Equal(t, before, excluded)
			})
		}
	}
}

func TestOpenAIGroupModelRouting_AuthAndScheduledGroups(t *testing.T) {
	const groupID int64 = 43003
	for _, mode := range modelRoutingModes {
		for _, source := range []string{"auth", "different group", "unhydrated auth", "lookup error", "missing group", "wrong group"} {
			t.Run(mode.name+"/"+source, func(t *testing.T) {
				authGroup := modelRoutingTestGroup(groupID)
				loaded := modelRoutingTestGroup(groupID)
				loaded.ModelRouting["gpt-restricted-test"] = []int64{28}
				groupRepo := &modelRoutingGroupRepo{group: loaded}
				want := int64(28)
				switch source {
				case "auth":
					want = 30
				case "different group":
					authGroup.ID++
				case "unhydrated auth":
					authGroup.Hydrated = false
				case "lookup error":
					authGroup.ID++
					groupRepo.err = errors.New("group lookup unavailable")
				case "missing group":
					authGroup.ID++
					groupRepo.group = nil
				case "wrong group":
					authGroup.ID++
					loaded.ID += 2
				}
				ctx := context.WithValue(context.Background(), ctxkey.Group, authGroup)
				svc, repo := modelRoutingTestService(groupID, mode, &schedulerTestGatewayCache{}, schedulerTestConcurrencyCache{})
				svc.schedulerSnapshot = &SchedulerSnapshotService{accountRepo: repo, groupRepo: groupRepo}
				selection, _, err := svc.SelectAccountWithScheduler(ctx, ptrInt64(groupID), "", "", "gpt-restricted-test", nil, OpenAIUpstreamTransportAny, false)
				if source == "lookup error" || source == "missing group" || source == "wrong group" {
					require.ErrorIs(t, err, ErrNoAvailableAccounts)
					require.Nil(t, selection)
					require.Equal(t, 1, groupRepo.calls)
					return
				}
				require.NoError(t, err)
				require.Equal(t, want, selection.Account.ID)
				if source == "auth" {
					require.Zero(t, groupRepo.calls, "valid auth group must avoid group-table reads")
				} else {
					require.Positive(t, groupRepo.calls)
					require.LessOrEqual(t, groupRepo.calls, 3, "route/privacy/profit load once each, never per candidate")
				}
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}
}

func TestOpenAIGroupModelRouting_DirectLegacyAndPreviousResponseEntries(t *testing.T) {
	const groupID int64 = 43004
	ctx := context.WithValue(context.Background(), ctxkey.Group, modelRoutingTestGroup(groupID))
	svc, _ := modelRoutingTestService(groupID, modelRoutingModes[0], &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:wrong": 27}}, schedulerTestConcurrencyCache{})
	account, err := svc.SelectAccountForModelWithExclusions(ctx, ptrInt64(groupID), "wrong", "gpt-restricted-test", nil)
	require.NoError(t, err)
	require.Equal(t, int64(30), account.ID)
	selection, err := svc.SelectAccountWithLoadAwareness(ctx, ptrInt64(groupID), "wrong", "gpt-restricted-test", nil)
	require.NoError(t, err)
	require.Equal(t, int64(30), selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, "resp_wrong_route", 27, time.Hour))
	selection, err = svc.SelectAccountByPreviousResponseID(ctx, ptrInt64(groupID), "resp_wrong_route", "gpt-restricted-test", nil, false)
	require.NoError(t, err)
	require.Nil(t, selection)
	require.Zero(t, svc.ResolveAccountIDByPreviousResponseIDForScheduler(ctx, ptrInt64(groupID), "resp_wrong_route", "gpt-restricted-test", nil, "", false))
}

func TestOpenAIGroupModelRouting_WeightedPreviousResponseCannotBypass(t *testing.T) {
	const groupID int64 = 43005
	ctx := context.WithValue(context.Background(), ctxkey.Group, modelRoutingTestGroup(groupID))
	svc, _ := modelRoutingTestService(groupID, modelRoutingModes[3], &schedulerTestGatewayCache{}, schedulerTestConcurrencyCache{})
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, "resp_movable_wrong_route", 27, time.Hour))
	selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, ptrInt64(groupID), "resp_movable_wrong_route", "", "gpt-restricted-test", nil, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityResponses, false, true, true)
	require.NoError(t, err)
	require.Equal(t, int64(30), selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGroupModelRouting_ReusedContextDoesNotLeak(t *testing.T) {
	const firstID int64 = 43006
	for _, mode := range modelRoutingModes {
		for _, changed := range []string{"model", "group with routing disabled"} {
			t.Run(mode.name+"/"+changed, func(t *testing.T) {
				group := modelRoutingTestGroup(firstID)
				ctx := context.WithValue(context.Background(), ctxkey.Group, group)
				svc, repo := modelRoutingTestService(firstID, mode, &schedulerTestGatewayCache{}, schedulerTestConcurrencyCache{})
				ctx, err := svc.withOpenAIGroupModelRouting(ctx, ptrInt64(firstID), PlatformOpenAI, "gpt-restricted-test")
				require.NoError(t, err)
				groupID, model := firstID, "gpt-normal-test"
				if changed == "group with routing disabled" {
					groupID++
					model = "gpt-restricted-test"
					for i := range repo.accounts {
						repo.accounts[i].GroupIDs = []int64{groupID}
					}
					destination := modelRoutingTestGroup(groupID)
					destination.ModelRoutingEnabled = false
					svc.schedulerSnapshot = &SchedulerSnapshotService{accountRepo: repo, groupRepo: &modelRoutingGroupRepo{group: destination}}
				}
				selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", model, map[int64]struct{}{28: {}, 30: {}}, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.Equal(t, int64(27), selection.Account.ID)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}
}

func TestOpenAIGroupModelRouting_FreshMembershipStillRequired(t *testing.T) {
	const groupID int64 = 43007
	for _, mode := range modelRoutingModes {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), ctxkey.Group, modelRoutingTestGroup(groupID))
			svc, repo := modelRoutingTestService(groupID, mode, &schedulerTestGatewayCache{}, schedulerTestConcurrencyCache{})
			stale := repo.accounts[2]
			repo.accounts[2].GroupIDs = []int64{groupID + 1}
			svc.schedulerSnapshot = &SchedulerSnapshotService{accountRepo: repo, cache: &openAISnapshotCacheStub{
				snapshotAccounts: []*Account{&repo.accounts[0], &repo.accounts[1], &stale},
				accountsByID:     map[int64]*Account{27: &repo.accounts[0], 28: &repo.accounts[1], 30: &stale},
			}}
			selection, _, err := svc.SelectAccountWithScheduler(ctx, ptrInt64(groupID), "", "", "gpt-restricted-test", nil, OpenAIUpstreamTransportAny, false)
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection)
		})
	}
}

func TestOpenAIGroupModelRouting_EmptyAndOverlappingRules(t *testing.T) {
	const groupID int64 = 43008
	for _, mode := range modelRoutingModes {
		for _, rule := range []string{"empty exact", "empty wildcard", "empty specific wildcard", "nonempty specific wildcard"} {
			t.Run(mode.name+"/"+rule, func(t *testing.T) {
				group := modelRoutingTestGroup(groupID)
				model := "gpt-restricted-test"
				switch rule {
				case "empty exact":
					group.ModelRouting[model] = nil
				case "empty wildcard":
					model = "codex-normal-test"
					group.ModelRouting["codex-*"] = []int64{}
				case "empty specific wildcard":
					model = "gpt-restricted-other-test"
					group.ModelRouting["gpt-restricted-*"] = nil
				case "nonempty specific wildcard":
					model = "gpt-restricted-other-test"
					group.ModelRouting["gpt-*"] = nil
					group.ModelRouting["gpt-restricted-*"] = []int64{30}
				}
				ctx := context.WithValue(context.Background(), ctxkey.Group, group)
				svc, _ := modelRoutingTestService(groupID, mode, &schedulerTestGatewayCache{}, schedulerTestConcurrencyCache{})
				selection, _, err := svc.SelectAccountWithScheduler(ctx, ptrInt64(groupID), "", "", model, nil, OpenAIUpstreamTransportAny, false)
				if rule != "nonempty specific wildcard" {
					require.ErrorIs(t, err, ErrNoAvailableAccounts)
					require.Nil(t, selection)
					require.ErrorContains(t, err, "model_routing_mismatch")
					return
				}
				require.NoError(t, err)
				require.Equal(t, int64(30), selection.Account.ID)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			})
		}
	}
}

func TestOpenAIGroupModelRouting_FixedAccountValidation(t *testing.T) {
	const groupID int64 = 43009
	ctx := context.WithValue(context.Background(), ctxkey.Group, modelRoutingTestGroup(groupID))
	svc := &OpenAIGatewayService{}
	require.NoError(t, svc.ValidateAccountModelRouting(ctx, ptrInt64(groupID), PlatformOpenAI, "gpt-normal-test", &Account{ID: 27}))
	require.ErrorIs(t, svc.ValidateAccountModelRouting(ctx, ptrInt64(groupID), PlatformOpenAI, "gpt-restricted-test", &Account{ID: 27}), ErrNoAvailableAccounts)
	require.NoError(t, svc.ValidateAccountModelRouting(ctx, ptrInt64(groupID), PlatformOpenAI, "gpt-restricted-test", &Account{ID: 30}))
	require.ErrorIs(t, svc.ValidateAccountModelRouting(context.Background(), nil, PlatformOpenAI, "", nil), ErrNoAvailableAccounts)
	lookupErr := errors.New("routing lookup unavailable")
	svc.schedulerSnapshot = &SchedulerSnapshotService{groupRepo: &modelRoutingGroupRepo{err: lookupErr}}
	require.ErrorIs(t, svc.ValidateAccountModelRouting(context.Background(), ptrInt64(groupID), PlatformOpenAI, "gpt-restricted-test", &Account{ID: 30}), lookupErr)
}

func TestOpenAIGroupModelRouting_AmbiguousCandidates(t *testing.T) {
	const groupID int64 = 43010
	svc := &OpenAIGatewayService{}
	for _, tc := range []struct {
		name                       string
		models                     []string
		disabled, empty, wantError bool
	}{
		{name: "one model", models: []string{"gpt-restricted-test"}},
		{name: "same value duplicates", models: []string{"gpt-restricted-test", "gpt-restricted-test", ""}},
		{name: "conflicting restricted model", models: []string{"other-test", "gpt-restricted-test"}, wantError: true},
		{name: "conflicting wildcard", models: []string{"other-test", "gpt-normal-test"}, wantError: true},
		{name: "empty rule still matched", models: []string{"other-test", "gpt-restricted-test"}, empty: true, wantError: true},
		{name: "no matching rule", models: []string{"other-test", "another-test"}},
		{name: "routing disabled", models: []string{"other-test", "gpt-restricted-test"}, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := modelRoutingTestGroup(groupID)
			group.ModelRoutingEnabled = !tc.disabled
			if tc.empty {
				group.ModelRouting["gpt-restricted-test"] = nil
			}
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			err := svc.ValidateModelRoutingCandidates(ctx, ptrInt64(groupID), PlatformOpenAI, tc.models)
			if tc.wantError {
				require.ErrorIs(t, err, ErrNoAvailableAccounts)
				require.ErrorContains(t, err, "ambiguous model routing")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
