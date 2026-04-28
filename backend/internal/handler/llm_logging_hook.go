package handler

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/client"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// LLMLoggingHook 实现 LLM 调用日志记录的 Hook
type LLMLoggingHook struct {
	llmLogClient      *client.LLMLogClient
	llmLoggingEnabled bool
}

// NewLLMLoggingHook 创建 LLM 日志记录 Hook
func NewLLMLoggingHook(llmLogClient *client.LLMLogClient, enabled bool) *LLMLoggingHook {
	return &LLMLoggingHook{
		llmLogClient:      llmLogClient,
		llmLoggingEnabled: enabled,
	}
}

// llmLoggingContext 保存 LLM 日志记录的上下文信息
type llmLoggingContext struct {
	llmLogID  int64
	platform  string
	startTime time.Time
}

// OnRequestStart 创建 LLM 调用日志记录
func (h *LLMLoggingHook) OnRequestStart(ctx context.Context, req *RequestStartEvent) interface{} {
	if !h.llmLoggingEnabled {
		return nil
	}

	// 只记录非 Gemini 平台的请求
	if req.Platform == service.PlatformGemini {
		return nil
	}

	// 尝试解析请求体为 ParsedRequest
	parsedReq, ok := req.Body.(*service.ParsedRequest)
	if !ok {
		// 如果不是 ParsedRequest 类型，跳过日志记录
		return nil
	}

	// 创建 LLM 调用日志（异步，不阻塞主流程）
	llmLogID := h.createLLMCallLog(ctx, parsedReq, req.User.ID)

	return &llmLoggingContext{
		llmLogID:  llmLogID,
		platform:  req.Platform,
		startTime: time.Now(),
	}
}

// OnRequestSuccess 更新 LLM 调用日志（成功）
func (h *LLMLoggingHook) OnRequestSuccess(ctx context.Context, requestCtx interface{}, event *RequestSuccessEvent) {
	if !h.llmLoggingEnabled {
		return
	}

	llmCtx, ok := requestCtx.(*llmLoggingContext)
	if !ok || llmCtx == nil || llmCtx.llmLogID == 0 {
		return
	}

	// 只记录非 Gemini 平台的请求
	if llmCtx.platform == service.PlatformGemini {
		return
	}

	// 异步更新日志（不阻塞主流程）
	go h.updateLLMCallLog(
		context.Background(),
		llmCtx.llmLogID,
		event.Result,
		nil, // errorMessage
		event.Account.ID,
		event.APIKey.ID,
		event.User.ID,
	)
}

// OnRequestError 更新 LLM 调用日志（失败）
func (h *LLMLoggingHook) OnRequestError(ctx context.Context, requestCtx interface{}, event *RequestErrorEvent) {
	if !h.llmLoggingEnabled {
		return
	}

	llmCtx, ok := requestCtx.(*llmLoggingContext)
	if !ok || llmCtx == nil || llmCtx.llmLogID == 0 {
		return
	}

	// 只记录非 Gemini 平台的请求
	if llmCtx.platform == service.PlatformGemini {
		return
	}

	// 异步更新日志（不阻塞主流程）
	errorMsg := event.Error.Error()
	accountID := int64(0)
	if event.Account != nil {
		accountID = event.Account.ID
	}

	go h.updateLLMCallLog(
		context.Background(),
		llmCtx.llmLogID,
		nil, // result
		&errorMsg,
		accountID,
		event.APIKey.ID,
		event.User.ID,
	)
}

// createLLMCallLog 创建 LLM 调用日志记录（内部方法）
func (h *LLMLoggingHook) createLLMCallLog(ctx context.Context, parsedReq *service.ParsedRequest, userID int64) int64 {
	if h.llmLogClient == nil {
		return 0
	}

	// 转换 messages 为 map 格式
	messages := make([]map[string]interface{}, 0, len(parsedReq.Messages))
	for _, msg := range parsedReq.Messages {
		msgMap := make(map[string]interface{})
		msgBytes, err := json.Marshal(msg)
		if err != nil {
			log.Printf("[LLM Logging] Failed to marshal message: %v", err)
			continue
		}
		if err := json.Unmarshal(msgBytes, &msgMap); err != nil {
			log.Printf("[LLM Logging] Failed to unmarshal message: %v", err)
			continue
		}
		messages = append(messages, msgMap)
	}

	req := &client.LLMCallCreateRequest{
		AgentName: h.llmLogClient.GetAgentName(),
		Model:     parsedReq.Model,
		Messages:  messages,
		Status:    "pending",
		Meta: map[string]interface{}{
			"user_id": userID,
		},
	}

	resp, err := h.llmLogClient.CreateLLMCall(ctx, req)
	if err != nil {
		log.Printf("[LLM Logging] Failed to create LLM call log: %v", err)
		return 0
	}

	return resp.ID
}

// updateLLMCallLog 更新 LLM 调用日志记录（内部方法）
func (h *LLMLoggingHook) updateLLMCallLog(
	ctx context.Context,
	llmLogID int64,
	result *service.ForwardResult,
	errorMessage *string,
	accountID int64,
	apiKeyID int64,
	userID int64,
) {
	if h.llmLogClient == nil || llmLogID == 0 {
		return
	}

	req := &client.LLMCallUpdateRequest{
		Meta: map[string]interface{}{
			"account_id": accountID,
			"api_key_id": apiKeyID,
			"user_id":    userID,
		},
	}

	if errorMessage != nil {
		// 请求失败
		req.Status = "error"
		req.ErrorMessage = errorMessage
	} else if result != nil {
		// 请求成功
		req.Status = "success"

		// 添加 token 使用量信息
		if result.Usage.InputTokens > 0 || result.Usage.OutputTokens > 0 {
			req.Meta["input_tokens"] = result.Usage.InputTokens
			req.Meta["output_tokens"] = result.Usage.OutputTokens
		}

		// 添加其他元数据
		if result.Model != "" {
			req.Meta["model"] = result.Model
		}
		if result.Duration > 0 {
			req.Meta["duration_ms"] = result.Duration.Milliseconds()
		}
	} else {
		// 无结果（可能是流式响应）
		req.Status = "completed"
	}

	if err := h.llmLogClient.UpdateLLMCall(ctx, llmLogID, req); err != nil {
		log.Printf("[LLM Logging] Failed to update LLM call log %d: %v", llmLogID, err)
	}
}
