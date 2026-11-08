package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func wsModelRoutingGroup(enabled bool) *service.Group {
	return &service.Group{
		ID: 4201, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true,
		AllowMessagesDispatch: true, ModelRoutingEnabled: enabled,
		ModelRouting: map[string][]int64{
			"gpt-restricted-routing-test": {9902},
			"gpt-*":                       {9901}, "codex-*": {9901},
		},
	}
}

func TestOpenAIResponsesWebSocket_ModelRoutingTurnGuard(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		for _, tc := range []struct {
			name, first, second string
			disabled, blocked   bool
			mapping             map[string]string
		}{
			{name: "model switch cannot escape route", first: "gpt-normal-routing-test", second: "gpt-restricted-routing-test", blocked: true},
			{name: "codex switch cannot escape route", first: "codex-normal-routing-test", second: "gpt-restricted-routing-test", blocked: true},
			{name: "same model continues", first: "gpt-normal-routing-test", second: "gpt-normal-routing-test"},
			{name: "same account different routed model continues", first: "gpt-normal-routing-test", second: "codex-normal-routing-test"},
			{name: "disabled preserves model switch", first: "gpt-normal-routing-test", second: "gpt-restricted-routing-test", disabled: true},
			{name: "unmatched preserves model switch", first: "other-routing-test", second: "another-routing-test"},
			{name: "channel mapped effective model is checked", first: "gpt-normal-routing-test", second: "alias-routing-test", blocked: true,
				mapping: map[string]string{"alias-routing-test": "gpt-restricted-routing-test"}},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
					firstPayload:  `{"type":"response.create","model":"` + tc.first + `","stream":false}`,
					secondPayload: `{"type":"response.create","model":"` + tc.second + `","stream":false}`,
					group:         wsModelRoutingGroup(!tc.disabled), ingressMode: mode, channelMapping: tc.mapping,
					secondTurnCloseExpected: tc.blocked, closeReason: openAIModelRoutingAccountMessage,
				})
				if !tc.blocked {
					require.Len(t, got.upstreamPayloads, 2)
					require.Len(t, got.clientEvents, 2)
				}
			})
		}
	}
}

func TestOpenAIResponsesWebSocket_ModelRoutingSessionUpdateGuard(t *testing.T) {
	runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload:  `{"type":"response.create","model":"gpt-normal-routing-test","stream":false}`,
		midPayload:    `{"type":"session.update","session":{"model":"gpt-restricted-routing-test"}}`,
		secondPayload: `{"type":"response.create","stream":false}`,
		group:         wsModelRoutingGroup(true), ingressMode: service.OpenAIWSIngressModePassthrough,
		secondTurnCloseExpected: true, closeReason: openAIModelRoutingAccountMessage,
	})
}

func TestOpenAIResponsesWebSocket_ModelRoutingAmbiguity(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		for _, first := range []bool{true, false} {
			name := "subsequent frame"
			if first {
				name = "first frame"
			}
			t.Run(mode+"/"+name, func(t *testing.T) {
				valid := `{"type":"response.create","model":"gpt-normal-routing-test","stream":false}`
				ambiguous := `{"type":"response.create","model":"gpt-normal-routing-test","model":"gpt-restricted-routing-test","stream":false}`
				params := openAIResponsesWSUsageLogCase{
					firstPayload: valid, secondPayload: ambiguous, group: wsModelRoutingGroup(true), ingressMode: mode,
					secondTurnCloseExpected: true, closeReason: openAIModelRoutingAmbiguousMessage,
				}
				if first {
					params.firstPayload = ambiguous
					params.secondPayload = ""
					params.firstFrameCloseExpected = true
					params.secondTurnCloseExpected = false
				}
				runOpenAIResponsesWebSocketUsageLogCase(t, params)
			})
		}
	}
}

func TestOpenAIResponsesWebSocket_ModelRoutingIdenticalDuplicatesContinue(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		t.Run(mode, func(t *testing.T) {
			body := `{"type":"response.create","model":"gpt-normal-routing-test","model":"gpt-normal-routing-test","stream":false}`
			got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload: body, secondPayload: body, group: wsModelRoutingGroup(true), ingressMode: mode,
			})
			require.Len(t, got.upstreamPayloads, 2)
		})
	}
}
