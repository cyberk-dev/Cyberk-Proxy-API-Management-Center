package promptlog

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

func newReadHandlerRig(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	// Seed one entry so the handler has something to find — the cursor
	// validation paths still 400 before the scan, but having a real key
	// keeps these tests honest if we later add positive load-more cases.
	writeJSONL(t, dir, "2026-05-17", Entry{
		KeyHash:   ratelimit.HashKey("sk-x"),
		SessionID: "s",
		CWD:       "/p",
		Prompt:    "x",
	})
	plogCfg := &Config{Enabled: true, Dir: dir}
	proxyCfg := &config.Config{}
	proxyCfg.RemoteManagement.SecretKey = "secret"
	proxyCfg.APIKeys = []string{"sk-x"}

	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	engine := gin.New()
	RegisterReadHandlers(engine, proxyCfg, plogCfg, nil, idx)
	return engine, "secret"
}

func doReadGet(engine *gin.Engine, path, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Management-Key", secret)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)
	return rr
}

func TestHandler_SessionBeforeRequiresCwd(t *testing.T) {
	engine, secret := newReadHandlerRig(t)
	rr := doReadGet(engine,
		"/v0/management/prompts/users/sk-x?session_before=2026-05-17T10:00:00Z%7Cs-1",
		secret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), "cwd") {
		t.Errorf("error body should mention cwd requirement: %s", rr.Body.String())
	}
}

func TestHandler_SessionIDRequiresCwd(t *testing.T) {
	engine, secret := newReadHandlerRig(t)
	rr := doReadGet(engine,
		"/v0/management/prompts/users/sk-x?session_id=abc",
		secret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandler_SessionBeforeExclusiveWithSessionID(t *testing.T) {
	// Combining session-cursor with single-session targeting is a
	// self-contradictory query — server must reject so the client
	// doesn't silently get an empty Sessions array with SessionCount>0.
	engine, secret := newReadHandlerRig(t)
	rr := doReadGet(engine,
		"/v0/management/prompts/users/sk-x?cwd=%2Fp&session_id=s&session_before=2026-05-17T10:00:00Z%7Cs",
		secret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), "mutually exclusive") {
		t.Errorf("error body should mention mutual exclusion: %s", rr.Body.String())
	}
}

func TestHandler_MessageBeforeRequiresSessionID(t *testing.T) {
	engine, secret := newReadHandlerRig(t)
	rr := doReadGet(engine,
		"/v0/management/prompts/users/sk-x?cwd=%2Fp&message_before=2026-05-17T10:00:00Z",
		secret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandler_SessionBeforeRejectsHeadersOnly(t *testing.T) {
	// session_before + headers_only is a client mistake — cursor is
	// meaningless when no sessions are returned. Surface a specific
	// message so the caller doesn't waste time looking at cwd config.
	engine, secret := newReadHandlerRig(t)
	rr := doReadGet(engine,
		"/v0/management/prompts/users/sk-x?cwd=%2Fp&headers_only=1&session_before=2026-05-17T10:00:00Z%7Cs-1",
		secret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), "headers_only") {
		t.Errorf("error body should mention headers_only conflict: %s", rr.Body.String())
	}
}

func TestHandler_SessionBeforeMalformed(t *testing.T) {
	engine, secret := newReadHandlerRig(t)
	// Missing pipe separator — only timestamp, no session id.
	rr := doReadGet(engine,
		"/v0/management/prompts/users/sk-x?cwd=%2Fp&session_before=2026-05-17T10:00:00Z",
		secret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing-pipe status=%d want 400", rr.Code)
	}

	// Bad timestamp format on left of pipe.
	rr = doReadGet(engine,
		"/v0/management/prompts/users/sk-x?cwd=%2Fp&session_before=not-a-time%7Cs",
		secret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad-ts status=%d want 400", rr.Code)
	}
}

func TestHandler_SearchRejectsShortQuery(t *testing.T) {
	engine, secret := newReadHandlerRig(t)
	// URL-encoded forms: "" (missing param), " " (single space), "a"
	// (single char), " b " (trimmed to one char).
	cases := []string{"", "%20", "a", "%20b%20"}
	for _, q := range cases {
		rr := doReadGet(engine, "/v0/management/prompts/users/sk-x/search?q="+q, secret)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("q=%q status=%d want 400, body=%s", q, rr.Code, rr.Body.String())
		}
	}
}

func TestHandler_SearchRejectsTooLongQuery(t *testing.T) {
	engine, secret := newReadHandlerRig(t)
	long := ""
	for i := 0; i < 201; i++ {
		long += "a"
	}
	rr := doReadGet(engine, "/v0/management/prompts/users/sk-x/search?q="+long, secret)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandler_SearchHappyPath(t *testing.T) {
	engine, secret := newReadHandlerRig(t)
	// Seeded entry has Prompt="x" which is below the 2-char min — re-seed
	// with a real prompt so we can verify the search path.
	dir := t.TempDir()
	writeJSONL(t, dir, "2026-05-17", Entry{
		KeyHash:   ratelimit.HashKey("sk-x"),
		SessionID: "s",
		CWD:       "/p",
		Prompt:    "fix the auth bug today",
	})
	plogCfg := &Config{Enabled: true, Dir: dir}
	proxyCfg := &config.Config{}
	proxyCfg.RemoteManagement.SecretKey = secret
	proxyCfg.APIKeys = []string{"sk-x"}
	idx, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	engine = gin.New()
	RegisterReadHandlers(engine, proxyCfg, plogCfg, nil, idx)

	rr := doReadGet(engine, "/v0/management/prompts/users/sk-x/search?q=auth", secret)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), "\"matches\"") {
		t.Errorf("expected matches field in response: %s", rr.Body.String())
	}
	if !contains(rr.Body.String(), "auth") {
		t.Errorf("expected match excerpt to contain query: %s", rr.Body.String())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
