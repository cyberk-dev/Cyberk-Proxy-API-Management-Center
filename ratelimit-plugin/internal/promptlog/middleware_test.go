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
	w, err := NewWriter(dir, 16)
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
	w, _ := NewWriter(dir, 16)
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
	w, _ := NewWriter(dir, 16)
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
