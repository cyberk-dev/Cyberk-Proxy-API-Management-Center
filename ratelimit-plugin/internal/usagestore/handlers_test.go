package usagestore

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupTestRouter(secretKey string, store *Store) *gin.Engine {
	engine := gin.New()
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = secretKey
	RegisterRoutes(engine, cfg, store)
	return engine
}

func seedTestStore(t *testing.T) *Store {
	t.Helper()
	s := New()
	records := []usage.Record{
		{
			APIKey:      "anderson",
			Model:       "gpt-5.4",
			RequestedAt: time.Date(2026, 5, 2, 8, 0, 0, 0, time.UTC),
			Latency:     5000 * time.Millisecond,
			Source:      "huykanzo@gmail.com",
			AuthIndex:   "4159558a8a1153eb",
			Detail:      usage.Detail{InputTokens: 10000, OutputTokens: 200, TotalTokens: 10200},
		},
		{
			APIKey:      "anderson",
			Model:       "gpt-5.4",
			RequestedAt: time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC),
			Latency:     3000 * time.Millisecond,
			Source:      "huykanzo@gmail.com",
			AuthIndex:   "4159558a8a1153eb",
			Detail:      usage.Detail{InputTokens: 5000, OutputTokens: 1000, CachedTokens: 4000, TotalTokens: 6000},
		},
		{
			APIKey:      "huycyberk",
			Model:       "claude-sonnet-4-6",
			RequestedAt: time.Date(2026, 5, 3, 14, 0, 0, 0, time.UTC),
			Latency:     4500 * time.Millisecond,
			Source:      "huybuidac.dev@gmail.com",
			AuthIndex:   "deadbeef",
			Detail:      usage.Detail{InputTokens: 5000, OutputTokens: 1200, TotalTokens: 6200},
		},
	}
	for _, r := range records {
		s.HandleUsage(context.Background(), r)
	}
	return s
}

func doGet(t *testing.T, router *gin.Engine, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestSummaryEndpoint_NoAuth(t *testing.T) {
	store := seedTestStore(t)
	router := setupTestRouter("secret", store)
	w := doGet(t, router, "/v0/management/usage/summary", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestSummaryEndpoint_AllTime(t *testing.T) {
	store := seedTestStore(t)
	router := setupTestRouter("secret", store)
	w := doGet(t, router, "/v0/management/usage/summary", "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var snap UsageSummarySnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.TotalRequests != 3 {
		t.Fatalf("total_requests: want 3, got %d", snap.TotalRequests)
	}
	if len(snap.APIs) != 2 {
		t.Fatalf("api count: want 2, got %d", len(snap.APIs))
	}
	gpt := snap.APIs["anderson"].Models["gpt-5.4"]
	if gpt == nil {
		t.Fatal("gpt-5.4 missing")
	}
	if len(gpt.Details) != 0 {
		t.Fatalf("summary should have empty details, got %d", len(gpt.Details))
	}
	if gpt.InputTokens != 15000 {
		t.Fatalf("gpt input: want 15000, got %d", gpt.InputTokens)
	}
	if gpt.CachedTokens != 4000 {
		t.Fatalf("gpt cached: want 4000, got %d", gpt.CachedTokens)
	}
}

func TestSummaryEndpoint_WithSince(t *testing.T) {
	store := seedTestStore(t)
	router := setupTestRouter("secret", store)

	// since = May 5 00:00 UTC (1778025600000 ms) → only anderson's May 10 record
	sinceMs := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC).UnixMilli()
	path := "/v0/management/usage/summary?since=" + strconv.FormatInt(sinceMs, 10)
	w := doGet(t, router, path, "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var snap UsageSummarySnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.TotalRequests != 1 {
		t.Fatalf("filtered requests: want 1, got %d", snap.TotalRequests)
	}
	if len(snap.APIs) != 1 {
		t.Fatalf("filtered api count: want 1, got %d", len(snap.APIs))
	}
	if _, ok := snap.APIs["huycyberk"]; ok {
		t.Fatal("huycyberk should be filtered out (May 3 only)")
	}
}

func TestKeysEndpoint_Existing(t *testing.T) {
	store := seedTestStore(t)
	router := setupTestRouter("secret", store)
	w := doGet(t, router, "/v0/management/usage/keys/anderson", "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var snap UsageSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.TotalRequests != 2 {
		t.Fatalf("anderson requests: want 2, got %d", snap.TotalRequests)
	}
	if len(snap.APIs) != 1 {
		t.Fatalf("api count: want 1, got %d", len(snap.APIs))
	}
	gpt := snap.APIs["anderson"].Models["gpt-5.4"]
	if gpt == nil {
		t.Fatal("gpt-5.4 missing")
	}
	if len(gpt.Details) != 2 {
		t.Fatalf("gpt details: want 2, got %d", len(gpt.Details))
	}
}

func TestKeysEndpoint_NotFound(t *testing.T) {
	store := seedTestStore(t)
	router := setupTestRouter("secret", store)
	w := doGet(t, router, "/v0/management/usage/keys/nonexistent", "secret")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestKeysEndpoint_NoAuth(t *testing.T) {
	store := seedTestStore(t)
	router := setupTestRouter("secret", store)
	w := doGet(t, router, "/v0/management/usage/keys/anderson", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}
