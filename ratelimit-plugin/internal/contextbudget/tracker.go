package contextbudget

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Gin-context keys used to hand session info from the request middleware
// across to the usage Plugin. We use string keys because the CLIProxyAPI
// SDK rebases the executor ctx on context.Background() (handlers.go:414)
// and only the gin.Context survives — so context.WithValue on r.Context()
// is dropped before HandleUsage runs.
const (
	ginKeySession  = "contextbudget.session"
	ginKeyProtocol = "contextbudget.protocol"
)

// trackerKey / protocolKey are the direct ctx.Value keys used as a
// fallback when no gin.Context is present (test code paths and any
// future caller that doesn't traverse Gin).
type trackerKey struct{}
type protocolKey struct{}

// WithSession stores the session key on ctx as a direct value. This is a
// TEST helper — production code MUST use SetGinSession on the *gin.Context
// because the SDK strips arbitrary ctx values before HandleUsage fires.
func WithSession(ctx context.Context, key SessionKey) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if key.ID == "" {
		return ctx
	}
	return context.WithValue(ctx, trackerKey{}, key)
}

// WithProtocol stores the protocol on ctx as a direct value. Same caveat
// as WithSession — production code uses SetGinProtocol.
func WithProtocol(ctx context.Context, p Protocol) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, protocolKey{}, p)
}

// SetGinSession stashes the session key on the gin.Context so the usage
// plugin can recover it after the response is dispatched. Must be called
// from middleware running on the same Gin pipeline as the handler.
func SetGinSession(c *gin.Context, key SessionKey) {
	if c == nil || key.ID == "" {
		return
	}
	c.Set(ginKeySession, key)
}

// SetGinProtocol stashes the request protocol on the gin.Context for the
// same reason as SetGinSession — HandleUsage needs to know which
// provider's token accounting rules to apply.
func SetGinProtocol(c *gin.Context, p Protocol) {
	if c == nil || p == ProtoUnknown {
		return
	}
	c.Set(ginKeyProtocol, p)
}

// sessionFromContext recovers a SessionKey, preferring the gin.Context
// injected by the SDK at handlers.go:414 over the direct ctx value (which
// only exists in tests).
func sessionFromContext(ctx context.Context) (SessionKey, bool) {
	if ctx == nil {
		return SessionKey{}, false
	}
	if gc, ok := ctx.Value("gin").(*gin.Context); ok && gc != nil {
		if v, exists := gc.Get(ginKeySession); exists {
			if k, ok := v.(SessionKey); ok && k.ID != "" {
				return k, true
			}
		}
	}
	if v, ok := ctx.Value(trackerKey{}).(SessionKey); ok && v.ID != "" {
		return v, true
	}
	return SessionKey{}, false
}

// protocolFromContext recovers the request protocol, preferring gin over
// direct ctx for the same reason as sessionFromContext.
func protocolFromContext(ctx context.Context) (Protocol, bool) {
	if ctx == nil {
		return ProtoUnknown, false
	}
	if gc, ok := ctx.Value("gin").(*gin.Context); ok && gc != nil {
		if v, exists := gc.Get(ginKeyProtocol); exists {
			if p, ok := v.(Protocol); ok && p != ProtoUnknown {
				return p, true
			}
		}
	}
	if v, ok := ctx.Value(protocolKey{}).(Protocol); ok && v != ProtoUnknown {
		return v, true
	}
	return ProtoUnknown, false
}

// trackerEntry is the unit stored per session.
type trackerEntry struct {
	tokens  int       // most recent total context size in tokens
	updated time.Time // for TTL eviction
	elem    *list.Element
}

// Tracker remembers the last observed input-token total per session so the
// middleware can estimate the NEXT request's size without re-tokenizing.
// Bounded LRU + TTL so memory usage is predictable; a busy proxy with
// 10000 unique sessions in flight will cap out and start evicting the
// oldest, which is fine because the soft/hard rules are applied per
// request and old sessions can simply fall back to char/4.
type Tracker struct {
	maxEntries int
	ttl        time.Duration

	mu    sync.Mutex
	order *list.List // newest at front
	items map[string]*trackerEntry
}

// NewTracker constructs a Tracker with reasonable defaults. maxEntries is
// the LRU cap; ttl is how stale a recorded count may be before it's
// considered useless (we re-estimate from scratch).
func NewTracker(maxEntries int, ttl time.Duration) *Tracker {
	if maxEntries <= 0 {
		maxEntries = 2048
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Tracker{
		maxEntries: maxEntries,
		ttl:        ttl,
		order:      list.New(),
		items:      make(map[string]*trackerEntry),
	}
}

// Record stores the latest observed token count for the session key.
// Called from the usage plugin after each upstream response.
func (t *Tracker) Record(key SessionKey, tokens int) {
	id := key.String()
	if id == "" || tokens <= 0 || t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if e, ok := t.items[id]; ok {
		e.tokens = tokens
		e.updated = time.Now()
		t.order.MoveToFront(e.elem)
		return
	}
	now := time.Now()
	entry := &trackerEntry{tokens: tokens, updated: now}
	entry.elem = t.order.PushFront(idHolder{id: id})
	t.items[id] = entry

	for t.order.Len() > t.maxEntries {
		oldest := t.order.Back()
		if oldest == nil {
			break
		}
		t.order.Remove(oldest)
		if h, ok := oldest.Value.(idHolder); ok {
			delete(t.items, h.id)
		}
	}
}

// Lookup returns the most recent observation for key, or (0, false) if
// the key is unknown or its entry is older than ttl. A fresh hit also
// promotes the entry to the front of the LRU.
//
// SEMANTIC NOTE: HandleUsage is invoked AFTER the response is dispatched.
// In an agentic client that pipelines turns rapidly, turn N+1's Lookup
// may run BEFORE turn N's Record has fired. In that case Lookup returns
// turn N-1's count (or 0 if N-1 didn't run either) and the middleware
// falls back to the char/4 estimator. This is correct degradation —
// the worst case is one extra turn of ±20% drift, not stale data being
// silently used as if fresh.
func (t *Tracker) Lookup(key SessionKey) (int, bool) {
	id := key.String()
	if id == "" || t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.items[id]
	if !ok {
		return 0, false
	}
	if time.Since(e.updated) > t.ttl {
		// Expired — purge so we don't keep a stale read alive on every
		// lookup and let the LRU surface fresher entries.
		t.order.Remove(e.elem)
		delete(t.items, id)
		return 0, false
	}
	t.order.MoveToFront(e.elem)
	return e.tokens, true
}

// Len returns the current number of entries (test helper).
func (t *Tracker) Len() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.items)
}

// idHolder lets the list carry the map key without a back-pointer cycle.
type idHolder struct{ id string }

// UsagePlugin adapts a Tracker to the cliproxy usage Plugin interface so
// it can be registered with the global usage Manager. On each response,
// it recovers the session key + protocol from the gin.Context (placed
// there by the middleware) and records the protocol-appropriate token
// total returned by upstream.
//
// Per-protocol accounting rules:
//
//   - Anthropic (Claude): input_tokens, cache_creation_input_tokens,
//     and cache_read_input_tokens are DISJOINT partitions that sum to
//     the true context size. CLIProxyAPI's parser only carries ONE of
//     the two cache fields into Detail.CachedTokens (read preferred,
//     creation as fallback). Best we can do without patching upstream
//     is Input+Cached; this under-counts by the unrecorded cache field
//     (typically cache_creation, which represents only the new turn's
//     bytes — usually <2% of total context). Documented limitation.
//
//   - OpenAI Chat / OpenAI Responses (incl. Codex CLI) / Gemini:
//     cached_tokens IS a subset of prompt_tokens/input_tokens (the
//     cached prefix is already counted there). So we must use ONLY
//     Input — adding Cached would double-count by the cached portion.
type UsagePlugin struct {
	tracker *Tracker
}

// NewUsagePlugin wraps the tracker as a usage Plugin.
func NewUsagePlugin(t *Tracker) *UsagePlugin {
	return &UsagePlugin{tracker: t}
}

// HandleUsage records the response's input-side token total against the
// session key stored on the gin.Context by the middleware. No-op when
// the key is missing (request didn't traverse the contextbudget middle-
// ware) or when the record represents a failed call (counts unreliable).
func (p *UsagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.tracker == nil || record.Failed {
		return
	}
	key, ok := sessionFromContext(ctx)
	if !ok {
		return
	}
	protocol, _ := protocolFromContext(ctx) // ProtoUnknown → safe default below

	total := effectiveInputTokens(protocol, record.Detail)
	if total <= 0 {
		return
	}
	p.tracker.Record(key, total)
}

// effectiveInputTokens collapses a usage.Detail into the single number
// our middleware uses to decide soft/hard thresholds. See UsagePlugin
// docs for the per-protocol rationale.
func effectiveInputTokens(p Protocol, d coreusage.Detail) int {
	switch p {
	case ProtoClaude:
		// Input + ONE of cache_read or cache_creation (whichever the
		// upstream parser preserved). Under-counts by the other cache
		// field when both are non-zero on the same turn — documented.
		return int(d.InputTokens + d.CachedTokens)
	case ProtoOpenAIChat, ProtoOpenAIResponses, ProtoGemini:
		// cached_tokens ⊂ prompt_tokens/input_tokens — Input already
		// includes the cached portion, so adding Cached double-counts.
		return int(d.InputTokens)
	default:
		// Unknown protocol → conservative default that won't double-
		// count on any known provider.
		return int(d.InputTokens)
	}
}
