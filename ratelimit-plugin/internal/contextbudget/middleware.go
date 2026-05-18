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
//  4. EstimateTokens char-based; compare against soft/hard thresholds.
//  5. >= hard: abort with 413 (JSON) or one SSE error event (streaming).
//     >= soft and < hard: inject <system-reminder> into the last user
//     message and replace c.Request.Body with the mutated bytes so the
//     downstream handler's c.GetRawData() picks up the new body.
//     < soft: pass through unchanged.
//
// IMPORTANT: this middleware must run AFTER promptlog so the prompt log
// records the original (unmutated) request body. It is safe to run after
// ratelimit/policy because mutation only happens on the body bytes that
// would be forwarded upstream — neither earlier middleware re-reads the
// peek cache after this point.
func Middleware(store *ConfigStore) gin.HandlerFunc {
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

		tokens := EstimateTokens(peek.Body, protocol)

		if hard > 0 && tokens >= hard {
			log.WithFields(log.Fields{
				"event":     "context_budget.hard_block",
				"protocol":  protocol.String(),
				"tokens":    tokens,
				"hard":      hard,
				"streaming": streaming,
				"path":      path,
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
