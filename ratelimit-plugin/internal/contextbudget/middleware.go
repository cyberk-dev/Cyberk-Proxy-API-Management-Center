package contextbudget

import (
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// Skip lists mirror policy / ratelimit so management UI, health checks, and
// model listings are never context-budget-blocked.
var (
	skipPrefixes = []string{
		"/v0/management",
		"/management.html",
		"/healthz",
		"/v0/ratelimit",
	}
	skipExact = map[string]bool{
		"/":              true,
		"/v1/models":     true,
		"/v1beta/models": true,
	}
)

// Middleware returns a Gin handler that enforces soft + hard context-budget
// rules on incoming JSON requests. The shape of the inspection is:
//
//  1. Bail on non-JSON, skip-listed paths, WebSocket upgrades, or disabled
//     config.
//  2. Reuse ratelimit.PeekJSONBody so the body is read at most once across
//     the middleware chain.
//  3. Detect the upstream protocol from the URL path so we know which JSON
//     shape to walk.
//  4. Pick the budget figure for this request:
//     - If a Tracker is wired in AND the prior turn's accurate count is
//     still fresh for this session, use that (avoids char/4 drift).
//     - Otherwise fall back to char/4 estimation.
//  5. >= hard: abort with 413 (JSON) or one SSE error event (streaming).
//     >= soft and < hard: reject once per session (RespondOverflow), then
//     pass subsequent in-burst requests through. < soft: pass through.
//
// NOTE: this middleware does NOT currently rewrite c.Request.Body — it only
// reads the peek for estimation and either rejects or passes through. If a
// future change adds body mutation here (e.g. injecting a <system-reminder>),
// it MUST read the live c.Request.Body, not peek.Body: an earlier middleware
// (policy strip, effortnormalize) may have already replaced the body without
// refreshing the peek cache, so rebuilding from peek.Body would silently
// revert those edits.
//
// The session key (header- or body-hash-derived) is stashed on the
// request context regardless of whether a tracker hit occurred, so the
// downstream usage hook can record this turn's accurate count and seed
// the tracker for NEXT turn.
//
// IMPORTANT: this middleware must run AFTER promptlog so the prompt log
// records the original (unmutated) request body.
func Middleware(store *ConfigStore, tracker *Tracker) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := store.Get()
		if !cfg.Enabled() {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		for _, p := range skipPrefixes {
			if strings.HasPrefix(path, p) {
				c.Next()
				return
			}
		}
		if skipExact[path] {
			c.Next()
			return
		}
		if strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
			c.Next()
			return
		}

		protocol := DetectProtocol(path)
		if protocol == ProtoUnknown {
			c.Next()
			return
		}

		peek := ratelimit.PeekJSONBodyResult(c)
		if len(peek.Body) == 0 {
			c.Next()
			return
		}

		hard := cfg.Hard()
		soft := cfg.Soft()
		streaming := isStreamingRequest(c, peek.Body)

		// A body that overflows the 16 MiB peek cap is, by construction,
		// vastly larger than any reasonable threshold AND cannot be parsed
		// by gjson (the JSON is sliced mid-object). Treat it as a hard-block
		// rather than silently falling through with a zero estimate — the
		// exact requests we most want to block are the ones that would
		// otherwise bypass us here.
		if peek.Truncated {
			log.WithFields(log.Fields{
				"event":     "context_budget.hard_block",
				"reason":    "peek_cap_exceeded",
				"protocol":  protocol.String(),
				"streaming": streaming,
				"path":      path,
			}).Warn("context budget: body exceeds peek cap, treating as over hard limit")
			// Report the byte count we saw as a lower-bound "used" figure
			// (in token-equivalent units) so operators get something
			// meaningful in logs and error envelopes; the actual count
			// is by definition larger.
			usedLowerBound := len(peek.Body) / charsPerToken
			RespondOverflow(c, protocol, BudgetHard, usedLowerBound, hard, 0)
			return
		}

		// Try the session-tracker first: if we recorded an accurate
		// input-token count from this conversation's previous turn AND
		// it's still fresh, use that as the budget figure for THIS
		// turn. History grows incrementally so prior_count is a tight
		// lower bound on current_count; char/4 typically over- or under-
		// estimates by 10-20% depending on content shape. The tracker
		// gives us the upstream provider's own number, modulo one turn
		// of drift.
		//
		// We stash session+protocol on the *gin.Context (not r.Context())
		// because the CLIProxyAPI SDK rebases the executor ctx on
		// context.Background() at handlers.go:414 — only the gin.Context
		// reference survives that rebase. Without this, HandleUsage
		// would never see the session and the tracker would stay empty.
		sessionKey := ExtractSession(c.Request, peek.Body, protocol)
		SetGinSession(c, sessionKey)
		SetGinProtocol(c, protocol)

		var tokens int
		var source string
		if tracker != nil {
			if prior, ok := tracker.Lookup(sessionKey); ok {
				tokens = prior
				source = "tracker_" + sessionKey.Source.String()
			}
		}
		if tokens == 0 {
			tokens = EstimateTokens(peek.Body, protocol)
			source = "char_estimate"
		}

		// Threshold policy:
		//
		//   - HARD: always reject. Hitting hard means the next request is
		//     guaranteed to fail upstream anyway; we'd rather fail fast
		//     and ask for /compact than burn the upstream call. Per CC's
		//     conversation semantics, EVERY subsequent user turn in this
		//     session will resend the bloated history and re-trigger this
		//     branch — the user is forced to /compact or /clear to make
		//     progress, which is the intended UX. The hard envelope is
		//     identical across retries; we don't try to rate-limit it
		//     because the bottleneck is the user, not the proxy.
		//
		//   - SOFT: reject ONCE per session (per tracker TTL window). The
		//     first time a session crosses soft we send the user a 400
		//     with a compact hint; the user can read it, decide whether
		//     to /compact, and retry. After that first warning we let
		//     subsequent soft crosses through so the user isn't deadlocked
		//     between soft and hard if they choose to keep going. The
		//     "warned" flag clears when token usage falls back below soft
		//     (the user did /compact) so future climbs re-arm the warning.
		//
		// Returning HTTP 400 (invalid_request_error) rather than 413+SSE
		// matters because Claude Code's agentic loop retries SSE-format
		// errors as transient connection failures (~3 req/sec for ~30 s
		// in practice) but stops on a JSON 400 — the message text is
		// surfaced to the user instead. Anthropic SDK retry policy
		// explicitly classifies 400 as non-retryable.
		if hard > 0 && tokens >= hard {
			log.WithFields(log.Fields{
				"event":     "context_budget.hard_block",
				"protocol":  protocol.String(),
				"tokens":    tokens,
				"hard":      hard,
				"streaming": streaming,
				"path":      path,
				"source":    source,
				"session":   sessionKey.ID,
			}).Warn("context budget: hard limit reached, rejecting request")
			RespondOverflow(c, protocol, BudgetHard, tokens, hard, soft)
			return
		}

		if soft > 0 && tokens >= soft {
			// MarkSoftBlock atomically check-and-updates the burst state and
			// returns enough metadata to log at the right verbosity. While
			// the burst window is open every request is 400'd (no count
			// budget — CC fires parallel sidecar requests that would chew
			// through any small budget); after the window expires we
			// passthrough until ClearWarning re-arms.
			decision := SoftDecision{Block: true, BlockIndex: 1}
			if tracker != nil {
				decision = tracker.MarkSoftBlock(sessionKey)
			}
			if decision.Block {
				fields := log.Fields{
					"event":       "context_budget.soft_block",
					"protocol":    protocol.String(),
					"tokens":      tokens,
					"soft":        soft,
					"hard":        hard,
					"streaming":   streaming,
					"path":        path,
					"source":      source,
					"session":     sessionKey.ID,
					"block_index": decision.BlockIndex,
				}
				if decision.BlockIndex == 1 {
					// State transition: storm just opened. INFO so operators
					// see one line per burst, not one per request in the storm.
					log.WithFields(fields).Info("context budget: soft threshold crossed, burst window opens (400)")
				} else {
					fields["burst_age_ms"] = decision.BurstAge.Milliseconds()
					log.WithFields(fields).Debug("context budget: in-burst block (400)")
				}
				RespondOverflow(c, protocol, BudgetSoft, tokens, hard, soft)
				return
			}
			fields := log.Fields{
				"event":        "context_budget.soft_passthrough",
				"protocol":     protocol.String(),
				"tokens":       tokens,
				"soft":         soft,
				"hard":         hard,
				"path":         path,
				"source":       source,
				"session":      sessionKey.ID,
				"burst_age_ms": decision.BurstAge.Milliseconds(),
			}
			if decision.BurstJustClosed {
				// State transition: storm just ended; user has seen the
				// error and is choosing to keep going. INFO once.
				log.WithFields(fields).Info("context budget: burst window closed, resuming passthrough")
			} else {
				log.WithFields(fields).Debug("context budget: above soft but burst over, passing through")
			}
		} else if tracker != nil && soft > 0 {
			// Below soft — clear any previous warning so future climbs
			// re-arm. Cheap unconditional Delete (no presence check).
			tracker.ClearWarning(sessionKey)
		}

		// Wrap c.Writer so we can scan the upstream response for usage
		// fields synchronously inside this request goroutine — see the
		// detailed rationale in capture.go. The wrapper is transparent
		// to handlers (gin.ResponseWriter contract preserved); we read
		// the captured numbers AFTER c.Next() returns and feed them to
		// the tracker before this turn's stack unwinds.
		var cap *usageCapturingWriter
		if tracker != nil && sessionKey.ID != "" {
			cap = newUsageCapturingWriter(c.Writer, protocol)
			c.Writer = cap
		}

		c.Next()

		if cap != nil {
			total := cap.EffectiveInputTokens()
			if total > 0 {
				tracker.Record(sessionKey, total)
				log.WithFields(log.Fields{
					"event":    "context_budget.usage_record",
					"protocol": protocol.String(),
					"tokens":   total,
					"session":  sessionKey.ID,
					"source":   sessionKey.Source.String(),
				}).Info("recorded session tokens from response capture")
			}
		}
	}
}

// isStreamingRequest detects whether the response will be served as SSE.
// OpenAI/Claude signal this via `stream:true` in the body; Gemini uses the
// `:streamGenerateContent` URL suffix.
func isStreamingRequest(c *gin.Context, body []byte) bool {
	if IsStreamingPath(c.Request.URL.Path) {
		return true
	}
	if len(body) == 0 {
		return false
	}
	return gjson.GetBytes(body, "stream").Bool()
}
