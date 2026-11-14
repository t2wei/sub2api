package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Anthropic-protocol responses must name the model the client asked for.
// Claude Code drops the signed thinking of every history message whose
// message.model differs from the model of the request it is building, so a
// response that names the model differently makes the model re-think every
// turn. Some upstreams answer with their own spelling of the same model:
// OpenRouter's Anthropic endpoint returns "anthropic/claude-opus-5" for
// "claude-opus-5" and "anthropic/claude-opus-4.8" for "claude-opus-4-8".
//
// The helpers below restore the client's name when the upstream declared an
// alias of it (or of the mapped model sub2api sent upstream). A model that is
// not an alias is a real substitution by the upstream and is left visible.
// The raw upstream name is observed before any rewrite and still lands in
// usage as upstream_response_model.

// anthropicModelAliasKey reduces a model ID to the form all spellings of one
// model share: a provider prefix ("anthropic/") is dropped, case is folded and
// version dots become dashes.
func anthropicModelAliasKey(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndexByte(key, '/'); slash >= 0 {
		key = key[slash+1:]
	}
	return strings.ReplaceAll(key, ".", "-")
}

// clientFacingAnthropicResponseModel returns the model name the client should
// see for upstreamModel, and whether it differs from upstreamModel.
// originalModel is the model the client requested; mappedModel is the model
// sub2api sent upstream after account mapping (equal to originalModel when the
// account does not map it).
func clientFacingAnthropicResponseModel(upstreamModel, originalModel, mappedModel string) (string, bool) {
	if upstreamModel == "" || originalModel == "" || upstreamModel == originalModel {
		return upstreamModel, false
	}
	if mappedModel != "" && upstreamModel == mappedModel {
		return originalModel, true
	}
	key := anthropicModelAliasKey(upstreamModel)
	if key == "" {
		return upstreamModel, false
	}
	if key == anthropicModelAliasKey(originalModel) {
		return originalModel, true
	}
	if mappedModel != "" && key == anthropicModelAliasKey(mappedModel) {
		return originalModel, true
	}
	return upstreamModel, false
}

// restoreClientModelInAnthropicResponseBody applies
// clientFacingAnthropicResponseModel to the top-level "model" of a
// non-streaming Messages response.
func restoreClientModelInAnthropicResponseBody(body []byte, originalModel, mappedModel string) ([]byte, error) {
	declared := gjson.GetBytes(body, "model")
	if declared.Type != gjson.String {
		return body, nil
	}
	clientModel, changed := clientFacingAnthropicResponseModel(declared.Str, originalModel, mappedModel)
	if !changed {
		return body, nil
	}
	restored, err := sjson.SetBytes(body, "model", clientModel)
	if err != nil {
		return nil, fmt.Errorf("restore client model %q in response body: %w", clientModel, err)
	}
	return restored, nil
}

// restoreClientModelInAnthropicStreamData applies
// clientFacingAnthropicResponseModel to message.model of a message_start SSE
// data payload. Other payloads come back unchanged.
func restoreClientModelInAnthropicStreamData(data, originalModel, mappedModel string) (string, bool, error) {
	if gjson.Get(data, "type").Str != "message_start" {
		return data, false, nil
	}
	declared := gjson.Get(data, "message.model")
	if declared.Type != gjson.String {
		return data, false, nil
	}
	clientModel, changed := clientFacingAnthropicResponseModel(declared.Str, originalModel, mappedModel)
	if !changed {
		return data, false, nil
	}
	restored, err := sjson.Set(data, "message.model", clientModel)
	if err != nil {
		return "", false, fmt.Errorf("restore client model %q in message_start: %w", clientModel, err)
	}
	return restored, true, nil
}
