package contextbudget

import "testing"

// Helper: assert estimate is within tolerance of expected. Char-based
// estimation is intentionally fuzzy, so we use ±10% tolerance on small
// fixtures and exact equality where the math is deterministic.
func assertTokensApprox(t *testing.T, got, want int) {
	t.Helper()
	if got == want {
		return
	}
	tol := want / 10
	if tol < 2 {
		tol = 2
	}
	if got < want-tol || got > want+tol {
		t.Errorf("EstimateTokens = %d, want %d ±%d", got, want, tol)
	}
}

func TestEstimateTokens_EmptyOrUnknown(t *testing.T) {
	if EstimateTokens(nil, ProtoClaude) != 0 {
		t.Error("nil body should yield 0")
	}
	if EstimateTokens([]byte(`{"messages":[]}`), ProtoUnknown) != 0 {
		t.Error("ProtoUnknown should yield 0")
	}
}

func TestEstimateTokens_Claude_StringContent(t *testing.T) {
	// 400 chars / 4 = 100 tokens. System (40 chars) + user (360 chars).
	body := `{
		"system": "0123456789012345678901234567890123456789",
		"messages": [
			{"role":"user","content":"` + repeat("a", 360) + `"}
		]
	}`
	got := EstimateTokens([]byte(body), ProtoClaude)
	assertTokensApprox(t, got, (40+360)/4)
}

func TestEstimateTokens_Claude_ArrayContent(t *testing.T) {
	body := `{
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"` + repeat("a", 200) + `"},
				{"type":"image","source":{"type":"base64","data":"` + repeat("Z", 5000) + `"}},
				{"type":"text","text":"` + repeat("b", 200) + `"}
			]}
		]
	}`
	got := EstimateTokens([]byte(body), ProtoClaude)
	// 400 chars of text / 4 = 100. Image base64 must be excluded.
	assertTokensApprox(t, got, 100)
}

func TestEstimateTokens_Claude_ToolResult(t *testing.T) {
	body := `{
		"messages": [
			{"role":"assistant","content":[{"type":"tool_use","name":"grep","input":{"pattern":"foo"}}]},
			{"role":"user","content":[{"type":"tool_result","content":"` + repeat("x", 400) + `"}]}
		]
	}`
	got := EstimateTokens([]byte(body), ProtoClaude)
	if got < 90 {
		t.Errorf("expected at least ~90 tokens from tool_result, got %d", got)
	}
}

func TestEstimateTokens_OpenAIChat_StringContent(t *testing.T) {
	body := `{
		"messages": [
			{"role":"system","content":"` + repeat("a", 40) + `"},
			{"role":"user","content":"` + repeat("b", 400) + `"}
		]
	}`
	got := EstimateTokens([]byte(body), ProtoOpenAIChat)
	// system role's content is also walked because we iterate ALL messages
	// regardless of role for OpenAI Chat.
	assertTokensApprox(t, got, (40+400)/4)
}

func TestEstimateTokens_OpenAIChat_ImageDataURISkipped(t *testing.T) {
	body := `{
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"` + repeat("a", 400) + `"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,` + repeat("Z", 5000) + `"}}
			]}
		]
	}`
	got := EstimateTokens([]byte(body), ProtoOpenAIChat)
	assertTokensApprox(t, got, 100)
}

func TestEstimateTokens_OpenAIResponses_StringInput(t *testing.T) {
	body := `{
		"instructions": "` + repeat("a", 40) + `",
		"input": "` + repeat("b", 400) + `"
	}`
	got := EstimateTokens([]byte(body), ProtoOpenAIResponses)
	assertTokensApprox(t, got, (40+400)/4)
}

func TestEstimateTokens_OpenAIResponses_ArrayInput(t *testing.T) {
	body := `{
		"input": [
			{"type":"message","role":"system","content":[{"type":"input_text","text":"` + repeat("a", 40) + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + repeat("b", 400) + `"}]},
			{"type":"function_call","name":"grep","arguments":"{\"pattern\":\"foo\"}"}
		]
	}`
	got := EstimateTokens([]byte(body), ProtoOpenAIResponses)
	if got < 100 {
		t.Errorf("expected >= 100 tokens, got %d", got)
	}
}

func TestEstimateTokens_Gemini_Contents(t *testing.T) {
	body := `{
		"systemInstruction": {"parts": [{"text":"` + repeat("a", 40) + `"}]},
		"contents": [
			{"role":"user","parts":[{"text":"` + repeat("b", 400) + `"}]},
			{"role":"model","parts":[{"text":"` + repeat("c", 200) + `"}]},
			{"role":"user","parts":[{"text":"` + repeat("d", 400) + `"}]}
		]
	}`
	got := EstimateTokens([]byte(body), ProtoGemini)
	assertTokensApprox(t, got, (40+400+200+400)/4)
}

func TestEstimateTokens_Gemini_InlineDataSkipped(t *testing.T) {
	body := `{
		"contents": [
			{"role":"user","parts":[
				{"text":"` + repeat("a", 400) + `"},
				{"inlineData":{"mimeType":"image/png","data":"` + repeat("Z", 5000) + `"}}
			]}
		]
	}`
	got := EstimateTokens([]byte(body), ProtoGemini)
	assertTokensApprox(t, got, 100)
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
