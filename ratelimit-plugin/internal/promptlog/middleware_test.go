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

func TestMiddleware_DropsClaudeCodeSubagent(t *testing.T) {
	r, w, dir := newTestRig(t)
	defer w.Close()

	// Subagent: claude-cli UA, no system block carrying "Primary working
	// directory:". Parent CLI always sends one — only Task-dispatched
	// subagents and other internal flows omit it.
	body := `{"model":"claude","system":"You are a web search agent.","messages":[{"role":"user","content":"Perform a web search for the query: foo"}]}`
	req, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("User-Agent", "claude-cli/2.1.141 (external, cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "sub-session-1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	w.Close()

	if entries := readAllEntries(t, dir); len(entries) != 0 {
		t.Fatalf("expected subagent request to be dropped, got %d entries", len(entries))
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

	// Non-Claude-Code clients (curl, raw SDK, opencode) often have no cwd
	// either, but they're not subagents — keep them.
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
