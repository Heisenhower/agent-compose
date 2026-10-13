package llms

import (
	"fmt"

	protocolbridge "github.com/chaitin/ai-api-protocol-bridge"
)

func ProtocolAdapter(protocol protocolbridge.Protocol) (protocolbridge.Adapter, error) {
	switch protocol {
	case protocolbridge.ProtocolOpenAIResponses:
		return protocolbridge.NewOpenAIResponsesAdapter(), nil
	case protocolbridge.ProtocolOpenAIChat:
		return protocolbridge.NewOpenAIChatAdapter(), nil
	case protocolbridge.ProtocolAnthropicMessages:
		return protocolbridge.NewAnthropicMessagesAdapter(), nil
	default:
		return nil, fmt.Errorf("unsupported llm protocol %q", protocol)
	}
}

// BridgeProtocol maps a daemon Protocol onto the protocol bridge's protocol.
func BridgeProtocol(protocol Protocol) (protocolbridge.Protocol, error) {
	switch protocol {
	case ProtocolResponses:
		return protocolbridge.ProtocolOpenAIResponses, nil
	case ProtocolChatCompletions:
		return protocolbridge.ProtocolOpenAIChat, nil
	case ProtocolMessages:
		return protocolbridge.ProtocolAnthropicMessages, nil
	default:
		return "", fmt.Errorf("unsupported llm protocol %q", protocol)
	}
}

// CanConvert reports whether the daemon can turn a request that arrived as
// inbound into one the upstream accepts. A pair that already matches re-encodes
// through the shared adapter; every other pair needs a registered bridge, so the
// answer is derived from the same lookup the request path uses rather than from
// a copy of its rules. Asking crossFamilyBridge here, instead of repeating its
// "look up by family, then check the upstream protocol" shape, is what keeps
// this preparation-time answer from disagreeing with the request-time bridge.
//
// Sharing a family is deliberately not enough. Chat completions and Responses
// are both OpenAI, but they are different wire formats with different streaming
// contracts, and only the bridge for that exact pair knows what converting
// between them costs.
func CanConvert(inbound, upstream Protocol) bool {
	inboundBridge, err := BridgeProtocol(inbound)
	if err != nil {
		return false
	}
	upstreamBridge, err := BridgeProtocol(upstream)
	if err != nil {
		return false
	}
	if inboundBridge == upstreamBridge {
		return true
	}
	_, err = crossFamilyBridge(inboundBridge, upstreamBridge)
	return err == nil
}

func UpstreamProtocolAndEndpoint(target ResolvedTarget) (protocolbridge.Protocol, string, error) {
	switch NormalizeProviderType(target.Provider.ProviderType) {
	case ProviderFamilyAnthropic:
		return protocolbridge.ProtocolAnthropicMessages, EndpointForProvider(target.Provider, APIProtocolMessages), nil
	case ProviderFamilyOpenAI:
		switch NormalizeWireAPI(target.WireAPI) {
		case APIProtocolChatCompletions:
			return protocolbridge.ProtocolOpenAIChat, EndpointForProvider(target.Provider, APIProtocolChatCompletions), nil
		case APIProtocolResponses:
			return protocolbridge.ProtocolOpenAIResponses, EndpointForProvider(target.Provider, APIProtocolResponses), nil
		default:
			return "", "", fmt.Errorf("unsupported openai wire api %q", target.WireAPI)
		}
	default:
		return "", "", fmt.Errorf("unsupported llm provider family %q", target.Provider.ProviderType)
	}
}

func UseGenericResponsesTextParts(target ResolvedTarget, upstreamProtocol protocolbridge.Protocol) bool {
	if upstreamProtocol != protocolbridge.ProtocolOpenAIResponses {
		return false
	}
	return target.Provider.UseGenericResponsesTextParts
}
