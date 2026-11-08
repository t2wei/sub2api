package handler

import (
	"context"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/client"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// LLMLoggingHook sends completed calls to the configured sink. Delivery is best
// effort and bounded: a slow sink never blocks inference or grows a work queue.
type LLMLoggingHook struct {
	llmLogClient      *client.LLMLogClient
	llmLoggingEnabled bool
	workers           chan struct{}
}

func NewLLMLoggingHook(llmLogClient *client.LLMLogClient, enabled bool) *LLMLoggingHook {
	return &LLMLoggingHook{
		llmLogClient: llmLogClient, llmLoggingEnabled: enabled,
		workers: make(chan struct{}, 16),
	}
}

type llmLoggingContext struct {
	request   *client.LLMCallCreateRequest
	startTime time.Time
}

func (h *LLMLoggingHook) OnRequestStart(_ context.Context, req *RequestStartEvent) any {
	if !h.llmLoggingEnabled || h.llmLogClient == nil || req == nil || req.Platform == service.PlatformGemini {
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
	llmCtx, ok := requestCtx.(*llmLoggingContext)
	if !h.llmLoggingEnabled || h.llmLogClient == nil || !ok || llmCtx == nil {
		return
	}
	if _, exists := update.Meta["duration_ms"]; !exists {
		update.Meta["duration_ms"] = time.Since(llmCtx.startTime).Milliseconds()
	}
	select {
	case h.workers <- struct{}{}:
	default:
		log.Print("[LLM Logging] Sink busy; dropping call log")
		return
	}
	// Snapshotting is complete: neither gin.Context, mutable request bodies,
	// accounts, API keys nor upstream headers are retained by this goroutine.
	go func() {
		defer func() { <-h.workers }()
		ctx := context.Background() // client cancellation must not cancel audit delivery
		created, err := h.llmLogClient.CreateLLMCall(ctx, llmCtx.request)
		if err != nil || created == nil || created.ID <= 0 {
			log.Print("[LLM Logging] Failed to create call log")
			return
		}
		if err := h.llmLogClient.UpdateLLMCall(ctx, created.ID, update); err != nil {
			log.Print("[LLM Logging] Failed to update call log")
		}
	}()
}
