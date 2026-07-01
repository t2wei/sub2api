package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexModelCatalogOmitsEmbeddingFamilies(t *testing.T) {
	t.Parallel()

	models := []string{
		"gpt-6-sol", "embedding-helper",
		"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002",
		"gemini-embedding-001", "text-embedding-004", "embedding-001",
		"openai/text-embedding-3-large", "models/gemini-embedding-001",
	}
	want := []string{"gpt-6-sol", "embedding-helper"}
	group := &Group{ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: models}}
	require.Equal(t, want, FilterCodexModelIDsForGroup(models, group))

	body, err := BuildCodexModelsManifest(models)
	require.NoError(t, err)
	require.Equal(t, want, codexManifestModelSlugs(t, body))
}

func TestBuildCodexModelsManifestForGroupOmitsEmbeddingTargetAliases(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		platform string
		target   string
		chat     string
	}{
		{platform: PlatformOpenAI, target: "openai/text-embedding-3-large", chat: "gpt-6-sol"},
		{platform: PlatformGemini, target: "models/gemini-embedding-001", chat: "gemini-2.5-pro"},
	} {
		for _, groupPlatform := range []string{tc.platform, PlatformComposite} {
			t.Run(tc.platform+"/"+groupPlatform, func(t *testing.T) {
				const groupID int64 = 7351
				svc := &GatewayService{accountRepo: codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{
					groupID: {{
						ID: 1, Platform: tc.platform, Type: AccountTypeAPIKey,
						Credentials: map[string]any{"model_mapping": map[string]any{
							"vectors": tc.target, "embedding-helper": tc.chat,
						}},
					}},
				}}}
				group := &Group{
					ID: groupID, Platform: groupPlatform,
					ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"vectors", "embedding-helper"}},
				}
				body, err := svc.BuildCodexModelsManifestForGroup(
					context.Background(), group, "", FilterCodexModelIDsForGroup(group.ModelAllowlist.Models, group),
				)
				require.NoError(t, err)
				require.Equal(t, []string{"embedding-helper"}, codexManifestModelSlugs(t, body))
			})
		}
	}
}
