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

func TestKeyDetail_AggregatesFromCounters(t *testing.T) {
	s := seedStore(t)
	now := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	detail := s.KeyDetail("anderson", time.Time{}, 0, nil, now)

	if detail == nil {
		t.Fatal("anderson detail should not be nil")
	}
	if detail.APIKey != "anderson" {
		t.Fatalf("api_key: want anderson, got %q", detail.APIKey)
	}
	if detail.TotalRequests != 4 {
		t.Fatalf("total_requests: want 4, got %d", detail.TotalRequests)
	}
	if detail.SuccessCount != 3 {
		t.Fatalf("success_count: want 3, got %d", detail.SuccessCount)
	}
	if detail.FailureCount != 1 {
		t.Fatalf("failure_count: want 1, got %d", detail.FailureCount)
	}
	if detail.InputTokens != 10223+36716+500+800 {
		t.Fatalf("input_tokens: want %d, got %d", 10223+36716+500+800, detail.InputTokens)
	}
	if detail.OutputTokens != 199+5855+0+400 {
		t.Fatalf("output_tokens: want %d, got %d", 199+5855+400, detail.OutputTokens)
	}
	if detail.CachedTokens != 35840 {
		t.Fatalf("cached_tokens: want 35840, got %d", detail.CachedTokens)
	}
	if detail.ReasoningTokens != 66 {
		t.Fatalf("reasoning_tokens: want 66, got %d", detail.ReasoningTokens)
	}
	if len(detail.Models) != 2 {
		t.Fatalf("models: want 2, got %d", len(detail.Models))
	}
	// gpt-5.4 has more requests than qwen3.5-plus → it should sort first.
	if detail.Models[0].Model != "gpt-5.4" {
		t.Fatalf("model[0]: want gpt-5.4, got %s", detail.Models[0].Model)
	}
	gpt := detail.Models[0]
	if gpt.TotalRequests != 3 || gpt.SuccessCount != 2 || gpt.FailureCount != 1 {
		t.Fatalf("gpt-5.4 stats unexpected: %+v", gpt)
	}
	if gpt.LastActive == "" {
		t.Fatal("gpt-5.4 last_active should be set")
	}
}

func TestKeyDetail_RespectsLimit(t *testing.T) {
	s := New()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		s.HandleUsage(context.Background(), usage.Record{
			APIKey:      "bulk",
			Model:       "test-model",
			RequestedAt: base.Add(time.Duration(i) * time.Second),
			Detail:      usage.Detail{InputTokens: int64(i), TotalTokens: int64(i)},
		})
	}

	detail := s.KeyDetail("bulk", time.Time{}, 100, nil, base.Add(time.Hour))
	if detail == nil {
		t.Fatal("bulk detail should not be nil")
	}
	if got := len(detail.RecentDetails); got != 100 {
		t.Fatalf("recent_details: want 100, got %d", got)
	}
	if detail.TotalRequests != 1000 {
		t.Fatalf("aggregate total_requests: want 1000, got %d", detail.TotalRequests)
	}
	// Returned details must be the newest 100.
	wantNewest := base.Add(999 * time.Second).Format(time.RFC3339Nano)
	if detail.RecentDetails[0].Timestamp != wantNewest {
		t.Fatalf("newest timestamp: want %s, got %s", wantNewest, detail.RecentDetails[0].Timestamp)
	}
	// And they must be sorted desc.
	for i := 1; i < len(detail.RecentDetails); i++ {
		if detail.RecentDetails[i].Timestamp > detail.RecentDetails[i-1].Timestamp {
			t.Fatalf("recent_details not desc-sorted at %d", i)
		}
	}
}

func TestKeyDetail_LimitClampedToMax(t *testing.T) {
	s := New()
	s.HandleUsage(context.Background(), usage.Record{
		APIKey:      "k",
		Model:       "m",
		RequestedAt: time.Now(),
		Detail:      usage.Detail{TotalTokens: 1},
	})
	detail := s.KeyDetail("k", time.Time{}, 999999, nil, time.Now())
	if detail == nil {
		t.Fatal("detail should not be nil")
	}
	// Only one record exists; the cap shouldn't matter for this assertion —
	// we just want to verify the call doesn't crash with a huge limit.
	if len(detail.RecentDetails) != 1 {
		t.Fatalf("recent_details: want 1, got %d", len(detail.RecentDetails))
	}
}

func TestKeyDetail_SinceFiltersDetailsAndAggregates(t *testing.T) {
	s := seedStore(t)
	since := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)

	detail := s.KeyDetail("anderson", since, 0, nil, now)
	if detail == nil {
		t.Fatal("anderson detail should not be nil")
	}
	// Only May 3 and May 4 records for anderson/gpt-5.4 pass the filter.
	if detail.TotalRequests != 2 {
		t.Fatalf("filtered total_requests: want 2, got %d", detail.TotalRequests)
	}
	if len(detail.Models) != 1 {
		t.Fatalf("filtered models: want 1, got %d", len(detail.Models))
	}
	if detail.Models[0].Model != "gpt-5.4" {
		t.Fatalf("filtered model name: want gpt-5.4, got %s", detail.Models[0].Model)
	}
	if got := len(detail.RecentDetails); got != 2 {
		t.Fatalf("filtered recent_details: want 2, got %d", got)
	}
	for _, d := range detail.RecentDetails {
		ts, _ := time.Parse(time.RFC3339Nano, d.Timestamp)
		if ts.Before(since) {
			t.Fatalf("recent detail %s should be on/after since=%s", d.Timestamp, since)
		}
	}
}

func TestKeyDetail_NilForUnknownKey(t *testing.T) {
	s := seedStore(t)
	if d := s.KeyDetail("ghost", time.Time{}, 0, nil, time.Now()); d != nil {
		t.Fatalf("unknown key should return nil, got %+v", d)
	}
}

func TestKeyDetail_NilRateLimitResolver(t *testing.T) {
	s := seedStore(t)
	detail := s.KeyDetail("anderson", time.Time{}, 0, nil, time.Now())
	if detail == nil {
		t.Fatal("detail should not be nil")
	}
	if len(detail.RateLimits) != 0 {
		t.Fatalf("nil resolver: want 0 rate_limits, got %d", len(detail.RateLimits))
	}
}

type stubResolver struct {
	limit  int
	window time.Duration
}

func (s stubResolver) Resolve(_, _ string) (int, time.Duration, bool) {
	return s.limit, s.window, s.limit > 0 && s.window > 0
}

func TestKeyDetail_RateLimitsCountAndResetsAt(t *testing.T) {
	s := New()
	// Three requests for the same model spaced 10 minutes apart.
	t0 := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		s.HandleUsage(context.Background(), usage.Record{
			APIKey:      "u",
			Model:       "claude",
			RequestedAt: t0.Add(time.Duration(i*10) * time.Minute),
			Detail:      usage.Detail{TotalTokens: 1},
		})
	}
	now := t0.Add(25 * time.Minute) // window of 20m covers the last 2 requests
	resolver := stubResolver{limit: 100, window: 20 * time.Minute}
	detail := s.KeyDetail("u", time.Time{}, 0, resolver, now)
	if detail == nil {
		t.Fatal("detail nil")
	}
	if len(detail.RateLimits) != 1 {
		t.Fatalf("rate_limits: want 1, got %d", len(detail.RateLimits))
	}
	rl := detail.RateLimits[0]
	if rl.Used != 2 {
		t.Fatalf("rl.Used: want 2, got %d", rl.Used)
	}
	if rl.Limit != 100 {
		t.Fatalf("rl.Limit: want 100, got %d", rl.Limit)
	}
	// Earliest in window = t0+10min; resets_at = earliest + 20min = t0+30min.
	wantResets := t0.Add(30*time.Minute).UnixMilli()
	if rl.ResetsAt != wantResets {
		t.Fatalf("rl.ResetsAt: want %d, got %d", wantResets, rl.ResetsAt)
	}
}

func TestKeyDetail_LastActive(t *testing.T) {
	s := seedStore(t)
	detail := s.KeyDetail("huycyberk", time.Time{}, 0, nil, time.Now())
	if detail == nil {
		t.Fatal("huycyberk detail nil")
	}
	if len(detail.Models) != 1 {
		t.Fatalf("models: want 1, got %d", len(detail.Models))
	}
	want := time.Date(2026, 5, 3, 14, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if detail.Models[0].LastActive != want {
		t.Fatalf("last_active: want %s, got %s", want, detail.Models[0].LastActive)
	}
}
