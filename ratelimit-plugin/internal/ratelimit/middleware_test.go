package ratelimit

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestServer(t *testing.T, store *ConfigStore, lim *Limiter) *httptest.Server {
	t.Helper()
	r := gin.New()
	r.Use(Middleware(store, lim))
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	})
	r.POST("/v1/messages", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.POST("/v1beta/models/*action", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.POST("/v0/management/config", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.GET("/v1/responses", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"upgraded": true})
	})
	return httptest.NewServer(r)
}

func sendJSON(t *testing.T, url, key, model string) *http.Response {
	t.Helper()
	body := []byte(`{"model":"` + model + `","messages":[]}`)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMiddleware_429AfterLimit(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 1h
  requests: 2
`)
	store := NewConfigStore(cfg)
	lim := NewLimiter()
	srv := newTestServer(t, store, lim)
	defer srv.Close()

	url := srv.URL + "/v1/chat/completions"
	for i := 0; i < 2; i++ {
		resp := sendJSON(t, url, "alice", "gpt-4")
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("req %d: got %d", i, resp.StatusCode)
		}
	}

	resp := sendJSON(t, url, "alice", "gpt-4")
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("3rd request: got %d", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("missing Retry-After")
	}

	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "rate_limit_exceeded" {
		t.Errorf("bad error payload: %+v", payload)
	}
}

func TestMiddleware_HeadersOnAllow(t *testing.T) {
	cfg := mustParse(t, `ratelimit: { window: 1h, requests: 10 }`)
	srv := newTestServer(t, NewConfigStore(cfg), NewLimiter())
	defer srv.Close()

	resp := sendJSON(t, srv.URL+"/v1/chat/completions", "alice", "gpt-4")
	defer resp.Body.Close()
	if resp.Header.Get("X-RateLimit-Limit") != "10" {
		t.Errorf("X-RateLimit-Limit: %q", resp.Header.Get("X-RateLimit-Limit"))
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "9" {
		t.Errorf("X-RateLimit-Remaining: %q", resp.Header.Get("X-RateLimit-Remaining"))
	}
}

func TestMiddleware_BodyPassthrough(t *testing.T) {
	cfg := mustParse(t, `ratelimit: { window: 1h, requests: 100 }`)
	srv := newTestServer(t, NewConfigStore(cfg), NewLimiter())
	defer srv.Close()

	body := `{"model":"gpt-4","extra":"preserved"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
}

func TestMiddleware_SkipManagement(t *testing.T) {
	cfg := mustParse(t, `ratelimit: { window: 1h, requests: 1 }`)
	srv := newTestServer(t, NewConfigStore(cfg), NewLimiter())
	defer srv.Close()

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", srv.URL+"/v0/management/config", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer alice")
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("management req %d: %d (should never 429)", i, resp.StatusCode)
		}
	}
}

func TestMiddleware_SkipWebSocketUpgrade(t *testing.T) {
	cfg := mustParse(t, `ratelimit: { window: 1h, requests: 1 }`)
	lim := NewLimiter()
	srv := newTestServer(t, NewConfigStore(cfg), lim)
	defer srv.Close()

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/responses", nil)
		req.Header.Set("Authorization", "Bearer alice")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("WS req %d: %d", i, resp.StatusCode)
		}
	}
	if lim.Size() != 0 {
		t.Errorf("limiter should not see WS requests, size=%d", lim.Size())
	}
}

func TestMiddleware_NoApiKeyPassThrough(t *testing.T) {
	cfg := mustParse(t, `ratelimit: { window: 1h, requests: 1 }`)
	srv := newTestServer(t, NewConfigStore(cfg), NewLimiter())
	defer srv.Close()

	body := `{"model":"gpt-4"}`
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("req %d without key: %d (should pass to downstream)", i, resp.StatusCode)
		}
	}
}

func TestMiddleware_PerModelIsolation(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 1h
  requests: 100
  models:
    expensive:
      requests: 1
`)
	srv := newTestServer(t, NewConfigStore(cfg), NewLimiter())
	defer srv.Close()

	resp := sendJSON(t, srv.URL+"/v1/chat/completions", "alice", "expensive")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("1st expensive: %d", resp.StatusCode)
	}
	resp = sendJSON(t, srv.URL+"/v1/chat/completions", "alice", "expensive")
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("2nd expensive should 429: %d", resp.StatusCode)
	}

	// Different model not affected.
	resp = sendJSON(t, srv.URL+"/v1/chat/completions", "alice", "cheap")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("cheap model should not be limited: %d", resp.StatusCode)
	}
}

func TestMiddleware_GeminiPath(t *testing.T) {
	cfg := mustParse(t, `
ratelimit:
  window: 1h
  models:
    gemini-2.5-pro:
      requests: 1
`)
	srv := newTestServer(t, NewConfigStore(cfg), NewLimiter())
	defer srv.Close()

	send := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader("{}"))
		req.Header.Set("X-Goog-Api-Key", "alice")
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		return resp.StatusCode
	}

	if s := send(); s != 200 {
		t.Fatalf("1st: %d", s)
	}
	if s := send(); s != 429 {
		t.Fatalf("2nd should 429: %d", s)
	}
}
