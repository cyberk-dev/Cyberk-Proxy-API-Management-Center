package policy

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

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestServer(t *testing.T, store *ConfigStore) *httptest.Server {
	t.Helper()
	r := gin.New()
	r.Use(Middleware(store))
	echo := func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	}
	r.POST("/v1/chat/completions", echo)
	r.POST("/v1/responses", echo)
	r.POST("/v1/messages", echo)
	r.POST("/v0/management/config", echo)
	r.POST("/v1/models", echo)
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	return httptest.NewServer(r)
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMiddleware_BlocksPriorityTier(t *testing.T) {
	cfg := mustParse(t, `policy: { block_service_tiers: [priority] }`)
	srv := newTestServer(t, NewConfigStore(cfg))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/chat/completions",
		`{"model":"gpt-5","service_tier":"priority","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "invalid_request_error" {
		t.Errorf("bad error payload: %+v", payload)
	}
	if errObj["service_tier"] != "priority" {
		t.Errorf("expected service_tier echoed in error, got %v", errObj["service_tier"])
	}
}

func TestMiddleware_AllowsDefaultTier(t *testing.T) {
	cfg := mustParse(t, `policy: { block_service_tiers: [priority] }`)
	srv := newTestServer(t, NewConfigStore(cfg))
	defer srv.Close()

	cases := []string{
		`{"model":"gpt-5"}`,
		`{"model":"gpt-5","service_tier":"auto"}`,
		`{"model":"gpt-5","service_tier":"default"}`,
		`{"model":"gpt-5","service_tier":"flex"}`,
	}
	for _, body := range cases {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("body %q should pass, got %d", body, resp.StatusCode)
		}
	}
}

func TestMiddleware_CaseInsensitive(t *testing.T) {
	cfg := mustParse(t, `policy: { block_service_tiers: [priority] }`)
	srv := newTestServer(t, NewConfigStore(cfg))
	defer srv.Close()

	for _, val := range []string{"priority", "Priority", "PRIORITY"} {
		body := `{"model":"gpt-5","service_tier":"` + val + `"}`
		resp := postJSON(t, srv.URL+"/v1/chat/completions", body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("tier %q should be blocked, got %d", val, resp.StatusCode)
		}
	}
}

func TestMiddleware_DisabledConfigPassthrough(t *testing.T) {
	cfg := mustParse(t, ``)
	srv := newTestServer(t, NewConfigStore(cfg))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/chat/completions",
		`{"model":"gpt-5","service_tier":"priority"}`)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("disabled config should not block, got %d", resp.StatusCode)
	}
}

func TestMiddleware_SkipsManagement(t *testing.T) {
	cfg := mustParse(t, `policy: { block_service_tiers: [priority] }`)
	srv := newTestServer(t, NewConfigStore(cfg))
	defer srv.Close()

	body := `{"model":"gpt-5","service_tier":"priority"}`
	resp := postJSON(t, srv.URL+"/v0/management/config", body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("management endpoint should not be policy-blocked, got %d", resp.StatusCode)
	}
}

func TestMiddleware_BodyPassthrough(t *testing.T) {
	cfg := mustParse(t, `policy: { block_service_tiers: [priority] }`)
	srv := newTestServer(t, NewConfigStore(cfg))
	defer srv.Close()

	body := `{"model":"gpt-5","service_tier":"flex","extra":"preserved"}`
	resp := postJSON(t, srv.URL+"/v1/chat/completions", body)
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body mismatch after policy peek:\n got: %q\nwant: %q", got, body)
	}
}

func TestMiddleware_NonJSONPassthrough(t *testing.T) {
	cfg := mustParse(t, `policy: { block_service_tiers: [priority] }`)
	srv := newTestServer(t, NewConfigStore(cfg))
	defer srv.Close()

	// multipart body that *contains* "service_tier=priority" — must not be parsed as JSON.
	body := []byte(`--bnd
Content-Disposition: form-data; name="service_tier"

priority
--bnd--`)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=bnd")
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("multipart should pass through, got %d", resp.StatusCode)
	}
}

func TestMiddleware_HotReload(t *testing.T) {
	store := NewConfigStore(mustParse(t, ``)) // start disabled
	srv := newTestServer(t, store)
	defer srv.Close()

	body := `{"model":"gpt-5","service_tier":"priority"}`

	resp := postJSON(t, srv.URL+"/v1/chat/completions", body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("before reload: should pass, got %d", resp.StatusCode)
	}

	store.Set(mustParse(t, `policy: { block_service_tiers: [priority] }`))

	resp = postJSON(t, srv.URL+"/v1/chat/completions", body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("after reload: should block, got %d", resp.StatusCode)
	}
}

// decodeEcho reads the echoed (post-middleware) request body the test server
// reflects back, so assertions can inspect what would be forwarded upstream.
func decodeEcho(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode echoed body %q: %v", raw, err)
	}
	return m
}

func TestMiddleware_StripsPriorityByDefault(t *testing.T) {
	// Empty config → strip defaults on. Priority is silently removed and the
	// request still succeeds (no 400), with all other fields preserved.
	srv := newTestServer(t, NewConfigStore(mustParse(t, ``)))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/responses",
		`{"model":"gpt-5","service_tier":"priority","input":"hi"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("strip should not reject, got %d", resp.StatusCode)
	}
	body := decodeEcho(t, resp)
	if _, ok := body["service_tier"]; ok {
		t.Errorf("service_tier should be stripped, got %v", body["service_tier"])
	}
	if body["model"] != "gpt-5" || body["input"] != "hi" {
		t.Errorf("other fields must survive strip, got %+v", body)
	}
}

func TestMiddleware_StripIsCaseInsensitive(t *testing.T) {
	srv := newTestServer(t, NewConfigStore(mustParse(t, ``)))
	defer srv.Close()

	for _, val := range []string{"priority", "Priority", "PRIORITY"} {
		resp := postJSON(t, srv.URL+"/v1/responses",
			`{"model":"gpt-5","service_tier":"`+val+`"}`)
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("tier %q: strip should not reject, got %d", val, resp.StatusCode)
		}
		if _, ok := decodeEcho(t, resp)["service_tier"]; ok {
			t.Errorf("tier %q should be stripped", val)
		}
	}
}

func TestMiddleware_StripLeavesNonPriorityUntouched(t *testing.T) {
	srv := newTestServer(t, NewConfigStore(mustParse(t, ``)))
	defer srv.Close()

	for _, val := range []string{"auto", "default", "flex"} {
		resp := postJSON(t, srv.URL+"/v1/responses",
			`{"model":"gpt-5","service_tier":"`+val+`"}`)
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("tier %q should pass, got %d", val, resp.StatusCode)
		}
		if got := decodeEcho(t, resp)["service_tier"]; got != val {
			t.Errorf("tier %q must be preserved, got %v", val, got)
		}
	}
}

func TestMiddleware_StripOptOut(t *testing.T) {
	// Explicit opt-out lets priority through unchanged.
	srv := newTestServer(t, NewConfigStore(
		mustParse(t, "policy:\n  strip_priority_service_tier: false\n")))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/responses",
		`{"model":"gpt-5","service_tier":"priority"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("opt-out should pass priority through, got %d", resp.StatusCode)
	}
	if got := decodeEcho(t, resp)["service_tier"]; got != "priority" {
		t.Errorf("opt-out must preserve priority, got %v", got)
	}
}

func TestMiddleware_StripFailsOpenOnTruncatedBody(t *testing.T) {
	// A body larger than the 16 MiB peek cap is truncated; strip can't safely
	// rewrite it (service_tier may sit beyond the buffer), so it fails open and
	// the request passes through with service_tier intact rather than 400ing.
	srv := newTestServer(t, NewConfigStore(mustParse(t, ``)))
	defer srv.Close()

	const maxBodyPeek = 16 << 20
	pad := strings.Repeat("x", maxBodyPeek+1024)
	body := `{"model":"gpt-5","service_tier":"priority","pad":"` + pad + `"}`

	resp := postJSON(t, srv.URL+"/v1/responses", body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("truncated body should fail open (200), got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got)[:512], `"service_tier":"priority"`) {
		t.Error("truncated body should pass through unstripped")
	}
}

func TestMiddleware_BlockWinsOverStrip(t *testing.T) {
	// priority is both default-stripped and explicitly blocked → block (the
	// loud 400) must win so operators get the rejection they asked for.
	srv := newTestServer(t, NewConfigStore(
		mustParse(t, `policy: { block_service_tiers: [priority] }`)))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/responses",
		`{"model":"gpt-5","service_tier":"priority"}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("block must take precedence over strip, got %d", resp.StatusCode)
	}
}
