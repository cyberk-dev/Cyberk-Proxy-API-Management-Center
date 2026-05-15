package promptlog

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestDetectProvider(t *testing.T) {
	cases := map[string]string{
		"/v1/messages":                                       ProviderAnthropic,
		"/v1/chat/completions":                               ProviderOpenAIChat,
		"/v1/responses":                                      ProviderOpenAIResponses,
		"/v1beta/models/gemini-2.5-pro:generateContent":      ProviderGemini,
		"/v1beta/models/gemini-2.5-pro:streamGenerateContent": ProviderGemini,
		"/v1/models":   "",
		"/healthz":     "",
		"/v1beta/models": "",
		"/v0/management/config": "",
	}
	for path, want := range cases {
		if got := detectProvider(path); got != want {
			t.Errorf("detectProvider(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestExtract_AnthropicStringContent(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-7",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi"},
			{"role": "user", "content": "what is 2+2?"}
		]
	}`)
	blocks := extractBlocks(body, ProviderAnthropic, 1000)
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "what is 2+2?" {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtract_AnthropicArrayContent_WithImage(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "describe this"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "` + b64 + `"}}
			]}
		]
	}`)
	blocks := extractBlocks(body, ProviderAnthropic, 1000)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "describe this" {
		t.Errorf("text block: %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].MediaType != "image/png" || blocks[1].Bytes != len(pngBytes) {
		t.Errorf("image block: %+v", blocks[1])
	}
	if blocks[1].SHA256 == "" {
		t.Errorf("expected sha256")
	}
}

func TestExtract_AnthropicSkipToolBlocks(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "x", "content": "result"},
				{"type": "text", "text": "follow-up"}
			]}
		]
	}`)
	blocks := extractBlocks(body, ProviderAnthropic, 1000)
	if len(blocks) != 1 || blocks[0].Text != "follow-up" {
		t.Fatalf("expected tool_result dropped, got %+v", blocks)
	}
}

func TestExtract_AnthropicImageURL(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "url", "url": "https://example.com/x.png", "media_type": "image/png"}}
			]}
		]
	}`)
	blocks := extractBlocks(body, ProviderAnthropic, 1000)
	if len(blocks) != 1 || blocks[0].URL != "https://example.com/x.png" {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtract_OpenAIChatString(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "system", "content": "you are helpful"},
			{"role": "user", "content": "ping"}
		]
	}`)
	blocks := extractBlocks(body, ProviderOpenAIChat, 1000)
	if len(blocks) != 1 || blocks[0].Text != "ping" {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtract_OpenAIChatDataURL(t *testing.T) {
	payload := []byte("fake-png-bytes")
	b64 := base64.StdEncoding.EncodeToString(payload)
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what's this"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,` + b64 + `"}}
			]}
		]
	}`)
	blocks := extractBlocks(body, ProviderOpenAIChat, 1000)
	if len(blocks) != 2 {
		t.Fatalf("got %d", len(blocks))
	}
	if blocks[1].Type != "image" || blocks[1].MediaType != "image/png" || blocks[1].Bytes != len(payload) {
		t.Errorf("image: %+v", blocks[1])
	}
}

func TestExtract_OpenAIChatRemoteURL(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "image_url", "image_url": {"url": "https://cdn.example.com/x.png"}}
			]}
		]
	}`)
	blocks := extractBlocks(body, ProviderOpenAIChat, 1000)
	if len(blocks) != 1 || blocks[0].URL != "https://cdn.example.com/x.png" {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtract_OpenAIResponsesStringInput(t *testing.T) {
	body := []byte(`{"model": "gpt-5", "input": "tell me a joke"}`)
	blocks := extractBlocks(body, ProviderOpenAIResponses, 1000)
	if len(blocks) != 1 || blocks[0].Text != "tell me a joke" {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtract_OpenAIResponsesArray(t *testing.T) {
	payload := []byte("xyz")
	b64 := base64.StdEncoding.EncodeToString(payload)
	body := []byte(`{
		"input": [
			{"role": "system", "content": [{"type": "input_text", "text": "sys"}]},
			{"role": "user", "content": [
				{"type": "input_text", "text": "summarize"},
				{"type": "input_file", "file_data": "` + b64 + `", "filename": "doc.pdf"}
			]}
		]
	}`)
	blocks := extractBlocks(body, ProviderOpenAIResponses, 1000)
	if len(blocks) != 2 {
		t.Fatalf("got %d: %+v", len(blocks), blocks)
	}
	if blocks[1].Type != "document" || blocks[1].URL != "doc.pdf" || blocks[1].Bytes != len(payload) {
		t.Errorf("file: %+v", blocks[1])
	}
}

func TestExtract_GeminiInlineData(t *testing.T) {
	payload := []byte("img")
	b64 := base64.StdEncoding.EncodeToString(payload)
	body := []byte(`{
		"contents": [
			{"role": "user", "parts": [
				{"text": "what is this"},
				{"inlineData": {"mimeType": "image/jpeg", "data": "` + b64 + `"}}
			]}
		]
	}`)
	blocks := extractBlocks(body, ProviderGemini, 1000)
	if len(blocks) != 2 {
		t.Fatalf("got %d", len(blocks))
	}
	if blocks[1].Type != "image" || blocks[1].MediaType != "image/jpeg" {
		t.Errorf("image: %+v", blocks[1])
	}
}

func TestExtract_GeminiMissingRoleTreatedAsUser(t *testing.T) {
	body := []byte(`{
		"contents": [
			{"parts": [{"text": "hello"}]}
		]
	}`)
	blocks := extractBlocks(body, ProviderGemini, 1000)
	if len(blocks) != 1 || blocks[0].Text != "hello" {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtract_GeminiSkipsFunctionCalls(t *testing.T) {
	body := []byte(`{
		"contents": [
			{"role": "user", "parts": [
				{"functionResponse": {"name": "x", "response": {}}},
				{"text": "ok"}
			]}
		]
	}`)
	blocks := extractBlocks(body, ProviderGemini, 1000)
	if len(blocks) != 1 || blocks[0].Text != "ok" {
		t.Fatalf("got %+v", blocks)
	}
}

func TestExtract_LastUserMessageWins(t *testing.T) {
	// Multi-turn — the FINAL user-role message is what we want, not the first.
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "first turn"},
			{"role": "assistant", "content": "reply"},
			{"role": "user", "content": "second turn"},
			{"role": "assistant", "content": "reply2"},
			{"role": "user", "content": "third turn"}
		]
	}`)
	blocks := extractBlocks(body, ProviderOpenAIChat, 1000)
	if len(blocks) != 1 || blocks[0].Text != "third turn" {
		t.Fatalf("expected last user turn, got %+v", blocks)
	}
}

func TestExtract_OpenAIChatSkipsToolRoundtrip(t *testing.T) {
	// Agent-loop continuation: user typed "do X" earlier, model returned
	// tool_calls, runtime appended the tool result. The last user-role
	// message is still "do X" — without the last-must-be-user guard we would
	// re-log "do X" on every iteration. Expect nil so the writer skips this
	// request entirely.
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "do X"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "f", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "result"}
		]
	}`)
	if blocks := extractBlocks(body, ProviderOpenAIChat, 1000); blocks != nil {
		t.Fatalf("expected nil for tool-roundtrip continuation, got %+v", blocks)
	}
}

func TestExtract_OpenAIChatSkipsAssistantTrailing(t *testing.T) {
	// Defensive: a request whose last message is role:"assistant" cannot be
	// a fresh user prompt and should be skipped.
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "ping"},
			{"role": "assistant", "content": "pong"}
		]
	}`)
	if blocks := extractBlocks(body, ProviderOpenAIChat, 1000); blocks != nil {
		t.Fatalf("expected nil when last message is assistant, got %+v", blocks)
	}
}

func TestExtract_OpenAIResponsesSkipsFunctionOutput(t *testing.T) {
	// Responses agent loop: the runtime appends a function_call_output item
	// (no role) after the model's function_call. Last item is not a user
	// message → skip.
	body := []byte(`{
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "compute"}]},
			{"type": "function_call", "call_id": "c1", "name": "f", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "c1", "output": "42"}
		]
	}`)
	if blocks := extractBlocks(body, ProviderOpenAIResponses, 1000); blocks != nil {
		t.Fatalf("expected nil for function_call_output trailing, got %+v", blocks)
	}
}

func TestExtract_OpenAIResponsesLogsNewUserMessage(t *testing.T) {
	// After the loop completes and the user types a follow-up, the new user
	// message becomes the trailing item — extract should fire normally.
	body := []byte(`{
		"input": [
			{"role": "user", "content": [{"type": "input_text", "text": "compute"}]},
			{"type": "function_call", "call_id": "c1", "name": "f", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "c1", "output": "42"},
			{"role": "assistant", "content": [{"type": "output_text", "text": "got 42"}]},
			{"role": "user", "content": [{"type": "input_text", "text": "now double it"}]}
		]
	}`)
	blocks := extractBlocks(body, ProviderOpenAIResponses, 1000)
	if len(blocks) != 1 || blocks[0].Text != "now double it" {
		t.Fatalf("expected the trailing user message, got %+v", blocks)
	}
}

func TestExtract_NoUserMessage(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "system", "content": "be helpful"},
			{"role": "assistant", "content": "hi"}
		]
	}`)
	if blocks := extractBlocks(body, ProviderOpenAIChat, 1000); blocks != nil {
		t.Fatalf("expected nil, got %+v", blocks)
	}
}

func TestExtract_DropsSystemReminderWrapper(t *testing.T) {
	body := []byte(`{
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "<system-reminder>\nUserPromptSubmit hook success: OK\n</system-reminder>\n"},
			{"type": "text", "text": "ok xử lý ddi"}
		]}]
	}`)
	blocks := extractBlocks(body, ProviderAnthropic, 1000)
	if len(blocks) != 1 || blocks[0].Text != "ok xử lý ddi" {
		t.Fatalf("expected wrapper dropped, got %+v", blocks)
	}
}

func TestExtract_DropsAllWrapperKinds(t *testing.T) {
	body := []byte(`{
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "<local-command-stdout>some output</local-command-stdout>"},
			{"type": "text", "text": "<command-name>/compact</command-name>"},
			{"type": "text", "text": "<local-command-caveat>caveat text</local-command-caveat>"},
			{"type": "text", "text": "real prompt"}
		]}]
	}`)
	blocks := extractBlocks(body, ProviderAnthropic, 1000)
	if len(blocks) != 1 || blocks[0].Text != "real prompt" {
		t.Fatalf("expected all wrappers dropped, got %+v", blocks)
	}
}

func TestExtract_AllWrappersBecomesNil(t *testing.T) {
	// User message that contains only wrapper noise → effectively no user
	// content → nil so the middleware skips logging.
	body := []byte(`{
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "<system-reminder>hint</system-reminder>"}
		]}]
	}`)
	if blocks := extractBlocks(body, ProviderAnthropic, 1000); blocks != nil {
		t.Fatalf("expected nil, got %+v", blocks)
	}
}

func TestExtract_DropsSyntheticCLIPrompts(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"suggestion-mode", "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]\n\nFIRST: Look at the user's recent messages…"},
		{"skill-body", "Base directory for this skill: /Users/x/.claude/plugins/foo\n\n# Systematic Debugging\n\nSteps:"},
		{"compaction", "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion…"},
		{"subagent", "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\n- Do NOT use Read, Bash, Grep…"},
		{"opencode-summary", "Create a new anchored summary from the conversation history above.\n\nOutput exactly the Markdown structure shown inside <template>…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(`{"messages":[{"role":"user","content":` + jsonString(tc.text) + `}]}`)
			if blocks := extractBlocks(payload, ProviderAnthropic, 5000); blocks != nil {
				t.Fatalf("expected nil for synthetic CLI prompt, got %+v", blocks)
			}
		})
	}
}

func TestExtract_KeepsProseMentioningSyntheticPrefix(t *testing.T) {
	// A real user message that merely talks about the prefix (not starting
	// with it) must be preserved. Detection is startswith on a trimmed
	// first text block, so leading prose anchors the message as human.
	body := []byte(`{"messages":[{"role":"user","content":"why does my log show '[SUGGESTION MODE:' entries?"}]}`)
	blocks := extractBlocks(body, ProviderAnthropic, 1000)
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("expected 1 text block, got %+v", blocks)
	}
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestExtract_KeepsTextThatMentionsTag(t *testing.T) {
	// Don't drop prose just because it CONTAINS a wrapper-looking string.
	body := []byte(`{
		"messages": [{"role": "user", "content": "the <system-reminder> tag is interesting"}]
	}`)
	blocks := extractBlocks(body, ProviderAnthropic, 1000)
	if len(blocks) != 1 || blocks[0].Text != "the <system-reminder> tag is interesting" {
		t.Fatalf("got %+v", blocks)
	}
}

func TestJoinPromptText(t *testing.T) {
	blocks := []Block{
		{Type: "text", Text: "describe this"},
		{Type: "image", MediaType: "image/png", Bytes: 12 * 1024, SHA256: "abc123"},
		{Type: "text", Text: "also explain"},
	}
	got := joinPromptText(blocks)
	want := "describe this\n\n[image:image/png 12KiB sha256=abc123]\n\nalso explain"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestJoinPromptText_FractionalSize(t *testing.T) {
	blocks := []Block{
		{Type: "image", MediaType: "image/png", Bytes: 12*1024 + 512, SHA256: "x"},
	}
	got := joinPromptText(blocks)
	want := "[image:image/png 12.5KiB sha256=x]"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestExtract_LongTextTruncated(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	body := []byte(`{"messages": [{"role": "user", "content": "` + long + `"}]}`)
	blocks := extractBlocks(body, ProviderOpenAIChat, 1024)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks", len(blocks))
	}
	if !blocks[0].Truncated {
		t.Error("expected truncated")
	}
	if blocks[0].OrigBytes != 100_000 {
		t.Errorf("orig bytes = %d", blocks[0].OrigBytes)
	}
}
