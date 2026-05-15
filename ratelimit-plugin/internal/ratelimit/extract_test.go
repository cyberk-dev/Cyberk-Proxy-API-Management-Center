package ratelimit

import (
	"bytes"
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

func TestExtractAPIKey_BearerToken(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer my-key-123")
	if got := ExtractAPIKey(r); got != "my-key-123" {
		t.Errorf("got %q", got)
	}
}

func TestExtractAPIKey_RawAuthorization(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "raw-key")
	if got := ExtractAPIKey(r); got != "raw-key" {
		t.Errorf("got %q", got)
	}
}

func TestExtractAPIKey_GoogleHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Goog-Api-Key", "goog-key")
	if got := ExtractAPIKey(r); got != "goog-key" {
		t.Errorf("got %q", got)
	}
}

func TestExtractAPIKey_AnthropicHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Api-Key", "anthropic-key")
	if got := ExtractAPIKey(r); got != "anthropic-key" {
		t.Errorf("got %q", got)
	}
}

func TestExtractAPIKey_QueryParam(t *testing.T) {
	r := httptest.NewRequest("POST", "/?key=qkey", nil)
	if got := ExtractAPIKey(r); got != "qkey" {
		t.Errorf("got %q", got)
	}
	r = httptest.NewRequest("POST", "/?auth_token=atok", nil)
	if got := ExtractAPIKey(r); got != "atok" {
		t.Errorf("got %q", got)
	}
}

func TestExtractAPIKey_None(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	if got := ExtractAPIKey(r); got != "" {
		t.Errorf("got %q", got)
	}
}

func makeGinCtx(r *http.Request) *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	return c
}

func TestExtractModel_GeminiPath(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	if got := ExtractModel(makeGinCtx(r)); got != "gemini-2.5-pro" {
		t.Errorf("got %q", got)
	}
}

func TestExtractModel_GeminiNoColon(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1beta/models/gemini-pro", nil)
	if got := ExtractModel(makeGinCtx(r)); got != "gemini-pro" {
		t.Errorf("got %q", got)
	}
}

func TestExtractModel_JSONBody(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	c := makeGinCtx(r)
	if got := ExtractModel(c); got != "gpt-4" {
		t.Errorf("got %q", got)
	}

	// Body must still be readable downstream.
	downstream, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downstream, body) {
		t.Errorf("body re-injection failed: got %q, want %q", downstream, body)
	}
}

func TestExtractModel_JSONWithCharset(t *testing.T) {
	body := []byte(`{"model":"Claude-3-Opus"}`)
	r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if got := ExtractModel(makeGinCtx(r)); got != "claude-3-opus" {
		t.Errorf("got %q", got)
	}
}

func TestExtractModel_MultipartSkipped(t *testing.T) {
	body := []byte(`--boundary\r\nContent-Disposition: form-data; name="model"\r\n\r\ngpt-4\r\n--boundary--`)
	r := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(body))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")

	c := makeGinCtx(r)
	if got := ExtractModel(c); got != "" {
		t.Errorf("multipart should return empty, got %q", got)
	}

	// Verify body NOT consumed.
	downstream, _ := io.ReadAll(c.Request.Body)
	if !bytes.Equal(downstream, body) {
		t.Errorf("multipart body should be untouched, got %d bytes", len(downstream))
	}
}

func TestExtractModel_LargeBodyMultiReader(t *testing.T) {
	// Build a body strictly larger than maxBodyPeek (4 MiB) so the peek
	// fills its buffer and the request body is reconstructed via MultiReader.
	// The "model" field is at the start so it falls inside the peek window.
	var buf bytes.Buffer
	buf.WriteString(`{"model":"gpt-4","padding":"`)
	buf.WriteString(strings.Repeat("x", 5<<20))
	buf.WriteString(`"}`)
	original := buf.Bytes()

	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(original))
	r.Header.Set("Content-Type", "application/json")

	c := makeGinCtx(r)
	if got := ExtractModel(c); got != "gpt-4" {
		t.Errorf("got %q", got)
	}

	downstream, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(downstream) != len(original) {
		t.Errorf("downstream missing bytes: got %d, want %d", len(downstream), len(original))
	}
	if !bytes.Equal(downstream, original) {
		t.Error("downstream body content mismatch")
	}
}

func TestExtractModel_NoContentType(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"x"}`)))
	if got := ExtractModel(makeGinCtx(r)); got != "" {
		t.Errorf("no content-type should return empty, got %q", got)
	}
}

func TestExtractModel_EmptyBody(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(nil))
	r.Header.Set("Content-Type", "application/json")
	if got := ExtractModel(makeGinCtx(r)); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestPeekJSONBodyResult_TruncatedFlag(t *testing.T) {
	// Padding sized to exceed maxBodyPeek by ~1 MiB so the test stays in
	// sync if the ceiling moves again (it bumped 4→16 MiB to handle inline
	// image base64 — see comment on maxBodyPeek).
	var buf bytes.Buffer
	buf.WriteString(`{"model":"gpt-4","padding":"`)
	buf.WriteString(strings.Repeat("x", maxBodyPeek+(1<<20)))
	buf.WriteString(`"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(buf.Bytes()))
	r.Header.Set("Content-Type", "application/json")

	res := PeekJSONBodyResult(makeGinCtx(r))
	if !res.Truncated {
		t.Errorf("expected Truncated=true for body > maxBodyPeek")
	}
	if int64(len(res.Body)) != maxBodyPeek {
		t.Errorf("expected peek len == maxBodyPeek (%d), got %d", maxBodyPeek, len(res.Body))
	}
}

func TestPeekJSONBodyResult_NotTruncated(t *testing.T) {
	body := []byte(`{"model":"gpt-4","service_tier":"priority"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	res := PeekJSONBodyResult(makeGinCtx(r))
	if res.Truncated {
		t.Errorf("expected Truncated=false for small body")
	}
	if !bytes.Equal(res.Body, body) {
		t.Errorf("body roundtrip mismatch")
	}
}

func TestPeekJSONBodyResult_CachesResult(t *testing.T) {
	body := []byte(`{"model":"gpt-4"}`)
	r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	c := makeGinCtx(r)

	first := PeekJSONBodyResult(c)
	// Drain c.Request.Body so a second uncached read would return empty.
	_, _ = io.Copy(io.Discard, c.Request.Body)
	second := PeekJSONBodyResult(c)
	if !bytes.Equal(first.Body, second.Body) {
		t.Error("cache miss: second peek differs from first")
	}
}
