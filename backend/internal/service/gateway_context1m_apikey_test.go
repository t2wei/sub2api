//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

func TestContext1M_APIKeyKeepsConfiguredFeatureBetas(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","thinking":{"type":"enabled"},"messages":[]}`)
	for _, endpoint := range []string{"messages", "count_tokens"} {
		for _, tc := range []struct {
			name             string
			inject           bool
			drop             map[string]struct{}
			wantContext      bool
			wantFeatureBetas bool
		}{
			{name: "merge feature and context betas", inject: true, wantContext: true, wantFeatureBetas: true},
			{name: "context beta without automatic feature injection", wantContext: true},
			{name: "context policy drop preserves feature betas", inject: true, drop: map[string]struct{}{claude.BetaContext1M: {}}, wantFeatureBetas: true},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				s := newTestGatewayServiceForBeta(tc.inject)
				compute := s.computeFinalAnthropicBeta
				if endpoint == "count_tokens" {
					compute = s.computeFinalCountTokensAnthropicBeta
				}
				beta, shouldSet := compute("apikey", false, "claude-opus-4-8", http.Header{}, body, tc.drop, claude.BetaContext1M)
				require.True(t, shouldSet)
				require.Equal(t, tc.wantContext, containsBetaToken(beta, claude.BetaContext1M), beta)
				for _, feature := range []string{claude.BetaInterleavedThinking, claude.BetaFineGrainedToolStreaming} {
					require.Equal(t, tc.wantFeatureBetas, containsBetaToken(beta, feature), "beta %s in %s", feature, beta)
				}
			})
		}
	}
}
