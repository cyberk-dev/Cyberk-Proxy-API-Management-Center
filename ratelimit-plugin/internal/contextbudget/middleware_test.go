package contextbudget

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"

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
	r.Use(Middleware(store, nil))
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

func TestMiddleware_SoftBlocksOnceThenPasses(t *testing.T) {
	// Policy: while inside the soft-block burst window EVERY request is
	// blocked (so CC's parallel requests + retry storm all hit 400, which
	// forces the client to surface the error to the user). After the burst
	// window expires, subsequent crosses passthrough so the user isn't
	// deadlocked if they choose to ignore the warning.
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
  hard_threshold_tokens: 1000
`))
	tracker := NewTracker(8, time.Hour)
	tracker.SetSoftBlockBurst(80 * time.Millisecond)

	r := gin.New()
	r.Use(func(c *gin.Context) { ratelimit.PeekJSONBody(c); c.Next() })
	r.Use(Middleware(store, tracker))
	r.POST("/v1/messages", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	// 500 chars / 4 = 125 tokens, between soft(100) and hard(1000).
	body := tinyClaudeBody(repeat("a", 500))

	send := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Claude-Code-Session-Id", "ses_soft_once")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Inside burst → all 400.
	if s := send(); s != http.StatusBadRequest {
		t.Fatalf("first soft cross should 400, got %d", s)
	}
	if s := send(); s != http.StatusBadRequest {
		t.Fatalf("in-burst soft cross should 400, got %d", s)
	}
	if s := send(); s != http.StatusBadRequest {
		t.Fatalf("in-burst soft cross should 400, got %d", s)
	}
	// Wait past burst → passthrough.
	time.Sleep(120 * time.Millisecond)
	if s := send(); s != http.StatusOK {
		t.Fatalf("post-burst soft cross should pass, got %d", s)
	}
}

func TestMiddleware_SoftWarningRearmsAfterCompact(t *testing.T) {
	// If the session drops back below soft (user did /compact) the
	// warning re-arms — the next time tokens climb above soft, the user
	// gets a fresh 400.
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
  hard_threshold_tokens: 1000
`))
	tracker := NewTracker(8, time.Hour)

	r := gin.New()
	r.Use(func(c *gin.Context) { ratelimit.PeekJSONBody(c); c.Next() })
	r.Use(Middleware(store, tracker))
	r.POST("/v1/messages", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	srv := httptest.NewServer(r)
	defer srv.Close()

	send := func(text, sid string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(tinyClaudeBody(text)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Claude-Code-Session-Id", sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Cross soft → warn.
	if r1 := send(repeat("a", 500), "ses_rearm"); r1.StatusCode != http.StatusBadRequest {
		r1.Body.Close()
		t.Fatalf("expected soft 400, got %d", r1.StatusCode)
	} else {
		r1.Body.Close()
	}
	// Drop below soft → warning should auto-clear via middleware.
	if r2 := send("hi", "ses_rearm"); r2.StatusCode != http.StatusOK {
		r2.Body.Close()
		t.Fatalf("under-soft passthrough should 200, got %d", r2.StatusCode)
	} else {
		r2.Body.Close()
	}
	// Cross soft again → must warn AGAIN (re-armed).
	r3 := send(repeat("a", 500), "ses_rearm")
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusBadRequest {
		t.Fatalf("re-armed soft should 400 again, got %d", r3.StatusCode)
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	payload, _ := io.ReadAll(resp.Body)
	// Claude 400 envelope: {type:"error", error:{type:"invalid_request_error"}}
	if gjson.GetBytes(payload, "error.type").String() != "invalid_request_error" {
		t.Errorf("unexpected error envelope: %s", payload)
	}
	if gjson.GetBytes(payload, "context_budget.hard_limit_tokens").Int() != 200 {
		t.Errorf("expected hard_limit_tokens in envelope, got: %s", payload)
	}
	if gjson.GetBytes(payload, "context_budget.severity").String() != "hard" {
		t.Errorf("expected severity=hard, got: %s", payload)
	}
	if !gjson.GetBytes(payload, "context_budget.compact_hint").Bool() {
		t.Errorf("expected compact_hint=true in envelope")
	}
}

func TestMiddleware_HardBlocks_StreamingAnthropic(t *testing.T) {
	// Streaming requests get the SAME JSON 400 as non-streaming under
	// the new policy. We deliberately do NOT emit an SSE error chunk
	// anymore: Claude Code's agentic loop treats SSE errors as transient
	// connection failures and retries at ~3 req/sec for ~30 s, whereas
	// a plain JSON 400 is non-retryable in that loop.
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 100
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
	resp := postJSON(t, srv.URL+"/v1/messages", string(b))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected JSON content-type (not SSE), got %q", ct)
	}
	payload, _ := io.ReadAll(resp.Body)
	if gjson.GetBytes(payload, "error.type").String() != "invalid_request_error" {
		t.Errorf("expected invalid_request_error, got: %s", payload)
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	payload, _ := io.ReadAll(resp.Body)
	if gjson.GetBytes(payload, "error.code").String() != "context_length_exceeded" {
		t.Errorf("expected OpenAI error.code=context_length_exceeded, got: %s", payload)
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected JSON content-type for hard-block (no more SSE), got %q", ct)
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
	r.Use(Middleware(store, nil))
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
	if resp.StatusCode != http.StatusBadRequest {
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 from truncated body even under high hard threshold, got %d", resp.StatusCode)
	}
}

func TestMiddleware_TrackerHitOverridesEstimate(t *testing.T) {
	// Tracker says this session is at 9000 tokens (over hard=8000), but
	// the request body's char/4 estimate would only be a few tokens.
	// Middleware must use the tracker's number → hard block.
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 8000
`))
	tracker := NewTracker(8, time.Hour)

	r := gin.New()
	r.Use(func(c *gin.Context) { ratelimit.PeekJSONBody(c); c.Next() })
	r.Use(Middleware(store, tracker))
	r.POST("/v1/messages", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	// Seed the tracker as if a previous turn ended at 9000 tokens for
	// session "abc-123".
	tracker.Record(SessionKey{APIKeyHash: "", ID: "abc-123", Source: SessionFromHeader}, 9000)

	// Tiny body that would estimate to ~5 tokens via char/4.
	body := tinyClaudeBody("hi")
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "abc-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tracker should have forced 413, got %d", resp.StatusCode)
	}
}

// TestMiddleware_GinContextPropagatesToUsagePlugin is the end-to-end canary
// for the S-HIGH bug oracle found: the CLIProxyAPI SDK rebases the executor
// ctx on context.Background() (handlers.go:414) and only the *gin.Context
// (stored under string key "gin") survives. So our middleware must stash
// the session key on the gin.Context, NOT on r.Context() — otherwise the
// downstream UsagePlugin never sees it and the tracker stays empty.
//
// This test simulates the production sequence:
//   1. Middleware runs, extracts a session, calls SetGinSession on c.
//   2. Handler runs, terminates the response.
//   3. SDK builds a ctx like `context.WithValue(Background(), "gin", c)`
//      and dispatches the usage Record to plugins.
//   4. UsagePlugin.HandleUsage must recover the session key and record
//      against it.
//
// If any step in the propagation chain is broken, tracker.Lookup at the
// end returns (0, false) and we fail loudly.
func TestMiddleware_GinContextPropagatesToUsagePlugin(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  soft_threshold_tokens: 1000000
  hard_threshold_tokens: 2000000
`))
	tracker := NewTracker(8, time.Hour)
	plugin := NewUsagePlugin(tracker)

	// Capture the gin.Context that the middleware decorated so we can
	// replay the SDK's "wrap c under string key 'gin' and dispatch" step.
	var captured *gin.Context

	r := gin.New()
	r.Use(func(c *gin.Context) { ratelimit.PeekJSONBody(c); c.Next() })
	r.Use(Middleware(store, tracker))
	r.POST("/v1/messages", func(c *gin.Context) {
		captured = c
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	body := tinyClaudeBody("first turn user content for fingerprinting")
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	req.Header.Set("X-Claude-Code-Session-Id", "ses_canary")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request failed: status=%d", resp.StatusCode)
	}
	if captured == nil {
		t.Fatal("handler never captured gin.Context")
	}

	// Verify middleware actually stashed the keys.
	if v, exists := captured.Get(ginKeySession); !exists {
		t.Fatal("middleware did not set session on gin.Context")
	} else if k, ok := v.(SessionKey); !ok || k.ID != "ses_canary" {
		t.Fatalf("session on gin.Context = %v, want ses_canary", v)
	}
	if v, exists := captured.Get(ginKeyProtocol); !exists {
		t.Fatal("middleware did not set protocol on gin.Context")
	} else if p, ok := v.(Protocol); !ok || p != ProtoClaude {
		t.Fatalf("protocol on gin.Context = %v, want ProtoClaude", v)
	}

	// Replay the SDK's usage-dispatch step verbatim.
	usageCtx := context.WithValue(context.Background(), "gin", captured)
	plugin.HandleUsage(usageCtx, coreusage.Record{
		Detail: coreusage.Detail{InputTokens: 4321, CachedTokens: 100},
	})

	// Now the next request for this session should hit the tracker.
	if got, ok := tracker.Lookup(SessionKey{APIKeyHash: hashAPIKey("alice"), ID: "ses_canary", Source: SessionFromHeader}); !ok {
		t.Fatal("tracker did not record session — gin-context propagation is broken")
	} else if got != 4421 {
		t.Errorf("recorded tokens = %d, want 4421 (Input 4321 + Cached 100 for Claude)", got)
	}
}

func TestMiddleware_TrackerMissFallsBackToEstimate(t *testing.T) {
	store := NewConfigStore(mustParse(t, `
context_budget:
  enabled: true
  hard_threshold_tokens: 200
`))
	tracker := NewTracker(8, time.Hour)

	r := gin.New()
	r.Use(func(c *gin.Context) { ratelimit.PeekJSONBody(c); c.Next() })
	r.Use(Middleware(store, tracker))
	r.POST("/v1/messages", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte(`{"ok":1}`))
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	// No tracker entry for this session → fallback to char/4 estimate of
	// the body. The body is 1200 chars → ~300 tokens > hard(200).
	body := tinyClaudeBody(repeat("a", 1200))
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "unseen-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("char/4 fallback should still block, got %d", resp.StatusCode)
	}
}

