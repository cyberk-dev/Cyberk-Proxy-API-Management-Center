// Package effortnormalize coerces unsupported OpenAI reasoning effort tiers
// to the closest supported value before the request reaches the upstream
// provider.
//
// The only rule today: reasoning.effort == "minimal"  →  "low".
//
// Background: clients like opencode send "minimal" to optimize cost/latency on
// title-gen and similar lightweight calls. Upstream Codex providers used in this
// deployment (gpt-5.x family) reject "minimal" as an unsupported tier and the
// CLIProxyAPI thinking validator returns
//   400 {"error":{"message":"level \"minimal\" not supported, valid levels: low, medium, high, xhigh"}}
// "low" is the closest tier all configured Codex backends accept, so we rewrite
// instead of failing the request. Gemini 3.x models do natively support
// "minimal" — if this deployment ever routes such requests through this proxy,
// gate the rewrite on the model name to preserve native support.
package effortnormalize

import (
	"bytes"
	"io"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// Middleware returns a Gin handler that rewrites reasoning.effort == "minimal"
// to "low" on JSON request bodies. Non-JSON requests, requests without the
// field, and truncated peek bodies are passed through untouched.
//
// Placement: must run AFTER promptlog so the prompt log preserves the original
// client value ("minimal"). Safe to run before policy/ratelimit/contextbudget —
// none of them inspect reasoning.effort.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		peek := ratelimit.PeekJSONBodyResult(c)
		if len(peek.Body) == 0 || peek.Truncated {
			c.Next()
			return
		}

		if gjson.GetBytes(peek.Body, "reasoning.effort").String() != "minimal" {
			c.Next()
			return
		}

		mutated, err := sjson.SetBytes(peek.Body, "reasoning.effort", "low")
		if err != nil {
			// Mutation failure shouldn't break the request — let it through as-is
			// and the upstream will return its usual 400.
			log.WithError(err).Warn("effortnormalize: sjson rewrite failed, passing through")
			c.Next()
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(mutated))
		c.Request.ContentLength = int64(len(mutated))

		log.WithFields(log.Fields{
			"event": "effortnormalize.rewrite",
			"from":  "minimal",
			"to":    "low",
			"model": gjson.GetBytes(mutated, "model").String(),
			"path":  c.Request.URL.Path,
		}).Debug("effortnormalize: coerced reasoning.effort")

		c.Next()
	}
}
