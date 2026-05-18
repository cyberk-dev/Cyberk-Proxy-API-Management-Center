package contextbudget

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// containsReminder reports whether the body bytes carry the system-reminder
// marker in either its raw or JSON-unicode-escaped form. The mutation path
// for Claude/Responses goes through encoding/json (via sjson typed setters)
// which writes the escaped form; the OpenAI Chat path concatenates the
// reminder directly into a string field which sjson preserves verbatim.
// Tests use this helper so assertions are robust to both code paths.
func containsReminder(body []byte) bool {
	s := string(body)
	return strings.Contains(s, reminderMarker) || strings.Contains(s, reminderMarkerEscaped)
}

// newTestServer wires a contextbudget.Middleware after a peek-priming layer
// that mimics the real chain (ratelimit/policy populate the peek cache). The
// echo handler returns the raw body that ARRIVED at the handler, so tests can
// assert whether the mutation reached the request body.
func newTestServer(t *testing.T, store *ConfigStore) *httptest.Server {
	t.Helper()
	r := gin.New()
	// Prime the peek cache so contextbudget gets a peek hit, mirroring
	// production where promptlog/policy/ratelimit run first.
	r.Use(func(c *gin.Context) {
		ratelimit.PeekJSONBody(c)
		c.Next()
	})
	r.Use(Middleware(store))
	echo := func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	}
	r.POST("/v1/messages", echo)
	r.POST("/v1/chat/completions", echo)
	r.POST("/v1/responses", echo)
	// Gemini routes use a `:action` suffix on the model name, which Gin's
	// router treats as a path parameter — registering both
	// `...:generateContent` and `...:streamGenerateContent` as static
	// strings collides at parameter-name binding time. The real proxy uses
	// a single wildcard handler that parses the colon manually; we do the
	// same here.
	r.POST("/v1beta/models/*action", echo)
	r.POST("/v0/management/config", echo)
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	return httptest.NewServer(r)
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func tinyClaudeBody(userText string) string {
	b, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": userText},
		},
	})
	return string(b)
}

func TestMiddleware_DisabledPassthrough(t *testing.T) {
	store := NewConfigStore(mustParse(t, ``))
	srv := newTestServer(t, store)
	defer srv.Close()

	// Even a huge body must pass through when disabled.
	body := tinyClaudeBody(repeat("a", 5_000_000))
	resp := postJSON(t, srv.URL+"/v1/messages", body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("disabled config blocked request: status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, []byte(body)) {
		t.Error("disabled config must not mutate body")
	}
}

func TestMiddleware_UnderSoftPassthrough(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
  hard_threshold_tokens: 200
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	body := tinyClaudeBody("hello")
	resp := postJSON(t, srv.URL+"/v1/messages", body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("under-soft request blocked: status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, []byte(body)) {
		t.Errorf("under-soft body mutated:\n got: %s\nwant: %s", got, body)
	}
}

func TestMiddleware_SoftInjects(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
  hard_threshold_tokens: 1000
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	// 500 chars / 4 = 125 tokens, between soft(100) and hard(1000).
	body := tinyClaudeBody(repeat("a", 500))
	resp := postJSON(t, srv.URL+"/v1/messages", body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("soft threshold should not block: status=%d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !containsReminder(got) {
		t.Error("expected <system-reminder> tag in forwarded body")
	}
	// Original content must still be present.
	if !strings.Contains(string(got), repeat("a", 500)) {
		t.Error("original user content was lost during injection")
	}
}

func TestMiddleware_HardBlocks_NonStream(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
  hard_threshold_tokens: 200
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	// 1200 chars / 4 = 300 tokens, > hard(200).
	body := tinyClaudeBody(repeat("a", 1200))
	resp := postJSON(t, srv.URL+"/v1/messages", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	payload, _ := io.ReadAll(resp.Body)
	// Claude envelope: {type:"error", error:{type:"request_too_large"}}
	if gjson.GetBytes(payload, "error.type").String() != "request_too_large" {
		t.Errorf("unexpected error envelope: %s", payload)
	}
	if gjson.GetBytes(payload, "context_budget.hard_limit_tokens").Int() != 200 {
		t.Errorf("expected hard_limit_tokens in envelope, got: %s", payload)
	}
}

func TestMiddleware_HardBlocks_StreamingAnthropic(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
  hard_threshold_tokens: 200
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	// Body declares stream:true; expect SSE error chunk.
	bodyMap := map[string]any{
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": repeat("a", 1200)},
		},
	}
	b, _ := json.Marshal(bodyMap)
	resp := postJSON(t, srv.URL+"/v1/messages", string(b))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("expected SSE content-type, got %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "event: error") {
		t.Errorf("expected SSE error event, got: %q", raw)
	}
	if !strings.Contains(string(raw), "request_too_large") {
		t.Errorf("expected request_too_large in SSE payload, got: %q", raw)
	}
}

func TestMiddleware_HardBlocks_StreamingOpenAI(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 200
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	bodyMap := map[string]any{
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": repeat("a", 1200)},
		},
	}
	b, _ := json.Marshal(bodyMap)
	resp := postJSON(t, srv.URL+"/v1/chat/completions", string(b))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "context_length_exceeded") {
		t.Errorf("expected OpenAI error code in SSE payload, got: %q", raw)
	}
	if !strings.Contains(string(raw), "[DONE]") {
		t.Errorf("expected [DONE] terminator in OpenAI SSE error, got: %q", raw)
	}
}

func TestMiddleware_HardBlocks_StreamingGeminiByPath(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 200
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	bodyMap := map[string]any{
		"contents": []map[string]any{
			{
				"role":  "user",
				"parts": []map[string]any{{"text": repeat("a", 1200)}},
			},
		},
	}
	b, _ := json.Marshal(bodyMap)
	resp := postJSON(t, srv.URL+"/v1beta/models/gemini-2.5-pro:streamGenerateContent", string(b))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("expected SSE content-type for streamGenerateContent, got %q", ct)
	}
}

func TestMiddleware_SkipsManagement(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 10
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	body := tinyClaudeBody(repeat("a", 1000))
	resp := postJSON(t, srv.URL+"/v0/management/config", body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("management endpoint should not be blocked, got %d", resp.StatusCode)
	}
}

func TestMiddleware_SkipsCountTokens(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 10
`))
	r := gin.New()
	r.Use(func(c *gin.Context) { ratelimit.PeekJSONBody(c); c.Next() })
	r.Use(Middleware(store))
	r.POST("/v1/messages/count_tokens", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	body := tinyClaudeBody(repeat("a", 1000))
	resp := postJSON(t, srv.URL+"/v1/messages/count_tokens", body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("count_tokens should not be blocked, got %d", resp.StatusCode)
	}
}

func TestMiddleware_NonJSONPassthrough(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 10
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	// large multipart body — must not be inspected as JSON
	huge := bytes.Repeat([]byte("X"), 5000)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", bytes.NewReader(huge))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=bnd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("non-JSON should pass through, got %d", resp.StatusCode)
	}
}

func TestMiddleware_HotReload(t *testing.T) {
	store := NewConfigStore(mustParse(t, ``)) // disabled
	srv := newTestServer(t, store)
	defer srv.Close()

	body := tinyClaudeBody(repeat("a", 1200))
	resp := postJSON(t, srv.URL+"/v1/messages", body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("before reload should pass, got %d", resp.StatusCode)
	}

	store.Set(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 100
`))
	resp = postJSON(t, srv.URL+"/v1/messages", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("after reload should block, got %d", resp.StatusCode)
	}
}

func TestMiddleware_TruncatedBodyHardBlocks(t *testing.T) {
	// A request body larger than the 16 MiB peek cap is sliced mid-object
	// before contextbudget sees it. The naive path is to gjson-walk the
	// truncated slice and end up with tokens=0, which would silently let
	// the exact requests we most want to stop slip through. We assert the
	// fix: a truncated peek must short-circuit to a hard block regardless
	// of the configured threshold.
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 999999999
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	// 17 MiB body. Use a flat text content so the JSON is well-formed up
	// to the cap; the assertion is about the truncation signal, not the
	// estimator.
	huge := tinyClaudeBody(repeat("a", 17*1024*1024))
	resp := postJSON(t, srv.URL+"/v1/messages", huge)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 from truncated body even under high hard threshold, got %d", resp.StatusCode)
	}
}

func TestMiddleware_ContentLengthUpdated(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
  hard_threshold_tokens: 1000
`))
	srv := newTestServer(t, store)
	defer srv.Close()

	body := tinyClaudeBody(repeat("a", 500))
	resp := postJSON(t, srv.URL+"/v1/messages", body)
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	// echo handler reads the body server-side using io.ReadAll so it gets
	// whatever bytes Gin has wired up. If ContentLength was not updated to
	// match the mutated body, the handler would have stopped reading at the
	// original ContentLength prefix and missed the appended reminder.
	if !containsReminder(got) {
		t.Error("mutated body not delivered in full to handler (ContentLength desync?)")
	}
}
