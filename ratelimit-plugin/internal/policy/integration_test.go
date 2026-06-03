package policy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/cyberk/ratelimit-plugin/internal/effortnormalize"
	"github.com/cyberk/ratelimit-plugin/internal/policy"
	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// Mounts policy + ratelimit middleware in the same order as main.go and
// verifies the chain behaves correctly: blocked requests must not consume
// rate-limit budget, and a shared body peek round-trips intact to handlers.

func init() {
	gin.SetMode(gin.TestMode)
}

func newChain(t *testing.T) (*httptest.Server, *ratelimit.Limiter) {
	t.Helper()
	policyCfg, err := policy.ParseBytes([]byte(`policy: { block_service_tiers: [priority] }`))
	if err != nil {
		t.Fatal(err)
	}
	rlCfg, err := ratelimit.ParseBytes([]byte(`ratelimit: { window: 1h, requests: 2 }`))
	if err != nil {
		t.Fatal(err)
	}
	lim := ratelimit.NewLimiter()

	r := gin.New()
	r.Use(policy.Middleware(policy.NewConfigStore(policyCfg)))
	r.Use(ratelimit.Middleware(ratelimit.NewConfigStore(rlCfg), lim))
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	})
	return httptest.NewServer(r), lim
}

func post(t *testing.T, url, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Policy block must not consume rate-limit quota: spam 100 priority requests
// then verify the 2-request budget is still intact for a non-blocked call.
func TestChain_PolicyBlockDoesNotConsumeQuota(t *testing.T) {
	srv, lim := newChain(t)
	defer srv.Close()
	url := srv.URL + "/v1/chat/completions"

	for i := 0; i < 100; i++ {
		resp := post(t, url, "alice", `{"model":"gpt-5","service_tier":"priority"}`)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("priority req %d: expected 400, got %d", i, resp.StatusCode)
		}
	}
	if lim.Size() != 0 {
		t.Errorf("limiter saw blocked requests: size=%d (should be 0)", lim.Size())
	}

	for i := 0; i < 2; i++ {
		resp := post(t, url, "alice", `{"model":"gpt-5"}`)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("legit req %d: expected 200, got %d", i, resp.StatusCode)
		}
	}
	resp := post(t, url, "alice", `{"model":"gpt-5"}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("3rd legit req: expected 400 (rate-limit), got %d", resp.StatusCode)
	}
}

// Both middlewares peek the body. Verify the cached peek means the downstream
// handler still sees the exact original bytes.
func TestChain_BodyRoundtripUnderTwoPeeks(t *testing.T) {
	srv, _ := newChain(t)
	defer srv.Close()

	body := `{"model":"gpt-5","service_tier":"auto","extra":"preserved"}`
	resp := post(t, srv.URL+"/v1/chat/completions", "alice", body)
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body changed under chain:\n got: %q\nwant: %q", got, body)
	}
}

// Policy rejection should produce the error shape we documented, with
// service_tier echoed back for client introspection.
func TestChain_PolicyErrorShape(t *testing.T) {
	srv, _ := newChain(t)
	defer srv.Close()

	resp := post(t, srv.URL+"/v1/chat/completions", "alice",
		`{"model":"gpt-5","service_tier":"priority"}`)
	defer resp.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("missing error object: %+v", payload)
	}
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("error.type: got %v", errObj["type"])
	}
	if errObj["service_tier"] != "priority" {
		t.Errorf("error.service_tier: got %v", errObj["service_tier"])
	}
	if errObj["param"] != "service_tier" {
		t.Errorf("error.param: got %v (want service_tier)", errObj["param"])
	}
}

// The strip path reads the *live* c.Request.Body, not the cached peek, so it
// composes with effortnormalize (which runs just before policy and replaces
// the body in place without refreshing the peek cache). This locks in that
// invariant: both mutations must survive on the same request. A regression to
// rebuilding from peek.Body would revert one of them and fail here.
func TestChain_EffortNormalizeThenPolicyStripCompose(t *testing.T) {
	policyCfg, err := policy.ParseBytes([]byte(``)) // strip defaults on
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(effortnormalize.Middleware())
	r.Use(policy.Middleware(policy.NewConfigStore(policyCfg)))
	r.POST("/v1/responses", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp := post(t, srv.URL+"/v1/responses", "alice",
		`{"model":"gpt-5","service_tier":"priority","reasoning":{"effort":"minimal"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if _, ok := body["service_tier"]; ok {
		t.Errorf("policy should have stripped service_tier, got %v", body["service_tier"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "low" {
		t.Errorf("effortnormalize fix must survive the strip, got reasoning=%v", body["reasoning"])
	}
}
