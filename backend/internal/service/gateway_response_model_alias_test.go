package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestClientFacingAnthropicResponseModel(t *testing.T) {
	tests := []struct {
		name          string
		upstreamModel string
		originalModel string
		mappedModel   string
		want          string
		wantChanged   bool
	}{
		{"openrouter prefix", "anthropic/claude-opus-5", "claude-opus-5", "claude-opus-5", "claude-opus-5", true},
		{"openrouter prefix and dotted version", "anthropic/claude-opus-4.8", "claude-opus-4-8", "claude-opus-4-8", "claude-opus-4-8", true},
		{"case only", "Anthropic/Claude-Sonnet-5", "claude-sonnet-5", "claude-sonnet-5", "claude-sonnet-5", true},
		{"exact echo of the mapped model", "vendor-opus", "claude-opus-5", "vendor-opus", "claude-opus-5", true},
		{"alias of the mapped model", "acme/vendor-opus-1.5", "claude-opus-5", "vendor-opus-1-5", "claude-opus-5", true},
		{"alias of the client model while mapped", "anthropic/claude-opus-5", "claude-opus-5", "claude-opus-5-custom", "claude-opus-5", true},
		{"same name", "claude-opus-5", "claude-opus-5", "claude-opus-5", "claude-opus-5", false},
		{"different model stays visible", "anthropic/claude-sonnet-5", "claude-opus-5", "claude-opus-5", "anthropic/claude-sonnet-5", false},
		{"different version stays visible", "anthropic/claude-opus-4.8", "claude-opus-5", "claude-opus-5", "anthropic/claude-opus-4.8", false},
		{"no declared model", "", "claude-opus-5", "claude-opus-5", "", false},
		{"no requested model", "anthropic/claude-opus-5", "", "", "anthropic/claude-opus-5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := clientFacingAnthropicResponseModel(tt.upstreamModel, tt.originalModel, tt.mappedModel)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.wantChanged, changed)
		})
	}
}

func newResponseModelAliasTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

func responseModelAliasSSE(model string) string {
	return strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"` + model + `","content":[],"usage":{"input_tokens":11,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
}

func responseModelAliasStreamResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func responseModelAliasJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

// responseModelAliasMessageStartModels returns message.model of every message_start data line the
// client received.
func responseModelAliasMessageStartModels(t *testing.T, sse string) []string {
	t.Helper()
	var models []string
	for _, line := range strings.Split(sse, "\n") {
		data, ok := extractAnthropicSSEDataLine(line)
		if !ok || gjson.Get(data, "type").Str != "message_start" {
			continue
		}
		models = append(models, gjson.Get(data, "message.model").Str)
	}
	return models
}

func TestHandleStreamingResponse_RestoresAliasedResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	svc := newStreamingResponseTestGatewayService()

	result, err := svc.handleStreamingResponse(context.Background(), responseModelAliasStreamResponse(responseModelAliasSSE("anthropic/claude-opus-5")), c, &Account{ID: 1}, time.Now(), "claude-opus-5", "claude-opus-5", false)

	require.NoError(t, err)
	require.Equal(t, 11, result.usage.InputTokens)
	require.Equal(t, 5, result.usage.OutputTokens)
	require.Equal(t, []string{"claude-opus-5"}, responseModelAliasMessageStartModels(t, rec.Body.String()))
	require.NotContains(t, rec.Body.String(), "anthropic/claude-opus-5")
	require.Equal(t, "anthropic/claude-opus-5", observedUpstreamResponseModel(c))
}

func TestHandleStreamingResponse_KeepsDifferentResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	svc := newStreamingResponseTestGatewayService()

	_, err := svc.handleStreamingResponse(context.Background(), responseModelAliasStreamResponse(responseModelAliasSSE("anthropic/claude-sonnet-5")), c, &Account{ID: 1}, time.Now(), "claude-opus-5", "claude-opus-5", false)

	require.NoError(t, err)
	require.Equal(t, []string{"anthropic/claude-sonnet-5"}, responseModelAliasMessageStartModels(t, rec.Body.String()))
	require.Equal(t, "anthropic/claude-sonnet-5", observedUpstreamResponseModel(c))
}

func TestHandleNonStreamingResponse_RestoresAliasedResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	svc := &GatewayService{cfg: &config.Config{}, rateLimitService: &RateLimitService{}}
	upstream := `{"id":"msg_1","type":"message","model":"anthropic/claude-opus-4.8","content":[],"usage":{"input_tokens":12,"output_tokens":7}}`

	usage, err := svc.handleNonStreamingResponse(context.Background(), responseModelAliasJSONResponse(upstream), c, &Account{ID: 1}, "claude-opus-4-8", "claude-opus-4-8")

	require.NoError(t, err)
	require.Equal(t, 7, usage.OutputTokens)
	require.JSONEq(t, `{"id":"msg_1","type":"message","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":12,"output_tokens":7}}`, rec.Body.String())
	require.Equal(t, "anthropic/claude-opus-4.8", observedUpstreamResponseModel(c))
}

func TestHandleNonStreamingResponse_KeepsDifferentResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	svc := &GatewayService{cfg: &config.Config{}, rateLimitService: &RateLimitService{}}
	upstream := `{"id":"msg_1","type":"message","model":"anthropic/claude-sonnet-5","content":[],"usage":{"input_tokens":12,"output_tokens":7}}`

	_, err := svc.handleNonStreamingResponse(context.Background(), responseModelAliasJSONResponse(upstream), c, &Account{ID: 1}, "claude-opus-5", "claude-opus-5")

	require.NoError(t, err)
	require.JSONEq(t, upstream, rec.Body.String())
	require.Equal(t, "anthropic/claude-sonnet-5", observedUpstreamResponseModel(c))
}

func TestHandleStreamingResponseAnthropicAPIKeyPassthrough_RestoresAliasedResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	svc := newStreamingResponseTestGatewayService()
	upstream := responseModelAliasSSE("anthropic/claude-opus-5")

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), responseModelAliasStreamResponse(upstream), c, &Account{ID: 1}, time.Now(), "claude-opus-5", "claude-opus-5")

	require.NoError(t, err)
	require.Equal(t, 11, result.usage.InputTokens)
	require.Equal(t, 5, result.usage.OutputTokens)
	// Only message.model changes; every other byte is passed through.
	want := strings.Replace(upstream, `"model":"anthropic/claude-opus-5"`, `"model":"claude-opus-5"`, 1)
	require.Equal(t, want, rec.Body.String())
	require.Equal(t, "anthropic/claude-opus-5", observedUpstreamResponseModel(c))
}

func TestHandleStreamingResponseAnthropicAPIKeyPassthrough_KeepsDifferentResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	svc := newStreamingResponseTestGatewayService()
	upstream := responseModelAliasSSE("anthropic/claude-sonnet-5")

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), responseModelAliasStreamResponse(upstream), c, &Account{ID: 1}, time.Now(), "claude-opus-5", "claude-opus-5")

	require.NoError(t, err)
	require.Equal(t, upstream, rec.Body.String())
	require.Equal(t, "anthropic/claude-sonnet-5", observedUpstreamResponseModel(c))
}

func TestHandleNonStreamingResponseAnthropicAPIKeyPassthrough_RestoresAliasedResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	svc := &GatewayService{cfg: &config.Config{}}
	upstream := `{"id":"msg_1","type":"message","model":"anthropic/claude-opus-4.8","content":[],"usage":{"input_tokens":5,"output_tokens":3}}`

	usage, err := svc.handleNonStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), responseModelAliasJSONResponse(upstream), c, &Account{ID: 2}, "claude-opus-4-8", "claude-opus-4-8")

	require.NoError(t, err)
	require.Equal(t, 3, usage.OutputTokens)
	require.Equal(t, `{"id":"msg_1","type":"message","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":5,"output_tokens":3}}`, rec.Body.String())
	require.Equal(t, "anthropic/claude-opus-4.8", observedUpstreamResponseModel(c))
}

func TestHandleNonStreamingResponseAnthropicAPIKeyPassthrough_KeepsDifferentResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	svc := &GatewayService{cfg: &config.Config{}}
	upstream := `{"id":"msg_1","type":"message","model":"anthropic/claude-sonnet-5","content":[],"usage":{"input_tokens":5,"output_tokens":3}}`

	_, err := svc.handleNonStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), responseModelAliasJSONResponse(upstream), c, &Account{ID: 2}, "claude-opus-5", "claude-opus-5")

	require.NoError(t, err)
	require.Equal(t, upstream, rec.Body.String())
	require.Equal(t, "anthropic/claude-sonnet-5", observedUpstreamResponseModel(c))
}

// The forwarded result keeps the raw upstream name for usage
// (upstream_response_model) while the client sees the requested name.
func TestForwardAnthropicAPIKeyPassthrough_RecordsRawUpstreamResponseModel(t *testing.T) {
	c, rec := newResponseModelAliasTestContext()
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: responseModelAliasJSONResponse(`{"id":"msg_1","type":"message","model":"anthropic/claude-opus-5","content":[],"usage":{"input_tokens":5,"output_tokens":3}}`),
	}
	svc := &GatewayService{
		cfg:              &config.Config{},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{},
	}
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, "claude-opus-5", "claude-opus-5", false, time.Now())

	require.NoError(t, err)
	require.Equal(t, "claude-opus-5", gjson.Get(rec.Body.String(), "model").Str)
	require.Equal(t, "claude-opus-5", result.Model)
	require.Equal(t, "anthropic/claude-opus-5", result.UpstreamResponseModel)
}
