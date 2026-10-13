package llms

import (
	"encoding/json"
	"strings"
	"testing"

	protocolbridge "github.com/chaitin/ai-api-protocol-bridge"
)

func TestRewriteRuntimeRequestForUpstreamPreservesAssistantTextAfterToolCall(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Call the demo tool."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I'll call the demo tool."}]},
			{"type":"function_call","name":"search_demo","arguments":"{\"query\":\"demo\"}","call_id":"call_1"},
			{"type":"function_call_output","call_id":"call_1","output":"mcp-tool-ok"}
		]
	}`)

	rewritten, err := RewriteRuntimeRequestForUpstream(body, ResolvedTarget{}, protocolbridge.ProtocolOpenAIResponses)
	if err != nil {
		t.Fatalf("RewriteRuntimeRequestForUpstream() error = %v", err)
	}

	var payload struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("unmarshal rewritten request: %v", err)
	}
	if got := payload.Input[1].Content[0].Type; got != "output_text" {
		t.Fatalf("assistant preamble text type = %q, want output_text", got)
	}
	if got := payload.Input[0].Content[0].Type; got != "input_text" {
		t.Fatalf("user text type = %q, want input_text", got)
	}
	if got := payload.Input[2].Type; got != "function_call" {
		t.Fatalf("tool call item type = %q, want function_call", got)
	}
	if got := payload.Input[3].Type; got != "function_call_output" {
		t.Fatalf("tool output item type = %q, want function_call_output", got)
	}
}

func TestRewriteRuntimeRequestForUpstreamNormalizesResponsesTextTypesByRole(t *testing.T) {
	tests := []struct {
		name   string
		target ResolvedTarget
		want   []string
	}{
		{
			name: "standard responses",
			want: []string{"input_text", "input_text", "output_text"},
		},
		{
			name: "generic provider",
			target: ResolvedTarget{Provider: Provider{
				UseGenericResponsesTextParts: true,
			}},
			want: []string{"text", "text", "text"},
		},
	}

	body := []byte(`{
		"input":[
			{"role":"developer","content":[{"text":"Follow the instructions."}]},
			{"role":"user","content":[{"type":"output_text","text":"Question"}]},
			{"role":"assistant","content":[{"type":"input_text","text":"Answer"}]}
		]
	}`)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rewritten, err := RewriteRuntimeRequestForUpstream(body, tt.target, protocolbridge.ProtocolOpenAIResponses)
			if err != nil {
				t.Fatalf("RewriteRuntimeRequestForUpstream() error = %v", err)
			}

			var payload struct {
				Input []struct {
					Content []struct {
						Type string `json:"type"`
					} `json:"content"`
				} `json:"input"`
			}
			if err := json.Unmarshal(rewritten, &payload); err != nil {
				t.Fatalf("unmarshal rewritten request: %v", err)
			}
			for i, want := range tt.want {
				if got := payload.Input[i].Content[0].Type; got != want {
					t.Errorf("input[%d] text type = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// TestCrossFamilyBridgeServesMessagesToChat pins the last cell of the conversion
// matrix. An Anthropic inbound has two OpenAI targets, so a family lookup always
// returned the Responses bridge; the library's protocol-precise constructor is
// what lets claude reach a chat-completions-only upstream at all.
func TestCrossFamilyBridgeServesMessagesToChat(t *testing.T) {
	bridge, err := crossFamilyBridge(protocolbridge.ProtocolAnthropicMessages, protocolbridge.ProtocolOpenAIChat)
	if err != nil {
		t.Fatalf("crossFamilyBridge() error = %v", err)
	}
	if got := bridge.UpstreamProtocol(); got != protocolbridge.ProtocolOpenAIChat {
		t.Fatalf("UpstreamProtocol() = %q, want %q", got, protocolbridge.ProtocolOpenAIChat)
	}

	req := &protocolbridge.LLMRequest{
		Protocol: protocolbridge.ProtocolAnthropicMessages,
		Model:    "claude-sonnet-4",
		Prompt: []protocolbridge.Message{
			{Role: protocolbridge.RoleUser, Parts: []protocolbridge.Part{{Type: protocolbridge.PartText, Text: &protocolbridge.TextPart{Text: "Hello"}}}},
		},
	}
	raw, err := EncodeRuntimeUpstreamRequest(protocolbridge.ProtocolAnthropicMessages, protocolbridge.ProtocolOpenAIChat, ResolvedTarget{Model: Model{Name: "claude-sonnet-4"}}, req)
	if err != nil {
		t.Fatalf("EncodeRuntimeUpstreamRequest() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, present := decoded["input"]; present {
		t.Fatal("a Responses-shaped payload was posted to a chat upstream")
	}
	if _, present := decoded["messages"]; !present {
		t.Fatalf("payload = %s, want chat messages", raw)
	}
}

// TestCrossFamilyBridgeKeepsToolResultsForChatUpstream pins the traffic defect
// that once forced this cell back to "unsupported": Anthropic carries a tool
// result in a user message, and an encoder that only reads text, refusal and
// tool-call parts would turn that turn into an empty user message, so the model
// silently receives no tool output from the second turn on.
func TestCrossFamilyBridgeKeepsToolResultsForChatUpstream(t *testing.T) {
	req := &protocolbridge.LLMRequest{
		Protocol: protocolbridge.ProtocolAnthropicMessages,
		Model:    "claude-sonnet-4",
		Prompt: []protocolbridge.Message{
			{
				Role: protocolbridge.RoleUser,
				Parts: []protocolbridge.Part{{
					Type: protocolbridge.PartToolResult,
					ToolResult: &protocolbridge.ToolResultPart{
						ToolCallID: "call_1",
						ToolName:   "search",
						Output:     protocolbridge.ToolResultOutput{Type: protocolbridge.ToolResultText, Text: "tool-output"},
					},
				}},
			},
		},
	}
	raw, err := EncodeRuntimeUpstreamRequest(protocolbridge.ProtocolAnthropicMessages, protocolbridge.ProtocolOpenAIChat, ResolvedTarget{Model: Model{Name: "claude-sonnet-4"}}, req)
	if err != nil {
		t.Fatalf("EncodeRuntimeUpstreamRequest() error = %v", err)
	}
	if !strings.Contains(string(raw), "tool-output") {
		t.Fatalf("payload = %s, want the tool result text preserved", raw)
	}
}

// TestCrossFamilyBridgeStillRejectsUnservedPairs guards the other half: the
// helper must keep failing for pairings nothing can serve.
func TestCrossFamilyBridgeStillRejectsUnservedPairs(t *testing.T) {
	if _, err := crossFamilyBridge(protocolbridge.Protocol("bogus"), protocolbridge.ProtocolOpenAIChat); err == nil {
		t.Fatal("crossFamilyBridge() served an unknown inbound protocol")
	}
	if _, err := crossFamilyBridge(protocolbridge.ProtocolAnthropicMessages, protocolbridge.Protocol("bogus")); err == nil {
		t.Fatal("crossFamilyBridge() served an unknown upstream protocol")
	}
}

// TestRuntimeStreamBridgeHoldsResponsesCompletionForChatUsage pins why a pair
// that differs must use its bridge even though both sides are OpenAI. A chat
// upstream reports finish_reason and usage in two separate chunks, and
// response.completed is the only Responses event that can carry usage. Encoding
// the finish the moment it arrives, which is what two hand-assembled adapters
// do, publishes the completion with no token counts at all, because the
// Responses encoder ignores the standalone usage part that follows it.
func TestRuntimeStreamBridgeHoldsResponsesCompletionForChatUsage(t *testing.T) {
	decoder, encoder, err := RuntimeStreamBridge(protocolbridge.ProtocolOpenAIResponses, protocolbridge.ProtocolOpenAIChat, "deepseek-flash")
	if err != nil {
		t.Fatalf("RuntimeStreamBridge() error = %v", err)
	}

	upstream := []protocolbridge.RawStreamEvent{
		{Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"deepseek-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":null}]}`)},
		{Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"deepseek-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)},
		{Data: []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"deepseek-flash","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)},
		{Data: []byte("[DONE]")},
	}

	var encoded []protocolbridge.RawStreamEvent
	for _, event := range upstream {
		parts, err := decoder.Decode(event)
		if err != nil {
			t.Fatalf("decoder.Decode() error = %v", err)
		}
		for _, part := range parts {
			events, err := encoder.Encode(part)
			if err != nil {
				t.Fatalf("encoder.Encode() error = %v", err)
			}
			encoded = append(encoded, events...)
		}
	}
	closed, err := encoder.Close()
	if err != nil {
		t.Fatalf("encoder.Close() error = %v", err)
	}
	encoded = append(encoded, closed...)

	var completed []byte
	for _, event := range encoded {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event.Data, &envelope); err != nil {
			t.Fatalf("json.Unmarshal(%s) error = %v", event.Data, err)
		}
		if envelope.Type == "response.completed" {
			completed = event.Data
			break
		}
	}
	if completed == nil {
		t.Fatalf("stream carried no response.completed event: %v", encoded)
	}

	var payload struct {
		Response struct {
			Usage *struct {
				InputTokens  *int `json:"input_tokens"`
				OutputTokens *int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(completed, &payload); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", completed, err)
	}
	usage := payload.Response.Usage
	if usage == nil || usage.InputTokens == nil || usage.OutputTokens == nil {
		t.Fatalf("response.completed = %s, want the usage that arrived after the finish chunk", completed)
	}
	if *usage.InputTokens != 11 || *usage.OutputTokens != 2 {
		t.Fatalf("response.completed usage = %s, want input_tokens 11 and output_tokens 2", completed)
	}
}
