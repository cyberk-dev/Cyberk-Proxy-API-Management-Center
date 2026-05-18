package contextbudget

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// blockMessage is the human-readable string surfaced in every error envelope
// (JSON and SSE). Operators tail the proxy log for this prefix to count hard
// blocks; clients display it verbatim to the user.
const blockMessagePrefix = "Request exceeds proxy context budget"

// RespondHardBlock writes the appropriate hard-block response and aborts the
// request. When streaming is true, the body is emitted as a single SSE event
// in the protocol's native error shape; otherwise it's a 413 JSON envelope.
//
// The function never returns an error — Gin handles flush failures internally
// and there's nothing useful for the middleware to do beyond logging at the
// call site if a write fails.
func RespondHardBlock(c *gin.Context, p Protocol, used, hard int, streaming bool) {
	msg := fmt.Sprintf("%s (%d tokens estimated, limit %d). Compact the conversation or start a new session before retrying.",
		blockMessagePrefix, used, hard)

	if streaming {
		respondStreamingError(c, p, msg, used, hard)
		return
	}
	respondJSONError(c, p, msg, used, hard)
}

func respondJSONError(c *gin.Context, p Protocol, msg string, used, hard int) {
	// Use each protocol's canonical error envelope so SDK clients parse it
	// without custom handling.
	switch p {
	case ProtoClaude:
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "request_too_large",
				"message": msg,
			},
			"context_budget": gin.H{
				"used_tokens_estimate": used,
				"hard_limit_tokens":    hard,
			},
		})
	case ProtoGemini:
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{
				"code":    http.StatusRequestEntityTooLarge,
				"status":  "RESOURCE_EXHAUSTED",
				"message": msg,
			},
			"context_budget": gin.H{
				"used_tokens_estimate": used,
				"hard_limit_tokens":    hard,
			},
		})
	default:
		// OpenAI Chat / Responses share the same error envelope shape.
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"code":    "context_length_exceeded",
				"message": msg,
				"param":   nil,
			},
			"context_budget": gin.H{
				"used_tokens_estimate": used,
				"hard_limit_tokens":    hard,
			},
		})
	}
}

// respondStreamingError emits one SSE event in the protocol's native format
// and aborts. We deliberately do not stream a fake start/end pair — most
// SDKs treat a lone error event with no preceding content as a clean
// terminal failure.
func respondStreamingError(c *gin.Context, p Protocol, msg string, used, hard int) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusRequestEntityTooLarge)

	w := c.Writer
	switch p {
	case ProtoClaude:
		payload := map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "request_too_large",
				"message": msg,
			},
		}
		writeSSEEvent(w, "error", payload)
	case ProtoGemini:
		payload := map[string]any{
			"error": map[string]any{
				"code":    http.StatusRequestEntityTooLarge,
				"status":  "RESOURCE_EXHAUSTED",
				"message": msg,
			},
		}
		writeSSEData(w, payload)
	default:
		// OpenAI Chat / Responses: error envelope followed by [DONE].
		payload := map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"code":    "context_length_exceeded",
				"message": msg,
			},
		}
		writeSSEData(w, payload)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_ = used
	_ = hard
	c.Abort()
}

func writeSSEEvent(w http.ResponseWriter, eventName string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, b)
}

func writeSSEData(w http.ResponseWriter, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}
