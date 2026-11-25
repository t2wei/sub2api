//go:build unit

package service

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

const lookupIssuer = "https://data.xsci.ai"

func newIdentityLookupTestClient(t *testing.T) *dbent.Client {
	t.Helper()
	db, err := sql.Open("sqlite", "file:oxsci_lookup_"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// identityLookupUserRepo reads users from the map of the base stub and answers the
// last-used time GetUser asks for.
type identityLookupUserRepo struct{ *userRepoStub }

func (identityLookupUserRepo) GetLatestUsedAtByUserID(context.Context, int64) (*time.Time, error) {
	return nil, nil
}

func seedIdentityUser(t *testing.T, client *dbent.Client, email, status, providerType, providerKey, subject string) *User {
	t.Helper()
	ctx := context.Background()
	u, err := client.User.Create().SetEmail(email).SetPasswordHash("hash").SetRole(RoleUser).SetStatus(status).Save(ctx)
	require.NoError(t, err)
	_, err = client.AuthIdentity.Create().SetUserID(u.ID).SetProviderType(providerType).SetProviderKey(providerKey).SetProviderSubject(subject).Save(ctx)
	require.NoError(t, err)
	return &User{ID: u.ID, Email: email, Role: RoleUser, Status: status}
}

func TestFindUserByOIDCIdentityUsesTheLoginKeyAndIgnoresStatus(t *testing.T) {
	client := newIdentityLookupTestClient(t)
	active := seedIdentityUser(t, client, "staff@xsci.com", StatusActive, "oidc", lookupIssuer, "user-7")
	disabled := seedIdentityUser(t, client, "former@xsci.com", StatusDisabled, "oidc", lookupIssuer, "user-8")
	// Same subject under another issuer, and under another provider type: other identities.
	other := seedIdentityUser(t, client, "elsewhere@example.com", StatusActive, "oidc", "https://data.oxsci.ai", "user-9")
	email := seedIdentityUser(t, client, "mail@example.com", StatusActive, "email", "email", "user-10")
	svc := &adminServiceImpl{
		entClient: client,
		userRepo: identityLookupUserRepo{&userRepoStub{usersByID: map[int64]*User{
			active.ID: active, disabled.ID: disabled, other.ID: other, email.ID: email,
		}}},
	}
	ctx := context.Background()

	got, err := svc.FindUserByOIDCIdentity(ctx, lookupIssuer, "user-7")
	require.NoError(t, err)
	require.Equal(t, active.ID, got.ID)

	got, err = svc.FindUserByOIDCIdentity(ctx, " "+lookupIssuer+" ", " user-8 ")
	require.NoError(t, err)
	require.Equal(t, disabled.ID, got.ID)
	require.Equal(t, StatusDisabled, got.Status)

	for _, key := range [][2]string{
		{lookupIssuer, "user-9"},           // that subject belongs to another issuer
		{lookupIssuer, "user-10"},          // and that one is an e-mail identity
		{"https://DATA.xsci.ai", "user-7"}, // the issuer is matched as the login matches it
		{lookupIssuer + "/", "user-7"},
		{lookupIssuer, "nobody"},
	} {
		_, err := svc.FindUserByOIDCIdentity(ctx, key[0], key[1])
		require.ErrorIs(t, err, ErrUserNotFound, "%v", key)
	}
}

func TestFindUserByOIDCIdentityOfADeletedOwnerIsNotFound(t *testing.T) {
	client := newIdentityLookupTestClient(t)
	gone := seedIdentityUser(t, client, "gone@xsci.com", StatusActive, "oidc", lookupIssuer, "user-11")
	// The users repository no longer answers the (soft-)deleted owner.
	svc := &adminServiceImpl{entClient: client, userRepo: identityLookupUserRepo{&userRepoStub{usersByID: map[int64]*User{}}}}

	_, err := svc.FindUserByOIDCIdentity(context.Background(), lookupIssuer, "user-11")
	require.ErrorIs(t, err, ErrUserNotFound)
	require.NotZero(t, gone.ID)
}

func TestFindUserByOIDCIdentityNeedsIssuerAndSubject(t *testing.T) {
	svc := &adminServiceImpl{entClient: newIdentityLookupTestClient(t)}
	for _, key := range [][2]string{{"", "user-7"}, {lookupIssuer, " "}} {
		_, err := svc.FindUserByOIDCIdentity(context.Background(), key[0], key[1])
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	}
}

func TestFindAuthIdentityOwnerIDAnswersZeroForNoIdentity(t *testing.T) {
	client := newIdentityLookupTestClient(t)
	owner := seedIdentityUser(t, client, "owner@xsci.com", StatusDisabled, "oidc", lookupIssuer, "user-12")

	id, err := FindAuthIdentityOwnerID(context.Background(), client, PendingAuthIdentityKey{ProviderType: "oidc", ProviderKey: lookupIssuer, ProviderSubject: "user-12"})
	require.NoError(t, err)
	require.Equal(t, owner.ID, id)

	id, err = FindAuthIdentityOwnerID(context.Background(), client, PendingAuthIdentityKey{ProviderType: "oidc", ProviderKey: lookupIssuer, ProviderSubject: "user-13"})
	require.NoError(t, err)
	require.Zero(t, id)
}

// statusFieldsUserRepo records which columns an update writes.
type statusFieldsUserRepo struct {
	*userRepoStub
	fields []UserUpdateFields
}

func (s *statusFieldsUserRepo) Update(_ context.Context, user *User, fields UserUpdateFields) error {
	s.fields = append(s.fields, fields)
	clone := *user
	s.user = &clone
	return nil
}

// POST /admin/users/:id/status calls UpdateUser with only Status: that writes the
// status column alone and drops the user's auth cache, so the user's keys answer
// USER_INACTIVE on their next request (and work again once restored).
func TestUpdateUserWithOnlyStatusWritesStatusAndDropsTheAuthCache(t *testing.T) {
	for _, tc := range []struct{ from, to string }{{StatusActive, StatusDisabled}, {StatusDisabled, StatusActive}} {
		repo := &statusFieldsUserRepo{userRepoStub: &userRepoStub{user: &User{ID: 42, Email: "u@example.com", Role: RoleUser, Status: tc.from, Concurrency: 3}}}
		invalidator := &authCacheInvalidatorStub{}
		svc := &adminServiceImpl{userRepo: repo, redeemCodeRepo: &redeemRepoStub{}, authCacheInvalidator: invalidator}

		updated, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{Status: tc.to, ActorAdminID: 9})
		require.NoError(t, err)
		require.Equal(t, tc.to, updated.Status)
		require.Equal(t, []UserUpdateFields{{Status: true}}, repo.fields)
		require.Equal(t, []int64{42}, invalidator.userIDs)
	}
}
