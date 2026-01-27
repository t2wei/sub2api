// Package client provides HTTP clients for external services.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LLMLogClient is an HTTP client for the data-service LLM logging API.
type LLMLogClient struct {
	baseURL    string
	httpClient *http.Client
	agentName  string
	apiKey     string // 访问 data-service 的 API Key (用于 lambda proxy)
}

// NewLLMLogClient creates a new LLM log client.
func NewLLMLogClient(baseURL, agentName string, timeoutSeconds int, apiKey string) *LLMLogClient {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 10
	}
	return &LLMLogClient{
		baseURL:   baseURL,
		agentName: agentName,
		apiKey:    apiKey,
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
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

// CreateCall creates a new LLM call log entry and returns the log ID.
func (c *LLMLogClient) CreateCall(ctx context.Context, req *LLMCallCreateRequest) (int64, error) {
	if req.AgentName == "" {
		req.AgentName = c.agentName
	}

	body, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var createResp LLMCallCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}

	return createResp.ID, nil
}

// UpdateCall updates an existing LLM call log entry.
func (c *LLMLogClient) UpdateCall(ctx context.Context, id int64, req *LLMCallUpdateRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/%d", c.baseURL, id)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
