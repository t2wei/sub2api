package service

import (
	"context"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// FindAuthIdentityOwnerID answers the id of the user that owns the canonical login
// identity (provider_type, provider_key, provider_subject), or 0 when no user does.
// It is the first lookup of a third-party login (for OIDC, provider_key is the
// issuer and provider_subject the subject); the login then also requires the owner
// to be active, the admin lookup by identity does not. client must not be nil.
func FindAuthIdentityOwnerID(ctx context.Context, client *dbent.Client, identity PendingAuthIdentityKey) (int64, error) {
	record, err := client.AuthIdentity.Query().
		Where(
			authidentity.ProviderTypeEQ(strings.TrimSpace(identity.ProviderType)),
			authidentity.ProviderKeyEQ(strings.TrimSpace(identity.ProviderKey)),
			authidentity.ProviderSubjectEQ(strings.TrimSpace(identity.ProviderSubject)),
		).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return 0, nil
		}
		return 0, infraerrors.InternalServer("AUTH_IDENTITY_LOOKUP_FAILED", "failed to inspect auth identity ownership").WithCause(err)
	}
	return record.UserID, nil
}

// AdminUserIdentityLookup finds a user by OIDC login identity for the admin API. It
// stands apart from AdminService so stubs of that interface need not implement it.
type AdminUserIdentityLookup interface {
	FindUserByOIDCIdentity(ctx context.Context, issuer, subject string) (*User, error)
}

// FindUserByOIDCIdentity answers the user whose OIDC login identity is (issuer,
// subject), whatever the user's status: the same identity lookup an OIDC login makes,
// with the same key (the issuer as the IdP states it, matched exactly). No identity,
// or a deleted owner, is ErrUserNotFound.
func (s *adminServiceImpl) FindUserByOIDCIdentity(ctx context.Context, issuer, subject string) (*User, error) {
	issuer, subject = strings.TrimSpace(issuer), strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return nil, infraerrors.BadRequest("INVALID_INPUT", "issuer and subject are required")
	}
	if s == nil || s.entClient == nil {
		return nil, ErrServiceUnavailable
	}
	userID, err := FindAuthIdentityOwnerID(ctx, s.entClient, PendingAuthIdentityKey{
		ProviderType:    "oidc",
		ProviderKey:     issuer,
		ProviderSubject: subject,
	})
	if err != nil {
		return nil, err
	}
	if userID <= 0 {
		return nil, ErrUserNotFound
	}
	return s.GetUser(ctx, userID)
}
