package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const llmLogCaptureLimit = 256 * 1024

// Embedding preserves Gin's Flush, Hijack, CloseNotify, status and size behavior.
// Only bytes successfully written to the client are copied, with a fixed limit.
type lifecycleResponseWriter struct {
	gin.ResponseWriter
	body      []byte
	truncated bool
}

func (w *lifecycleResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *lifecycleResponseWriter) capture(p []byte) {
	remaining := llmLogCaptureLimit - len(w.body)
	if len(p) > remaining {
		w.truncated = true
		p = p[:remaining]
	}
	w.body = append(w.body, p...)
}

func (w *lifecycleResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.capture(p[:n])
	return n, err
}

func (w *lifecycleResponseWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	// Slice before conversion to avoid copying an unbounded string.
	captureLen := min(n, llmLogCaptureLimit-len(w.body))
	w.capture([]byte(s[:captureLen]))
	if captureLen < n {
		w.truncated = true
	}
	return n, err
}

type gatewayRequestLifecycle struct {
	hook       RequestLifecycleHook
	requestCtx any
	writer     *lifecycleResponseWriter
	apiKey     *service.APIKey
	account    *service.Account
	result     *service.ForwardResult
	forwardErr error
}

func (h *GatewayHandler) beginRequestLifecycle(c *gin.Context, parsed *service.ParsedRequest, platform string, apiKey *service.APIKey) *gatewayRequestLifecycle {
	if h.lifecycleHook == nil || platform == service.PlatformGemini {
		return nil
	}
	requestCtx := h.lifecycleHook.OnRequestStart(c.Request.Context(), &RequestStartEvent{
		Body: parsed, Platform: platform, APIKey: apiKey, User: apiKey.User,
	})
	if requestCtx == nil {
		return nil
	}
	writer := &lifecycleResponseWriter{ResponseWriter: c.Writer}
	c.Writer = writer
	return &gatewayRequestLifecycle{hook: h.lifecycleHook, requestCtx: requestCtx, writer: writer, apiKey: apiKey}
}

// Usage recording can mutate the ForwardResult on a worker goroutine. Copy the
// logging fields before submitting that work, and omit upstream headers entirely.
func (l *gatewayRequestLifecycle) recordForwardResult(result *service.ForwardResult, err error) {
	l.forwardErr = err
	l.result = nil
	if result != nil {
		l.result = &service.ForwardResult{
			Model: result.Model, Usage: result.Usage, Duration: result.Duration,
			ClientDisconnect: result.ClientDisconnect,
		}
	}
}

func (l *gatewayRequestLifecycle) finish(c *gin.Context) {
	c.Writer = l.writer.ResponseWriter
	messages, streamError := llmResponseMessages(l.writer.body, l.writer.Header().Get("Content-Type"))
	err := l.forwardErr
	if err == nil && l.writer.Status() >= http.StatusBadRequest {
		err = fmt.Errorf("LLM request returned HTTP %d", l.writer.Status())
	}
	if err == nil && streamError {
		err = errors.New("LLM stream returned an error")
	}
	if err == nil && l.result == nil && len(messages) == 0 && !l.writer.truncated {
		err = errors.New("LLM request ended without a response")
	}
	if err == nil && (c.Request.Context().Err() != nil || (l.result != nil && l.result.ClientDisconnect)) {
		err = errors.New("LLM client disconnected")
	}
	if err != nil {
		l.hook.OnRequestError(c.Request.Context(), l.requestCtx, &RequestErrorEvent{
			Error: err, Result: l.result, Account: l.account, APIKey: l.apiKey, User: l.apiKey.User,
			ResponseMessages: messages, ResponseTruncated: l.writer.truncated,
		})
		return
	}
	l.hook.OnRequestSuccess(c.Request.Context(), l.requestCtx, &RequestSuccessEvent{
		Result: l.result, Account: l.account, APIKey: l.apiKey, User: l.apiKey.User,
		ResponseMessages: messages, ResponseTruncated: l.writer.truncated,
	})
}

func llmRequestMessages(parsed *service.ParsedRequest) ([]map[string]any, bool) {
	// Do not decode large image/file payloads just to log them. The marker makes
	// omission explicit, and no partial JSON or arbitrary top-level metadata leaks.
	if len(parsed.MessagesRaw())+len(parsed.SystemRaw()) > llmLogCaptureLimit {
		return []map[string]any{}, true
	}
	messages := []map[string]any{}
	if system, ok := parsed.SystemValue(); ok && system != nil {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	var input []map[string]any
	if err := parsed.DecodeMessages(&input); err != nil {
		return messages, true
	}
	for _, message := range input {
		messages = append(messages, map[string]any{"role": message["role"], "content": message["content"]})
	}
	return messages, false
}

type llmStreamBlock struct {
	content map[string]any
	deltas  map[string]*strings.Builder
}

// Messages endpoints use Anthropic JSON/SSE even when routed to another provider.
// Reconstruct message content, including tool inputs and thinking, from complete
// SSE lines. Incomplete data beyond the capture bound is explicitly marked above.
func llmResponseMessages(body []byte, contentType string) ([]map[string]any, bool) {
	if !strings.Contains(contentType, "text/event-stream") {
		var message map[string]any
		if json.Unmarshal(body, &message) != nil {
			return nil, false
		}
		if message["type"] == "error" || message["error"] != nil {
			return nil, true
		}
		if content, ok := message["content"]; ok {
			return []map[string]any{{"role": "assistant", "content": content}}, false
		}
		return nil, false
	}

	blocks := make(map[int]*llmStreamBlock)
	streamError := false
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var event struct {
			Type    string         `json:"type"`
			Index   int            `json:"index"`
			Block   map[string]any `json:"content_block"`
			Delta   map[string]any `json:"delta"`
			Message struct {
				Content []map[string]any `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
			continue
		}
		switch event.Type {
		case "error":
			streamError = true
		case "message_start":
			for index, content := range event.Message.Content {
				blocks[index] = &llmStreamBlock{content: content, deltas: make(map[string]*strings.Builder)}
			}
		case "content_block_start":
			if event.Block != nil {
				blocks[event.Index] = &llmStreamBlock{content: event.Block, deltas: make(map[string]*strings.Builder)}
			}
		case "content_block_delta":
			block := blocks[event.Index]
			if block == nil {
				continue
			}
			for _, key := range []string{"text", "thinking", "signature", "partial_json"} {
				if value, ok := event.Delta[key].(string); ok {
					builder := block.deltas[key]
					if builder == nil {
						builder = &strings.Builder{}
						if initial, ok := block.content[key].(string); ok {
							_, _ = builder.WriteString(initial)
						}
						block.deltas[key] = builder
					}
					_, _ = builder.WriteString(value)
				}
			}
		}
	}
	if len(blocks) == 0 {
		return nil, streamError
	}
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	content := make([]map[string]any, 0, len(indices))
	for _, index := range indices {
		block := blocks[index]
		for key, builder := range block.deltas {
			if key == "partial_json" {
				var input any
				if json.Unmarshal([]byte(builder.String()), &input) == nil {
					block.content["input"] = input
				}
			} else {
				block.content[key] = builder.String()
			}
		}
		content = append(content, block.content)
	}
	return []map[string]any{{"role": "assistant", "content": content}}, streamError
}
