// Package gemini provides minimal fallback model metadata for Gemini native endpoints.
// It is used when upstream model listing is unavailable (e.g. OAuth token missing AI Studio scopes).
package gemini

import (
	"strings"
)

type Model struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName,omitempty"`
	Description                string   `json:"description,omitempty"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
}

type ModelsListResponse struct {
	Models []Model `json:"models"`
}

func DefaultModels() []Model {
	chatMethods := []string{"generateContent", "streamGenerateContent"}
	embeddingMethods := []string{"embedContent", "batchEmbedContents"}
	return []Model{
		{Name: "models/gemini-2.0-flash", SupportedGenerationMethods: chatMethods},
		{Name: "models/gemini-2.5-flash", SupportedGenerationMethods: chatMethods},
		{Name: "models/gemini-2.5-pro", SupportedGenerationMethods: chatMethods},
		{Name: "models/gemini-3-flash-preview", SupportedGenerationMethods: chatMethods},
		{Name: "models/gemini-3-pro-preview", SupportedGenerationMethods: chatMethods},
		{Name: "models/gemini-3.1-pro-preview", SupportedGenerationMethods: chatMethods},
		{Name: "models/text-embedding-004", SupportedGenerationMethods: embeddingMethods},
		{Name: "models/embedding-001", SupportedGenerationMethods: embeddingMethods},
	}
}

func FallbackModelsList() ModelsListResponse {
	return ModelsListResponse{Models: DefaultModels()}
}

func FallbackModel(model string) Model {
	chatMethods := []string{"generateContent", "streamGenerateContent"}
	embeddingMethods := []string{"embedContent", "batchEmbedContents"}

	// Determine if model is for embedding based on name patterns
	isEmbedding := false
	if model != "" {
		lower := strings.ToLower(model)
		isEmbedding = strings.Contains(lower, "embedding") || strings.Contains(lower, "embed")
	}

	methods := chatMethods
	if isEmbedding {
		methods = embeddingMethods
	}

	if model == "" {
		return Model{Name: "models/unknown", SupportedGenerationMethods: methods}
	}
	if len(model) >= 7 && model[:7] == "models/" {
		return Model{Name: model, SupportedGenerationMethods: methods}
	}
	return Model{Name: "models/" + model, SupportedGenerationMethods: methods}
}
