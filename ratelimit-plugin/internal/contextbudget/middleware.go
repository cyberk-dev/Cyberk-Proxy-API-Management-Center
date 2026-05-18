package contextbudget

import (
	"bytes"
	"io"
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
//       still fresh for this session, use that (avoids char/4 drift).
//     - Otherwise fall back to char/4 estimation.
//  5. >= hard: abort with 413 (JSON) or one SSE error event (streaming).
//     >= soft and < hard: inject <system-reminder> into the last user
//     message and replace c.Request.Body with the mutated bytes so the
//     downstream handler's c.GetRawData() picks up the new body.
//     < soft: pass through unchanged.
//
// The session key (header- or body-hash-derived) is stashed on the
// request context regardless of whether a tracker hit occurred, so the
// downstream usage hook can record this turn's accurate count and seed
// the tracker for NEXT turn.
//
// IMPORTANT: this middleware must run AFTER promptlog so the prompt log
// records the original (unmutated) request body. It is safe to run after
// ratelimit/policy because mutation only happens on the body bytes that
// would be forwarded upstream — neither earlier middleware re-reads the
// peek cache after this point.
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
			RespondHardBlock(c, protocol, usedLowerBound, hard, streaming)
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
			}).Warn("context budget: hard limit reached, blocking request")
			RespondHardBlock(c, protocol, tokens, hard, streaming)
			return
		}

		if soft > 0 && tokens >= soft {
			reminder := cfg.Reminder(tokens)
			mutated := InjectSystemReminder(peek.Body, protocol, reminder)
			if !bytes.Equal(mutated, peek.Body) {
				// Replace the request body so the downstream handler's
				// c.GetRawData() reads the mutated bytes. We deliberately
				// do NOT update ratelimit's peek cache: by design, this is
				// the last middleware in the chain and no later code reads
				// the peek after us.
				c.Request.Body = io.NopCloser(bytes.NewReader(mutated))
				c.Request.ContentLength = int64(len(mutated))
				// Defensively drop the parsed Content-Length / Transfer-
				// Encoding metadata so any future code path that proxies
				// the *http.Request as-is doesn't end up with a stale
				// header winning over the updated ContentLength field.
				c.Request.Header.Del("Content-Length")
				c.Request.TransferEncoding = nil
				log.WithFields(log.Fields{
					"event":    "context_budget.soft_reminder",
					"protocol": protocol.String(),
					"tokens":   tokens,
					"soft":     soft,
					"hard":     hard,
					"path":     path,
					"source":   source,
					"session":  sessionKey.ID,
				}).Info("context budget: soft threshold reached, injected system-reminder")
			}
		}

		c.Next()
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
