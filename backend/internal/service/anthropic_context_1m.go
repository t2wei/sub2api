package service

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

type anthropicContext1MKey struct{}

func WithAnthropicContext1M(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, anthropicContext1MKey{}, true)
}

func AnthropicContext1MRequested(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, ok := ctx.Value(anthropicContext1MKey{}).(bool)
	return ok && enabled
}

func withAnthropicContext1MFromParsed(ctx context.Context, parsed *ParsedRequest) context.Context {
	if parsed != nil && parsed.ClaudeCodeLongContext1M {
		return WithAnthropicContext1M(ctx)
	}
	return ctx
}

func anthropicContext1MRequiredBetas(ctx context.Context) []string {
	if !AnthropicContext1MRequested(ctx) {
		return nil
	}
	return []string{claude.BetaContext1M}
}

func mergeAnthropicContext1MIntoBeta(ctx context.Context, beta string, account *Account) string {
	if !AnthropicContext1MRequested(ctx) {
		return beta
	}
	if account != nil {
		if _, ok := account.HeaderOverrideValue("anthropic-beta"); ok {
			return beta
		}
	}
	return mergeAnthropicBeta(anthropicContext1MRequiredBetas(ctx), beta)
}

func applyAnthropicContext1MHeader(ctx context.Context, req *http.Request, account *Account) {
	if req == nil || !AnthropicContext1MRequested(ctx) {
		return
	}
	if account != nil {
		if _, ok := account.HeaderOverrideValue("anthropic-beta"); ok {
			return
		}
	}
	beta := mergeAnthropicBeta(anthropicContext1MRequiredBetas(ctx), getHeaderRaw(req.Header, "anthropic-beta"))
	deleteHeaderAllForms(req.Header, "anthropic-beta")
	if beta != "" {
		setHeaderRaw(req.Header, "anthropic-beta", beta)
	}
}
