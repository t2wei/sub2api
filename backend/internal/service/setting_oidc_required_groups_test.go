package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestOIDCGroupsSatisfy(t *testing.T) {
	for name, tc := range map[string]struct {
		required []string
		groups   []string
		want     bool
	}{
		"no requirement, no groups":        {nil, nil, true},
		"empty requirement, any groups":    {[]string{}, []string{"collaborator.basic"}, true},
		"prefix match":                     {[]string{"staff.*"}, []string{"collaborator.basic", "staff.basic"}, true},
		"prefix miss":                      {[]string{"staff.*"}, []string{"collaborator.assistant-owner"}, false},
		"prefix is literal, not a segment": {[]string{"staff*"}, []string{"staffing.x"}, true},
		"exact match":                      {[]string{"staff.basic"}, []string{"staff.basic"}, true},
		"exact is not prefix":              {[]string{"staff.basic"}, []string{"staff.basic-extra"}, false},
		"case sensitive":                   {[]string{"staff.*"}, []string{"Staff.basic"}, false},
		"any of several entries":           {[]string{"admins", "staff.*"}, []string{"staff.dist"}, true},
		"requirement but no groups claim":  {[]string{"staff.*"}, nil, false},
		"requirement but empty groups":     {[]string{"staff.*"}, []string{}, false},
		"lone star needs some group":       {[]string{"*"}, []string{"anything"}, true},
		"lone star with no groups":         {[]string{"*"}, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, OIDCGroupsSatisfy(tc.required, tc.groups))
		})
	}
}

func TestNormalizeOIDCRequiredGroups(t *testing.T) {
	got, err := NormalizeOIDCRequiredGroups([]string{" staff.* ", "", "staff.*", "admins", "  "})
	require.NoError(t, err)
	require.Equal(t, []string{"staff.*", "admins"}, got)

	got, err = NormalizeOIDCRequiredGroups(nil)
	require.NoError(t, err)
	require.Equal(t, []string{}, got)

	for _, bad := range []string{"staff .*", "st*aff", "**", "a\tb", string(make([]byte, 129))} {
		_, err := NormalizeOIDCRequiredGroups([]string{"staff.*", bad})
		require.Error(t, err, "entry %q", bad)
		require.Equal(t, "OIDC_REQUIRED_GROUPS_INVALID_ENTRY", infraerrors.Reason(err), "entry %q", bad)
	}

	tooMany := make([]string, 0, MaxOIDCRequiredGroups+1)
	for i := 0; i <= MaxOIDCRequiredGroups; i++ {
		tooMany = append(tooMany, fmt.Sprintf("group-%d", i))
	}
	_, err = NormalizeOIDCRequiredGroups(tooMany)
	require.Equal(t, "OIDC_REQUIRED_GROUPS_TOO_MANY", infraerrors.Reason(err))
}

func TestOIDCRequiredGroupsDefaultsToNoRequirement(t *testing.T) {
	svc := NewSettingService(&panelRateLimitSettingRepo{values: map[string]string{}}, nil)
	groups, err := svc.GetOIDCRequiredGroups(context.Background())
	require.NoError(t, err)
	require.Empty(t, groups)

	svc = NewSettingService(&panelRateLimitSettingRepo{values: map[string]string{SettingKeyOIDCConnectRequiredGroups: "  "}}, nil)
	groups, err = svc.GetOIDCRequiredGroups(context.Background())
	require.NoError(t, err)
	require.Empty(t, groups)
}

func TestOIDCRequiredGroupsRoundTrip(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{}}
	svc := NewSettingService(repo, nil)

	saved, err := svc.SetOIDCRequiredGroups(context.Background(), []string{"staff.*", " staff.* ", "admins"})
	require.NoError(t, err)
	require.Equal(t, []string{"staff.*", "admins"}, saved)
	require.JSONEq(t, `["staff.*","admins"]`, repo.values[SettingKeyOIDCConnectRequiredGroups])

	groups, err := svc.GetOIDCRequiredGroups(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"staff.*", "admins"}, groups)

	saved, err = svc.SetOIDCRequiredGroups(context.Background(), []string{})
	require.NoError(t, err)
	require.Empty(t, saved)
	require.Equal(t, `[]`, repo.values[SettingKeyOIDCConnectRequiredGroups])

	_, err = svc.SetOIDCRequiredGroups(context.Background(), []string{"bad entry"})
	require.Error(t, err)
	require.Equal(t, `[]`, repo.values[SettingKeyOIDCConnectRequiredGroups], "a refused write leaves the saved value alone")
}

func TestOIDCRequiredGroupsUnreadableValueIsAnError(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":      `staff.*`,
		"not an array":  `{"groups":["staff.*"]}`,
		"invalid entry": `["staff .*"]`,
	} {
		t.Run(name, func(t *testing.T) {
			svc := NewSettingService(&panelRateLimitSettingRepo{values: map[string]string{SettingKeyOIDCConnectRequiredGroups: raw}}, nil)
			_, err := svc.GetOIDCRequiredGroups(context.Background())
			require.Error(t, err)
			require.Equal(t, "OIDC_REQUIRED_GROUPS_CORRUPT", infraerrors.Reason(err))
		})
	}

	svc := NewSettingService(&panelRateLimitSettingRepo{getValueErr: errors.New("db down")}, nil)
	_, err := svc.GetOIDCRequiredGroups(context.Background())
	require.Error(t, err)
}
