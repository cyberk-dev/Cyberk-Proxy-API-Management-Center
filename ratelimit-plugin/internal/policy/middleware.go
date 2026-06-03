package policy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// Skip lists mirror ratelimit.Middleware so management UI, health checks, and
// model listings are never policy-blocked.
var (
	skipPrefixes = []string{"/v0/management", "/management.html", "/healthz", "/v0/ratelimit"}
	skipExact    = map[string]bool{
		"/":              true,
		"/v1/models":     true,
		"/v1beta/models": true,
	}
)

// Middleware returns a Gin handler that enforces service_tier policy on JSON
// request bodies. Two independent actions, applied globally (all keys, all
// models):
//
//   - BlockServiceTiers: reject the request with HTTP 400.
//   - StripPriority (default-on): silently remove service_tier:"priority" so
//     the request runs at the upstream's default tier instead of fast-mode.
//
// A blocklisted tier takes precedence over stripping: an operator who lists a
// tier in block_service_tiers wants a hard 400, not a quiet downgrade. Only
// JSON bodies are inspected — multipart and non-JSON content-types pass
// through unchanged. Reading the body reuses the shared peek cache from
// ratelimit.PeekJSONBody so it is parsed at most once per request.
func Middleware(store *ConfigStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := store.Get()
		if !cfg.Active() {
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

		peek := ratelimit.PeekJSONBodyResult(c)
		if len(peek.Body) == 0 {
			c.Next()
			return
		}

		tier := strings.TrimSpace(gjson.GetBytes(peek.Body, "service_tier").String())
		if tier == "" {
			// If the body was truncated and we couldn't find the field in
			// the prefix, log so operators can correlate with abuse reports.
			// We deliberately fail-open here: legitimate large requests that
			// happen to have no service_tier should not be rejected, and
			// adversarial bypass via a 4 MiB+ body is a realistic but
			// limited concern for this deployment's threat model.
			if peek.Truncated {
				log.WithFields(log.Fields{
					"event":    "policy.peek_truncated",
					"key_hash": ratelimit.HashKey(ratelimit.ExtractAPIKey(c.Request)),
					"path":     path,
				}).Warn("policy peek hit body cap; field-level checks may be incomplete")
			}
			c.Next()
			return
		}

		// Explicit reject list wins over the silent strip below.
		if cfg.IsBlockedTier(tier) {
			apiKey := ratelimit.ExtractAPIKey(c.Request)
			log.WithFields(log.Fields{
				"event":        "policy.rejected",
				"reason":       "service_tier_blocked",
				"key_hash":     ratelimit.HashKey(apiKey),
				"service_tier": tier,
				"path":         path,
			}).Warn("request blocked by policy")

			// Error shape mirrors OpenAI's v1 error envelope so SDK clients
			// (openai-python, openai-node, LangChain) parse it without custom
			// handling. service_tier is included as a non-standard field for
			// human/log debugging — clients ignore unknown fields.
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":         "invalid_request_error",
					"message":      fmt.Sprintf("service_tier %q is not allowed by proxy policy.", tier),
					"param":        "service_tier",
					"code":         nil,
					"service_tier": tier,
				},
			})
			return
		}

		if cfg.ShouldStripPriority() && strings.EqualFold(tier, "priority") {
			stripServiceTier(c, peek, path)
		}

		c.Next()
	}
}

// stripServiceTier removes the service_tier field from the forwarded request
// body so callers can't obtain priority fast-mode processing.
//
// It rewrites the *current* c.Request.Body rather than the cached peek so it
// composes with any earlier in-place body mutation — effortnormalize runs just
// before this middleware and replaces c.Request.Body without refreshing the
// peek cache, so rebuilding from peek.Body here would silently revert its
// reasoning.effort fix. A truncated peek means service_tier may sit beyond the
// 16 MiB buffer; rather than load an unbounded body we log and pass through.
func stripServiceTier(c *gin.Context, peek ratelimit.PeekResult, path string) {
	if peek.Truncated {
		log.WithFields(log.Fields{
			"event": "policy.strip_skipped_truncated",
			"path":  path,
		}).Warn("policy: body exceeded peek cap; cannot safely strip service_tier")
		return
	}

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.WithError(err).Warn("policy: read body for service_tier strip failed, passing through")
		c.Request.Body = io.NopCloser(bytes.NewReader(peek.Body))
		return
	}

	stripped, err := sjson.DeleteBytes(raw, "service_tier")
	if err != nil {
		log.WithError(err).Warn("policy: delete service_tier failed, passing through")
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}

	c.Request.Body = io.NopCloser(bytes.NewReader(stripped))
	c.Request.ContentLength = int64(len(stripped))
	// Drop the parsed Content-Length / Transfer-Encoding metadata so any code
	// path that proxies the *http.Request as-is uses the updated ContentLength.
	c.Request.Header.Del("Content-Length")
	c.Request.TransferEncoding = nil

	log.WithFields(log.Fields{
		"event": "policy.stripped_priority",
		"path":  path,
		"model": gjson.GetBytes(stripped, "model").String(),
	}).Debug("policy: stripped service_tier=priority")
}
