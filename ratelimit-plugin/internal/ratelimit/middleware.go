package ratelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

var (
	skipPrefixes = []string{"/v0/management", "/management.html", "/healthz", "/v0/ratelimit"}
	skipExact    = map[string]bool{
		"/":              true,
		"/v1/models":     true,
		"/v1beta/models": true,
	}
)

func Middleware(store *ConfigStore, lim *Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
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

		apiKey := ExtractAPIKey(c.Request)
		if apiKey == "" {
			c.Next()
			return
		}

		model := ExtractModel(c)

		cfg := store.Get()
		if cfg == nil || !cfg.Enabled() {
			c.Next()
			return
		}

		// Canonicalize OAuth aliases to their upstream model before keying.
		// The core resolves aliases *after* this middleware, so without this an
		// alias like "claude-opus-4-8" (forks to gpt-5.5) would get its own
		// counter and dodge the gpt-5.5 per-key cap.
		model = cfg.Canonical(model)

		limit, window, ok := cfg.Resolve(apiKey, model)
		if !ok {
			c.Next()
			return
		}

		allowed, remaining, resetAt := lim.Take(apiKey, model, limit, window)
		c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
		if !resetAt.IsZero() {
			c.Header("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
		}

		if !allowed {
			retryAfter := int(time.Until(resetAt).Seconds())
			if retryAfter < 1 {
				retryAfter = 1
			}
			c.Header("Retry-After", strconv.Itoa(retryAfter))

			log.WithFields(log.Fields{
				"event":    "ratelimit.rejected",
				"key_hash": HashKey(apiKey),
				"model":    model,
				"limit":    limit,
				"window":   window.String(),
				"retry_s":  retryAfter,
				"path":     path,
			}).Warn("rate limit exceeded")

			displayModel := model
			if displayModel == "" {
				displayModel = "*"
			}
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"type": "error",
				"error": gin.H{
					"type":     "invalid_request_error",
					"message":  fmt.Sprintf("Quota exceeded for model %q — %d req / %s. Try again in %ds or switch model.", displayModel, limit, window, retryAfter),
					"model":    displayModel,
					"limit":    limit,
					"window":   window.String(),
					"reset_at": resetAt.Unix(),
				},
			})
			return
		}
		c.Next()
	}
}

func HashKey(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:6])
}
