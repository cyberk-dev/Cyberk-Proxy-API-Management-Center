package promptlog

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestRig(t *testing.T) (*gin.Engine, *Writer, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWriter(dir, 16, nil, TemplatesConfig{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Enabled: true, Dir: dir, MaxTextBytes: 1024, QueueSize: 16}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/messages", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.POST("/blocked", func(c *gin.Context) { c.JSON(http.StatusBadRequest, gin.H{"error": "x"}) })
	r.POST("/v1/messages-blocked", func(c *gin.Context) { c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "no"}) })
	return r, w, dir
}

func TestMiddleware_LogsAnthropic(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("entries=%d", len(entries))
	}
	if entries[0]["provider"] != "anthropic" {
		t.Errorf("provider=%v", entries[0]["provider"])
	}
	if int(entries[0]["status"].(float64)) != 200 {
		t.Errorf("status=%v", entries[0]["status"])
	}
}

func TestMiddleware_CapturesRejectedRequest(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{Enabled: true, Dir: dir, MaxTextBytes: 1024, QueueSize: 16}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "rejected"})
	})

	body := `{"messages":[{"role":"user","content":"blocked prompt"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("entries=%d", len(entries))
	}
	if int(entries[0]["status"].(float64)) != 403 {
		t.Errorf("expected status=403 for rejected request, got %v", entries[0]["status"])
	}
}

func TestMiddleware_SkipsUnknownPaths(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	req, _ := http.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	if entries := readAllEntries(t, dir); len(entries) != 0 {
		t.Fatalf("expected no entries for /healthz, got %d", len(entries))
	}
}

func TestMiddleware_SkipsWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	defer w.Close()
	cfg := &Config{Enabled: false, Dir: dir}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/messages", func(c *gin.Context) { c.JSON(200, gin.H{}) })

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()
	if entries := readAllEntries(t, dir); len(entries) != 0 {
		t.Fatalf("expected no entries when disabled, got %d", len(entries))
	}
}

// Regression: sysHead must only strip the leading "x-anthropic-billing-header"
// preamble when its marker is actually present. A user-authored system
// prompt with a blank line in the first paragraph used to lose that
// paragraph (the early `idx < 512` strip was unconditional). Now the strip
// is gated on the marker, so a non-Claude-Code prompt with a blank line
// keeps its full head visible to the detectors.
func TestSysHead_PreservesParagraphsWithoutBillingHeader(t *testing.T) {
	sys := "Generate a concise summary of the following.\n\nDo not include personal opinions.\nReturn JSON."
	got := sysHead(sys, 256)
	if !strings.HasPrefix(got, "generate a concise summary") {
		t.Errorf("expected head to start with the user prompt, got %q", got)
	}
}

func TestSysHead_StripsClaudeCodeBillingPreamble(t *testing.T) {
	sys := "x-anthropic-billing-header: cc_version=2.1.143.3a9; cc_entrypoint=cli; cch=88d46;\n\nYou are Claude Code, Anthropic's official CLI for Claude."
	got := sysHead(sys, 256)
	if !strings.HasPrefix(got, "you are claude code") {
		t.Errorf("expected head to start past the billing preamble, got %q", got)
	}
}

func TestMiddleware_KeepsClaudeCodeSubagentWithAgentId(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	// Subagent: claude-cli UA + X-Claude-Code-Agent-Id (the unambiguous
	// header that Claude Code 2.1.143+ sets on every Task-tool dispatch).
	// Subagent shares parent's session id but has its own agent id. Used
	// to be dropped to avoid duplicate "(unknown)" UI cards — now kept
	// and tagged so the reader can render it indented under the parent.
	body := `{"model":"claude-haiku-4-5","system":"You are an agent for Claude Code","messages":[{"role":"user","content":"Calculate 1+1"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("User-Agent", "claude-cli/2.1.143 (external, cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "parent-session-1")
	req.Header.Set("X-Claude-Code-Agent-Id", "a84564f0326e0281b")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected subagent request to be kept, got %d entries", len(entries))
	}
	if got := entries[0]["agent_id"]; got != "a84564f0326e0281b" {
		t.Errorf("agent_id=%v want=a84564f0326e0281b", got)
	}
	if got := entries[0]["session_id"]; got != "parent-session-1" {
		t.Errorf("session_id=%v (should be parent's, kept verbatim)", got)
	}
}

func TestMiddleware_DropsClaudeCodeTitleGen(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	// Title-gen: Claude Code runs a parallel sub-call per conversation
	// turn whose system prompt starts with "Generate a concise…". It
	// has no env block (so cwd would be ""), no X-Claude-Code-Agent-Id,
	// and would otherwise be indistinguishable from a parent turn with
	// missing env. The sysHead-based detector catches it.
	body := `{"model":"claude","system":"Generate a concise, sentence-case title (3-7 words) that captures the main topic of this conversation.","messages":[{"role":"user","content":"<session>do thing</session>"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("User-Agent", "claude-cli/2.1.143 (external, cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "parent-session-1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	if entries := readAllEntries(t, dir); len(entries) != 0 {
		t.Fatalf("expected title-gen to be dropped, got %d entries: %+v", len(entries), entries)
	}
}

func TestMiddleware_KeepsOpencodeSubagentWithParentId(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	// Opencode 1.15.5+ feature: subagent dispatches carry X-Parent-Session-Id
	// pointing to the spawning session. Subagent has its own Session_id.
	// Reader will merge this entry's messages into the parent's session
	// card; here we just assert the middleware preserves the linkage on
	// disk. Body is an OpenAI-Responses-shaped request (opencode's path).
	body := `{"model":"gpt-5.5","input":[` +
		`{"role":"developer","content":"You are OpenCode, you and the user share the same workspace."},` +
		`{"role":"user","content":[{"type":"input_text","text":"Calculate 1+1. Return only the numeric answer."}]}` +
		`]}`
	req, _ := http.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("User-Agent", "opencode/1.15.5 (darwin 25.4.0; arm64) ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
	req.Header.Set("Session_id", "ses_1bc965ecaffeYsbNVmshuHo4aT")
	req.Header.Set("X-Parent-Session-Id", "ses_1bc967092ffeGtKzXWe1lpywr4")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected opencode subagent request to be kept, got %d entries", len(entries))
	}
	if got := entries[0]["parent_session_id"]; got != "ses_1bc967092ffeGtKzXWe1lpywr4" {
		t.Errorf("parent_session_id=%v", got)
	}
	if got := entries[0]["session_id"]; got != "ses_1bc965ecaffeYsbNVmshuHo4aT" {
		t.Errorf("session_id=%v (should be subagent's own, not parent's)", got)
	}
}

func TestMiddleware_KeepsClaudeCodeParentWithCWD(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	// Parent CLI: same UA but system text carries the env block, so cwd
	// extraction succeeds and the entry must be kept.
	body := `{"model":"claude","system":"# Environment\nYou have been invoked in the following environment:\n - Primary working directory: /home/u/proj\n","messages":[{"role":"user","content":"hello"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("User-Agent", "claude-cli/2.1.141 (external, cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "parent-session-1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected parent CLI request to be kept, got %d entries", len(entries))
	}
	if entries[0]["cwd"] != "/home/u/proj" {
		t.Errorf("cwd=%v", entries[0]["cwd"])
	}
}

func TestMiddleware_KeepsNonClaudeCodeWithoutCWD(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	// Non-Claude-Code clients without a session header (curl, raw SDK calls)
	// often have no cwd either, but they're not subagents — keep them. The
	// sub-call skip only fires when Session_id is present OR the client is
	// Claude Code, so curl/no-session/no-cwd passes through.
	body := `{"model":"claude","messages":[{"role":"user","content":"hello"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("User-Agent", "curl/8.7.1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	if entries := readAllEntries(t, dir); len(entries) != 1 {
		t.Fatalf("expected curl request to be kept, got %d entries", len(entries))
	}
}

func TestMiddleware_DropsOpencodeTitleGen(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	// opencode 1.15+ runs a parallel gpt-5-nano title-generation call per
	// turn. It carries the same Session_id as the main chat but ships a
	// synthetic "You are a title generator…" developer block instead of the
	// usual env block — so cwd extraction returns "". Without this skip the
	// title-gen call would surface as a second session card in the UI under
	// "(unknown)" alongside the real (project_cwd, sid) one.
	body := `{"model":"gpt-5-nano","input":[` +
		`{"role":"developer","content":"You are a title generator. You output ONLY a thread title."},` +
		`{"role":"user","content":[{"type":"input_text","text":"Generate a title for this conversation:"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"how do I get rich?"}]}` +
		`]}`
	req, _ := http.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("User-Agent", "opencode/1.15.4 (darwin 25.4.0; arm64) ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13")
	req.Header.Set("Session_id", "ses_1c41dd458ffePWlRar2V4bKbNL")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	if entries := readAllEntries(t, dir); len(entries) != 0 {
		t.Fatalf("expected opencode title-gen to be dropped, got %d entries: %+v", len(entries), entries)
	}
}

func TestMiddleware_KeepsOpencodeMainChatWithCWD(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	// Main opencode chat: same Session_id as the title-gen above, but the
	// developer block carries the env hint that resolves to a cwd. The skip
	// must NOT fire here — this is the entry that should appear in the UI.
	body := `{"model":"gpt-5.5","input":[` +
		`{"role":"developer","content":"You are OpenCode.\n<env>\n  Working directory: /home/u/proj\n</env>"},` +
		`{"role":"user","content":[{"type":"input_text","text":"how do I get rich?"}]}` +
		`]}`
	req, _ := http.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("User-Agent", "opencode/1.15.4 (darwin 25.4.0; arm64) ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13")
	req.Header.Set("Session_id", "ses_1c41dd458ffePWlRar2V4bKbNL")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected opencode main chat to be kept, got %d entries", len(entries))
	}
	if entries[0]["session_id"] != "ses_1c41dd458ffePWlRar2V4bKbNL" {
		t.Errorf("session_id=%v", entries[0]["session_id"])
	}
	if entries[0]["cwd"] != "/home/u/proj" {
		t.Errorf("cwd=%v", entries[0]["cwd"])
	}
}

func TestMiddleware_LogsAssistantResponseAnthropic(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{
		Enabled:              true,
		Dir:                  dir,
		MaxTextBytes:         1024,
		QueueSize:            16,
		LogAssistantResponse: true,
		MaxResponseBytes:     64 * 1024,
	}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"id":      "msg_1",
			"role":    "assistant",
			"content": []gin.H{{"type": "text", "text": "hello back"}},
		})
	})

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 2 {
		t.Fatalf("expected user+assistant pair, got %d: %+v", len(entries), entries)
	}
	var sawUser, sawAssistant bool
	for _, e := range entries {
		role, _ := e["role"].(string)
		switch role {
		case "user":
			sawUser = true
			if e["prompt"] != "hi" {
				t.Errorf("user prompt: %v", e["prompt"])
			}
		case "assistant":
			sawAssistant = true
			if e["prompt"] != "hello back" {
				t.Errorf("assistant prompt: %v", e["prompt"])
			}
		}
	}
	if !sawUser || !sawAssistant {
		t.Errorf("missing role(s): user=%v assistant=%v", sawUser, sawAssistant)
	}
}

func TestMiddleware_AssistantSkipsWhenDisabled(t *testing.T) {
	// Default Config (LogAssistantResponse unset) must NOT emit an assistant
	// entry — preserves opt-in behavior when callers construct cfg without
	// going through ParseBytes.
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{Enabled: true, Dir: dir, MaxTextBytes: 1024, QueueSize: 16}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"content": []gin.H{{"type": "text", "text": "hello"}}})
	})

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected only user entry when assistant logging off, got %d", len(entries))
	}
	if role, _ := entries[0]["role"].(string); role != "user" {
		t.Errorf("expected role=user, got %q", role)
	}
}

func TestMiddleware_AssistantSkipsOnErrorStatus(t *testing.T) {
	// 4xx/5xx responses have no useful assistant content — skip the second
	// entry even when LogAssistantResponse is on.
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{
		Enabled:              true,
		Dir:                  dir,
		MaxTextBytes:         1024,
		QueueSize:            16,
		LogAssistantResponse: true,
		MaxResponseBytes:     64 * 1024,
	}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit"})
	})

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("error response must not yield assistant entry, got %d", len(entries))
	}
}

// Agent-loop continuation tests: each provider has a "last message is not a
// fresh user prompt" shape (Anthropic: user content is only tool_result;
// OpenAI Chat: role:"tool"; OpenAI Responses: function_call_output; Gemini:
// user content is only functionResponse). In every case the human prompt was
// already logged on the turn it first appeared, so the middleware suppresses
// the user-side entry — but MUST still capture the assistant response, which
// carries the next tool_use or the final text answer. The previous behavior
// was to bail out entirely, which dropped both sides and left the UI showing
// only turn 1 of a multi-tool-call session.

func TestMiddleware_AssistantCapturedOnAnthropicToolLoop(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{
		Enabled:              true,
		Dir:                  dir,
		MaxTextBytes:         1024,
		QueueSize:            16,
		LogAssistantResponse: true,
		MaxResponseBytes:     64 * 1024,
	}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"id":      "msg_2",
			"role":    "assistant",
			"content": []gin.H{{"type": "text", "text": "the answer is 42"}},
		})
	})

	// Tool-loop continuation: assistant emitted a tool_use last turn, client
	// now POSTs the conversation back with the tool_result appended as the
	// new last user-role message. extractBlocks returns [tool_result], which
	// is isToolOnly — user-side must be suppressed, assistant-side must not.
	body := `{"model":"claude","messages":[` +
		`{"role":"user","content":"compute 6*7"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"calc","input":{"expr":"6*7"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"42"}]}` +
		`]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 assistant entry (user suppressed), got %d: %+v", len(entries), entries)
	}
	if role, _ := entries[0]["role"].(string); role != "assistant" {
		t.Errorf("expected role=assistant, got %q", role)
	}
	if entries[0]["prompt"] != "the answer is 42" {
		t.Errorf("assistant prompt=%v", entries[0]["prompt"])
	}
}

func TestMiddleware_AssistantCapturedOnAnthropicSSEToolLoop(t *testing.T) {
	// SSE variant — production traffic is almost always streamed, so the
	// JSON-shaped test above misses the dominant path. Handler emits a
	// minimal Anthropic stream (one text block) so parseAnthropicSSE has
	// to assemble it.
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{
		Enabled:              true,
		Dir:                  dir,
		MaxTextBytes:         1024,
		QueueSize:            16,
		LogAssistantResponse: true,
		MaxResponseBytes:     64 * 1024,
	}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.WriteHeader(http.StatusOK)
		// Minimal frame set: open a text block, deliver one delta, close.
		// parseAnthropicSSE only needs content_block_start + _delta to
		// produce a text block; message_start / message_delta / stop are
		// ignored here for brevity.
		_, _ = c.Writer.Write([]byte(
			"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the answer is 42"}}` + "\n\n",
		))
	})

	body := `{"model":"claude","messages":[` +
		`{"role":"user","content":"compute 6*7"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"calc","input":{"expr":"6*7"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"42"}]}` +
		`]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 assistant entry (user suppressed), got %d: %+v", len(entries), entries)
	}
	if role, _ := entries[0]["role"].(string); role != "assistant" {
		t.Errorf("expected role=assistant, got %q", role)
	}
	if entries[0]["prompt"] != "the answer is 42" {
		t.Errorf("assistant prompt=%v", entries[0]["prompt"])
	}
}

func TestMiddleware_AssistantCapturedOnOpenAIChatToolLoop(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{
		Enabled:              true,
		Dir:                  dir,
		MaxTextBytes:         1024,
		QueueSize:            16,
		LogAssistantResponse: true,
		MaxResponseBytes:     64 * 1024,
	}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"choices": []gin.H{
				{"message": gin.H{"role": "assistant", "content": "the answer is 42"}},
			},
		})
	})

	// OpenAI Chat tool-loop continuation: last message has role:"tool"
	// (NOT "user"). extractIfLastIsUser returns nil → blocks empty → user
	// entry must be suppressed. Assistant capture must still happen.
	body := `{"model":"gpt-4","messages":[` +
		`{"role":"user","content":"compute 6*7"},` +
		`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"6*7\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"42"}` +
		`]}`
	req, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 assistant entry (user suppressed), got %d: %+v", len(entries), entries)
	}
	if role, _ := entries[0]["role"].(string); role != "assistant" {
		t.Errorf("expected role=assistant, got %q", role)
	}
	if entries[0]["prompt"] != "the answer is 42" {
		t.Errorf("assistant prompt=%v", entries[0]["prompt"])
	}
}

func TestMiddleware_AssistantCapturedOnOpenAIResponsesToolLoop(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{
		Enabled:              true,
		Dir:                  dir,
		MaxTextBytes:         1024,
		QueueSize:            16,
		LogAssistantResponse: true,
		MaxResponseBytes:     64 * 1024,
	}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1/responses", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"output": []gin.H{
				{
					"type": "message",
					"content": []gin.H{
						{"type": "output_text", "text": "the answer is 42"},
					},
				},
			},
		})
	})

	// OpenAI Responses tool-loop continuation: last input item is a typed
	// function_call_output (no role). lastUserContent returns false →
	// blocks nil → user entry suppressed; assistant capture must still fire.
	body := `{"model":"gpt-5","input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"compute 6*7"}]},` +
		`{"type":"function_call","name":"calc","arguments":"{\"expr\":\"6*7\"}","call_id":"call_1"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"42"}` +
		`]}`
	req, _ := http.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 assistant entry (user suppressed), got %d: %+v", len(entries), entries)
	}
	if role, _ := entries[0]["role"].(string); role != "assistant" {
		t.Errorf("expected role=assistant, got %q", role)
	}
	if entries[0]["prompt"] != "the answer is 42" {
		t.Errorf("assistant prompt=%v", entries[0]["prompt"])
	}
}

func TestMiddleware_AssistantCapturedOnGeminiToolLoop(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir, 16, nil, TemplatesConfig{})
	cfg := &Config{
		Enabled:              true,
		Dir:                  dir,
		MaxTextBytes:         1024,
		QueueSize:            16,
		LogAssistantResponse: true,
		MaxResponseBytes:     64 * 1024,
	}
	r := gin.New()
	r.Use(Middleware(cfg, w))
	r.POST("/v1beta/models/gemini-2.5-pro:generateContent", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"candidates": []gin.H{
				{
					"content": gin.H{
						"parts": []gin.H{{"text": "the answer is 42"}},
					},
				},
			},
		})
	})

	// Gemini tool-loop continuation: last role:"user" content carries only
	// a functionResponse part (the tool result). extractGeminiPart maps it
	// to a tool_result block → isToolOnly true → user entry suppressed,
	// assistant capture must still fire.
	body := `{"contents":[` +
		`{"role":"user","parts":[{"text":"compute 6*7"}]},` +
		`{"role":"model","parts":[{"functionCall":{"name":"calc","args":{"expr":"6*7"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"calc","response":{"result":42}}}]}` +
		`]}`
	req, _ := http.NewRequest("POST", "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	w.Close()

	entries := readAllEntries(t, dir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 assistant entry (user suppressed), got %d: %+v", len(entries), entries)
	}
	if role, _ := entries[0]["role"].(string); role != "assistant" {
		t.Errorf("expected role=assistant, got %q", role)
	}
	if entries[0]["prompt"] != "the answer is 42" {
		t.Errorf("assistant prompt=%v", entries[0]["prompt"])
	}
}

func readAllEntries(t *testing.T, dir string) []map[string]any {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "prompts-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, m := range matches {
		date := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "prompts-"), ".jsonl")
		out = append(out, readDailyFile(t, dir, date)...)
	}
	return out
}
