package promptlog

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cyberk/ratelimit-plugin/internal/ratelimit"
)

// skipPrefixes / skipExact mirror policy and ratelimit so management UI,
// health checks, and model listings never trigger extraction. Kept as a
// separate list (rather than imported) to avoid coupling — these three
// middlewares share a concept but have independent skip evolution.
var (
	skipPrefixes = []string{"/v0/management", "/management.html", "/healthz", "/v0/ratelimit"}
	skipExact    = map[string]bool{
		"/":              true,
		"/v1/models":     true,
		"/v1beta/models": true,
	}
)

// Middleware returns a Gin handler that captures the final user message of
// each JSON chat request and submits it to writer for asynchronous logging.
// Mount this BEFORE policy and ratelimit middlewares so that requests
// rejected by those layers are still recorded — analyzing rejected attempts
// is one of the main reasons to enable promptlog at all.
func Middleware(cfg *Config, writer *Writer) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !cfg.IsEnabled() || writer == nil {
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

		provider := detectProvider(path)
		if provider == "" {
			c.Next()
			return
		}

		peek := ratelimit.PeekJSONBodyResult(c)
		if len(peek.Body) == 0 {
			c.Next()
			return
		}

		blocks := extractBlocks(peek.Body, provider, cfg.MaxTextBytes)
		if len(blocks) == 0 {
			// No user-authored content (no user-role message, or only
			// tool_result / wrapper-noise blocks). Nothing meaningful to log.
			c.Next()
			return
		}

		client := IdentifyClient(c.Request.Header)
		cwd := extractCWD(extractSystemText(peek.Body, provider))

		entry := &Entry{
			Timestamp:     time.Now().UTC(),
			Provider:      provider,
			Path:          path,
			Model:         ratelimit.ExtractModel(c),
			KeyHash:       ratelimit.HashKey(ratelimit.ExtractAPIKey(c.Request)),
			Client:        client.Name,
			ClientVersion: client.Version,
			SessionID:     client.SessionID,
			CWD:           cwd,
			Prompt:        joinPromptText(blocks),
			Blocks:        blocks,
			BodyTruncated: peek.Truncated,
		}

		c.Next()

		entry.Status = c.Writer.Status()
		writer.Submit(entry)
	}
}
