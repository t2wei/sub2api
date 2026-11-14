package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// [OXSCI] IssuerService is the data service's narrow issuance API (P135 T13 K1;
// contract in OxSci-AI/oxsci-data-service docs/architecture/model-budgets.md).
//
// One interview = one account here, its balance the budget (a standard group bills
// the balance, so the balance is the hard cap); the data service issues a short-lived
// key per board session and closes the account when the interview ends. Everything
// this service touches is bounded twice, whatever the caller sends: users whose email
// is in the reserved domain, and groups in the configured list. It never sees or
// changes anyone else.
type IssuerService struct {
	cfg            config.IssuerConfig
	userRepo       UserRepository
	adminService   AdminService
	apiKeyService  *APIKeyService
	usageService   *UsageService
	settingService *SettingService
}

// NewIssuerService wires the issuer over the existing user, key and usage services.
func NewIssuerService(
	cfg *config.Config,
	userRepo UserRepository,
	adminService AdminService,
	apiKeyService *APIKeyService,
	usageService *UsageService,
	settingService *SettingService,
) *IssuerService {
	var issuerCfg config.IssuerConfig
	if cfg != nil {
		issuerCfg = cfg.Issuer
	}
	return &IssuerService{
		cfg:            issuerCfg,
		userRepo:       userRepo,
		adminService:   adminService,
		apiKeyService:  apiKeyService,
		usageService:   usageService,
		settingService: settingService,
	}
}

// IssuerKeyMaxLifetime is the longest a key issued here may live.
const IssuerKeyMaxLifetime = 24 * time.Hour

// IssuerUsageMaxPageSize is the largest usage page the issuer answers.
const IssuerUsageMaxPageSize = 1000

// Issuer error codes (the envelope's `reason`).
const (
	IssuerReasonDisabled     = "ISSUER_DISABLED"
	IssuerReasonUnauthorized = "ISSUER_UNAUTHORIZED"
	IssuerReasonForbidden    = "ISSUER_FORBIDDEN"
	IssuerReasonUserNotFound = "ISSUER_USER_NOT_FOUND"
	IssuerReasonUserDisabled = "ISSUER_USER_DISABLED"
	IssuerReasonInvalid      = "ISSUER_INVALID"
)

func issuerInvalid(format string, a ...any) error {
	return infraerrors.Newf(http.StatusUnprocessableEntity, IssuerReasonInvalid, format, a...)
}

func issuerForbidden(message string) error {
	return infraerrors.Forbidden(IssuerReasonForbidden, message)
}

// IssuerUser is a reserved-domain user as the issuer answers it.
type IssuerUser struct {
	ID            int64   `json:"id"`
	Email         string  `json:"email"`
	Balance       float64 `json:"balance"`
	Status        string  `json:"status"`
	AllowedGroups []int64 `json:"allowed_groups"`
}

func issuerUserOf(u *User) IssuerUser {
	groups := append([]int64{}, u.AllowedGroups...)
	return IssuerUser{ID: u.ID, Email: u.Email, Balance: u.Balance, Status: u.Status, AllowedGroups: groups}
}

// IssuerCreateUserInput is POST /issuer/users.
type IssuerCreateUserInput struct {
	Email         string
	Balance       float64
	AllowedGroups []int64
	Notes         string
}

func (s *IssuerService) checkGroups(groups []int64) error {
	if len(groups) == 0 {
		return issuerInvalid("allowed_groups must name at least one group")
	}
	for _, id := range groups {
		if !s.cfg.AllowsGroup(id) {
			return issuerForbidden(fmt.Sprintf("group %d is not one the issuer may bind", id))
		}
	}
	return nil
}

// CreateUser creates a reserved-domain user with the given balance and groups, or
// answers the existing one untouched (created=false) — the data service retries this
// call until it has written its own row. The user is created directly, not through the
// admin path: an interview account must not pick up the site's default subscriptions,
// and its only groups are the ones named here. Its password is random and never told
// to anyone, and the reserved domain receives no mail, so nobody can sign in as it.
func (s *IssuerService) CreateUser(ctx context.Context, in IssuerCreateUserInput) (IssuerUser, bool, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !s.cfg.InDomain(email) {
		return IssuerUser{}, false, issuerForbidden("the issuer only creates users in its reserved email domain")
	}
	if len(email) > 255 {
		return IssuerUser{}, false, issuerInvalid("email is longer than 255 characters")
	}
	if !(in.Balance > 0) {
		return IssuerUser{}, false, issuerInvalid("balance must be greater than zero")
	}
	if err := s.checkGroups(in.AllowedGroups); err != nil {
		return IssuerUser{}, false, err
	}
	if existing, err := s.userRepo.GetByEmail(ctx, email); err == nil {
		return s.existingUser(existing)
	} else if !errors.Is(err, ErrUserNotFound) {
		return IssuerUser{}, false, fmt.Errorf("look up issuer user: %w", err)
	}
	password, err := issuerRandomHex(32)
	if err != nil {
		return IssuerUser{}, false, err
	}
	groups := issuerDedupeInt64(in.AllowedGroups)
	user := &User{
		Email:                email,
		Username:             issuerTruncateRunes(email[:strings.Index(email, "@")], 100),
		Notes:                in.Notes,
		Role:                 RoleUser,
		Balance:              in.Balance,
		Status:               StatusActive,
		AllowedGroups:        groups,
		RestrictPublicGroups: true,
		// what the repository stores for an admin-created user (never a sign-in source)
		SignupSource: "email",
	}
	if s.settingService != nil {
		user.Concurrency = s.settingService.GetDefaultConcurrency(ctx)
		user.RPMLimit = s.settingService.GetDefaultUserRPMLimit(ctx)
	}
	if err := user.SetPassword(password); err != nil {
		return IssuerUser{}, false, fmt.Errorf("set issuer user password: %w", err)
	}
	if err := s.userRepo.Create(ctx, user); err != nil {
		if errors.Is(err, ErrEmailExists) {
			// A concurrent retry created it first: answer that one.
			existing, getErr := s.userRepo.GetByEmail(ctx, email)
			if getErr != nil {
				return IssuerUser{}, false, fmt.Errorf("look up issuer user after a concurrent create: %w", getErr)
			}
			return s.existingUser(existing)
		}
		return IssuerUser{}, false, fmt.Errorf("create issuer user: %w", err)
	}
	return issuerUserOf(user), true, nil
}

func (s *IssuerService) existingUser(u *User) (IssuerUser, bool, error) {
	if !s.cfg.InDomain(u.Email) {
		return IssuerUser{}, false, issuerForbidden("the issuer only manages users in its reserved email domain")
	}
	return issuerUserOf(u), false, nil
}

// boundedUser is the user with this id if it is the issuer's to see.
func (s *IssuerService) boundedUser(ctx context.Context, id int64) (*User, error) {
	if id <= 0 {
		return nil, infraerrors.NotFound(IssuerReasonUserNotFound, "no such user")
	}
	u, err := s.userRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, infraerrors.NotFound(IssuerReasonUserNotFound, "no such user")
		}
		return nil, fmt.Errorf("get issuer user: %w", err)
	}
	if !s.cfg.InDomain(u.Email) {
		return nil, issuerForbidden("the issuer only manages users in its reserved email domain")
	}
	return u, nil
}

// GetUser answers a reserved-domain user.
func (s *IssuerService) GetUser(ctx context.Context, id int64) (IssuerUser, error) {
	u, err := s.boundedUser(ctx, id)
	if err != nil {
		return IssuerUser{}, err
	}
	return issuerUserOf(u), nil
}

// IssuerKey is a key as issued (the only time its value is answered).
type IssuerKey struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	GroupID   int64     `json:"group_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// IssueKey creates a key for a reserved-domain user, bound to a configured group the
// user is allowed, expiring at expiresAt (in the future, at most 24 h away).
func (s *IssuerService) IssueKey(ctx context.Context, userID int64, name string, groupID int64, expiresAt time.Time, now time.Time) (IssuerKey, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 100 {
		return IssuerKey{}, issuerInvalid("name must be 1-100 characters")
	}
	if !expiresAt.After(now) {
		return IssuerKey{}, issuerInvalid("expires_at must be in the future")
	}
	if expiresAt.Sub(now) > IssuerKeyMaxLifetime {
		return IssuerKey{}, issuerInvalid("expires_at must be at most %s away", IssuerKeyMaxLifetime)
	}
	u, err := s.boundedUser(ctx, userID)
	if err != nil {
		return IssuerKey{}, err
	}
	if !s.cfg.AllowsGroup(groupID) || !containsInt64(u.AllowedGroups, groupID) {
		return IssuerKey{}, issuerForbidden(fmt.Sprintf("group %d is not one the issuer may bind for this user", groupID))
	}
	if u.Status != StatusActive {
		return IssuerKey{}, infraerrors.Conflict(IssuerReasonUserDisabled, "the user is disabled: no key is issued")
	}
	key, err := s.apiKeyService.IssueExpiringKey(ctx, u.ID, groupID, name, expiresAt.UTC())
	if err != nil {
		if errors.Is(err, ErrGroupNotAllowed) {
			return IssuerKey{}, issuerForbidden(fmt.Sprintf("group %d cannot be bound by this user", groupID))
		}
		return IssuerKey{}, err
	}
	out := IssuerKey{ID: key.ID, Key: key.Key, Name: name, GroupID: groupID, ExpiresAt: expiresAt.UTC()}
	if key.ExpiresAt != nil {
		out.ExpiresAt = key.ExpiresAt.UTC()
	}
	return out, nil
}

// DisableUser switches a reserved-domain user off; idempotent. Every key of the user
// is refused from the next request on: the admin disable path invalidates the API-key
// auth cache when the status changes, and the cache is invalidated once more here so a
// retry (status already disabled) also takes effect on every instance.
func (s *IssuerService) DisableUser(ctx context.Context, id int64) (IssuerUser, error) {
	u, err := s.boundedUser(ctx, id)
	if err != nil {
		return IssuerUser{}, err
	}
	if u.Status != StatusDisabled {
		updated, err := s.adminService.UpdateUser(ctx, u.ID, &UpdateUserInput{Status: StatusDisabled})
		if err != nil {
			return IssuerUser{}, fmt.Errorf("disable issuer user: %w", err)
		}
		u = updated
	}
	if s.apiKeyService != nil {
		s.apiKeyService.InvalidateAuthCacheByUserID(ctx, u.ID)
	}
	return issuerUserOf(u), nil
}

// IssuerUsageRow is one usage row as the issuer answers it.
type IssuerUsageRow struct {
	ID                  int64     `json:"id"`
	CreatedAt           time.Time `json:"created_at"`
	Model               string    `json:"model"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
	CacheCreationTokens int       `json:"cache_creation_tokens"`
	CacheReadTokens     int       `json:"cache_read_tokens"`
	TotalCost           float64   `json:"total_cost"`
	ActualCost          float64   `json:"actual_cost"`
	RequestID           string    `json:"request_id"`
	SessionID           *string   `json:"session_id"`
	APIKeyID            int64     `json:"api_key_id"`
	UserAgent           *string   `json:"user_agent"`
	IPAddress           *string   `json:"ip_address"`
}

// IssuerUsageQuery is GET /issuer/users/:id/usage.
type IssuerUsageQuery struct {
	Page      int
	PageSize  int
	StartTime *time.Time
	EndTime   *time.Time
}

// Usage answers a reserved-domain user's usage rows, newest first.
func (s *IssuerService) Usage(ctx context.Context, userID int64, q IssuerUsageQuery) ([]IssuerUsageRow, *pagination.PaginationResult, error) {
	if q.Page < 1 {
		return nil, nil, issuerInvalid("page must be at least 1")
	}
	if q.PageSize < 1 || q.PageSize > IssuerUsageMaxPageSize {
		return nil, nil, issuerInvalid("page_size must be 1-%d", IssuerUsageMaxPageSize)
	}
	if q.StartTime != nil && q.EndTime != nil && q.EndTime.Before(*q.StartTime) {
		return nil, nil, issuerInvalid("end_time is before start_time")
	}
	u, err := s.boundedUser(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	logs, result, err := s.usageService.ListWithFilters(ctx,
		pagination.PaginationParams{Page: q.Page, PageSize: q.PageSize, SortBy: "id", SortOrder: pagination.SortOrderDesc},
		usagestats.UsageLogFilters{UserID: u.ID, StartTime: q.StartTime, EndTime: q.EndTime, ExactTotal: true},
	)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]IssuerUsageRow, 0, len(logs))
	for _, l := range logs {
		rows = append(rows, IssuerUsageRow{
			ID:                  l.ID,
			CreatedAt:           l.CreatedAt.UTC(),
			Model:               l.Model,
			InputTokens:         l.InputTokens,
			OutputTokens:        l.OutputTokens,
			CacheCreationTokens: l.CacheCreationTokens,
			CacheReadTokens:     l.CacheReadTokens,
			TotalCost:           l.TotalCost,
			ActualCost:          l.ActualCost,
			RequestID:           l.RequestID,
			SessionID:           l.SessionID,
			APIKeyID:            l.APIKeyID,
			UserAgent:           l.UserAgent,
			IPAddress:           l.IPAddress,
		})
	}
	return rows, result, nil
}

func issuerRandomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func issuerDedupeInt64(in []int64) []int64 {
	seen := make(map[int64]bool, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func issuerTruncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
