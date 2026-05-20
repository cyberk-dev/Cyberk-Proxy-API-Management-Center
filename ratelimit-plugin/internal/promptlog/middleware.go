package promptlog

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

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

		sysText := extractSystemText(peek.Body, provider)
		cwd := extractCWD(sysText)
		res := identifyRequest(c.Request.Header, sysHead(sysText, 256))
		client := res.Client

		// Title-gen is pure infrastructure noise (one-shot model call that
		// summarizes the user message into a thread title — gpt-5-nano on
		// opencode, claude-haiku-shape on Claude Code). It carries no
		// conversation value and would otherwise clutter the Prompts UI
		// with one extra "(unknown)" session card per turn. Subagent
		// dispatches (KindSubagent) used to be dropped here too, but are
		// now kept and recorded with AgentID / ParentSessionID so the
		// reader can render them indented under their dispatching parent.
		// Stays a full early-return: we don't want assistant entries from
		// title-gen calls surfacing in the UI either.
		if res.Kind == KindTitleGen {
			c.Next()
			return
		}

		keyHash := ratelimit.HashKey(ratelimit.ExtractAPIKey(c.Request))
		model := ratelimit.ExtractModel(c)

		// Whether the user message in this request carries fresh human
		// content. False on agent-loop continuations whose last user-role
		// message is just tool_result references — those are logged on the
		// turn where the human prompt first appeared, and re-logging them
		// every loop iteration would inflate entry count without adding
		// new prompt information. The flag gates user-side submission
		// only; the assistant response is captured either way so the
		// later turns of a tool loop still surface in the UI.
		hasUserContent := len(blocks) > 0 && !isToolOnly(blocks)

		var entry *Entry
		if hasUserContent {
			entry = &Entry{
				Timestamp:       time.Now().UTC(),
				Provider:        provider,
				Path:            path,
				Role:            "user",
				Model:           model,
				KeyHash:         keyHash,
				Client:          client.Name,
				ClientVersion:   client.Version,
				SessionID:       client.SessionID,
				CWD:             cwd,
				AgentID:         res.AgentID,
				ParentSessionID: res.ParentSessionID,
				Prompt:          joinPromptText(blocks),
				Blocks:          blocks,
				BodyTruncated:   peek.Truncated,
			}
		}

		// Install the response capturer BEFORE c.Next() so the wrapper sees
		// every Write the downstream handler emits. Skipped entirely when
		// assistant logging is off: avoids any allocation on the hot path.
		// Installed regardless of `hasUserContent` so tool-loop continuation
		// turns (where the user message is just tool_result) still capture
		// the assistant's tool_use / final text reply.
		var capturer *responseCapturer
		if cfg.LogAssistantResponse && cfg.MaxResponseBytes > 0 {
			capturer = newResponseCapturer(c.Writer, cfg.MaxResponseBytes)
			c.Writer = capturer
		}

		c.Next()

		status := c.Writer.Status()
		if entry != nil {
			entry.Status = status
			writer.Submit(entry)
		}

		// Only build an assistant entry when the response is plausibly a
		// model reply. 2xx bodies cover both streaming SSE (status 200, body
		// is the event stream) and non-streaming JSON. Errors carry no
		// useful assistant content for offline analysis.
		if capturer == nil || status < 200 || status >= 300 {
			return
		}
		respBlocks := parseAssistantResponse(capturer.Body(), provider, cfg.MaxTextBytes)
		if len(respBlocks) == 0 {
			// Body had bytes but the per-provider parser couldn't extract
			// anything. Most often: cap hit before the first usable event,
			// or upstream emitted an event type we don't yet handle.
			// Log a single line (head only) so future debugging can match
			// drops against novel stream shapes.
			if body := capturer.Body(); len(body) > 0 {
				head := body
				if len(head) > 256 {
					head = head[:256]
				}
				log.Warnf("promptlog: assistant parse produced 0 blocks (provider=%s len=%d truncated=%v head=%q)",
					provider, len(body), capturer.Truncated(), head)
			}
			return
		}
		writer.Submit(&Entry{
			Timestamp:       time.Now().UTC(),
			Provider:        provider,
			Path:            path,
			Status:          status,
			Role:            "assistant",
			Model:           model,
			KeyHash:         keyHash,
			Client:          client.Name,
			ClientVersion:   client.Version,
			SessionID:       client.SessionID,
			CWD:             cwd,
			AgentID:         res.AgentID,
			ParentSessionID: res.ParentSessionID,
			Prompt:          joinPromptText(respBlocks),
			Blocks:          respBlocks,
			BodyTruncated:   capturer.Truncated(),
		})
	}
}

// sysHead returns a normalized prefix of system text for detector matching:
// lowercased + capped at maxLen, with a leading Claude-Code-style
// "x-anthropic-billing-header" preamble stripped when present. Detectors
// rely on this so they can do plain HasPrefix checks against title-gen
// fingerprints (always lowercase here) without caring about wrapper
// billing-headers or letter case in the wire prompt.
//
// The preamble strip is gated on "x-anthropic-billing-header" actually
// appearing in the first scan window — otherwise a user-authored system
// prompt with a blank line in the first paragraph could lose that
// paragraph silently. Scan window must be larger than maxLen so the
// gating works for any preamble that fits within it.
func sysHead(systemText string, maxLen int) string {
	if systemText == "" {
		return ""
	}
	const scanWindow = 1024
	head := systemText
	if len(head) > scanWindow {
		head = head[:scanWindow]
	}
	// Strip the Claude Code billing-header preamble only when its marker is
	// actually present in the scan window. The marker terminates with a
	// blank line; jump past that. Other system prompts pass through whole.
	if strings.Contains(head, "x-anthropic-billing-header") {
		if idx := strings.Index(head, "\n\n"); idx >= 0 {
			head = head[idx+2:]
		}
	}
	head = strings.TrimLeft(head, " \t\r\n")
	if len(head) > maxLen {
		head = head[:maxLen]
	}
	return strings.ToLower(head)
}
