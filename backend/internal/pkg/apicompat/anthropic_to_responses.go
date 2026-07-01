package apicompat

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// AnthropicToResponses converts an Anthropic Messages request directly into
// a Responses API request. This preserves fields that would be lost in a
// Chat Completions intermediary round-trip (e.g. thinking, cache_control,
// structured system prompts).
func AnthropicToResponses(req *AnthropicRequest) (*ResponsesRequest, error) {
	input, err := convertAnthropicToResponsesInput(req.System, req.Messages)
	if err != nil {
		return nil, err
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}

	out := &ResponsesRequest{
		Model:   req.Model,
		Input:   inputJSON,
		Stream:  req.Stream,
		Include: []string{"reasoning.encrypted_content"},
	}

	// Reasoning models (gpt-5.x) served via the Responses API do not accept
	// sampling parameters. Sending temperature or top_p causes a 400
	// "Unsupported parameter" error, so we only forward them for non-reasoning
	// models.
	if !isReasoningModel(req.Model) {
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	}

	storeFalse := false
	out.Store = &storeFalse
	parallelToolCalls := true
	out.ParallelToolCalls = &parallelToolCalls
	out.Text = &ResponsesText{Verbosity: "medium"}

	if req.MaxTokens > 0 {
		v := req.MaxTokens
		if v < minMaxOutputTokens {
			v = minMaxOutputTokens
		}
		out.MaxOutputTokens = &v
	}

	if len(req.Tools) > 0 {
		out.Tools = convertAnthropicToolsToResponses(req.Tools)
	}

	// Determine reasoning effort: only output_config.effort controls the
	// level; thinking.type is ignored. Default follows Codex CLI / airgate's
	// Anthropic bridge shape, which uses medium when unset.
	// Anthropic levels map 1:1 to OpenAI: low→low, medium→medium, high→high, max→xhigh.
	effort := "medium"
	if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
		effort = req.OutputConfig.Effort
	}
	out.Reasoning = &ResponsesReasoning{
		Effort:  mapAnthropicEffortToResponses(effort),
		Summary: "auto",
	}

	// Convert tool_choice
	if len(req.ToolChoice) > 0 {
		tc, err := convertAnthropicToolChoiceToResponses(req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("convert tool_choice: %w", err)
		}
		out.ToolChoice = tc
	}

	return out, nil
}

// convertAnthropicToolChoiceToResponses maps Anthropic tool_choice to Responses format.
//
//	{"type":"auto"}            → "auto"
//	{"type":"any"}             → "required"
//	{"type":"none"}            → "none"
//	{"type":"tool","name":"X"} → {"type":"function","name":"X"}
func convertAnthropicToolChoiceToResponses(raw json.RawMessage) (json.RawMessage, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, err
	}

	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "none":
		return json.Marshal("none")
	case "tool":
		return json.Marshal(map[string]any{
			"type": "function",
			"name": tc.Name,
		})
	default:
		// Pass through unknown types as-is
		return raw, nil
	}
}

// convertAnthropicToResponsesInput builds the Responses API input items array
// from the Anthropic system field and message list.
func convertAnthropicToResponsesInput(system json.RawMessage, msgs []AnthropicMessage) ([]ResponsesInputItem, error) {
	var out []ResponsesInputItem

	// System prompt → developer role input item. ChatGPT Codex SSE behaves like
	// Codex CLI here: keeping Anthropic system text in input preserves the
	// conversation/cache shape better than moving it into instructions.
	if len(system) > 0 {
		sysParts, err := parseAnthropicSystemContentParts(system)
		if err != nil {
			return nil, err
		}
		if len(sysParts) > 0 {
			content, _ := json.Marshal(sysParts)
			out = append(out, ResponsesInputItem{
				Type:    "message",
				Role:    "developer",
				Content: content,
			})
		}
	}

	for _, m := range msgs {
		items, err := anthropicMsgToResponsesItems(m)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// parseAnthropicSystemContentParts handles the Anthropic system field which can
// be a plain string or an array of text blocks. Claude Code may include an
// x-anthropic-billing-header block; airgate drops it before sending to Codex.
func parseAnthropicSystemContentParts(raw json.RawMessage) ([]ResponsesContentPart, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if isAnthropicBillingHeaderText(s) || s == "" {
			return nil, nil
		}
		return []ResponsesContentPart{{Type: "input_text", Text: s}}, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	var parts []ResponsesContentPart
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" && !isAnthropicBillingHeaderText(b.Text) {
			parts = append(parts, ResponsesContentPart{Type: "input_text", Text: b.Text})
		}
	}
	return parts, nil
}

func isAnthropicBillingHeaderText(text string) bool {
	return strings.HasPrefix(text, "x-anthropic-billing-header: ")
}

// anthropicMsgToResponsesItems converts a single Anthropic message into one
// or more Responses API input items.
func anthropicMsgToResponsesItems(m AnthropicMessage) ([]ResponsesInputItem, error) {
	switch m.Role {
	case "user":
		return anthropicUserToResponses(m.Content)
	case "assistant":
		return anthropicAssistantToResponses(m.Content)
	default:
		return anthropicUserToResponses(m.Content)
	}
}

// anthropicUserToResponses handles an Anthropic user message. Content can be a
// plain string or an array of blocks. tool_result blocks are extracted into
// function_call_output items. Image blocks are converted to input_image parts.
func anthropicUserToResponses(raw json.RawMessage) ([]ResponsesInputItem, error) {
	// Try plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		parts := []ResponsesContentPart{{Type: "input_text", Text: s}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		return []ResponsesInputItem{{Type: "message", Role: "user", Content: partsJSON}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var out []ResponsesInputItem
	var toolResultImageParts []ResponsesContentPart

	// Extract tool_result blocks → function_call_output items.
	// Images inside tool_results are extracted separately because the
	// Responses API function_call_output.output only accepts strings.
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		outputText, imageParts := convertToolResultOutput(b)
		out = append(out, ResponsesInputItem{
			Type:   "function_call_output",
			CallID: toResponsesCallID(b.ToolUseID),
			Output: outputText,
		})
		toolResultImageParts = append(toolResultImageParts, imageParts...)
	}

	// Remaining text + image blocks → user message with content parts.
	// Also include images extracted from tool_results so the model can see them.
	var parts []ResponsesContentPart
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, ResponsesContentPart{Type: "input_text", Text: b.Text})
			}
		case "image":
			if uri := anthropicImageToDataURI(b.Source); uri != "" {
				parts = append(parts, ResponsesContentPart{Type: "input_image", ImageURL: uri})
			}
		}
	}
	parts = append(parts, toolResultImageParts...)

	if len(parts) > 0 {
		content, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		out = append(out, ResponsesInputItem{Type: "message", Role: "user", Content: content})
	}

	return out, nil
}

// anthropicAssistantToResponses handles an Anthropic assistant message.
// Text content → assistant message with output_text parts.
// tool_use blocks → function_call items.
// thinking blocks with signature → reasoning items (encrypted_content) so
// multi-turn Grok/Codex prompt cache can reuse prior reasoning prefixes.
// thinking without signature remains ignored (not accepted as plain text input).
func anthropicAssistantToResponses(raw json.RawMessage) ([]ResponsesInputItem, error) {
	// Try plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		parts := []ResponsesContentPart{{Type: "output_text", Text: s}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		return []ResponsesInputItem{{Type: "message", Role: "assistant", Content: partsJSON}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var items []ResponsesInputItem

	// Preserve turn order: reasoning → assistant text → tool calls. xAI/Codex
	// multi-turn cache and tool continuations expect reasoning before the
	// assistant message that followed it.
	for _, b := range blocks {
		if b.Type != "thinking" {
			continue
		}
		sig := strings.TrimSpace(b.Signature)
		// Only replay provider ciphertext. Skip GPT/Codex-style gAAAA blobs and
		// empty placeholders — xAI returns 400 on decrypt for foreign signatures.
		if sig == "" || strings.HasPrefix(sig, "gAAAA") {
			continue
		}
		items = append(items, ResponsesInputItem{
			Type:             "reasoning",
			EncryptedContent: sig,
		})
	}

	// Text content → assistant message with output_text content parts.
	text := extractAnthropicTextFromBlocks(blocks)
	if text != "" {
		parts := []ResponsesContentPart{{Type: "output_text", Text: text}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		items = append(items, ResponsesInputItem{Type: "message", Role: "assistant", Content: partsJSON})
	}

	// tool_use → function_call items.
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := "{}"
		if len(b.Input) > 0 {
			args = string(b.Input)
		}
		fcID := toResponsesCallID(b.ID)
		items = append(items, ResponsesInputItem{
			Type:      "function_call",
			CallID:    fcID,
			Name:      b.Name,
			Arguments: args,
		})
	}

	return items, nil
}

// toResponsesCallID preserves Anthropic tool IDs as Responses call_id values.
// Claude Code sends tool_result.tool_use_id back verbatim, and ChatGPT Codex
// continuation expects that call_id to match the original tool_use id.
func toResponsesCallID(id string) string {
	return id
}

// responsesCallIDPrefix is a legacy prefix previously added when converting
// Anthropic tool IDs to Responses call_id values.
const responsesCallIDPrefix = "fc_a_"

// fromResponsesCallID reverses old prefixed IDs while preserving current IDs.
func fromResponsesCallID(id string) string {
	if after, ok := strings.CutPrefix(id, responsesCallIDPrefix); ok {
		return after
	}
	// Legacy compat: also try stripping bare "fc_" for IDs that were
	// generated before the prefix change.
	if after, ok := strings.CutPrefix(id, "fc_"); ok {
		if strings.HasPrefix(after, "toolu_") || strings.HasPrefix(after, "call_") {
			return after
		}
	}
	return id
}

// anthropicImageToDataURI converts an AnthropicImageSource to a data URI string.
// Returns "" if the source is nil or has no data.
func anthropicImageToDataURI(src *AnthropicImageSource) string {
	if src == nil || src.Data == "" {
		return ""
	}
	mediaType := src.MediaType
	if mediaType == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + src.Data
}

// convertToolResultOutput extracts text and image content from a tool_result
// block. Returns the text as a string for the function_call_output Output
// field, plus any image parts that must be sent in a separate user message
// (the Responses API output field only accepts strings).
func convertToolResultOutput(b AnthropicContentBlock) (string, []ResponsesContentPart) {
	if len(b.Content) == 0 {
		return "(empty)", nil
	}

	// Try plain string content.
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		if s == "" {
			s = "(empty)"
		}
		return s, nil
	}

	// Array of content blocks — may contain text and/or images.
	var inner []AnthropicContentBlock
	if err := json.Unmarshal(b.Content, &inner); err != nil {
		return "(empty)", nil
	}

	// Separate text (for function_call_output) from images (for user message).
	var textParts []string
	var imageParts []ResponsesContentPart
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			if ib.Text != "" {
				textParts = append(textParts, ib.Text)
			}
		case "image":
			if uri := anthropicImageToDataURI(ib.Source); uri != "" {
				imageParts = append(imageParts, ResponsesContentPart{Type: "input_image", ImageURL: uri})
			}
		}
	}

	text := strings.Join(textParts, "\n\n")
	if text == "" {
		text = "(empty)"
	}
	return text, imageParts
}

// extractAnthropicTextFromBlocks joins all text blocks, ignoring thinking/
// tool_use/tool_result blocks.
func extractAnthropicTextFromBlocks(blocks []AnthropicContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// mapAnthropicEffortToResponses converts Anthropic reasoning effort levels to
// OpenAI Responses API effort levels.
//
// Both APIs default to "high". The mapping is 1:1 for shared levels;
// only Anthropic's "max" (Opus 4.6 exclusive) maps to OpenAI's "xhigh"
// (GPT-5.2+ exclusive) as both represent the highest reasoning tier.
//
//	low    → low
//	medium → medium
//	high   → high
//	max    → xhigh
func mapAnthropicEffortToResponses(effort string) string {
	if effort == "max" {
		return "xhigh"
	}
	return effort // low→low, medium→medium, high→high, unknown→passthrough
}

// convertAnthropicToolsToResponses maps Anthropic tool definitions to
// Responses API tools. Server-side tools like web_search are mapped to their
// OpenAI equivalents; regular tools become function tools.
func convertAnthropicToolsToResponses(tools []AnthropicTool) []ResponsesTool {
	var out []ResponsesTool
	for _, t := range tools {
		if isAnthropicWebSearchTool(t) {
			out = append(out, convertAnthropicWebSearchToolToResponses(t))
			continue
		}
		out = append(out, ResponsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  normalizeToolParameters(t.InputSchema),
			Strict:      boolPtr(false),
		})
	}
	return out
}

func boolPtr(v bool) *bool {
	return &v
}

// isReasoningModel reports whether model is a reasoning model that does not
// support sampling parameters (temperature, top_p) via the Responses API.
// All gpt-5.x models are reasoning-only; the Responses API returns
// "Unsupported parameter: temperature" if these fields are present.
func isReasoningModel(model string) bool {
	return strings.HasPrefix(model, "gpt-5")
}

func isAnthropicWebSearchTool(tool AnthropicTool) bool {
	return strings.HasPrefix(tool.Type, "web_search") || tool.Name == "web_search"
}

func convertAnthropicWebSearchToolToResponses(tool AnthropicTool) ResponsesTool {
	out := ResponsesTool{Type: "web_search"}
	filters := map[string][]string{}
	if tool.Filters != nil {
		if domains := normalizeOpenAIWebSearchDomains(tool.Filters.AllowedDomains); len(domains) > 0 {
			filters["allowed_domains"] = appendUniqueStrings(filters["allowed_domains"], domains...)
		}
		if domains := normalizeOpenAIWebSearchDomains(tool.Filters.BlockedDomains); len(domains) > 0 {
			filters["blocked_domains"] = appendUniqueStrings(filters["blocked_domains"], domains...)
		}
	}
	if domains := normalizeOpenAIWebSearchDomains(tool.AllowedDomains); len(domains) > 0 {
		filters["allowed_domains"] = appendUniqueStrings(filters["allowed_domains"], domains...)
	}
	if domains := normalizeOpenAIWebSearchDomains(tool.BlockedDomains); len(domains) > 0 {
		filters["blocked_domains"] = appendUniqueStrings(filters["blocked_domains"], domains...)
	}
	if len(filters) > 0 {
		out.Filters = filters
	}
	if userLocation := normalizeOpenAIWebSearchUserLocation(tool.UserLocation); userLocation != nil {
		out.UserLocation = userLocation
	}
	switch tool.SearchContextSize {
	case "low", "medium", "high":
		out.SearchContextSize = tool.SearchContextSize
	}
	if tool.ExternalWebAccess != nil {
		out.ExternalWebAccess = tool.ExternalWebAccess
	}
	return out
}

func normalizeOpenAIWebSearchDomains(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	domains := make([]string, 0, len(values))
	for _, value := range values {
		domain := normalizeOpenAIWebSearchDomain(value)
		if domain == "" {
			continue
		}
		domains = appendUniqueStrings(domains, domain)
	}
	return domains
}

func normalizeOpenAIWebSearchDomain(value string) string {
	rawValue := strings.TrimSpace(value)
	if rawValue == "" {
		return ""
	}
	parseValue := rawValue
	if !strings.Contains(parseValue, "://") {
		parseValue = "https://" + parseValue
	}
	parsed, err := url.Parse(parseValue)
	domain := ""
	if err == nil {
		domain = parsed.Host
		if domain == "" {
			domain = parsed.Path
		}
	}
	if domain == "" {
		domain = rawValue
	}
	domain = strings.SplitN(domain, "/", 2)[0]
	domain = strings.Trim(strings.ToLower(domain), " .")
	return domain
}

func appendUniqueStrings(values []string, additions ...string) []string {
	for _, addition := range additions {
		if addition == "" {
			continue
		}
		seen := false
		for _, value := range values {
			if value == addition {
				seen = true
				break
			}
		}
		if !seen {
			values = append(values, addition)
		}
	}
	return values
}

func normalizeOpenAIWebSearchUserLocation(raw json.RawMessage) *ResponsesWebSearchUserLocation {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var location map[string]json.RawMessage
	if err := json.Unmarshal(raw, &location); err != nil || len(location) == 0 {
		return nil
	}
	source := location
	if approximateRaw, ok := location["approximate"]; ok && len(approximateRaw) > 0 && string(approximateRaw) != "null" {
		var approximate map[string]json.RawMessage
		if err := json.Unmarshal(approximateRaw, &approximate); err == nil && len(approximate) > 0 {
			source = approximate
		}
	}
	out := &ResponsesWebSearchUserLocation{Type: "approximate"}
	out.Country = trimJSONString(source["country"])
	out.City = trimJSONString(source["city"])
	out.Region = trimJSONString(source["region"])
	out.Timezone = trimJSONString(source["timezone"])
	if out.Country == "" && out.City == "" && out.Region == "" && out.Timezone == "" {
		return nil
	}
	return out
}

func trimJSONString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// normalizeToolParameters ensures the tool parameter schema is valid for
// OpenAI's Responses API, which requires "properties" on object schemas.
//
//   - nil/empty → {"type":"object","properties":{}}
//   - type=object without properties → adds "properties": {}
//   - otherwise → returned unchanged
func normalizeToolParameters(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 || string(schema) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(schema, &m); err != nil {
		return schema
	}

	typ := m["type"]
	if string(typ) != `"object"` {
		return schema
	}

	if _, ok := m["properties"]; ok {
		return schema
	}

	m["properties"] = json.RawMessage(`{}`)
	out, err := json.Marshal(m)
	if err != nil {
		return schema
	}
	return out
}
