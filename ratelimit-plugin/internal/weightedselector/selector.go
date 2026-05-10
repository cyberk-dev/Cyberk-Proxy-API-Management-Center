package weightedselector

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// codexProvider is the provider key the SDK uses when selecting for Codex
// routes (see SDK selector.go preferCodexWebsocketAuths and service.go call sites).
const codexProvider = "codex"

// mixedProvider is the literal string the SDK passes to selector.Pick when
// multiple providers are eligible for a request. Every Execute / ExecuteStream
// call for OAuth routes goes through pickNextMixed → Pick(ctx, "mixed", ...)
// (SDK conductor.go:2786), even when only one provider ("codex") actually has
// matching auths. We must handle this path or weighted routing never fires.
const mixedProvider = "mixed"

// maxPoolKeys caps the pools map to protect against a hostile or buggy client
// spamming distinct (provider, model, ws) combinations. Matches the SDK's
// RoundRobinSelector.maxKeys default (sdk/cliproxy/auth/selector.go:274,324-328):
// when exceeded, the entire map is blown away and repopulated on demand.
const maxPoolKeys = 4096

// Selector wraps a base coreauth.Selector and takes over pick decisions for
// Codex requests, distributing them across accounts proportionally to the
// per-account weight derived from auth.Attributes["plan_type"].
//
// For non-Codex providers (Claude, Gemini, etc.) the selector delegates
// verbatim to the base so existing round-robin / fill-first / session-affinity
// wiring is unaffected.
type Selector struct {
	base coreauth.Selector
	cfg  Config

	mu    sync.Mutex
	pools map[string]*pool
}

// New constructs a Selector. `base` must be non-nil; callers typically pass
// &coreauth.RoundRobinSelector{} or &coreauth.FillFirstSelector{} depending on
// cfg.Routing.Strategy.
func New(base coreauth.Selector, cfg Config) *Selector {
	if base == nil {
		base = &coreauth.RoundRobinSelector{}
	}
	return &Selector{base: base, cfg: cfg, pools: make(map[string]*pool)}
}

// Pick satisfies coreauth.Selector. Non-codex providers fall through to the
// base selector immediately. Codex requests go through: (a) priority + cooldown
// filter (mirroring the SDK's getAvailableAuths semantics), (b) websocket
// preference filter for ws-downstream (mirroring preferCodexWebsocketAuths),
// (c) weight lookup from Attributes["plan_type"], (d) SWRR pick keyed by
// (provider, canonical model, ws flag).
//
// If weighted pick cannot produce a result (every eligible auth has weight 0,
// or weighting is disabled) the selector falls back to the base selector so we
// never strand a request.
func (s *Selector) Pick(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	auths []*coreauth.Auth,
) (*coreauth.Auth, error) {
	if !s.cfg.Enabled {
		return s.base.Pick(ctx, provider, model, opts, auths)
	}
	// Accept two shapes:
	//   (a) provider == "codex" — direct single-provider pick (rare; only the
	//       scheduler fast path uses this, and our non-built-in selector
	//       disables the fast path anyway, so this mostly never fires).
	//   (b) provider == "mixed" with a candidate set that is entirely Codex.
	//       This is the common case: SDK conductor.go:2786 always routes via
	//       pickNextMixed, so every Codex request arrives here as "mixed".
	// Anything else (explicit non-codex provider, or a truly cross-provider
	// mixed set) falls through to the base selector unchanged.
	providerLower := strings.ToLower(strings.TrimSpace(provider))
	if providerLower != codexProvider && !(providerLower == mixedProvider && allCodexAuths(auths)) {
		return s.base.Pick(ctx, provider, model, opts, auths)
	}

	now := time.Now()
	available := filterAvailable(auths, model, now)
	if len(available) == 0 {
		// Let the base selector surface the canonical error (cooldown / unavailable).
		return s.base.Pick(ctx, provider, model, opts, auths)
	}

	// Codex + downstream websocket: prefer ws-enabled auths just like the SDK
	// does inside RoundRobin/FillFirst. Critical for the openai_responses_websocket
	// handler path — routing a ws-downstream request to a non-ws auth breaks
	// transport.
	wsDownstream := cliproxyexecutor.DownstreamWebsocket(ctx)
	available = preferWebsocketAuths(available, wsDownstream)

	weights := make(map[string]int, len(available))
	ids := make([]string, 0, len(available))
	anyPositive := false
	for _, a := range available {
		w := s.cfg.WeightFor(planTypeOf(a))
		weights[a.ID] = w
		ids = append(ids, a.ID)
		if w > 0 {
			anyPositive = true
		}
	}
	if !anyPositive {
		return s.base.Pick(ctx, provider, model, opts, auths)
	}

	// Always key the pool under the canonical "codex" provider — even when the
	// caller passed "mixed" — so SWRR state is stable across both code paths.
	key := poolKey(codexProvider, model, wsDownstream)
	p := s.getOrCreatePool(key)
	chosen := p.pick(ids, weights)
	if chosen == "" {
		return s.base.Pick(ctx, provider, model, opts, auths)
	}
	for _, a := range available {
		if a.ID == chosen {
			return a, nil
		}
	}
	// Shouldn't happen: pool returned an ID that was in `ids`. Safety net.
	return s.base.Pick(ctx, provider, model, opts, auths)
}

// Stop forwards to the base selector if it implements StoppableSelector so
// resources like the session-affinity cache are released on shutdown. Note
// that SessionAffinitySelector.Stop() does NOT forward to its Fallback, so if
// this Selector is wrapped by SA, Stop() here won't be reached. Keep it
// forwarding anyway: the composition in main.go puts this Selector's base at
// the RR/FF level, which has no resources to release today.
func (s *Selector) Stop() {
	if st, ok := s.base.(coreauth.StoppableSelector); ok {
		st.Stop()
	}
}

// getOrCreatePool returns the pool for a key, creating one if needed.
// Enforces maxPoolKeys by blowing away the whole map on overflow (cheaper than
// tracking LRU; bounded-input clients never hit this path).
func (s *Selector) getOrCreatePool(key string) *pool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pools[key]; ok {
		return p
	}
	if len(s.pools) >= maxPoolKeys {
		s.pools = make(map[string]*pool, 16)
	}
	p := &pool{}
	s.pools[key] = p
	return p
}

func poolKey(provider, model string, ws bool) string {
	prefix := ""
	if ws {
		prefix = "ws:"
	}
	return prefix + provider + ":" + canonicalModelKey(model)
}

// allCodexAuths returns true when the candidate list is non-empty and every
// entry is a Codex auth. Used to distinguish "mixed route that happens to
// resolve to only Codex candidates" from a genuine cross-provider pick.
func allCodexAuths(auths []*coreauth.Auth) bool {
	seen := false
	for _, a := range auths {
		if a == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(a.Provider), codexProvider) {
			return false
		}
		seen = true
	}
	return seen
}

// planTypeOf extracts the plan_type attribute written by the SDK's Codex JWT
// synthesizer (internal/watcher/synthesizer/file.go). Missing value yields ""
// which WeightFor treats as an unknown plan (weight = defaultFallbackWeight=1).
// If you change WeightFor's "" handling, revisit this.
func planTypeOf(a *coreauth.Auth) string {
	if a == nil || a.Attributes == nil {
		return ""
	}
	return a.Attributes["plan_type"]
}

// The helpers below mirror unexported logic in the SDK's selector.go as of
// v6.9.34. If the SDK changes its priority / cooldown / websocket semantics,
// these must be updated to match. Keeping them here is cheaper than forking
// the SDK; the surface is small and stable.

// filterAvailable mirrors getAvailableAuths (SDK selector.go:220-255): removes
// disabled/cooled-down auths for this model, then keeps only the highest
// priority bucket among survivors.
func filterAvailable(auths []*coreauth.Auth, model string, now time.Time) []*coreauth.Auth {
	if len(auths) == 0 {
		return nil
	}
	byPriority := map[int][]*coreauth.Auth{}
	for _, a := range auths {
		if isBlockedForModel(a, model, now) {
			continue
		}
		byPriority[priorityOf(a)] = append(byPriority[priorityOf(a)], a)
	}
	if len(byPriority) == 0 {
		return nil
	}
	best := 0
	found := false
	for p := range byPriority {
		if !found || p > best {
			best = p
			found = true
		}
	}
	out := byPriority[best]
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// priorityOf mirrors SDK authPriority (selector.go:116-129).
func priorityOf(a *coreauth.Auth) int {
	if a == nil || a.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(a.Attributes["priority"])
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return v
}

// isBlockedForModel mirrors the subset of SDK isAuthBlockedForModel relevant
// to selection (selector.go:371-428).
//
// Critical semantic: when `model != ""`, the per-model state is authoritative.
// If no matching ModelState exists, the auth is treated as available for this
// model regardless of the top-level Unavailable flag. This matches the SDK's
// `return false` at selector.go:412 — without this, an auth whose top-level
// Unavailable was set by a transient failure (e.g. refresh error) would be
// wrongly skipped for fresh per-model requests.
//
// The top-level Unavailable check only runs when `model == ""`.
func isBlockedForModel(a *coreauth.Auth, model string, now time.Time) bool {
	if a == nil {
		return true
	}
	if a.Disabled || a.Status == coreauth.StatusDisabled {
		return true
	}
	if model != "" {
		if len(a.ModelStates) > 0 {
			state, ok := a.ModelStates[model]
			if !ok || state == nil {
				if base := canonicalModelKey(model); base != "" && base != model {
					state, ok = a.ModelStates[base]
				}
			}
			if ok && state != nil {
				if state.Status == coreauth.StatusDisabled {
					return true
				}
				if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
					return true
				}
				return false
			}
		}
		// No matching model state: available for this model.
		// Matches SDK selector.go:412 — DO NOT fall through to auth-level Unavailable.
		return false
	}
	if a.Unavailable && a.NextRetryAfter.After(now) {
		return true
	}
	return false
}

// canonicalModelKey mirrors SDK canonicalModelKey (selector.go:131-142) by
// stripping the `(value)` thinking suffix. Keeping parity with the SDK means
// ModelStates lookups hit the same buckets the SDK uses, and the SWRR pool
// doesn't fragment across `gpt-5(high)` / `gpt-5(low)` / `gpt-5`.
func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	// Inlined thinking.ParseSuffix (internal package, can't import).
	// Format: model-name(value). Only strip when both ( and trailing ) are present.
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen == -1 || !strings.HasSuffix(model, ")") {
		return model
	}
	name := strings.TrimSpace(model[:lastOpen])
	if name == "" {
		return model
	}
	return name
}

// preferWebsocketAuths mirrors SDK preferCodexWebsocketAuths (selector.go:176-198):
// when the downstream is a websocket, filter to auths whose websockets flag
// is enabled. If none match, fall back to the full list so the request still
// goes through (same degradation the SDK accepts).
func preferWebsocketAuths(auths []*coreauth.Auth, wsDownstream bool) []*coreauth.Auth {
	if !wsDownstream || len(auths) == 0 {
		return auths
	}
	ws := make([]*coreauth.Auth, 0, len(auths))
	for _, a := range auths {
		if authWebsocketsEnabled(a) {
			ws = append(ws, a)
		}
	}
	if len(ws) > 0 {
		return ws
	}
	return auths
}

// authWebsocketsEnabled mirrors SDK authWebsocketsEnabled (selector.go:144-174):
// read Attributes["websockets"] as bool-ish, or Metadata["websockets"] as bool
// or string. Unparseable values return false.
func authWebsocketsEnabled(a *coreauth.Auth) bool {
	if a == nil {
		return false
	}
	if len(a.Attributes) > 0 {
		if raw := strings.TrimSpace(a.Attributes["websockets"]); raw != "" {
			if v, err := strconv.ParseBool(raw); err == nil {
				return v
			}
		}
	}
	if len(a.Metadata) == 0 {
		return false
	}
	raw, ok := a.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		if parsed, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return parsed
		}
	}
	return false
}
