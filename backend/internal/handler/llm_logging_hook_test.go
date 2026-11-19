package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	body   map[string]any
}

func requireLLMLogValue[T any](t *testing.T, value any) T {
	t.Helper()
	typed, ok := value.(T)
	require.Truef(t, ok, "expected %T, got %T", *new(T), value)
	return typed
}

func newTestLLMLogSink(t *testing.T) (*LLMLoggingHook, <-chan loggedCall) {
	t.Helper()
	calls := make(chan loggedCall, 10)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls <- loggedCall{r.Method, r.URL.Path, r.Header.Get("X-API-Key"), body}
		w.Header().Set("Content-Type", "application/json")
		// data-service's standard envelope: the created row sits under "data".
		_, _ = io.WriteString(w, `{"status":"success","message":null,"data":{"id":42,"status":"pending"}}`)
	}))
	t.Cleanup(sink.Close)
	return NewLLMLoggingHook(client.NewLLMLogClient(config.LLMLoggingConfig{
		URL: sink.URL + "/llm-calls/", AgentName: "test-proxy", APIKey: "sink-secret", TimeoutSeconds: 1,
	}), llmLoggingOn), calls
}

func llmLoggingOn(context.Context) bool  { return true }
func llmLoggingOff(context.Context) bool { return false }

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
		lifecycle.account = &service.Account{ID: 9, Credentials: map[string]any{"access_token": "account-secret"}}
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
	require.Equal(t, "/llm-calls/", create.path, "data-service's create route has the trailing slash")
	require.Equal(t, "sink-secret", create.key)
	require.Equal(t, "pending", create.body["status"])
	require.Equal(t, "test-proxy", create.body["agent_name"])
	require.Len(t, create.body["messages"], 2)
	require.Equal(t, http.MethodPut, update.method)
	require.Equal(t, "/llm-calls/42", update.path)
	require.Equal(t, "success", update.body["status"])
	meta := requireLLMLogValue[map[string]any](t, update.body["meta"])
	require.Equal(t, float64(12), meta["input_tokens"])
	require.Equal(t, float64(9), meta["account_id"])
	messages := requireLLMLogValue[[]any](t, update.body["response_messages"])
	require.Len(t, messages, 1)
	message := requireLLMLogValue[map[string]any](t, messages[0])
	content := requireLLMLogValue[[]any](t, message["content"])
	require.Len(t, content, 1)
	require.Contains(t, requireLLMLogValue[map[string]any](t, content[0]), "text")
	encoded, err := json.Marshal([]map[string]any{create.body, update.body})
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
	messages := requireLLMLogValue[[]any](t, update.body["response_messages"])
	require.Len(t, messages, 1)
	message := requireLLMLogValue[map[string]any](t, messages[0])
	content := requireLLMLogValue[[]any](t, message["content"])
	require.Len(t, content, 2)
	require.Equal(t, "hello", requireLLMLogValue[map[string]any](t, content[0])["text"])
	require.Equal(t, map[string]any{"q": "hello"}, requireLLMLogValue[map[string]any](t, content[1])["input"])
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
	require.Equal(t, true, requireLLMLogValue[map[string]any](t, update.body["meta"])["response_truncated"])

	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(`{"model":"claude-test","messages":[{"role":"user","content":"`+large+`"}]}`)), service.PlatformAnthropic)
	require.NoError(t, err)
	messages, truncated := llmRequestMessages(parsed)
	require.Empty(t, messages)
	require.True(t, truncated)

	sinkClient := client.NewLLMLogClient(config.LLMLoggingConfig{URL: "http://127.0.0.1:1/llm_calls"})
	for _, disabled := range []RequestLifecycleHook{
		nil, &NoOpHook{}, NewLLMLoggingHook(nil, llmLoggingOn), NewLLMLoggingHook(sinkClient, nil),
		NewLLMLoggingHook(sinkClient, llmLoggingOff), NewCompositeHook(NewLLMLoggingHook(sinkClient, llmLoggingOff)),
	} {
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
	hook := NewLLMLoggingHook(client.NewLLMLogClient(config.LLMLoggingConfig{URL: sink.URL, TimeoutSeconds: 1}), llmLoggingOn)
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
	withURL := &config.Config{LLMLogging: config.LLMLoggingConfig{URL: "https://example.invalid/llm_calls"}}
	settings := service.NewSettingService(&llmLoggingSettingRepo{}, withURL)
	for _, tc := range []struct {
		name     string
		cfg      *config.Config
		settings *service.SettingService
		wired    bool
	}{
		{"no config", nil, settings, false},
		{"missing URL", &config.Config{}, settings, false},
		{"no setting service", withURL, nil, false},
		{"URL configured", withURL, settings, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewGatewayHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, tc.cfg, tc.settings)
			require.Equal(t, tc.wired, h.lifecycleHook != nil)
		})
	}

	// Wired but the switch is off by default: the gateway skips the lifecycle.
	h := NewGatewayHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, withURL, settings)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}]}`)), service.PlatformAnthropic)
	require.NoError(t, err)
	require.Nil(t, h.beginRequestLifecycle(c, parsed, service.PlatformAnthropic, &service.APIKey{ID: 7, User: &service.User{ID: 8}}))
}

// llmLoggingSettingRepo is a SettingRepository with no rows: every setting reads as absent.
type llmLoggingSettingRepo struct{ service.SettingRepository }

func (llmLoggingSettingRepo) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}

// countingSink answers like data-service (201 on create, 200 on update) unless a
// failure status is given, and counts every request it receives.
func countingSink(t *testing.T, failure int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case failure != 0:
			w.WriteHeader(failure)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusOK)
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"id":42}}`)
	}))
	t.Cleanup(sink.Close)
	return sink, &hits
}

func TestLLMLoggingSwitchDecidesPerRequest(t *testing.T) {
	sink, hits := countingSink(t, 0)
	var on atomic.Bool
	hook := NewLLMLoggingHook(client.NewLLMLogClient(config.LLMLoggingConfig{URL: sink.URL + "/llm_calls", TimeoutSeconds: 1}),
		func(context.Context) bool { return on.Load() })
	var logged atomic.Int64
	hook.failures.printf = func(string, ...any) { logged.Add(1) }

	// Off: no capture, no delivery, no log line.
	c, _, lifecycle := testLLMLifecycle(t, hook)
	require.Nil(t, lifecycle)
	_, wrapped := c.Writer.(*lifecycleResponseWriter)
	require.False(t, wrapped)
	hook.OnRequestSuccess(context.Background(), nil, &RequestSuccessEvent{})
	hook.OnRequestError(context.Background(), nil, &RequestErrorEvent{})

	// On: the next request is delivered (create + update).
	on.Store(true)
	c, _, lifecycle = testLLMLifecycle(t, hook)
	require.NotNil(t, lifecycle)
	c.JSON(http.StatusOK, gin.H{"content": []gin.H{{"type": "text", "text": "hello"}}})
	lifecycle.finish(c)
	require.Eventually(t, func() bool { return hits.Load() == 2 && len(hook.workers) == 0 }, 5*time.Second, 10*time.Millisecond)

	// Off again: nothing more reaches the sink.
	on.Store(false)
	_, _, lifecycle = testLLMLifecycle(t, hook)
	require.Nil(t, lifecycle)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int64(2), hits.Load())
	require.Zero(t, logged.Load())
	require.Len(t, hook.workers, 0)
}

func TestLLMLoggingFailureLogCarriesReason(t *testing.T) {
	sink, hits := countingSink(t, http.StatusNotFound)
	hook := NewLLMLoggingHook(client.NewLLMLogClient(config.LLMLoggingConfig{
		URL: sink.URL + "/llm_calls", APIKey: "sink-secret", TimeoutSeconds: 1,
	}), llmLoggingOn)
	lines := make(chan string, 10)
	hook.failures.printf = func(format string, args ...any) { lines <- fmt.Sprintf(format, args...) }

	c, _, lifecycle := testLLMLifecycle(t, hook)
	c.JSON(http.StatusOK, gin.H{"content": "hello"})
	lifecycle.finish(c)
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("failure was not logged")
	}
	require.Equal(t, "[LLM Logging] create call log failed: unexpected status code: 404", line)
	require.Equal(t, int64(1), hits.Load(), "no update after a failed create")
	require.NotContains(t, line, "secret")
	require.NotContains(t, line, sink.URL)
}

func TestLLMLoggingFailureLogOmitsURL(t *testing.T) {
	// Nothing listens on port 1; the URL carries credentials that must not be logged.
	hook := NewLLMLoggingHook(client.NewLLMLogClient(config.LLMLoggingConfig{
		URL: "http://user:url-secret@127.0.0.1:1/llm_calls", TimeoutSeconds: 1,
	}), llmLoggingOn)
	lines := make(chan string, 10)
	hook.failures.printf = func(format string, args ...any) { lines <- fmt.Sprintf(format, args...) }

	c, _, lifecycle := testLLMLifecycle(t, hook)
	c.JSON(http.StatusOK, gin.H{"content": "hello"})
	lifecycle.finish(c)
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("failure was not logged")
	}
	require.True(t, strings.HasPrefix(line, "[LLM Logging] create call log failed: do request: "), line)
	require.NotContains(t, line, "secret")
	require.NotContains(t, line, "/llm_calls")
}

func TestLLMLoggingFailureLogIsRateLimited(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var lines []string
	failures := newLLMLogFailureLog(time.Minute)
	failures.now = func() time.Time { return now }
	failures.printf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	notFound := &client.StatusError{StatusCode: http.StatusNotFound}

	for i := 0; i < 1000; i++ {
		failures.report("create call log", notFound)
		now = now.Add(10 * time.Millisecond) // 1000 failures in 10s
	}
	require.Equal(t, []string{"[LLM Logging] create call log failed: unexpected status code: 404"}, lines)

	now = now.Add(time.Minute)
	failures.report("update call log", &client.StatusError{StatusCode: http.StatusUnprocessableEntity})
	require.Len(t, lines, 2)
	require.Equal(t, "[LLM Logging] update call log failed: unexpected status code: 422 (999 more failures since the previous line were not logged)", lines[1])

	failures.report("create call log", notFound)
	require.Len(t, lines, 2, "still inside the minute")
}
