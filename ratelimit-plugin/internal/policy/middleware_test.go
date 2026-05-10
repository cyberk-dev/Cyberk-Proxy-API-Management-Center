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
