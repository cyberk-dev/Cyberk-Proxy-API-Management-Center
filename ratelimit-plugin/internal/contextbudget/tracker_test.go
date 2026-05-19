package contextbudget

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func newKey(id string) SessionKey {
	return SessionKey{APIKeyHash: "k", ID: id, Source: SessionFromHeader}
}

func TestTracker_RecordAndLookup(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	tr.Record(newKey("s1"), 12345)
	got, ok := tr.Lookup(newKey("s1"))
	if !ok || got != 12345 {
		t.Errorf("Lookup = (%d,%v), want (12345,true)", got, ok)
	}
}

func TestTracker_LookupMissEmpty(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	if _, ok := tr.Lookup(newKey("nope")); ok {
		t.Error("Lookup of unknown key should miss")
	}
}

func TestTracker_NilSafe(t *testing.T) {
	var tr *Tracker
	tr.Record(newKey("s"), 100) // must not panic
	if _, ok := tr.Lookup(newKey("s")); ok {
		t.Error("nil tracker should never hit")
	}
}

func TestTracker_TTLExpiry(t *testing.T) {
	tr := NewTracker(8, 10*time.Millisecond)
	tr.Record(newKey("s"), 100)
	time.Sleep(25 * time.Millisecond)
	if _, ok := tr.Lookup(newKey("s")); ok {
		t.Error("entry should have expired")
	}
	if tr.Len() != 0 {
		t.Errorf("expired entry should be purged, Len = %d", tr.Len())
	}
}

func TestTracker_LRUEviction(t *testing.T) {
	tr := NewTracker(3, time.Hour)
	tr.Record(newKey("a"), 1)
	tr.Record(newKey("b"), 2)
	tr.Record(newKey("c"), 3)
	tr.Record(newKey("d"), 4) // evicts "a" (least recently used)

	if _, ok := tr.Lookup(newKey("a")); ok {
		t.Error("oldest entry should have been evicted")
	}
	if _, ok := tr.Lookup(newKey("d")); !ok {
		t.Error("newest entry should be present")
	}
}

func TestTracker_LRUPromotionOnLookup(t *testing.T) {
	tr := NewTracker(3, time.Hour)
	tr.Record(newKey("a"), 1)
	tr.Record(newKey("b"), 2)
	tr.Record(newKey("c"), 3)

	// Touch "a" to promote it.
	if _, ok := tr.Lookup(newKey("a")); !ok {
		t.Fatal("a should still be present")
	}

	tr.Record(newKey("d"), 4) // should evict "b" now (LRU after a was promoted)
	if _, ok := tr.Lookup(newKey("a")); !ok {
		t.Error("a was promoted; should survive eviction")
	}
	if _, ok := tr.Lookup(newKey("b")); ok {
		t.Error("b should have been evicted, not a")
	}
}

func TestTracker_RecordIgnoresEmptyOrZero(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	tr.Record(SessionKey{}, 100)    // empty key
	tr.Record(newKey("s"), 0)       // zero tokens
	tr.Record(newKey("s"), -1)      // negative
	if tr.Len() != 0 {
		t.Errorf("no entry should have been recorded, Len = %d", tr.Len())
	}
}

func TestUsagePlugin_RecordsFromContext_Claude(t *testing.T) {
	// Anthropic: Input + Cached (best effort given upstream parser's
	// single-cache-field truncation).
	tr := NewTracker(8, time.Hour)
	plugin := NewUsagePlugin(tr)

	key := newKey("ses_1")
	ctx := WithProtocol(WithSession(context.Background(), key), ProtoClaude)

	plugin.HandleUsage(ctx, coreusage.Record{
		Detail: coreusage.Detail{
			InputTokens:  100,
			CachedTokens: 50,
		},
	})

	got, ok := tr.Lookup(key)
	if !ok || got != 150 {
		t.Errorf("Lookup after HandleUsage (Claude) = (%d,%v), want (150,true)", got, ok)
	}
}

func TestUsagePlugin_RecordsFromContext_OpenAI(t *testing.T) {
	// OpenAI/Codex/Gemini: cached_tokens ⊂ input_tokens — adding Cached
	// would double-count, so we use Input only.
	tr := NewTracker(8, time.Hour)
	plugin := NewUsagePlugin(tr)

	for _, p := range []Protocol{ProtoOpenAIChat, ProtoOpenAIResponses, ProtoGemini} {
		key := newKey("ses_" + p.String())
		ctx := WithProtocol(WithSession(context.Background(), key), p)
		plugin.HandleUsage(ctx, coreusage.Record{
			Detail: coreusage.Detail{
				InputTokens:  100,
				CachedTokens: 50,
			},
		})
		got, ok := tr.Lookup(key)
		if !ok || got != 100 {
			t.Errorf("protocol %v: Lookup after HandleUsage = (%d,%v), want (100,true)", p, got, ok)
		}
	}
}

func TestUsagePlugin_RecordsFromContext_UnknownProtocolUsesInput(t *testing.T) {
	// No WithProtocol → conservative default: Input only.
	tr := NewTracker(8, time.Hour)
	plugin := NewUsagePlugin(tr)

	key := newKey("ses_unknown")
	ctx := WithSession(context.Background(), key)

	plugin.HandleUsage(ctx, coreusage.Record{
		Detail: coreusage.Detail{InputTokens: 100, CachedTokens: 50},
	})

	got, ok := tr.Lookup(key)
	if !ok || got != 100 {
		t.Errorf("Lookup after HandleUsage (unknown) = (%d,%v), want (100,true)", got, ok)
	}
}

func TestUsagePlugin_SkipsFailed(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	plugin := NewUsagePlugin(tr)

	key := newKey("ses_fail")
	ctx := WithSession(context.Background(), key)

	plugin.HandleUsage(ctx, coreusage.Record{
		Failed: true,
		Detail: coreusage.Detail{InputTokens: 100},
	})

	if _, ok := tr.Lookup(key); ok {
		t.Error("failed record should not be persisted")
	}
}

func TestUsagePlugin_SkipsContextWithoutSession(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	plugin := NewUsagePlugin(tr)

	// Plain context — middleware never ran for this request.
	plugin.HandleUsage(context.Background(), coreusage.Record{
		Detail: coreusage.Detail{InputTokens: 100},
	})

	if tr.Len() != 0 {
		t.Error("usage without session ctx should be ignored")
	}
}

func TestWithSession_RoundTrip(t *testing.T) {
	key := newKey("x")
	ctx := WithSession(context.Background(), key)
	got, ok := sessionFromContext(ctx)
	if !ok {
		t.Fatal("session should round-trip")
	}
	if got.ID != key.ID {
		t.Errorf("got %q, want %q", got.ID, key.ID)
	}
}

func TestWithSession_EmptyIDNoop(t *testing.T) {
	ctx := WithSession(context.Background(), SessionKey{})
	if _, ok := sessionFromContext(ctx); ok {
		t.Error("empty session key should not be stored")
	}
}

// makeGinCtx builds the kind of context the CLIProxyAPI SDK produces for
// the usage Plugin: a fresh ctx with the gin.Context stored under the
// literal string key "gin" (handlers.go:414). This is the production
// recovery path that sessionFromContext must traverse.
func makeGinCtx(setup func(*gin.Context)) context.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if setup != nil {
		setup(c)
	}
	return context.WithValue(context.Background(), "gin", c)
}

func TestSessionFromContext_PrefersGin(t *testing.T) {
	key := newKey("from_gin")
	ctx := makeGinCtx(func(c *gin.Context) {
		SetGinSession(c, key)
	})
	got, ok := sessionFromContext(ctx)
	if !ok || got.ID != key.ID {
		t.Errorf("expected to recover %v from gin, got (%v,%v)", key.ID, got.ID, ok)
	}
}

func TestProtocolFromContext_PrefersGin(t *testing.T) {
	ctx := makeGinCtx(func(c *gin.Context) {
		SetGinProtocol(c, ProtoClaude)
	})
	got, ok := protocolFromContext(ctx)
	if !ok || got != ProtoClaude {
		t.Errorf("expected ProtoClaude from gin, got (%v,%v)", got, ok)
	}
}

func TestUsagePlugin_RecoversFromGin_EndToEnd(t *testing.T) {
	// This is the canary test for the S-HIGH bug we fixed: production ctx
	// only has the gin.Context, NOT a direct WithSession/WithProtocol
	// value, because the SDK rebases on context.Background(). If this test
	// passes the integration path works.
	tr := NewTracker(8, time.Hour)
	plugin := NewUsagePlugin(tr)

	key := newKey("ses_prod")
	ctx := makeGinCtx(func(c *gin.Context) {
		SetGinSession(c, key)
		SetGinProtocol(c, ProtoClaude)
	})

	plugin.HandleUsage(ctx, coreusage.Record{
		Detail: coreusage.Detail{InputTokens: 200, CachedTokens: 80},
	})

	got, ok := tr.Lookup(key)
	if !ok {
		t.Fatal("usage plugin failed to record via gin-context recovery path")
	}
	if got != 280 {
		t.Errorf("recovered total = %d, want 280 (Input+Cached for Claude)", got)
	}
}

func TestSetGinSession_EmptyKeyNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	SetGinSession(c, SessionKey{}) // empty ID — must not store
	if _, exists := c.Get(ginKeySession); exists {
		t.Error("empty session key should not be stored on gin context")
	}
}

func TestTracker_MarkWarnedIfFirst_BlocksWithinBurst(t *testing.T) {
	// Every call inside the burst window must return true (block) so we
	// keep returning 400s while CC's retry storm rages. After the window
	// expires, calls return false (passthrough) until ClearWarning.
	tr := NewTracker(8, time.Hour)
	tr.SetSoftBlockBurst(50 * time.Millisecond)
	key := newKey("ses_burst")

	if !tr.MarkWarnedIfFirst(key) {
		t.Fatal("first call must block (window opens)")
	}
	if !tr.MarkWarnedIfFirst(key) {
		t.Fatal("call inside burst must block")
	}
	if !tr.MarkWarnedIfFirst(key) {
		t.Fatal("3rd call still inside burst must block")
	}
	time.Sleep(80 * time.Millisecond)
	if tr.MarkWarnedIfFirst(key) {
		t.Error("call after burst window must passthrough (user has been warned)")
	}
}

func TestTracker_MarkWarnedIfFirst_RespectsConfiguredBurst(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	tr.SetSoftBlockBurst(200 * time.Millisecond)
	key := newKey("ses_burst_long")

	if !tr.MarkWarnedIfFirst(key) {
		t.Fatal("must block at window open")
	}
	time.Sleep(100 * time.Millisecond)
	if !tr.MarkWarnedIfFirst(key) {
		t.Fatal("must still block at t=100ms < 200ms")
	}
	time.Sleep(150 * time.Millisecond)
	if tr.MarkWarnedIfFirst(key) {
		t.Error("must passthrough past 200ms window")
	}
}

func TestTracker_MarkWarnedIfFirst_ConcurrentAllBlockInBurst(t *testing.T) {
	// 100 concurrent calls for the same session inside the burst window
	// must ALL return true — that's the whole point of the time-based
	// window: parallel CC requests (main turn + sidecar) must all 400
	// to surface the error, not race for a finite budget.
	tr := NewTracker(8, time.Hour)
	tr.SetSoftBlockBurst(time.Second)
	key := newKey("ses_race")

	const n = 100
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() { results <- tr.MarkWarnedIfFirst(key) }()
	}
	trueCount := 0
	for i := 0; i < n; i++ {
		if <-results {
			trueCount++
		}
	}
	if trueCount != n {
		t.Errorf("expected all %d concurrent calls to block, got %d", n, trueCount)
	}
}

func TestTracker_MarkWarnedIfFirst_RearmsAfterTTL(t *testing.T) {
	tr := NewTracker(8, 10*time.Millisecond)
	key := newKey("ses_ttl_rearm")

	if !tr.MarkWarnedIfFirst(key) {
		t.Fatal("first call must flip flag")
	}
	time.Sleep(25 * time.Millisecond)
	if !tr.MarkWarnedIfFirst(key) {
		t.Error("call after TTL expiry must re-arm (return true)")
	}
}

func TestTracker_ClearWarning_AllowsNextWarn(t *testing.T) {
	tr := NewTracker(8, time.Hour)
	key := newKey("ses_clear")

	tr.MarkWarnedIfFirst(key)
	tr.ClearWarning(key)
	if !tr.MarkWarnedIfFirst(key) {
		t.Error("after ClearWarning, next call must return true again")
	}
}

func TestTracker_ClearWarning_FullBurstRearm(t *testing.T) {
	// Models the post-compact re-arm: after the user compacts and the
	// tracker drops below soft, the middleware calls ClearWarning. The
	// next time tokens climb above soft a fresh burst window must open
	// so the user gets a new round of 400s (otherwise the warning would
	// be silently skipped on every subsequent climb).
	tr := NewTracker(8, time.Hour)
	tr.SetSoftBlockBurst(50 * time.Millisecond)
	key := newKey("ses_full_rearm")

	// First burst.
	if !tr.MarkWarnedIfFirst(key) {
		t.Fatal("initial block must return true")
	}
	if !tr.MarkWarnedIfFirst(key) {
		t.Fatal("in-burst block must return true")
	}
	time.Sleep(80 * time.Millisecond)
	if tr.MarkWarnedIfFirst(key) {
		t.Fatal("post-burst must passthrough")
	}

	// Simulate compact → tokens dropped below soft → middleware clears.
	tr.ClearWarning(key)

	// Fresh burst.
	if !tr.MarkWarnedIfFirst(key) {
		t.Error("post-clear block must return true (fresh window)")
	}
	if !tr.MarkWarnedIfFirst(key) {
		t.Error("post-clear in-burst block must return true")
	}
	time.Sleep(80 * time.Millisecond)
	if tr.MarkWarnedIfFirst(key) {
		t.Error("post-clear post-burst must passthrough")
	}
}

func TestTracker_SweepWarned_PurgesStale(t *testing.T) {
	// Sweep needs both TTL expiry AND burst-window expiry — the defensive
	// check refuses to evict an entry whose burst is still active so we
	// don't silently drop a warning mid-storm.
	tr := NewTracker(8, 10*time.Millisecond)
	tr.SetSoftBlockBurst(5 * time.Millisecond)
	for i := 0; i < 5; i++ {
		tr.MarkWarnedIfFirst(newKey("ses_sweep_" + string(rune('a'+i))))
	}
	if tr.WarnedLen() != 5 {
		t.Fatalf("expected 5 warned entries, got %d", tr.WarnedLen())
	}
	time.Sleep(25 * time.Millisecond)
	tr.SweepWarned()
	if tr.WarnedLen() != 0 {
		t.Errorf("SweepWarned should have purged all stale entries, got %d remaining", tr.WarnedLen())
	}
}

func TestTracker_SweepWarned_RefusesActiveBurst(t *testing.T) {
	// Defensive: if an operator misconfigures TTL << softBlockBurst, sweep
	// must not delete entries whose burst is still active — that would let
	// the user-invisible passthrough bug return mid-storm.
	tr := NewTracker(8, 10*time.Millisecond)
	tr.SetSoftBlockBurst(500 * time.Millisecond) // burst >> ttl
	tr.MarkWarnedIfFirst(newKey("ses_active_burst"))
	time.Sleep(25 * time.Millisecond) // past ttl
	tr.SweepWarned()
	if tr.WarnedLen() != 1 {
		t.Errorf("sweep must keep entries whose burst is still active, got %d", tr.WarnedLen())
	}
}

func TestTracker_MarkWarnedIfFirst_NilTrackerAllowsWarn(t *testing.T) {
	// Nil tracker can't dedupe — return true so caller still emits
	// the (occasional duplicate) warning rather than silently skipping.
	var tr *Tracker
	if !tr.MarkWarnedIfFirst(newKey("ses_nil")) {
		t.Error("nil tracker should default to allowing the warn")
	}
}

func TestSetGinProtocol_UnknownNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	SetGinProtocol(c, ProtoUnknown)
	if _, exists := c.Get(ginKeyProtocol); exists {
		t.Error("ProtoUnknown should not be stored on gin context")
	}
}
