package contextbudget

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

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
//
// The tracker also carries a separate per-session "soft-warned" flag used
// to guarantee that the soft 400 fires at most once per session before
// the user has a chance to /compact. Without this flag, every subsequent
// turn over the soft threshold would 400 the client and the conversation
// would deadlock — the user is allowed to keep going after one warning;
// only the hard limit is enforced unconditionally.
type Tracker struct {
	maxEntries int
	ttl        time.Duration

	mu    sync.Mutex
	order *list.List // newest at front
	items map[string]*trackerEntry

	warnedMu       sync.Mutex
	warnedSessions map[string]warnState // session-id → first-block timestamp

	// softBlockBurst is the duration after a session's FIRST soft block
	// during which every subsequent request is also blocked. Counts-based
	// budgets (e.g. "block 2 requests then pass") don't work because
	// Claude Code fires parallel sidecar requests (main turn + haiku
	// auto-title), exhausting any small budget in one instant, and CC's
	// retry of either request then trips the passthrough — so the user
	// never sees the error. A time window blocks the entire retry storm
	// (CC's exponential backoff exhausts within ~4 s) regardless of how
	// many parallel/retried requests fire, then lets the user keep
	// working without compacting if they choose to ignore the warning.
	//
	// 5 s comfortably covers CC's 3-attempt 1s/2s/4s backoff, with margin.
	softBlockBurst time.Duration
}

// warnState records when the current soft-block burst window started for
// a session. Once time.Since(firstBlockAt) exceeds softBlockBurst the
// session is no longer blocked (CC's retry storm is over and the user
// has seen the error); the entry stays in the map so we don't re-fire
// the burst on every subsequent turn — only when tokens fall below soft
// (ClearWarning) does the window re-arm.
type warnState struct {
	firstBlockAt        time.Time
	lastSeenAt          time.Time // for TTL sweep (request activity, not just initial block)
	blockCount          int       // how many requests we've blocked in this burst
	postBurstAnnounced  bool      // true after the first passthrough that crossed the burst boundary was logged
}

// SoftDecision describes the outcome of a MarkSoftBlock call so the middle-
// ware can emit a structured log at the right level. The fields are derived
// from warnState atomically — there is no second tracker lookup needed.
type SoftDecision struct {
	// Block reports whether this request must be 400'd.
	Block bool
	// BlockIndex is the 1-based position of this block within the current
	// burst window. 0 when Block is false.
	BlockIndex int
	// BurstAge is time.Since(firstBlockAt) at the moment of the decision,
	// useful for `burst_age_ms` logging on storm exit.
	BurstAge time.Duration
	// BurstJustClosed is true the FIRST time we emit a passthrough after
	// the burst window closed (state transition). Used by middleware to
	// emit a one-shot INFO line marking the end of the storm, instead of
	// spamming DEBUG for every subsequent turn.
	BurstJustClosed bool
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
		maxEntries:     maxEntries,
		ttl:            ttl,
		order:          list.New(),
		items:          make(map[string]*trackerEntry),
		warnedSessions: make(map[string]warnState),
		softBlockBurst: 5 * time.Second,
	}
}

// SetSoftBlockBurst overrides the soft-block burst window (default 5 s).
// Test helper — production code should leave the default in place.
func (t *Tracker) SetSoftBlockBurst(d time.Duration) {
	if t == nil || d <= 0 {
		return
	}
	t.warnedMu.Lock()
	defer t.warnedMu.Unlock()
	t.softBlockBurst = d
}

// MarkWarnedIfFirst returns true while the session is inside its
// soft-block burst window — i.e. either this is the first time the
// session has crossed soft (start the window), or we're still within
// softBlockBurst seconds of that first cross. After the window expires
// the session sees passthrough on subsequent crosses; the warning will
// only re-arm when tokens drop below soft (ClearWarning) so the user
// is not nagged on every turn of an admittedly-large conversation.
//
// Why a time window instead of a count: Claude Code fires parallel
// requests at the same instant (main turn + haiku auto-title) AND
// silently retries a 400 once with ~1 s backoff. A count budget of 2
// gets consumed by the two parallel initial requests in one millisecond,
// leaving zero budget for the retry — which then hits passthrough and
// gets a 200, masking the error from the user. A time window blocks
// EVERY request that arrives during CC's full retry storm, which forces
// CC to surface the error text. 5 s comfortably covers CC's 3-attempt
// 1s/2s/4s backoff with margin.
//
// The atomic check-under-mutex avoids the TOCTOU race the previous
// IsWarned + MarkWarned pair had where two concurrent turns of the
// same session could both observe "not warned yet" and both start
// independent windows.
func (t *Tracker) MarkWarnedIfFirst(key SessionKey) bool {
	return t.MarkSoftBlock(key).Block
}

// MarkSoftBlock is the rich-result form of MarkWarnedIfFirst. It atomically
// updates the burst state and returns enough metadata for the caller to log
// at the right verbosity (first block / in-burst block / first passthrough
// after burst close / quiet passthrough).
func (t *Tracker) MarkSoftBlock(key SessionKey) SoftDecision {
	id := key.String()
	if id == "" || t == nil {
		// No tracker / no session key → can't dedupe. Treating as a fresh
		// first block is the safer default — occasional duplicate warning
		// beats silently skipping.
		return SoftDecision{Block: true, BlockIndex: 1}
	}
	t.warnedMu.Lock()
	defer t.warnedMu.Unlock()
	now := time.Now()
	state, ok := t.warnedSessions[id]
	if ok && now.Sub(state.lastSeenAt) > t.ttl {
		// Session went dormant past TTL → purge and treat as fresh
		// so the warning re-arms when the user comes back.
		delete(t.warnedSessions, id)
		ok = false
		state = warnState{}
	}
	if !ok {
		// First cross — start a fresh burst window.
		t.warnedSessions[id] = warnState{
			firstBlockAt: now,
			lastSeenAt:   now,
			blockCount:   1,
		}
		return SoftDecision{Block: true, BlockIndex: 1, BurstAge: 0}
	}
	state.lastSeenAt = now
	age := now.Sub(state.firstBlockAt)
	if age < t.softBlockBurst {
		// Inside the burst window → keep blocking through CC's retry storm.
		state.blockCount++
		t.warnedSessions[id] = state
		return SoftDecision{Block: true, BlockIndex: state.blockCount, BurstAge: age}
	}
	// Burst expired → user has seen the error; allow them to continue.
	// Mark the FIRST such passthrough so middleware can log a one-shot
	// "storm ended" INFO line instead of spamming DEBUG every turn.
	justClosed := !state.postBurstAnnounced
	state.postBurstAnnounced = true
	t.warnedSessions[id] = state
	return SoftDecision{Block: false, BurstAge: age, BurstJustClosed: justClosed}
}

// ClearWarning removes any active warning for the session. The middleware
// calls this when an observed token count drops below the soft threshold
// (the user did /compact, the session got smaller, future soft crosses
// should re-arm) so the user gets a fresh warning if they hit it again.
func (t *Tracker) ClearWarning(key SessionKey) {
	id := key.String()
	if id == "" || t == nil {
		return
	}
	t.warnedMu.Lock()
	defer t.warnedMu.Unlock()
	delete(t.warnedSessions, id)
}

// SweepWarned removes warning entries older than the tracker's TTL.
// Without this, warned sessions that go dormant past TTL never get
// touched again by ClearWarning or MarkWarnedIfFirst (those only run
// on active sessions), so the map grows unbounded in production. Call
// this periodically — main.go runs it on the same minute-tick used by
// the rate-limit Limiter pruner.
func (t *Tracker) SweepWarned() {
	if t == nil {
		return
	}
	cutoff := time.Now().Add(-t.ttl)
	t.warnedMu.Lock()
	defer t.warnedMu.Unlock()
	now := time.Now()
	for id, state := range t.warnedSessions {
		if !state.lastSeenAt.Before(cutoff) {
			continue
		}
		// Defensive: refuse to evict an entry whose burst window is still
		// active even if lastSeenAt fell outside ttl. Today ttl (30 min)
		// >> softBlockBurst (5 s) so this can't happen, but if an operator
		// ever shrinks ttl below the burst we'd otherwise drop the warning
		// mid-storm and let the user-invisible passthrough bug return.
		if now.Sub(state.firstBlockAt) < t.softBlockBurst {
			continue
		}
		delete(t.warnedSessions, id)
	}
}

// WarnedLen reports the number of currently-warned sessions. Test/debug
// only.
func (t *Tracker) WarnedLen() int {
	if t == nil {
		return 0
	}
	t.warnedMu.Lock()
	defer t.warnedMu.Unlock()
	return len(t.warnedSessions)
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
	if p == nil || p.tracker == nil {
		log.WithField("event", "context_budget.usage_drop").Debug("plugin or tracker nil")
		return
	}
	if record.Failed {
		log.WithField("event", "context_budget.usage_drop").Debug("record failed")
		return
	}

	// Diagnostic: surface the recovery state so we can tell apart "gin not
	// in ctx", "gin in ctx but Keys reset", "session present but record-
	// rejected".
	ginPresent := false
	keysCount := -1
	if gc, ok := ctx.Value("gin").(*gin.Context); ok && gc != nil {
		ginPresent = true
		keysCount = len(gc.Keys)
	}

	key, sessOK := sessionFromContext(ctx)
	if !sessOK {
		log.WithFields(log.Fields{
			"event":        "context_budget.usage_drop",
			"reason":       "no_session_in_ctx",
			"gin_present":  ginPresent,
			"gin_keys":     keysCount,
			"record_model": record.Model,
			"record_apik":  truncate(record.APIKey, 6),
		}).Info("usage record dropped — session missing from ctx")
		return
	}
	protocol, _ := protocolFromContext(ctx) // ProtoUnknown → safe default below

	total := effectiveInputTokens(protocol, record.Detail)
	if total <= 0 {
		log.WithFields(log.Fields{
			"event":   "context_budget.usage_drop",
			"reason":  "zero_total",
			"session": key.ID,
		}).Debug("usage record dropped — total tokens 0")
		return
	}
	p.tracker.Record(key, total)
	log.WithFields(log.Fields{
		"event":    "context_budget.usage_record",
		"session":  key.ID,
		"source":   key.Source.String(),
		"protocol": protocol.String(),
		"tokens":   total,
	}).Info("recorded session tokens for next-turn tracking")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
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
