// Package client provides HTTP clients for external services.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
		baseURL:   cfg.URL,
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
	ContextID *string                  `json:"context_id,omitempty"`
	AgentName string                   `json:"agent_name"`
	Model     string                   `json:"model"`
	Messages  []map[string]interface{} `json:"messages"`
	Status    string                   `json:"status"`
	Meta      map[string]interface{}   `json:"meta,omitempty"`
}

// LLMCallUpdateRequest represents the request to update an LLM call log.
type LLMCallUpdateRequest struct {
	ResponseMessages []map[string]interface{} `json:"response_messages,omitempty"`
	Status           string                   `json:"status"`
	ErrorMessage     *string                  `json:"error_message,omitempty"`
	Meta             map[string]interface{}   `json:"meta,omitempty"`
}

// LLMCallCreateResponse represents the response from creating an LLM call log.
type LLMCallCreateResponse struct {
	ID int64 `json:"id"`
}

// CreateLLMCall creates a new LLM call log entry.
func (c *LLMLogClient) CreateLLMCall(ctx context.Context, req *LLMCallCreateRequest) (*LLMCallCreateResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// [OXSCI] Add API Key header if configured (for Lambda Proxy access)
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result LLMCallCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
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
		return fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// [OXSCI] Add API Key header if configured (for Lambda Proxy access)
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}
