package handler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/client"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// LLMLoggingSwitch reports whether call logging is on. It is asked once per request,
// so it must be cheap (SettingService.LLMLoggingEnabled serves it from a cache).
type LLMLoggingSwitch func(ctx context.Context) bool

// LLMLoggingHook sends completed calls to the configured sink while the runtime
// switch is on. The switch is read when a request starts: while it is off the hook
// captures nothing, starts no goroutine, makes no HTTP call and logs nothing.
// Delivery is best effort and bounded: a slow sink never blocks inference or grows a
// work queue.
type LLMLoggingHook struct {
	llmLogClient *client.LLMLogClient
	enabled      LLMLoggingSwitch
	workers      chan struct{}
	failures     *llmLogFailureLog
}

func NewLLMLoggingHook(llmLogClient *client.LLMLogClient, enabled LLMLoggingSwitch) *LLMLoggingHook {
	return &LLMLoggingHook{
		llmLogClient: llmLogClient, enabled: enabled,
		workers:  make(chan struct{}, llmLogMaxInFlight),
		failures: newLLMLogFailureLog(llmLogFailureLogInterval),
	}
}

// llmLogMaxInFlight caps concurrent deliveries; a call beyond it is dropped.
const llmLogMaxInFlight = 16

var (
	errSinkBusy = fmt.Errorf("sink busy (%d deliveries in flight), call dropped", llmLogMaxInFlight)
	errNoCallID = errors.New("sink returned no call id")
)

// llmLogFailureLogInterval bounds failure logging to one line per interval, so a
// broken sink cannot flood the log with a line per request.
const llmLogFailureLogInterval = time.Minute

// llmLogFailureLog writes the first delivery failure at once and then at most one
// line per interval, carrying the reason (status code or transport cause; never
// request or response content, keys or headers) and how many failures were not
// logged since the previous line.
type llmLogFailureLog struct {
	mu         sync.Mutex
	interval   time.Duration
	now        func() time.Time
	printf     func(format string, args ...any)
	lastAt     time.Time
	suppressed int64
}

func newLLMLogFailureLog(interval time.Duration) *llmLogFailureLog {
	return &llmLogFailureLog{
		interval: interval,
		now:      time.Now,
		printf: func(format string, args ...any) {
			logger.LegacyPrintf("handler.llm_logging", format, args...)
		},
	}
}

func (l *llmLogFailureLog) report(stage string, err error) {
	l.mu.Lock()
	now := l.now()
	if !l.lastAt.IsZero() && now.Sub(l.lastAt) < l.interval {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed
	l.suppressed = 0
	l.lastAt = now
	l.mu.Unlock()

	reason := "unknown error"
	if err != nil {
		reason = err.Error()
	}
	if suppressed > 0 {
		l.printf("[LLM Logging] %s failed: %s (%d more failures since the previous line were not logged)", stage, reason, suppressed)
		return
	}
	l.printf("[LLM Logging] %s failed: %s", stage, reason)
}

type llmLoggingContext struct {
	request   *client.LLMCallCreateRequest
	startTime time.Time
}

func (h *LLMLoggingHook) OnRequestStart(ctx context.Context, req *RequestStartEvent) any {
	if h.llmLogClient == nil || h.enabled == nil || req == nil || req.Platform == service.PlatformGemini {
		return nil
	}
	if !h.enabled(ctx) {
		return nil
	}
	parsedReq, ok := req.Body.(*service.ParsedRequest)
	if !ok || parsedReq == nil {
		return nil
	}
	messages, truncated := llmRequestMessages(parsedReq)
	meta := map[string]any{"request_truncated": truncated}
	if req.User != nil {
		meta["user_id"] = req.User.ID
	}
	if req.APIKey != nil {
		meta["api_key_id"] = req.APIKey.ID
	}
	return &llmLoggingContext{
		request: &client.LLMCallCreateRequest{
			AgentName: h.llmLogClient.GetAgentName(), Model: parsedReq.Model,
			Messages: messages, Status: "pending", Meta: meta,
		},
		startTime: time.Now(),
	}
}

func (h *LLMLoggingHook) OnRequestSuccess(_ context.Context, requestCtx any, event *RequestSuccessEvent) {
	if event == nil {
		return
	}
	req := &client.LLMCallUpdateRequest{
		Status: "success", ResponseMessages: event.ResponseMessages,
		Meta: llmResultMeta(event.Result, event.Account, event.APIKey, event.User, event.ResponseTruncated),
	}
	h.deliver(requestCtx, req)
}

func (h *LLMLoggingHook) OnRequestError(_ context.Context, requestCtx any, event *RequestErrorEvent) {
	if event == nil {
		return
	}
	// Transport errors can contain credential-bearing URLs or upstream bodies.
	// Record a safe classification; never serialize raw upstream errors/headers.
	message := "LLM request failed"
	req := &client.LLMCallUpdateRequest{
		Status: "error", ErrorMessage: &message, ResponseMessages: event.ResponseMessages,
		Meta: llmResultMeta(event.Result, event.Account, event.APIKey, event.User, event.ResponseTruncated),
	}
	h.deliver(requestCtx, req)
}

func llmResultMeta(result *service.ForwardResult, account *service.Account, apiKey *service.APIKey, user *service.User, truncated bool) map[string]any {
	meta := map[string]any{"response_truncated": truncated}
	if account != nil {
		meta["account_id"] = account.ID
	}
	if apiKey != nil {
		meta["api_key_id"] = apiKey.ID
	}
	if user != nil {
		meta["user_id"] = user.ID
	}
	if result != nil {
		meta["input_tokens"] = result.Usage.InputTokens
		meta["output_tokens"] = result.Usage.OutputTokens
		meta["model"] = result.Model
		meta["duration_ms"] = result.Duration.Milliseconds()
	}
	return meta
}

func (h *LLMLoggingHook) deliver(requestCtx any, update *client.LLMCallUpdateRequest) {
	// A request context exists only if the switch was on when the request started;
	// such a request is delivered even if the switch is turned off meanwhile.
	llmCtx, ok := requestCtx.(*llmLoggingContext)
	if h.llmLogClient == nil || !ok || llmCtx == nil {
		return
	}
	if _, exists := update.Meta["duration_ms"]; !exists {
		update.Meta["duration_ms"] = time.Since(llmCtx.startTime).Milliseconds()
	}
	select {
	case h.workers <- struct{}{}:
	default:
		h.failures.report("call log delivery", errSinkBusy)
		return
	}
	// Snapshotting is complete: neither gin.Context, mutable request bodies,
	// accounts, API keys nor upstream headers are retained by this goroutine.
	go func() {
		defer func() { <-h.workers }()
		ctx := context.Background() // client cancellation must not cancel audit delivery
		created, err := h.llmLogClient.CreateLLMCall(ctx, llmCtx.request)
		if err == nil && (created == nil || created.ID <= 0) {
			err = errNoCallID
		}
		if err != nil {
			h.failures.report("create call log", err)
			return
		}
		if err := h.llmLogClient.UpdateLLMCall(ctx, created.ID, update); err != nil {
			h.failures.report("update call log", err)
		}
	}()
}
