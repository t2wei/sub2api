package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnthropicToResponses_WebSearchPreservesRequestOptions(t *testing.T) {
	for name, location := range map[string]string{
		"flat":   `{"type":"approximate","country":"GB","city":"Oxford","region":"Oxfordshire","timezone":"Europe/London"}`,
		"nested": `{"type":"approximate","approximate":{"country":"GB","city":"Oxford","region":"Oxfordshire","timezone":"Europe/London"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var request AnthropicRequest
			require.NoError(t, json.Unmarshal([]byte(`{
				"model":"gpt-6-astra","max_tokens":1024,
				"messages":[{"role":"user","content":"Find the release notes"}],
				"tools":[{
					"type":"web_search_20250305","name":"web_search",
					"allowed_domains":["https://Example.com/docs","example.com"],
					"blocked_domains":["https://blocked.example/path"],
					"filters":{"allowed_domains":["docs.example.org","example.com"]},
					"search_context_size":"high","external_web_access":false
				}]
			}`), &request))
			request.Tools[0].UserLocation = json.RawMessage(location)

			converted, err := AnthropicToResponses(&request)
			require.NoError(t, err)
			require.Len(t, converted.Tools, 1)
			encoded, err := json.Marshal(converted.Tools[0])
			require.NoError(t, err)
			assert.JSONEq(t, `{
				"type":"web_search",
				"filters":{"allowed_domains":["docs.example.org","example.com"],"blocked_domains":["blocked.example"]},
				"user_location":{"type":"approximate","country":"GB","city":"Oxford","region":"Oxfordshire","timezone":"Europe/London"},
				"search_context_size":"high","external_web_access":false
			}`, string(encoded))
		})
	}
}

func TestResponsesToAnthropic_WebSearchPreservesResultsAndCitations(t *testing.T) {
	for name, searchFields := range map[string]string{
		"search results": `"action":{"type":"search","query":"release notes"},"search_results":[{"url":"https://example.com/release","title":"Release notes","snippet":"New capabilities","page_age":"1 day"}]`,
		"URL citations":  `"search_query":"release notes"`,
	} {
		t.Run(name, func(t *testing.T) {
			var response ResponsesResponse
			require.NoError(t, json.Unmarshal([]byte(`{
				"id":"resp_search","status":"completed","output":[
					{"type":"web_search_call","id":"ws_1","status":"completed",`+searchFields+`},
					{"type":"message","role":"assistant","content":[{
						"type":"output_text","text":"Here are the release notes.",
						"annotations":[
							{"type":"url_citation","url":"https://example.com/release","title":"Release notes","snippet":"New capabilities","page_age":"1 day"},
							{"type":"url_citation","url":"https://example.com/release","title":"Release notes"}
						]
					}]}
				]
			}`), &response))

			converted := ResponsesToAnthropic(&response, "claude-sonnet-5-5")
			require.Len(t, converted.Content, 3)
			call, result := converted.Content[0], converted.Content[1]
			assert.Equal(t, "server_tool_use", call.Type)
			assert.Equal(t, "web_search", call.Name)
			require.NotEmpty(t, call.ID)
			assert.JSONEq(t, `{"query":"release notes"}`, string(call.Input))
			assert.Equal(t, "web_search_tool_result", result.Type)
			assert.Equal(t, call.ID, result.ToolUseID)
			assert.JSONEq(t, `[{"type":"web_search_result","url":"https://example.com/release","title":"Release notes","page_content":"New capabilities","page_age":"1 day"}]`, string(result.Content))
			assert.Equal(t, "Here are the release notes.", converted.Content[2].Text)
			require.NotNil(t, converted.StopReason)
			assert.Equal(t, "end_turn", *converted.StopReason)
		})
	}
}

func TestResponsesEventToAnthropicEvents_WebSearchKeepsBlockLifecycle(t *testing.T) {
	var searchDone ResponsesStreamEvent
	require.NoError(t, json.Unmarshal([]byte(`{
		"type":"response.output_item.done","output_index":1,
		"item":{"type":"web_search_call","id":"ws_1","status":"completed",
			"action":{"type":"search","query":"release notes"},
			"results":[{"url":"https://example.com/release","title":"Release notes","page_content":"New capabilities"}]
		}
	}`), &searchDone))

	events := feedResponsesEvents(
		responsesCreated(),
		&ResponsesStreamEvent{Type: "response.output_text.delta", Delta: "Searching. "},
		&searchDone,
		&ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 2, Delta: "Found the release notes."},
		&ResponsesStreamEvent{Type: "response.completed", Response: &ResponsesResponse{Status: "completed"}},
	)
	requireAnthropicBlockLifecycle(t, events)
	assert.Equal(t, "Searching. Found the release notes.", collectAnthropicText(events))

	var calls, results []AnthropicContentBlock
	for _, event := range events {
		if event.Type != "content_block_start" || event.ContentBlock == nil {
			continue
		}
		switch event.ContentBlock.Type {
		case "server_tool_use":
			calls = append(calls, *event.ContentBlock)
		case "web_search_tool_result":
			results = append(results, *event.ContentBlock)
		}
	}
	require.Len(t, calls, 1)
	require.Len(t, results, 1)
	assert.Equal(t, "web_search", calls[0].Name)
	assert.JSONEq(t, `{"query":"release notes"}`, string(calls[0].Input))
	assert.Equal(t, calls[0].ID, results[0].ToolUseID)
	assert.JSONEq(t, `[{"type":"web_search_result","url":"https://example.com/release","title":"Release notes","page_content":"New capabilities"}]`, string(results[0].Content))
	assert.Equal(t, "message_stop", events[len(events)-1].Type)
}
