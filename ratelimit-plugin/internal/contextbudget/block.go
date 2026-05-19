package contextbudget

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// BudgetSeverity tells RespondOverflow which threshold was crossed so the
// envelope's wording matches: a soft cross is a polite "approaching limit,
// please compact"; a hard cross is a firm "must compact before continuing."
type BudgetSeverity int

const (
	BudgetSoft BudgetSeverity = iota
	BudgetHard
)

// RespondOverflow writes a uniform HTTP 400 envelope for either a soft- or
// hard-threshold cross and aborts the request. We deliberately use 400
// (not 413) for two reasons:
//
//  1. Anthropic and OpenAI both classify "request prompt too long" as a
//     400 invalid_request_error in their public APIs; 413 is technically
//     reserved for transport-level oversize bodies. Returning the
//     provider-canonical shape means the upstream SDK and any agentic
//     wrapper recognize it immediately.
//
//  2. We previously emitted 413 inside an SSE error chunk for streaming
//     requests, which Claude Code's agentic loop treated as a transient
//     connection failure and retried at ~3 req/sec for ~30 s. Empirically
//     a JSON 400 is non-retryable in that loop — the client surfaces the
//     message text to the user instead.
//
// For Anthropic, OpenAI Chat/Responses, and Gemini we use each provider's
// canonical 400 envelope so SDK error parsers pick it up without custom
// handling. The proxy-specific telemetry (used/hard/soft and a `compact_hint`
// flag) is added under a `context_budget` side-channel field that clients
// can ignore — no SDK we know of validates field whitelists on errors.
func RespondOverflow(c *gin.Context, p Protocol, severity BudgetSeverity, used, hard, soft int) {
	msg := overflowMessage(severity, used, hard, soft)

	telemetry := gin.H{
		"severity":             severityString(severity),
		"used_tokens_estimate": used,
		"hard_limit_tokens":    hard,
		"soft_limit_tokens":    soft,
		"compact_hint":         true,
	}

	switch p {
	case ProtoClaude:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": msg,
			},
			"context_budget": telemetry,
		})
	case ProtoGemini:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"code":    http.StatusBadRequest,
				"status":  "INVALID_ARGUMENT",
				"message": msg,
			},
			"context_budget": telemetry,
		})
	default:
		// OpenAI Chat / Responses (and Codex CLI which speaks Responses).
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"code":    "context_length_exceeded",
				"message": msg,
				"param":   nil,
			},
			"context_budget": telemetry,
		})
	}
}

// overflowMessage builds the human-readable string surfaced to the user.
// It must be self-contained — the client may show only this text and
// nothing from the structured envelope.
func overflowMessage(severity BudgetSeverity, used, hard, soft int) string {
	switch severity {
	case BudgetSoft:
		return fmt.Sprintf(
			"[Cyberk Context Guard] %d/%d tokens used (hard %d). "+
				"/compact NOW — at the hard limit, /compact itself will "+
				"fail and you'll have to /clear and start over.",
			used, soft, hard)
	case BudgetHard:
		fallthrough
	default:
		return fmt.Sprintf(
			"[Cyberk Context Guard] %d > %d hard limit. /clear required "+
				"— /compact cannot recover at this size, it sends the full "+
				"history upstream.",
			used, hard)
	}
}

func severityString(s BudgetSeverity) string {
	switch s {
	case BudgetSoft:
		return "soft"
	case BudgetHard:
		return "hard"
	default:
		return "unknown"
	}
}
