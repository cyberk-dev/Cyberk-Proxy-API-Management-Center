package policy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

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

// Middleware returns a Gin handler that rejects requests carrying a
// service_tier value present in the configured blocklist. The check is global
// (all keys, all models) and applies only to JSON bodies — multipart and
// non-JSON content-types pass through unchanged. Reading the body reuses the
// shared peek cache from ratelimit.PeekJSONBody so the body is parsed at most
// once per request, regardless of middleware ordering.
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

		if !cfg.IsBlockedTier(tier) {
			c.Next()
			return
		}

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
	}
}
