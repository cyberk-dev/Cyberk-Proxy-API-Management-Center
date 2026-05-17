package promptlog

import (
	"strings"
	"testing"
)

// findBlock returns the first block whose Type matches, or nil.
func findBlock(blocks []Block, kind string) *Block {
	for i := range blocks {
		if blocks[i].Type == kind {
			return &blocks[i]
		}
	}
	return nil
}

func TestParseAssistant_AnthropicJSON(t *testing.T) {
	body := []byte(`{
		"id": "msg_1",
		"role": "assistant",
		"content": [
			{"type": "text", "text": "Here is the answer."},
			{"type": "tool_use", "id": "u1", "name": "Read", "input": {"file_path": "/x.go"}},
			{"type": "thinking", "thinking": "let me check the file first"}
		]
	}`)
	blocks := parseAssistantResponse(body, ProviderAnthropic, 1000)
	if len(blocks) != 3 {
		t.Fatalf("got %+v", blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "Here is the answer." {
		t.Errorf("text: %+v", blocks[0])
	}
	if blocks[1].Type != "tool_use" || blocks[1].Tool != "Read" || blocks[1].SHA256 == "" {
		t.Errorf("tool_use: %+v", blocks[1])
	}
	if blocks[2].Type != "thinking" || blocks[2].Bytes == 0 || blocks[2].SHA256 == "" {
		t.Errorf("thinking ref: %+v", blocks[2])
	}
}

func TestParseAssistant_AnthropicSSE(t *testing.T) {
	body := []byte("" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"u1\",\"name\":\"Bash\",\"input\":{}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"ls\\\"}\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n")
	blocks := parseAssistantResponse(body, ProviderAnthropic, 1000)
	if len(blocks) != 2 {
		t.Fatalf("got %+v", blocks)
	}
	if blocks[0].Text != "Hello world" {
		t.Errorf("text concat wrong: %q", blocks[0].Text)
	}
	if blocks[1].Type != "tool_use" || blocks[1].Tool != "Bash" {
		t.Errorf("tool: %+v", blocks[1])
	}
	if blocks[1].Bytes == 0 || blocks[1].SHA256 == "" {
		t.Errorf("tool input not hashed: %+v", blocks[1])
	}
}

func TestParseAssistant_OpenAIChatJSON(t *testing.T) {
	body := []byte(`{
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "Hi there",
				"tool_calls": [
					{"id": "c1", "type": "function", "function": {"name": "fetch", "arguments": "{\"u\":1}"}}
				],
				"reasoning_content": "I should call fetch"
			}
		}]
	}`)
	blocks := parseAssistantResponse(body, ProviderOpenAIChat, 1000)
	if t1 := findBlock(blocks, "text"); t1 == nil || t1.Text != "Hi there" {
		t.Errorf("text: %+v", blocks)
	}
	if tu := findBlock(blocks, "tool_use"); tu == nil || tu.Tool != "fetch" {
		t.Errorf("tool_use: %+v", blocks)
	}
	if th := findBlock(blocks, "thinking"); th == nil || th.SHA256 == "" {
		t.Errorf("thinking: %+v", blocks)
	}
}

func TestParseAssistant_OpenAIChatSSE(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"u\":1}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	blocks := parseAssistantResponse(body, ProviderOpenAIChat, 1000)
	if txt := findBlock(blocks, "text"); txt == nil || txt.Text != "Hello" {
		t.Errorf("text assembly: %+v", blocks)
	}
	if tu := findBlock(blocks, "tool_use"); tu == nil || tu.Tool != "f" || tu.SHA256 == "" {
		t.Errorf("tool_use: %+v", blocks)
	}
}

func TestParseAssistant_OpenAIResponsesJSON(t *testing.T) {
	body := []byte(`{
		"output": [
			{"type":"message","role":"assistant","content":[
				{"type":"output_text","text":"hello"},
				{"type":"refusal","refusal":"can't"}
			]},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":1}"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thought 1"},{"type":"summary_text","text":" thought 2"}]}
		]
	}`)
	blocks := parseAssistantResponse(body, ProviderOpenAIResponses, 1000)
	if t1 := findBlock(blocks, "text"); t1 == nil || t1.Text != "hello" {
		t.Errorf("text: %+v", blocks)
	}
	if r := findBlock(blocks, "refusal"); r == nil || r.Text != "can't" {
		t.Errorf("refusal: %+v", blocks)
	}
	if tu := findBlock(blocks, "tool_use"); tu == nil || tu.Tool != "f" {
		t.Errorf("tool_use: %+v", blocks)
	}
	if th := findBlock(blocks, "thinking"); th == nil || th.Bytes == 0 {
		t.Errorf("thinking: %+v", blocks)
	}
}

func TestParseAssistant_OpenAIResponsesSSE(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"type":"response.created"}`,
		`data: {"type":"response.output_item.added","item":{"id":"i1","type":"function_call","name":"f"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"i1","name":"f","delta":"{\""}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"i1","delta":"a\":1}"}`,
		`data: {"type":"response.output_text.delta","delta":"He"}`,
		`data: {"type":"response.output_text.delta","delta":"llo"}`,
		`data: {"type":"response.completed"}`,
		``,
	}, "\n\n"))
	blocks := parseAssistantResponse(body, ProviderOpenAIResponses, 1000)
	if txt := findBlock(blocks, "text"); txt == nil || txt.Text != "Hello" {
		t.Errorf("text: %+v", blocks)
	}
	if tu := findBlock(blocks, "tool_use"); tu == nil || tu.Tool != "f" {
		t.Errorf("tool_use: %+v", blocks)
	}
}

func TestParseAssistant_OpenAIResponsesSSE_TruncatedToolCalls(t *testing.T) {
	// Realistic truncation scenario: text deltas land, but the
	// function_call_arguments.delta tail is cut by the response buffer cap.
	// `response.completed` still arrives (it's the final event) with the
	// full output snapshot — we must recover the missed tool_use from it.
	body := []byte(strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"Done."}`,
		// NOTE: no function_call_arguments.delta events present — simulates
		// them being truncated by the buffer cap.
		`data: {"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]},{"type":"function_call","call_id":"c1","name":"deployTool","arguments":"{\"env\":\"prod\"}"}]}}`,
		``,
	}, "\n\n"))
	blocks := parseAssistantResponse(body, ProviderOpenAIResponses, 1000)
	if txt := findBlock(blocks, "text"); txt == nil || txt.Text != "Done." {
		t.Errorf("text from deltas: %+v", blocks)
	}
	tu := findBlock(blocks, "tool_use")
	if tu == nil || tu.Tool != "deployTool" {
		t.Errorf("tool_use missing — final-snapshot recovery failed: %+v", blocks)
	}
}

func TestParseAssistant_OpenAIResponsesSSE_FinalDedupesDeltaTool(t *testing.T) {
	// When deltas DID produce the tool_use, the final snapshot must not
	// double-emit it. Dedup key is (tool name + arg sha) so identical
	// content from both sources collapses.
	body := []byte(strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"i1","type":"function_call","name":"f"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"i1","delta":"{\"x\":1}"}`,
		`data: {"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"i1","name":"f","arguments":"{\"x\":1}"}]}}`,
		``,
	}, "\n\n"))
	blocks := parseAssistantResponse(body, ProviderOpenAIResponses, 1000)
	count := 0
	for _, b := range blocks {
		if b.Type == "tool_use" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 tool_use after dedup, got %d: %+v", count, blocks)
	}
}

func TestParseAssistant_GeminiJSON(t *testing.T) {
	body := []byte(`{
		"candidates": [{
			"content": {
				"role": "model",
				"parts": [
					{"text": "Hello from Gemini"},
					{"functionCall": {"name": "search", "args": {"q": "foo"}}}
				]
			}
		}]
	}`)
	blocks := parseAssistantResponse(body, ProviderGemini, 1000)
	if t1 := findBlock(blocks, "text"); t1 == nil || t1.Text != "Hello from Gemini" {
		t.Errorf("text: %+v", blocks)
	}
	if tu := findBlock(blocks, "tool_use"); tu == nil || tu.Tool != "search" {
		t.Errorf("tool_use: %+v", blocks)
	}
}

func TestParseAssistant_GeminiSSE(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"He"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"llo"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]}}]}`,
		``,
	}, "\n\n"))
	blocks := parseAssistantResponse(body, ProviderGemini, 1000)
	if txt := findBlock(blocks, "text"); txt == nil || txt.Text != "Hello" {
		t.Errorf("text: %+v", blocks)
	}
	if tu := findBlock(blocks, "tool_use"); tu == nil || tu.Tool != "f" {
		t.Errorf("tool_use: %+v", blocks)
	}
}

func TestParseAssistant_GeminiArrayChunks(t *testing.T) {
	body := []byte(`[{"candidates":[{"content":{"parts":[{"text":"a"}]}}]},{"candidates":[{"content":{"parts":[{"text":"b"}]}}]}]`)
	blocks := parseAssistantResponse(body, ProviderGemini, 1000)
	if txt := findBlock(blocks, "text"); txt == nil || txt.Text != "ab" {
		t.Errorf("merged text: %+v", blocks)
	}
}

func TestParseAssistant_EmptyAndErrorBodies(t *testing.T) {
	if blocks := parseAssistantResponse(nil, ProviderAnthropic, 1000); blocks != nil {
		t.Errorf("nil body should produce nil blocks, got %+v", blocks)
	}
	// Anthropic error response shape — no `content` array.
	if blocks := parseAssistantResponse([]byte(`{"type":"error","error":{"message":"x"}}`), ProviderAnthropic, 1000); blocks != nil {
		t.Errorf("error body should produce nil blocks, got %+v", blocks)
	}
}
