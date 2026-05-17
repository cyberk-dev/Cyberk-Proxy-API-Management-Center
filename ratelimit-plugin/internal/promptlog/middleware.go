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
		if len(blocks) == 0 || isToolOnly(blocks) {
			// Nothing human-authored: empty extraction (no user-role message,
			// wrapper-noise only) or pure agent-loop continuation whose last
			// user message is just tool_use / tool_result references. Logging
			// the latter would balloon entry count without adding new prompt
			// content (the assistant turn that issued the tool call is not in
			// this request's user-role content).
			c.Next()
			return
		}

		client := IdentifyClient(c.Request.Header)
		cwd := extractCWD(extractSystemText(peek.Body, provider))

		// Claude Code subagents (Task tool dispatches: web search, Explore,
		// Plan, etc.) reuse the parent's UA + session id but ship their own
		// system prompt without the env block — so cwd extraction returns "".
		// They contain no human-typed content, just the dispatcher's framing
		// like "Perform a web search for the query: ...". Drop them with the
		// same rationale as the synthetic-CLI prefix list (see extract.go).
		if client.Name == ClientClaudeCode && cwd == "" {
			c.Next()
			return
		}

		keyHash := ratelimit.HashKey(ratelimit.ExtractAPIKey(c.Request))
		model := ratelimit.ExtractModel(c)

		entry := &Entry{
			Timestamp:     time.Now().UTC(),
			Provider:      provider,
			Path:          path,
			Role:          "user",
			Model:         model,
			KeyHash:       keyHash,
			Client:        client.Name,
			ClientVersion: client.Version,
			SessionID:     client.SessionID,
			CWD:           cwd,
			Prompt:        joinPromptText(blocks),
			Blocks:        blocks,
			BodyTruncated: peek.Truncated,
		}

		// Install the response capturer BEFORE c.Next() so the wrapper sees
		// every Write the downstream handler emits. Skipped entirely when
		// assistant logging is off: avoids any allocation on the hot path.
		var capturer *responseCapturer
		if cfg.LogAssistantResponse && cfg.MaxResponseBytes > 0 {
			capturer = newResponseCapturer(c.Writer, cfg.MaxResponseBytes)
			c.Writer = capturer
		}

		c.Next()

		status := c.Writer.Status()
		entry.Status = status
		writer.Submit(entry)

		// Only build an assistant entry when the response is plausibly a
		// model reply. 2xx bodies cover both streaming SSE (status 200, body
		// is the event stream) and non-streaming JSON. Errors carry no
		// useful assistant content for offline analysis.
		if capturer == nil || status < 200 || status >= 300 {
			return
		}
		respBlocks := parseAssistantResponse(capturer.Body(), provider, cfg.MaxTextBytes)
		if len(respBlocks) == 0 {
			return
		}
		writer.Submit(&Entry{
			Timestamp:     time.Now().UTC(),
			Provider:      provider,
			Path:          path,
			Status:        status,
			Role:          "assistant",
			Model:         model,
			KeyHash:       keyHash,
			Client:        client.Name,
			ClientVersion: client.Version,
			SessionID:     client.SessionID,
			CWD:           cwd,
			Prompt:        joinPromptText(respBlocks),
			Blocks:        respBlocks,
			BodyTruncated: capturer.Truncated(),
		})
	}
}
