package usagepush

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupRouter(secretKey string) *gin.Engine {
	engine := gin.New()
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = secretKey
	Register(engine, cfg)
	return engine
}

func TestPostUsageQueue_NoAuth(t *testing.T) {
	router := setupRouter("test-secret")

	body := `[{"timestamp":"2026-05-05T10:00:00Z","model":"test","failed":false}]`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/usage-queue", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPostUsageQueue_WrongKey(t *testing.T) {
	router := setupRouter("test-secret")

	body := `[{"timestamp":"2026-05-05T10:00:00Z","model":"test","failed":false}]`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/usage-queue", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Management-Key", "wrong-key")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPostUsageQueue_ValidKey(t *testing.T) {
	router := setupRouter("test-secret")

	body := `[{"timestamp":"2026-05-05T10:00:00Z","model":"claude-sonnet","provider":"claude","tokens":{"input_tokens":100,"output_tokens":50,"total_tokens":150},"failed":false}]`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/usage-queue", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Management-Key", "test-secret")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["pushed"] != 1 {
		t.Fatalf("expected pushed=1, got %d", result["pushed"])
	}
	if result["total"] != 1 {
		t.Fatalf("expected total=1, got %d", result["total"])
	}
}

func TestPostUsageQueue_InvalidBody(t *testing.T) {
	router := setupRouter("test-secret")

	req := httptest.NewRequest(http.MethodPost, "/v0/management/usage-queue", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Management-Key", "test-secret")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPostUsageQueue_BearerAuth(t *testing.T) {
	router := setupRouter("test-secret")

	body := `[{"timestamp":"2026-05-05T10:00:00Z","model":"test","failed":false}]`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/usage-queue", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
