package promptlog

import "testing"

func TestExtractSystemText_AnthropicString(t *testing.T) {
	body := []byte(`{"system": "be helpful", "messages": []}`)
	if got := extractSystemText(body, ProviderAnthropic); got != "be helpful" {
		t.Errorf("got %q", got)
	}
}

func TestExtractSystemText_AnthropicArray(t *testing.T) {
	body := []byte(`{"system": [{"type":"text","text":"a"},{"type":"text","text":"b"}]}`)
	got := extractSystemText(body, ProviderAnthropic)
	if got != "a\nb\n" {
		t.Errorf("got %q", got)
	}
}

func TestExtractSystemText_OpenAIChat(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"sys1"},
		{"role":"user","content":"hi"},
		{"role":"developer","content":"sys2"}
	]}`)
	got := extractSystemText(body, ProviderOpenAIChat)
	if got != "sys1\nsys2\n" {
		t.Errorf("got %q", got)
	}
}

func TestExtractSystemText_OpenAIResponses(t *testing.T) {
	body := []byte(`{
		"instructions": "global instr",
		"input": [
			{"role":"system","content":[{"type":"input_text","text":"sys block"}]},
			{"role":"user","content":"hi"}
		]
	}`)
	got := extractSystemText(body, ProviderOpenAIResponses)
	if got != "global instr\nsys block\n" {
		t.Errorf("got %q", got)
	}
}

func TestExtractSystemText_Gemini(t *testing.T) {
	body := []byte(`{
		"systemInstruction": {"role":"system","parts":[{"text":"sys1"},{"text":"sys2"}]},
		"contents": []
	}`)
	got := extractSystemText(body, ProviderGemini)
	if got != "sys1\nsys2\n" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCWD_ClaudeCodeNew(t *testing.T) {
	in := "# Environment\nYou have been invoked in the following environment: \n - Primary working directory: /Users/u/proj\n - Platform: darwin\n"
	if got := extractCWD(in); got != "/Users/u/proj" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCWD_ClaudeCodeOld(t *testing.T) {
	in := "# Environment\nHere is useful information:\nWorking directory: /Users/u/old\n"
	if got := extractCWD(in); got != "/Users/u/old" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCWD_AmpWorkspaceRoot(t *testing.T) {
	in := "# Environment\nWorkspace root folder: /Users/User/Desktop/logs\n"
	if got := extractCWD(in); got != "/Users/User/Desktop/logs" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCWD_OpencodeEnvBlock(t *testing.T) {
	in := "<env>\n  Working directory: /Users/u/proj\n  Is directory a git repo: yes\n</env>\n"
	if got := extractCWD(in); got != "/Users/u/proj" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCWD_FirstPatternWins(t *testing.T) {
	// Both Primary and Workspace root appear (old Claude Code does this).
	// Primary takes priority since it's the most specific.
	in := "Primary working directory: /a\nWorkspace root folder: /b\n"
	if got := extractCWD(in); got != "/a" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCWD_None(t *testing.T) {
	if got := extractCWD("some unrelated system prompt"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCWD_EmptyInput(t *testing.T) {
	if got := extractCWD(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCWD_TrimsTrailingPunct(t *testing.T) {
	in := `Primary working directory: "/Users/u/proj",`
	if got := extractCWD(in); got != "/Users/u/proj" {
		t.Errorf("got %q", got)
	}
}
