//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiForwardNative_EmbeddingActionsPreservePayloadAndInputUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		action   string
		request  string
		response string
	}{
		{
			action:   "embedContent",
			request:  `{"model":"models/gemini-embedding-001","content":{"parts":[{"text":"Oxford"}]},"taskType":"RETRIEVAL_DOCUMENT","outputDimensionality":256}`,
			response: `{"embedding":{"values":[0.1,0.2]},"usageMetadata":{"promptTokenCount":12}}`,
		},
		{
			action:   "batchEmbedContents",
			request:  `{"requests":[{"model":"models/gemini-embedding-001","content":{"parts":[{"text":"Oxford"}]}},{"model":"models/gemini-embedding-001","content":{"parts":[{"text":"Singapore"}]}}]}`,
			response: `{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}],"usageMetadata":{"promptTokenCount":12}}`,
		},
	} {
		t.Run(tc.action, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{
				responses: []*http.Response{{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(tc.response)),
				}},
				onCall: func(req *http.Request, _ *queuedHTTPUpstreamStub) {
					require.Equal(t, "/v1beta/models/gemini-embedding-001:"+tc.action, req.URL.Path)
					require.Equal(t, http.MethodPost, req.Method)
				},
			}
			svc := &GeminiMessagesCompatService{httpUpstream: upstream, cfg: &config.Config{}}
			c, rec := newGeminiNativeTestContext(t)
			result, err := svc.ForwardNative(context.Background(), c, geminiSignalTestAccount(),
				"gemini-embedding-001", tc.action, false, []byte(tc.request))
			require.NoError(t, err)
			require.Len(t, upstream.requestBodies, 1)
			require.JSONEq(t, tc.request, string(upstream.requestBodies[0]))
			require.Equal(t, http.StatusOK, rec.Code)
			require.JSONEq(t, tc.response, rec.Body.String())
			require.Equal(t, 12, result.Usage.InputTokens)
			require.Zero(t, result.Usage.OutputTokens)
			require.Empty(t, GetOpsStreamErrors(c))
			require.Empty(t, upstreamErrorEventsFromContext(t, c))
		})
	}
}
