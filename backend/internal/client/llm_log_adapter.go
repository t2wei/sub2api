// Package client provides HTTP clients and adapters for external services.
package client

import (
	"encoding/json"
)

// ConvertClaudeToLLMCallRequest converts a Claude API request to LLMCallCreateRequest format.
// The body parameter should be the raw request body (JSON).
func ConvertClaudeToLLMCallRequest(body []byte, agentName string) (*LLMCallCreateRequest, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}

	messages := make([]map[string]interface{}, 0)

	// Add system prompt if present
	if system, ok := req["system"].(string); ok && system != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": system,
		})
	} else if systemArr, ok := req["system"].([]interface{}); ok {
		// System can also be an array of content blocks
		content := extractTextContent(systemArr)
		if content != "" {
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": content,
			})
		}
	}

	// Convert messages
	if rawMessages, ok := req["messages"].([]interface{}); ok {
		for _, rawMsg := range rawMessages {
			if msg, ok := rawMsg.(map[string]interface{}); ok {
				role, _ := msg["role"].(string)
				content := extractMessageContent(msg["content"])
				messages = append(messages, map[string]interface{}{
					"role":    role,
					"content": content,
				})
			}
		}
	}

	// Extract model
	model, _ := req["model"].(string)

	// Build meta
	meta := map[string]interface{}{
		"source": "claude_api",
	}

	// Add request parameters to meta
	if maxTokens, ok := req["max_tokens"].(float64); ok {
		meta["max_tokens"] = int(maxTokens)
	}
	if temperature, ok := req["temperature"].(float64); ok {
		meta["temperature"] = temperature
	}
	if stream, ok := req["stream"].(bool); ok {
		meta["stream"] = stream
	}
	if metadata, ok := req["metadata"].(map[string]interface{}); ok {
		if userID, ok := metadata["user_id"].(string); ok {
			meta["metadata_user_id"] = userID
		}
	}

	return &LLMCallCreateRequest{
		AgentName: agentName,
		Model:     model,
		Messages:  messages,
		Status:    "pending",
		Meta:      meta,
	}, nil
}

// extractMessageContent extracts text content from Claude message content field.
// Content can be a string or an array of content blocks.
func extractMessageContent(content interface{}) string {
	// String content
	if str, ok := content.(string); ok {
		return str
	}

	// Array content (content blocks)
	if arr, ok := content.([]interface{}); ok {
		return extractTextContent(arr)
	}

	return ""
}

// extractTextContent extracts combined text from content blocks.
func extractTextContent(blocks []interface{}) string {
	var result string
	for _, block := range blocks {
		if blockMap, ok := block.(map[string]interface{}); ok {
			blockType, _ := blockMap["type"].(string)
			switch blockType {
			case "text":
				if text, ok := blockMap["text"].(string); ok {
					if result != "" {
						result += "\n"
					}
					result += text
				}
			case "tool_use":
				// Record tool use as structured info
				name, _ := blockMap["name"].(string)
				id, _ := blockMap["id"].(string)
				if result != "" {
					result += "\n"
				}
				result += "[tool_use: " + name + " (" + id + ")]"
			case "tool_result":
				toolUseID, _ := blockMap["tool_use_id"].(string)
				if result != "" {
					result += "\n"
				}
				result += "[tool_result: " + toolUseID + "]"
				// Try to extract text from tool result content
				if toolContent, ok := blockMap["content"].([]interface{}); ok {
					for _, tc := range toolContent {
						if tcMap, ok := tc.(map[string]interface{}); ok {
							if tcMap["type"] == "text" {
								if text, ok := tcMap["text"].(string); ok {
									result += "\n" + text
								}
							}
						}
					}
				} else if toolContentStr, ok := blockMap["content"].(string); ok {
					result += "\n" + toolContentStr
				}
			case "image":
				if result != "" {
					result += "\n"
				}
				result += "[image]"
			case "thinking", "redacted_thinking":
				// Skip thinking blocks in log
				continue
			}
		}
	}
	return result
}

// BuildLLMCallUpdateFromResult builds an LLMCallUpdateRequest from a forward result.
func BuildLLMCallUpdateFromResult(
	status string,
	errorMessage *string,
	responseText string,
	usage *ClaudeUsageForLog,
	durationMs int64,
	firstTokenMs *int,
	accountID int64,
	apiKeyID int64,
	userID int64,
) *LLMCallUpdateRequest {
	var responseMessages []map[string]interface{}
	if responseText != "" {
		responseMessages = []map[string]interface{}{
			{
				"role":    "assistant",
				"content": responseText,
			},
		}
	}

	meta := map[string]interface{}{
		"duration_ms": durationMs,
		"account_id":  accountID,
		"api_key_id":  apiKeyID,
		"user_id":     userID,
	}

	if firstTokenMs != nil {
		meta["first_token_ms"] = *firstTokenMs
	}

	if usage != nil {
		meta["usage"] = map[string]interface{}{
			"input_tokens":                usage.InputTokens,
			"output_tokens":               usage.OutputTokens,
			"cache_creation_input_tokens": usage.CacheCreationInputTokens,
			"cache_read_input_tokens":     usage.CacheReadInputTokens,
		}
	}

	return &LLMCallUpdateRequest{
		ResponseMessages: responseMessages,
		Status:           status,
		ErrorMessage:     errorMessage,
		Meta:             meta,
	}
}

// ClaudeUsageForLog represents token usage for logging purposes.
type ClaudeUsageForLog struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}
