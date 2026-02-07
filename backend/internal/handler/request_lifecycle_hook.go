package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// RequestLifecycleHook 定义请求生命周期的钩子接口（扩展点）
// 符合开闭原则：对扩展开放，对修改封闭
type RequestLifecycleHook interface {
	// OnRequestStart 在请求开始时调用
	// 返回一个 opaque context 值，用于在后续 Hook 调用中关联上下文
	OnRequestStart(ctx context.Context, req *RequestStartEvent) interface{}

	// OnRequestSuccess 在请求成功时调用
	OnRequestSuccess(ctx context.Context, requestCtx interface{}, event *RequestSuccessEvent)

	// OnRequestError 在请求失败时调用
	OnRequestError(ctx context.Context, requestCtx interface{}, event *RequestErrorEvent)
}

// RequestStartEvent 请求开始事件
type RequestStartEvent struct {
	Body     interface{}  // 请求体（可能是 *service.ParsedRequest 或其他类型）
	Platform string       // 平台名称
	APIKey   *ent.APIKey  // API Key
	User     *ent.User    // 用户信息
}

// RequestSuccessEvent 请求成功事件
type RequestSuccessEvent struct {
	Result  *service.ForwardResult // 转发结果
	Account *ent.Account           // 使用的账号
	APIKey  *ent.APIKey            // API Key
	User    *ent.User              // 用户信息
}

// RequestErrorEvent 请求失败事件
type RequestErrorEvent struct {
	Error   error        // 错误信息
	Account *ent.Account // 使用的账号（可能为 nil）
	APIKey  *ent.APIKey  // API Key
	User    *ent.User    // 用户信息
}

// NoOpHook 是一个空操作的 Hook 实现（默认实现）
type NoOpHook struct{}

func (h *NoOpHook) OnRequestStart(ctx context.Context, req *RequestStartEvent) interface{} {
	return nil
}

func (h *NoOpHook) OnRequestSuccess(ctx context.Context, requestCtx interface{}, event *RequestSuccessEvent) {
}

func (h *NoOpHook) OnRequestError(ctx context.Context, requestCtx interface{}, event *RequestErrorEvent) {
}

// CompositeHook 组合多个 Hook，按顺序执行
type CompositeHook struct {
	hooks []RequestLifecycleHook
}

// NewCompositeHook 创建一个组合 Hook
func NewCompositeHook(hooks ...RequestLifecycleHook) *CompositeHook {
	return &CompositeHook{
		hooks: hooks,
	}
}

func (h *CompositeHook) OnRequestStart(ctx context.Context, req *RequestStartEvent) interface{} {
	// 为每个子 Hook 创建一个 context map
	contexts := make(map[int]interface{})
	for i, hook := range h.hooks {
		contexts[i] = hook.OnRequestStart(ctx, req)
	}
	return contexts
}

func (h *CompositeHook) OnRequestSuccess(ctx context.Context, requestCtx interface{}, event *RequestSuccessEvent) {
	contexts, ok := requestCtx.(map[int]interface{})
	if !ok {
		// 如果类型不匹配，尝试直接传递（兼容性处理）
		for _, hook := range h.hooks {
			hook.OnRequestSuccess(ctx, requestCtx, event)
		}
		return
	}

	for i, hook := range h.hooks {
		hook.OnRequestSuccess(ctx, contexts[i], event)
	}
}

func (h *CompositeHook) OnRequestError(ctx context.Context, requestCtx interface{}, event *RequestErrorEvent) {
	contexts, ok := requestCtx.(map[int]interface{})
	if !ok {
		// 如果类型不匹配，尝试直接传递（兼容性处理）
		for _, hook := range h.hooks {
			hook.OnRequestError(ctx, requestCtx, event)
		}
		return
	}

	for i, hook := range h.hooks {
		hook.OnRequestError(ctx, contexts[i], event)
	}
}
