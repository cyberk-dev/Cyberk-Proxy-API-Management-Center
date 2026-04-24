package weightedselector

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// fakeBase records delegations and returns the first auth (or a fixed error).
type fakeBase struct {
	calls int
	err   error
}

func (f *fakeBase) Pick(_ context.Context, _ string, _ string, _ cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(auths) == 0 {
		return nil, errors.New("no auths")
	}
	return auths[0], nil
}

func makeAuth(id, planType string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:         id,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": planType},
	}
}

func makeAuthProvider(id, planType, provider string) *coreauth.Auth {
	a := makeAuth(id, planType)
	a.Provider = provider
	return a
}

func TestSelectorDelegatesNonCodex(t *testing.T) {
	base := &fakeBase{}
	s := New(base, Config{Enabled: true, Weights: map[string]int{"pro": 10, "plus": 1}})

	auths := []*coreauth.Auth{makeAuth("a", "pro"), makeAuth("b", "plus")}
	for _, provider := range []string{"claude", "gemini", "openai"} {
		_, err := s.Pick(context.Background(), provider, "m", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("%s pick err: %v", provider, err)
		}
	}
	if base.calls != 3 {
		t.Fatalf("base.calls = %d, want 3", base.calls)
	}
}

func TestSelectorDelegatesWhenDisabled(t *testing.T) {
	base := &fakeBase{}
	s := New(base, Config{Enabled: false})
	auths := []*coreauth.Auth{makeAuth("a", "pro"), makeAuth("b", "plus")}
	_, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("pick err: %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("base.calls = %d, want 1 (disabled -> delegate)", base.calls)
	}
}

func TestSelectorWeightedDistributionForCodex(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`
codex_weights:
  pro: 10
  plus: 1
`))
	s := New(base, cfg)

	auths := []*coreauth.Auth{makeAuth("a_plus", "plus"), makeAuth("b_pro", "pro")}
	counts := map[string]int{}
	for i := 0; i < 110; i++ {
		a, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		counts[a.ID]++
	}
	if counts["b_pro"] != 100 || counts["a_plus"] != 10 {
		t.Fatalf("pro=%d plus=%d over 110 ticks, want 100/10", counts["b_pro"], counts["a_plus"])
	}
	if base.calls != 0 {
		t.Fatalf("base.calls = %d, want 0 (codex should not delegate when weights resolve)", base.calls)
	}
}

func TestSelectorSkipsDisabledAuth(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`
codex_weights:
  pro: 10
  plus: 1
`))
	s := New(base, cfg)

	pro := makeAuth("pro1", "pro")
	plus := makeAuth("plus1", "plus")
	pro.Disabled = true
	auths := []*coreauth.Auth{pro, plus}

	for i := 0; i < 5; i++ {
		a, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if a.ID != "plus1" {
			t.Fatalf("step %d: got %q, want plus1 (pro disabled)", i, a.ID)
		}
	}
}

// With a non-empty model, per-model cooldown (auth.ModelStates[model].Unavailable
// + NextRetryAfter) is the canonical way to mark an auth as blocked. Top-level
// Unavailable is only consulted when model == "" — see TestSelector...
// WhenModelSpecified / WhenModelEmpty for that split.
func TestSelectorSkipsPerModelCooldown(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	pro := makeAuth("pro1", "pro")
	pro.ModelStates = map[string]*coreauth.ModelState{
		"gpt-5": {
			Status:         coreauth.StatusActive,
			Unavailable:    true,
			NextRetryAfter: time.Now().Add(10 * time.Minute),
		},
	}
	plus := makeAuth("plus1", "plus")
	auths := []*coreauth.Auth{pro, plus}

	a, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("pick err: %v", err)
	}
	if a.ID != "plus1" {
		t.Fatalf("got %q, want plus1 (pro per-model cooldown)", a.ID)
	}
}

func TestSelectorRespectsPriorityBucket(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	hiPri := makeAuth("hi", "plus")
	hiPri.Attributes["priority"] = "5"
	loPri := makeAuth("lo", "pro")
	loPri.Attributes["priority"] = "1"
	auths := []*coreauth.Auth{hiPri, loPri}

	// Even though pro has higher weight, priority=5 bucket is chosen exclusively.
	for i := 0; i < 5; i++ {
		a, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if a.ID != "hi" {
			t.Fatalf("step %d: got %q, want hi (priority 5 wins over priority 1)", i, a.ID)
		}
	}
}

func TestSelectorUnknownPlanTypeFallsBackToOne(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10 }`))
	s := New(base, cfg)

	pro := makeAuth("pro1", "pro")
	weird := makeAuth("weird1", "some_new_tier_2027")
	auths := []*coreauth.Auth{pro, weird}

	counts := map[string]int{}
	for i := 0; i < 11; i++ {
		a, _ := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		counts[a.ID]++
	}
	// pro=10, unknown=fallback=1 -> 10:1 distribution.
	if counts["pro1"] != 10 || counts["weird1"] != 1 {
		t.Fatalf("pro=%d weird=%d, want 10/1", counts["pro1"], counts["weird1"])
	}
}

func TestSelectorFallsBackWhenAllWeightsZero(t *testing.T) {
	base := &fakeBase{}
	// Build a config whose explicit overrides zero out both plans present.
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 0, plus: 0, free: 0, team: 0, business: 0, go: 0, prolite: 0 }`))
	s := New(base, cfg)

	auths := []*coreauth.Auth{makeAuth("a", "pro"), makeAuth("b", "plus")}
	_, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("pick err: %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("base.calls = %d, want 1 (zero weights should fall back to base)", base.calls)
	}
}

func TestSelectorPropagatesBaseError(t *testing.T) {
	want := errors.New("boom")
	base := &fakeBase{err: want}
	s := New(base, Config{Enabled: false})
	_, err := s.Pick(context.Background(), "claude", "m", cliproxyexecutor.Options{}, []*coreauth.Auth{makeAuth("a", "")})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// Regression for oracle HIGH-1: when `model != ""` but auth has no
// ModelStates at all, the auth MUST be treated as available, even if its
// top-level Unavailable flag is set. The SDK returns false at selector.go:412
// in this branch; our replica previously fell through to the auth-level check.
func TestSelectorIgnoresAuthLevelUnavailableWhenModelSpecified(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	// pro has top-level Unavailable (e.g. transient refresh failure) but
	// empty ModelStates. For a fresh "gpt-5" request, SDK treats it as
	// available — so must we.
	pro := makeAuth("pro1", "pro")
	pro.Unavailable = true
	pro.NextRetryAfter = time.Now().Add(10 * time.Minute)
	plus := makeAuth("plus1", "plus")
	auths := []*coreauth.Auth{pro, plus}

	counts := map[string]int{}
	for i := 0; i < 11; i++ {
		a, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		counts[a.ID]++
	}
	// Pro stays in rotation because model != "" and ModelStates is empty.
	if counts["pro1"] != 10 || counts["plus1"] != 1 {
		t.Fatalf("pro=%d plus=%d, want 10/1 (pro must stay eligible)", counts["pro1"], counts["plus1"])
	}
}

// Same regression, but with an empty model string: now the top-level
// Unavailable MUST take effect.
func TestSelectorRespectsAuthLevelUnavailableWhenModelEmpty(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	pro := makeAuth("pro1", "pro")
	pro.Unavailable = true
	pro.NextRetryAfter = time.Now().Add(10 * time.Minute)
	plus := makeAuth("plus1", "plus")
	auths := []*coreauth.Auth{pro, plus}

	for i := 0; i < 5; i++ {
		a, err := s.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if a.ID != "plus1" {
			t.Fatalf("step %d: got %q, want plus1 (model='' honors auth-level cooldown)", i, a.ID)
		}
	}
}

// Regression for oracle HIGH-2: websocket-downstream Codex requests must only
// pick from ws-enabled auths when any exist; if none exist, fall back to all.
func TestSelectorPrefersWebsocketAuthsWhenDownstreamIsWS(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	wsPro := makeAuth("ws_pro", "pro")
	wsPro.Attributes["websockets"] = "true"
	plainPro := makeAuth("plain_pro", "pro")
	plainPlus := makeAuth("plain_plus", "plus")
	auths := []*coreauth.Auth{wsPro, plainPro, plainPlus}

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	for i := 0; i < 5; i++ {
		a, err := s.Pick(ctx, "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if a.ID != "ws_pro" {
			t.Fatalf("step %d: got %q, want ws_pro (only ws-enabled auth)", i, a.ID)
		}
	}
}

func TestSelectorWebsocketFallbackWhenNoneEnabled(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	// No ws-enabled auth present. SDK falls back to the full list; we must too.
	auths := []*coreauth.Auth{makeAuth("pro1", "pro"), makeAuth("plus1", "plus")}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	a, err := s.Pick(ctx, "codex", "gpt-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("pick err: %v", err)
	}
	if a == nil {
		t.Fatalf("got nil auth; must fall back to full list")
	}
}

func TestSelectorWebsocketCacheKeyIsolation(t *testing.T) {
	// ws-downstream and non-ws-downstream must use separate SWRR pools so their
	// counters don't bleed into each other.
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	wsPro := makeAuth("ws_pro", "pro")
	wsPro.Attributes["websockets"] = "true"
	plainPro := makeAuth("plain_pro", "pro")
	plainPlus := makeAuth("plain_plus", "plus")
	allAuths := []*coreauth.Auth{wsPro, plainPro, plainPlus}

	wsCtx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	plainCtx := context.Background()

	// Alternate ws and non-ws calls. If pools share state, non-ws distribution
	// will drift because ws-only ws_pro dominates the cursor. With separate
	// pools, non-ws distribution stays 100:10 across (ws_pro,plain_pro,plain_plus)
	// filtered by cooldown (none).
	wsCount := map[string]int{}
	plainCount := map[string]int{}
	for i := 0; i < 11; i++ {
		a, _ := s.Pick(wsCtx, "codex", "gpt-5", cliproxyexecutor.Options{}, allAuths)
		wsCount[a.ID]++
	}
	for i := 0; i < 11; i++ {
		a, _ := s.Pick(plainCtx, "codex", "gpt-5", cliproxyexecutor.Options{}, allAuths)
		plainCount[a.ID]++
	}
	// All ws picks must be ws_pro (only ws-enabled auth).
	if wsCount["ws_pro"] != 11 {
		t.Fatalf("ws picks: ws_pro=%d, want 11", wsCount["ws_pro"])
	}
	// Non-ws picks see all 3 auths with pro=10, plus=1. Both "pro" auths get
	// weight 10, plus gets 1: over 21 picks pro-bucket = 20, plus = 1.
	// Over 11 picks, ratio is ~20:1 so plus gets ~0-1.
	if plainCount["plain_plus"] > 2 {
		t.Fatalf("non-ws plus picks = %d, want <=2 over 11 calls", plainCount["plain_plus"])
	}
}

// Regression for MEDIUM-1: canonicalModelKey should strip the thinking suffix
// so gpt-5(high) and gpt-5(low) share a pool rather than fragmenting state.
func TestSelectorCanonicalModelKeyStripsSuffix(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)
	auths := []*coreauth.Auth{makeAuth("pro1", "pro"), makeAuth("plus1", "plus")}

	counts := map[string]int{}
	// 11 calls split across two thinking suffixes — must land in the same pool.
	for i := 0; i < 11; i++ {
		model := "gpt-5(high)"
		if i%2 == 1 {
			model = "gpt-5(low)"
		}
		a, _ := s.Pick(context.Background(), "codex", model, cliproxyexecutor.Options{}, auths)
		counts[a.ID]++
	}
	// If the pools were separate per-suffix, each would start from zero and
	// plus would be picked more (the "11th" plus call never arrives because
	// each pool only runs ~5-6 picks). With shared pool, ratio is 10:1 exact.
	if counts["pro1"] != 10 || counts["plus1"] != 1 {
		t.Fatalf("pro=%d plus=%d, want 10/1 (suffix-stripped pool)", counts["pro1"], counts["plus1"])
	}
}

// Regression for MEDIUM-3: the pool map must not grow unboundedly. Hostile
// client spams distinct model names; we must reset at maxPoolKeys rather than
// OOM.
func TestSelectorPoolMapCapped(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)
	auths := []*coreauth.Auth{makeAuth("pro1", "pro"), makeAuth("plus1", "plus")}

	// Fire enough distinct models to force at least one reset.
	for i := 0; i < maxPoolKeys+100; i++ {
		model := "model-" + strconv.Itoa(i)
		if _, err := s.Pick(context.Background(), "codex", model, cliproxyexecutor.Options{}, auths); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	s.mu.Lock()
	size := len(s.pools)
	s.mu.Unlock()
	if size > maxPoolKeys {
		t.Fatalf("pools size = %d, must be <= %d", size, maxPoolKeys)
	}
}

// Regression for the v6.9.34 deployment miss: the SDK routes every
// Execute/ExecuteStream call through pickNextMixed, which calls
// selector.Pick(ctx, "mixed", ...). A strict provider=="codex" check skips the
// weighted branch and all Codex traffic falls back to round-robin. Mixed-path
// with an all-Codex candidate set must still be weighted.
func TestSelectorAppliesWeightToMixedProviderPath(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	auths := []*coreauth.Auth{makeAuth("pro1", "pro"), makeAuth("plus1", "plus")}
	counts := map[string]int{}
	for i := 0; i < 11; i++ {
		a, err := s.Pick(context.Background(), "mixed", "gpt-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		counts[a.ID]++
	}
	if counts["pro1"] != 10 || counts["plus1"] != 1 {
		t.Fatalf("mixed-path: pro=%d plus=%d, want 10/1", counts["pro1"], counts["plus1"])
	}
	if base.calls != 0 {
		t.Fatalf("base.calls = %d, want 0 for mixed all-codex", base.calls)
	}
}

// Mixed path with an empty candidate list must delegate — allCodexAuths returns
// false and the base selector owns the "no auths" error shape.
func TestSelectorMixedPathEmptyAuthsDelegates(t *testing.T) {
	base := &fakeBase{err: errors.New("no auth")}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	_, err := s.Pick(context.Background(), "mixed", "gpt-5", cliproxyexecutor.Options{}, nil)
	if err == nil {
		t.Fatalf("expected delegate error")
	}
	if base.calls != 1 {
		t.Fatalf("base.calls = %d, want 1", base.calls)
	}
}

// Mixed path with a truly cross-provider candidate set must delegate — the
// weighted branch is only safe when every candidate is a Codex auth.
func TestSelectorMixedPathCrossProviderDelegates(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	auths := []*coreauth.Auth{
		makeAuth("codex1", "pro"),
		makeAuthProvider("claude1", "", "claude"),
	}
	if _, err := s.Pick(context.Background(), "mixed", "some-model", cliproxyexecutor.Options{}, auths); err != nil {
		t.Fatalf("pick err: %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("base.calls = %d, want 1 (cross-provider delegates)", base.calls)
	}
}

// Both entry shapes — provider=="codex" and provider=="mixed" with all-codex
// candidates — must share SWRR state under the same (model, ws) key. Otherwise
// a process that sees both paths (e.g. some internal sub-request using "codex"
// directly) fragments the distribution.
func TestSelectorMixedAndCodexSharePool(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)

	auths := []*coreauth.Auth{makeAuth("pro1", "pro"), makeAuth("plus1", "plus")}
	counts := map[string]int{}
	for i := 0; i < 11; i++ {
		provider := "codex"
		if i%2 == 1 {
			provider = "mixed"
		}
		a, _ := s.Pick(context.Background(), provider, "gpt-5", cliproxyexecutor.Options{}, auths)
		counts[a.ID]++
	}
	if counts["pro1"] != 10 || counts["plus1"] != 1 {
		t.Fatalf("shared pool: pro=%d plus=%d, want 10/1", counts["pro1"], counts["plus1"])
	}
}

// Peace-of-mind concurrency test. 16 goroutines × 500 picks against a fixed
// auth set. With -race, any torn reads or lock-ordering issues surface.
func TestSelectorConcurrentPicks(t *testing.T) {
	base := &fakeBase{}
	cfg, _ := parseBytes([]byte(`codex_weights: { pro: 10, plus: 1 }`))
	s := New(base, cfg)
	auths := []*coreauth.Auth{makeAuth("pro1", "pro"), makeAuth("plus1", "plus")}

	const workers = 16
	const perWorker = 500

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if _, err := s.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, auths); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent pick err: %v", err)
	}
}
