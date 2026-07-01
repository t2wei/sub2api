//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newEmailOAuthAutoAuthService(
	userRepo UserRepository,
	settings map[string]string,
	quotaRepo UserPlatformQuotaRepository,
) *AuthService {
	return newEmailOAuthAutoAuthServiceWithRedeemRepo(userRepo, nil, settings, quotaRepo)
}

func newEmailOAuthAutoAuthServiceWithRedeemRepo(
	userRepo UserRepository,
	redeemRepo RedeemCodeRepository,
	settings map[string]string,
	quotaRepo UserPlatformQuotaRepository,
) *AuthService {
	cfg := &config.Config{
		JWT: config.JWTConfig{
			Secret:                   "test-secret",
			ExpireHour:               1,
			AccessTokenExpireMinutes: 60,
			RefreshTokenExpireDays:   7,
		},
		Default: config.DefaultConfig{
			UserBalance:     3.5,
			UserConcurrency: 2,
		},
	}

	settingService := NewSettingService(&settingRepoStub{values: settings}, cfg)

	return NewAuthService(
		nil, // entClient — nil, updateUserSignupSource early return
		userRepo,
		redeemRepo,
		&refreshTokenCacheStub{},
		cfg,
		settingService,
		nil, // emailService
		nil, // turnstileService
		nil, // emailQueueService
		nil, // promoService
		nil, // defaultSubAssigner — nil, assignSubscriptions early return
		nil, // affiliateService — nil, bindOAuthAffiliate early return
		quotaRepo,
	)
}

func TestEmailOAuthAuto_SnapshotsPlatformQuotaDefaults(t *testing.T) {
	userRepo := &userRepoStub{nextID: 88}
	quotaRepo := &userPlatformQuotaRepoStub{}

	svc := newEmailOAuthAutoAuthService(
		userRepo,
		map[string]string{
			SettingKeyRegistrationEnabled:   "true",
			SettingKeyDefaultPlatformQuotas: `{"gemini": {"monthly": 100.0}}`,
		},
		quotaRepo,
	)

	user, err := svc.createEmailOAuthUser(
		context.Background(),
		"newoauth@example.com",
		"newoauth",
		"github",
		"", // invitationCode
		"", // affiliateCode
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, int64(88), user.ID)

	require.Len(t, quotaRepo.bulkInsertCalls, 1, "createEmailOAuthUser must snapshot platform quotas via BulkInsertInitial")

	records := quotaRepo.bulkInsertCalls[0]
	var geminiRecord *UserPlatformQuotaRecord
	for i := range records {
		if records[i].Platform == "gemini" {
			geminiRecord = &records[i]
			break
		}
	}
	require.NotNil(t, geminiRecord, "expected gemini platform record")
	require.NotNil(t, geminiRecord.MonthlyLimitUSD)
	require.InDelta(t, 100.0, *geminiRecord.MonthlyLimitUSD, 0.0001)
}

// registration_enabled=false 时，可信 OIDC 提供商仍应允许 verified-email 直登建号，
// 与 LoginOrRegisterOAuthWithTokenPair 的注册门槛保持一致。
func TestEmailOAuthAuto_OIDCBypassesRegistrationDisabled(t *testing.T) {
	userRepo := &userRepoStub{nextID: 91}
	quotaRepo := &userPlatformQuotaRepoStub{}

	svc := newEmailOAuthAutoAuthService(
		userRepo,
		map[string]string{
			SettingKeyRegistrationEnabled: "false",
		},
		quotaRepo,
	)

	user, err := svc.createEmailOAuthUser(
		context.Background(),
		"oidc-newuser@example.com",
		"oidc_newuser",
		"oidc",
		"", // invitationCode
		"", // affiliateCode
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, int64(91), user.ID)
	require.Equal(t, "oidc", user.SignupSource)
}

// registration_enabled=false 时，非可信提供商（github/google）仍应被拒绝。
func TestEmailOAuthAuto_NonOIDCRejectedWhenRegistrationDisabled(t *testing.T) {
	userRepo := &userRepoStub{nextID: 92}
	quotaRepo := &userPlatformQuotaRepoStub{}

	svc := newEmailOAuthAutoAuthService(
		userRepo,
		map[string]string{
			SettingKeyRegistrationEnabled: "false",
		},
		quotaRepo,
	)

	_, err := svc.createEmailOAuthUser(
		context.Background(),
		"github-newuser@example.com",
		"github_newuser",
		"github",
		"",
		"",
		false,
	)
	require.ErrorIs(t, err, ErrRegDisabled)
	require.Empty(t, quotaRepo.bulkInsertCalls, "rejected registration must not snapshot quotas")
}

func TestEmailOAuthAuto_TrustedOrgBypassesInvitationRequirement(t *testing.T) {
	userRepo := &userRepoStub{nextID: 93}
	quotaRepo := &userPlatformQuotaRepoStub{}

	svc := newEmailOAuthAutoAuthServiceWithRedeemRepo(
		userRepo,
		&redeemCodeRepoStub{},
		map[string]string{
			SettingKeyRegistrationEnabled:   "true",
			SettingKeyInvitationCodeEnabled: "true",
		},
		quotaRepo,
	)

	user, err := svc.createEmailOAuthUser(
		context.Background(),
		"trusted@example.com",
		"trusted_user",
		"oidc",
		"",
		"",
		true,
	)
	require.NoError(t, err)
	require.NotNil(t, user)
	require.Equal(t, int64(93), user.ID)

	_, err = svc.createEmailOAuthUser(
		context.Background(),
		"untrusted@example.com",
		"untrusted_user",
		"oidc",
		"",
		"",
		false,
	)
	require.ErrorIs(t, err, ErrOAuthInvitationRequired)
}

func TestEmailOAuthAuto_TrustedOrgOIDCBypassesRegistrationSuffixWhitelist(t *testing.T) {
	svc := newEmailOAuthAutoAuthService(
		&userRepoStub{},
		map[string]string{
			SettingKeyRegistrationEmailSuffixWhitelist: `["@qq.com"]`,
		},
		nil,
	)

	require.NoError(t,
		svc.validateVerifiedEmailOAuthRegistrationPolicy(context.Background(), "oidc", "member@xsci.com", true),
		"trusted OIDC organization members should not depend on the public signup whitelist",
	)

	require.ErrorIs(t,
		svc.validateVerifiedEmailOAuthRegistrationPolicy(context.Background(), "oidc", "member@xsci.com", false),
		ErrEmailSuffixNotAllowed,
	)
	require.ErrorIs(t,
		svc.validateVerifiedEmailOAuthRegistrationPolicy(context.Background(), "oidc", "member@xsci.ai", false),
		ErrEmailSuffixNotAllowed,
		"email suffixes alone must not bypass the registration whitelist",
	)
	require.ErrorIs(t,
		svc.validateVerifiedEmailOAuthRegistrationPolicy(context.Background(), "github", "member@xsci.com", true),
		ErrEmailSuffixNotAllowed,
		"the bypass must stay scoped to OIDC SSO",
	)
}
