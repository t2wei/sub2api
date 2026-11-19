// Package client provides HTTP clients for external services.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// LLMLogClient is an HTTP client for the data-service LLM logging API.
type LLMLogClient struct {
	baseURL    string
	httpClient *http.Client
	agentName  string
	apiKey     string // [OXSCI] API Key for Lambda Proxy access
}

// NewLLMLogClient creates a new LLM log client.
func NewLLMLogClient(cfg config.LLMLoggingConfig) *LLMLogClient {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return &LLMLogClient{
		baseURL:   strings.TrimRight(cfg.URL, "/"),
		agentName: cfg.AgentName,
		apiKey:    cfg.APIKey, // [OXSCI] Store API Key
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// GetAgentName returns the agent name for this client
func (c *LLMLogClient) GetAgentName() string {
	return c.agentName
}

// LLMCallCreateRequest represents the request to create an LLM call log.
type LLMCallCreateRequest struct {
	ContextID *string          `json:"context_id,omitempty"`
	AgentName string           `json:"agent_name"`
	Model     string           `json:"model"`
	Messages  []map[string]any `json:"messages"`
	Status    string           `json:"status"`
	Meta      map[string]any   `json:"meta,omitempty"`
}

// LLMCallUpdateRequest represents the request to update an LLM call log.
type LLMCallUpdateRequest struct {
	ResponseMessages []map[string]any `json:"response_messages,omitempty"`
	Status           string           `json:"status"`
	ErrorMessage     *string          `json:"error_message,omitempty"`
	Meta             map[string]any   `json:"meta,omitempty"`
}

// LLMCallCreateResponse represents the response from creating an LLM call log.
type LLMCallCreateResponse struct {
	ID int64 `json:"id"`
}

// llmCallCreateEnvelope accepts data-service's standard response, which carries the
// created row under "data", as well as a bare {"id": ...}.
type llmCallCreateEnvelope struct {
	ID   int64                  `json:"id"`
	Data *LLMCallCreateResponse `json:"data"`
}

// StatusError is a sink response with an unexpected HTTP status.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status code: %d", e.StatusCode)
}

// withoutURL drops the request URL from a parse or transport failure, keeping only
// the cause (refused, timeout, DNS ...), so a URL carrying credentials never reaches
// a log.
func withoutURL(op string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return fmt.Errorf("%s: %w", op, urlErr.Err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// createURL is the collection URL with a trailing slash: data-service registers the
// create route as POST {prefix}/llm_calls/ and does not redirect a missing slash.
func (c *LLMLogClient) createURL() string {
	return c.baseURL + "/"
}

// CreateLLMCall creates a new LLM call log entry.
func (c *LLMLogClient) CreateLLMCall(ctx context.Context, req *LLMCallCreateRequest) (*LLMCallCreateResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.createURL(), bytes.NewReader(body))
	if err != nil {
		return nil, withoutURL("create request", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// [OXSCI] Add API Key header if configured (for Lambda Proxy access)
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, withoutURL("do request", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, &StatusError{StatusCode: resp.StatusCode}
	}

	var envelope llmCallCreateEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		// The decoder's message can quote response bytes; keep only the fact.
		return nil, errors.New("decode response: body is not the expected JSON")
	}
	id := envelope.ID
	if envelope.Data != nil && envelope.Data.ID > 0 {
		id = envelope.Data.ID
	}
	if id <= 0 {
		return nil, errors.New("decode response: no call id in the response")
	}

	return &LLMCallCreateResponse{ID: id}, nil
}

// UpdateLLMCall updates an existing LLM call log entry.
func (c *LLMLogClient) UpdateLLMCall(ctx context.Context, id int64, req *LLMCallUpdateRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/%d", c.baseURL, id)
	httpReq, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(body))
	if err != nil {
		return withoutURL("create request", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// [OXSCI] Add API Key header if configured (for Lambda Proxy access)
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return withoutURL("do request", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return &StatusError{StatusCode: resp.StatusCode}
	}

	return nil
}
