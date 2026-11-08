package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/client"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type loggedCall struct {
	method string
	path   string
	key    string
	body   map[string]interface{}
}

func newTestLLMLogSink(t *testing.T) (*LLMLoggingHook, <-chan loggedCall) {
	t.Helper()
	calls := make(chan loggedCall, 10)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls <- loggedCall{r.Method, r.URL.Path, r.Header.Get("X-API-Key"), body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	t.Cleanup(sink.Close)
	return NewLLMLoggingHook(client.NewLLMLogClient(config.LLMLoggingConfig{
		URL: sink.URL + "/llm-calls/", AgentName: "test-proxy", APIKey: "sink-secret", TimeoutSeconds: 1,
	}), true), calls
}

func receiveLLMCall(t *testing.T, calls <-chan loggedCall) loggedCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("logging sink did not receive call")
		return loggedCall{}
	}
}

func testLLMLifecycle(t *testing.T, hook RequestLifecycleHook) (*gin.Context, *httptest.ResponseRecorder, *gatewayRequestLifecycle) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("Authorization", "Bearer client-secret")
	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(`{"model":"claude-test","system":"be concise","messages":[{"role":"user","content":"hello"}],"api_key":"body-secret","metadata":{"token":"metadata-secret"}}`)), service.PlatformAnthropic)
	require.NoError(t, err)
	h := &GatewayHandler{lifecycleHook: hook}
	lifecycle := h.beginRequestLifecycle(c, parsed, service.PlatformAnthropic, &service.APIKey{
		ID: 7, Key: "api-key-secret", User: &service.User{ID: 8},
	})
	if lifecycle != nil {
		lifecycle.account = &service.Account{ID: 9, Credentials: map[string]interface{}{"access_token": "account-secret"}}
	}
	return c, rec, lifecycle
}

func TestLLMLoggingJSONDelivery(t *testing.T) {
	hook, calls := newTestLLMLogSink(t)
	c, rec, lifecycle := testLLMLifecycle(t, hook)
	body := `{"type":"message","role":"assistant","content":[{"type":"text","text":"hello back"}],"credentials":"response-secret"}`
	c.Header("Content-Type", "application/json")
	_, err := c.Writer.WriteString(body)
	require.NoError(t, err)
	result := &service.ForwardResult{Model: "claude-test", Usage: service.ClaudeUsage{InputTokens: 12, OutputTokens: 3}, Duration: time.Second}
	lifecycle.recordForwardResult(result, nil)
	result.Usage.InputTokens = 0 // later cache-billing bookkeeping must not alter the captured usage
	lifecycle.finish(c)
	require.Equal(t, body, rec.Body.String())
	require.Equal(t, http.StatusOK, rec.Code)
	create := receiveLLMCall(t, calls)
	update := receiveLLMCall(t, calls)
	require.Equal(t, http.MethodPost, create.method)
	require.Equal(t, "/llm-calls", create.path)
	require.Equal(t, "sink-secret", create.key)
	require.Equal(t, "pending", create.body["status"])
	require.Equal(t, "test-proxy", create.body["agent_name"])
	require.Len(t, create.body["messages"], 2)
	require.Equal(t, http.MethodPut, update.method)
	require.Equal(t, "/llm-calls/42", update.path)
	require.Equal(t, "success", update.body["status"])
	require.Equal(t, float64(12), update.body["meta"].(map[string]interface{})["input_tokens"])
	require.Equal(t, float64(9), update.body["meta"].(map[string]interface{})["account_id"])
	require.Contains(t, update.body["response_messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})[0], "text")
	encoded, err := json.Marshal([]map[string]interface{}{create.body, update.body})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret")
}

func TestLLMLoggingStreamPreservesFlushAndCapturesTools(t *testing.T) {
	hook, calls := newTestLLMLogSink(t)
	c, rec, lifecycle := testLLMLifecycle(t, hook)
	c.Header("Content-Type", "text/event-stream")
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n",
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"lookup\",\"input\":{}}}\n\n",
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"hello\\\"}\"}}\n\n",
		"data: {\"type\":\"message_stop\"}\n\n",
	}
	for _, chunk := range chunks {
		// Deliberately split JSON in the middle of individual writes.
		_, _ = c.Writer.Write([]byte(chunk[:len(chunk)/2]))
		_, _ = c.Writer.WriteString(chunk[len(chunk)/2:])
		c.Writer.Flush()
	}
	require.True(t, rec.Flushed)
	require.Equal(t, strings.Join(chunks, ""), rec.Body.String())
	lifecycle.finish(c)
	_ = receiveLLMCall(t, calls)
	update := receiveLLMCall(t, calls)
	require.Equal(t, "success", update.body["status"])
	messages := update.body["response_messages"].([]interface{})
	content := messages[0].(map[string]interface{})["content"].([]interface{})
	require.Equal(t, "hello", content[0].(map[string]interface{})["text"])
	require.Equal(t, map[string]interface{}{"q": "hello"}, content[1].(map[string]interface{})["input"])
}

func TestLLMLoggingErrorsAndCancellation(t *testing.T) {
	for _, scenario := range []string{"http", "stream", "forward", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			hook, calls := newTestLLMLogSink(t)
			c, _, lifecycle := testLLMLifecycle(t, hook)
			switch scenario {
			case "http":
				c.JSON(http.StatusBadGateway, gin.H{"type": "error", "error": gin.H{"message": "upstream-secret"}})
			case "stream":
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString("data: {\"type\":\"error\",\"error\":{\"message\":\"upstream-secret\"}}\n\n")
			case "forward":
				lifecycle.forwardErr = errors.New("https://user:upstream-secret@example.invalid")
			case "cancel":
				ctx, cancel := context.WithCancel(c.Request.Context())
				c.Request = c.Request.WithContext(ctx)
				cancel()
			}
			lifecycle.finish(c)
			_ = receiveLLMCall(t, calls)
			update := receiveLLMCall(t, calls)
			require.Equal(t, "error", update.body["status"])
			body, err := json.Marshal(update.body)
			require.NoError(t, err)
			require.NotContains(t, string(body), "upstream-secret")
		})
	}
}

func TestLLMLoggingCaptureBoundAndDisabled(t *testing.T) {
	hook, calls := newTestLLMLogSink(t)
	c, rec, lifecycle := testLLMLifecycle(t, hook)
	large := strings.Repeat("x", 2*llmLogCaptureLimit)
	_, _ = c.Writer.WriteString(large)
	require.Len(t, lifecycle.writer.body, llmLogCaptureLimit)
	require.True(t, lifecycle.writer.truncated)
	require.Equal(t, large, rec.Body.String())
	lifecycle.finish(c)
	_ = receiveLLMCall(t, calls)
	update := receiveLLMCall(t, calls)
	require.Equal(t, true, update.body["meta"].(map[string]interface{})["response_truncated"])

	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(`{"model":"claude-test","messages":[{"role":"user","content":"`+large+`"}]}`)), service.PlatformAnthropic)
	require.NoError(t, err)
	messages, truncated := llmRequestMessages(parsed)
	require.Empty(t, messages)
	require.True(t, truncated)

	for _, disabled := range []RequestLifecycleHook{nil, &NoOpHook{}, NewLLMLoggingHook(nil, false)} {
		c, _, lifecycle := testLLMLifecycle(t, disabled)
		require.Nil(t, lifecycle)
		_, wrapped := c.Writer.(*lifecycleResponseWriter)
		require.False(t, wrapped)
	}
	require.Nil(t, hook.OnRequestStart(context.Background(), &RequestStartEvent{Body: parsed, Platform: service.PlatformGemini}))
}

func TestLLMLoggingSlowSinkDoesNotBlockRequests(t *testing.T) {
	entered := make(chan struct{}, 1)
	updated := make(chan struct{}, 1)
	release := make(chan struct{})
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			entered <- struct{}{}
			<-release
		}
		_, _ = io.WriteString(w, `{"id":42}`)
		if r.Method == http.MethodPut {
			updated <- struct{}{}
		}
	}))
	defer sink.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	hook := NewLLMLoggingHook(client.NewLLMLogClient(config.LLMLoggingConfig{URL: sink.URL, TimeoutSeconds: 1}), true)
	c, _, lifecycle := testLLMLifecycle(t, hook)
	c.JSON(http.StatusOK, gin.H{"content": "hello"})
	done := make(chan struct{})
	go func() { lifecycle.finish(c); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request blocked on logging sink")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("logging delivery did not start")
	}
	// Saturation drops delivery instead of queuing another goroutine.
	for len(hook.workers) < cap(hook.workers) {
		hook.workers <- struct{}{}
	}
	hook.OnRequestSuccess(context.Background(), lifecycle.requestCtx, &RequestSuccessEvent{})
	require.Len(t, hook.workers, cap(hook.workers))
	close(release)
	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("logging update did not finish after releasing sink")
	}
}

func TestGatewayLLMLoggingConfigurationGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     *config.Config
		enabled bool
	}{
		{"no config", nil, false},
		{"disabled", &config.Config{LLMLogging: config.LLMLoggingConfig{URL: "https://example.invalid"}}, false},
		{"missing URL", &config.Config{LLMLogging: config.LLMLoggingConfig{Enabled: true}}, false},
		{"enabled", &config.Config{LLMLogging: config.LLMLoggingConfig{Enabled: true, URL: "https://example.invalid"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewGatewayHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, tc.cfg, nil)
			require.Equal(t, tc.enabled, h.lifecycleHook != nil)
		})
	}
}
