package usagestore

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func seedStore(t *testing.T) *Store {
	t.Helper()
	s := New()

	records := []usage.Record{
		{
			APIKey:      "anderson",
			Model:       "gpt-5.4",
			RequestedAt: time.Date(2026, 5, 2, 8, 18, 47, 0, time.UTC),
			Latency:     5685 * time.Millisecond,
			Source:      "huykanzo@gmail.com",
			AuthIndex:   "4159558a8a1153eb",
			Detail:      usage.Detail{InputTokens: 10223, OutputTokens: 199, ReasoningTokens: 66, TotalTokens: 10422},
			Failed:      false,
		},
		{
			APIKey:      "anderson",
			Model:       "gpt-5.4",
			RequestedAt: time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC),
			Latency:     3200 * time.Millisecond,
			Source:      "huykanzo@gmail.com",
			AuthIndex:   "4159558a8a1153eb",
			Detail:      usage.Detail{InputTokens: 36716, OutputTokens: 5855, CachedTokens: 35840, TotalTokens: 42571},
			Failed:      false,
		},
		{
			APIKey:      "anderson",
			Model:       "gpt-5.4",
			RequestedAt: time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
			Latency:     100 * time.Millisecond,
			Source:      "huykanzo@gmail.com",
			AuthIndex:   "4159558a8a1153eb",
			Detail:      usage.Detail{InputTokens: 500, OutputTokens: 0, TotalTokens: 500},
			Failed:      true,
		},
		{
			APIKey:      "anderson",
			Model:       "qwen3.5-plus",
			RequestedAt: time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC),
			Latency:     2000 * time.Millisecond,
			Source:      "huykanzo@gmail.com",
			AuthIndex:   "abc123",
			Detail:      usage.Detail{InputTokens: 800, OutputTokens: 400, TotalTokens: 1200},
			Failed:      false,
		},
		{
			APIKey:      "huycyberk",
			Model:       "claude-sonnet-4-6",
			RequestedAt: time.Date(2026, 5, 3, 14, 0, 0, 0, time.UTC),
			Latency:     4500 * time.Millisecond,
			Source:      "huybuidac.dev@gmail.com",
			AuthIndex:   "deadbeef",
			Detail:      usage.Detail{InputTokens: 5000, OutputTokens: 1200, CachedTokens: 3000, TotalTokens: 6200},
			Failed:      false,
		},
		{
			APIKey:      "huycyberk",
			Model:       "claude-sonnet-4-6",
			RequestedAt: time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC),
			Latency:     3000 * time.Millisecond,
			Source:      "huybuidac.dev@gmail.com",
			AuthIndex:   "deadbeef",
			Detail:      usage.Detail{InputTokens: 2000, OutputTokens: 500, TotalTokens: 2500},
			Failed:      false,
		},
	}

	for _, r := range records {
		s.HandleUsage(context.Background(), r)
	}
	return s
}

func TestSummarySnapshot_AllTime(t *testing.T) {
	s := seedStore(t)
	snap := s.SummarySnapshot(time.Time{})

	if snap.TotalRequests != 6 {
		t.Fatalf("total_requests: want 6, got %d", snap.TotalRequests)
	}
	if snap.SuccessCount != 5 {
		t.Fatalf("success_count: want 5, got %d", snap.SuccessCount)
	}
	if snap.FailureCount != 1 {
		t.Fatalf("failure_count: want 1, got %d", snap.FailureCount)
	}
	if len(snap.APIs) != 2 {
		t.Fatalf("api count: want 2, got %d", len(snap.APIs))
	}

	anderson := snap.APIs["anderson"]
	if anderson == nil {
		t.Fatal("anderson api missing")
	}
	if anderson.TotalRequests != 4 {
		t.Fatalf("anderson requests: want 4, got %d", anderson.TotalRequests)
	}
	if len(anderson.Models) != 2 {
		t.Fatalf("anderson model count: want 2, got %d", len(anderson.Models))
	}

	gpt := anderson.Models["gpt-5.4"]
	if gpt == nil {
		t.Fatal("gpt-5.4 model missing")
	}
	if gpt.TotalRequests != 3 {
		t.Fatalf("gpt-5.4 requests: want 3, got %d", gpt.TotalRequests)
	}
	if gpt.SuccessCount != 2 {
		t.Fatalf("gpt-5.4 success: want 2, got %d", gpt.SuccessCount)
	}
	if gpt.FailureCount != 1 {
		t.Fatalf("gpt-5.4 failure: want 1, got %d", gpt.FailureCount)
	}
	if gpt.InputTokens != 10223+36716+500 {
		t.Fatalf("gpt-5.4 input: want %d, got %d", 10223+36716+500, gpt.InputTokens)
	}
	if gpt.OutputTokens != 199+5855+0 {
		t.Fatalf("gpt-5.4 output: want %d, got %d", 199+5855, gpt.OutputTokens)
	}
	if gpt.CachedTokens != 35840 {
		t.Fatalf("gpt-5.4 cached: want 35840, got %d", gpt.CachedTokens)
	}
	if gpt.ReasoningTokens != 66 {
		t.Fatalf("gpt-5.4 reasoning: want 66, got %d", gpt.ReasoningTokens)
	}
	if len(gpt.Details) != 0 {
		t.Fatalf("gpt-5.4 details should be empty, got %d", len(gpt.Details))
	}
	if gpt.LastActive == "" {
		t.Fatal("gpt-5.4 last_active should be set")
	}

	huy := snap.APIs["huycyberk"]
	if huy == nil {
		t.Fatal("huycyberk api missing")
	}
	if huy.TotalRequests != 2 {
		t.Fatalf("huycyberk requests: want 2, got %d", huy.TotalRequests)
	}
	claude := huy.Models["claude-sonnet-4-6"]
	if claude == nil {
		t.Fatal("claude-sonnet-4-6 model missing")
	}
	if claude.CachedTokens != 3000 {
		t.Fatalf("claude cached: want 3000, got %d", claude.CachedTokens)
	}
}

func TestSummarySnapshot_WithSinceFilter(t *testing.T) {
	s := seedStore(t)
	since := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	snap := s.SummarySnapshot(since)

	// Only records on or after May 3: anderson/gpt-5.4 (May 3, May 4), huycyberk/claude (May 3)
	if snap.TotalRequests != 3 {
		t.Fatalf("total_requests: want 3, got %d", snap.TotalRequests)
	}

	anderson := snap.APIs["anderson"]
	if anderson == nil {
		t.Fatal("anderson should be present")
	}
	if anderson.TotalRequests != 2 {
		t.Fatalf("anderson requests: want 2, got %d", anderson.TotalRequests)
	}
	if len(anderson.Models) != 1 {
		t.Fatalf("anderson model count: want 1 (qwen filtered out), got %d", len(anderson.Models))
	}
	if _, ok := anderson.Models["qwen3.5-plus"]; ok {
		t.Fatal("qwen3.5-plus should be filtered out (May 2 only)")
	}
}

func TestSummarySnapshot_Empty(t *testing.T) {
	s := New()
	snap := s.SummarySnapshot(time.Time{})

	if snap.TotalRequests != 0 {
		t.Fatalf("empty store: want 0 requests, got %d", snap.TotalRequests)
	}
	if len(snap.APIs) != 0 {
		t.Fatalf("empty store: want 0 apis, got %d", len(snap.APIs))
	}
}

func TestSummarySnapshot_SinceFiltersAll(t *testing.T) {
	s := seedStore(t)
	since := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := s.SummarySnapshot(since)

	if snap.TotalRequests != 0 {
		t.Fatalf("future since: want 0 requests, got %d", snap.TotalRequests)
	}
	if len(snap.APIs) != 0 {
		t.Fatalf("future since: want 0 apis, got %d", len(snap.APIs))
	}
}

func TestKeySnapshot_ExistingKey(t *testing.T) {
	s := seedStore(t)
	snap := s.KeySnapshot("anderson")

	if snap == nil {
		t.Fatal("anderson snapshot should not be nil")
	}
	if snap.TotalRequests != 4 {
		t.Fatalf("total_requests: want 4, got %d", snap.TotalRequests)
	}
	if snap.SuccessCount != 3 {
		t.Fatalf("success_count: want 3, got %d", snap.SuccessCount)
	}
	if snap.FailureCount != 1 {
		t.Fatalf("failure_count: want 1, got %d", snap.FailureCount)
	}
	if len(snap.APIs) != 1 {
		t.Fatalf("api count: want 1, got %d", len(snap.APIs))
	}
	if _, ok := snap.APIs["anderson"]; !ok {
		t.Fatal("anderson key should be in snapshot")
	}
	gpt := snap.APIs["anderson"].Models["gpt-5.4"]
	if gpt == nil {
		t.Fatal("gpt-5.4 model missing")
	}
	if len(gpt.Details) != 3 {
		t.Fatalf("gpt-5.4 details: want 3, got %d", len(gpt.Details))
	}
}

func TestKeySnapshot_MissingKey(t *testing.T) {
	s := seedStore(t)
	snap := s.KeySnapshot("nonexistent")
	if snap != nil {
		t.Fatal("missing key should return nil")
	}
}

func TestKeySnapshot_DeepCopy(t *testing.T) {
	s := seedStore(t)
	snap1 := s.KeySnapshot("anderson")
	snap2 := s.KeySnapshot("anderson")

	// Mutate snap1, verify snap2 is unaffected
	snap1.APIs["anderson"].Models["gpt-5.4"].Details = nil
	if snap2.APIs["anderson"].Models["gpt-5.4"].Details == nil {
		t.Fatal("KeySnapshot should return deep copies")
	}
}
