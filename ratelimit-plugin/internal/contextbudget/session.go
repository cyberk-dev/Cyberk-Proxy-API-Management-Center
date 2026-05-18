package contextbudget

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// SessionSource records how a session key was derived. It's surfaced in
// debug logs so operators can tell apart "we trusted the client's header"
// from "we fingerprinted the body" — useful when chasing why two requests
// did or did not collide in the tracker.
type SessionSource int

const (
	SessionFromUnknown SessionSource = iota
	SessionFromHeader
	SessionFromBodyHash
)

func (s SessionSource) String() string {
	switch s {
	case SessionFromHeader:
		return "header"
	case SessionFromBodyHash:
		return "body_hash"
	default:
		return "unknown"
	}
}

// SessionKey is the composite identifier the tracker uses to look up prior
// token counts. The API-key hash is mixed in so two distinct users running
// the same client (and therefore producing identical session IDs by
// coincidence) can't accidentally read each other's history.
type SessionKey struct {
	APIKeyHash string
	ID         string
	Source     SessionSource
}

// String returns a stable string form of the key, suitable for use as a
// map index. Empty IDs map to "" so callers can range-check before use.
func (k SessionKey) String() string {
	if k.ID == "" {
		return ""
	}
	if k.APIKeyHash == "" {
		return k.Source.String() + ":" + k.ID
	}
	return k.APIKeyHash + ":" + k.Source.String() + ":" + k.ID
}

// Common header names we look up. HTTP header names are case-insensitive
// but Go's net/http canonicalizes them by capitalizing the first letter and
// the first letter after each HYPHEN — underscores are NOT separators. So
// `Session_id`/`session_id`/`SESSION_ID` all canonicalize to `Session_id`,
// while `Session-Id`/`session-id` canonicalize to `Session-Id` (a different
// map key entirely). We list both forms for any client that might send
// either.
var sessionHeaderNames = []string{
	"X-Claude-Code-Session-Id", // Claude Code (>= 2.1.97)
	"X-Amp-Thread-Id",          // Amp
	"Session_id",               // opencode + Codex CLI (rust wire form)
	"Conversation_id",          // Codex CLI alt header (same UUID as session_id)
	"Session-Id",               // hyphen variant (Codex CLI #11732 envoy-compat)
	"X-Session-Id",             // generic
	"X-Conversation-Id",        // generic future-proof
}

// ExtractSession derives a SessionKey from the request, preferring a
// well-known client-supplied header. When no header is present, it falls
// back to a body-content fingerprint so untagged clients (Codex CLI,
// Gemini CLI, raw SDK callers) still get stable session grouping across
// turns of the same conversation.
//
// The body fingerprint hashes only the FIRST user-authored message in
// each protocol's natural shape; that prefix stays byte-identical as
// the conversation grows, so subsequent turns hash to the same value.
func ExtractSession(r *http.Request, body []byte, protocol Protocol) SessionKey {
	keyHash := hashAPIKey(extractAPIKey(r))
	if id := headerSessionID(r); id != "" {
		return SessionKey{APIKeyHash: keyHash, ID: id, Source: SessionFromHeader}
	}
	if id := bodyFingerprint(body, protocol); id != "" {
		return SessionKey{APIKeyHash: keyHash, ID: id, Source: SessionFromBodyHash}
	}
	return SessionKey{APIKeyHash: keyHash}
}

func headerSessionID(r *http.Request) string {
	if r == nil || r.Header == nil {
		return ""
	}
	for _, name := range sessionHeaderNames {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// bodyFingerprint hashes the first user-authored message in protocol-aware
// form. We deliberately seek the first entry with role=="user", skipping
// any developer/system/assistant prelude — clients like opencode emit a
// long fixed `developer` system block as messages[0]/input[0], which is
// identical across distinct conversations from the same client version.
// Hashing that block would collide every conversation from that user.
//
// Because the user's first turn never moves (multi-turn conversations
// append new entries), the resulting fingerprint stays stable across all
// subsequent turns of the same conversation.
func bodyFingerprint(body []byte, p Protocol) string {
	if len(body) == 0 {
		return ""
	}
	var seed string
	switch p {
	case ProtoClaude, ProtoOpenAIChat:
		seed = firstUserSeed(gjson.GetBytes(body, "messages"))
	case ProtoOpenAIResponses:
		inp := gjson.GetBytes(body, "input")
		switch {
		case inp.Type == gjson.String:
			seed = "input\x00" + inp.String()
		case inp.IsArray():
			seed = firstUserSeed(inp)
		}
		// Fall back to instructions when input has no user-authored content
		// (e.g. continuation requests that arrive with only assistant/tool
		// state). The instructions block is conversation-scoped for Codex
		// CLI so this still collides correctly within a session.
		if seed == "" {
			if instr := gjson.GetBytes(body, "instructions").String(); instr != "" {
				seed = "instructions\x00" + instr
			}
		}
	case ProtoGemini:
		seed = firstUserSeedGemini(gjson.GetBytes(body, "contents"))
		if seed == "" {
			// systemInstruction as last resort — stable for the duration of
			// a given Gemini CLI session.
			if sys := gjson.GetBytes(body, "systemInstruction.parts.0.text").String(); sys != "" {
				seed = "sys\x00" + sys
			}
		}
	}
	if strings.TrimSpace(seed) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	// 12 bytes (24 hex chars) ≈ 96 bits; birthday collision below 2^-48
	// at a tracker capped at 4096 entries — plenty for an in-memory map.
	return hex.EncodeToString(sum[:12])
}

// firstUserSeed walks a Claude/OpenAI-style messages array for the first
// entry whose role is "user" and seeds it as "<role>\x00<text>". Returns
// "" if no user entry is present.
func firstUserSeed(arr gjson.Result) string {
	if !arr.IsArray() {
		return ""
	}
	for _, m := range arr.Array() {
		if m.Get("role").String() != "user" {
			continue
		}
		text := firstText(m.Get("content"))
		if text == "" {
			continue
		}
		return "user\x00" + text
	}
	return ""
}

// firstUserSeedGemini walks Gemini's `contents` array for the first entry
// whose role is "user". Gemini uses role="user" for the human side and
// role="model" for the assistant; system instructions live at the
// top-level `systemInstruction` field instead of being a contents[] entry.
func firstUserSeedGemini(arr gjson.Result) string {
	if !arr.IsArray() {
		return ""
	}
	for _, c := range arr.Array() {
		if c.Get("role").String() != "user" {
			continue
		}
		// Concatenate text from all parts so multi-part user turns
		// (text + inline image, etc.) still produce a stable seed; we
		// skip non-text parts because their bytes are typically large
		// base64 blobs that vary in encoding.
		var sb strings.Builder
		for _, part := range c.Get("parts").Array() {
			if t := part.Get("text").String(); t != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\x01')
				}
				sb.WriteString(t)
			}
		}
		if sb.Len() == 0 {
			continue
		}
		return "user\x00" + sb.String()
	}
	return ""
}

// firstText extracts the first textual content from either a string or an
// array of content blocks. Handles the three known block-text fields:
//   - `text` — Claude/OpenAI plain text block
//   - `input_text` — OpenAI Responses input
//   - `content` — Claude tool_result wrapper (recurses since content can
//     itself be a string or an array of blocks)
//
// Returns "" if no textual content is found.
func firstText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	for _, block := range content.Array() {
		if t := block.Get("text"); t.Exists() && t.Type == gjson.String && t.String() != "" {
			return t.String()
		}
		if t := block.Get("input_text"); t.Exists() && t.Type == gjson.String && t.String() != "" {
			return t.String()
		}
		// tool_result blocks wrap their textual content under `content`,
		// which itself may be a string or another array of blocks.
		if inner := block.Get("content"); inner.Exists() {
			if t := firstText(inner); t != "" {
				return t
			}
		}
	}
	return ""
}

// extractAPIKey mirrors ratelimit.ExtractAPIKey but is inlined to keep
// this file dependency-free from the ratelimit package — the headers we
// want are a small known set and inlining avoids a circular import in
// case ratelimit later needs to depend on contextbudget for some reason.
func extractAPIKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		parts := strings.SplitN(h, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return strings.TrimSpace(parts[1])
		}
		return h
	}
	if h := strings.TrimSpace(r.Header.Get("X-Goog-Api-Key")); h != "" {
		return h
	}
	if h := strings.TrimSpace(r.Header.Get("X-Api-Key")); h != "" {
		return h
	}
	if r.URL != nil {
		if k := r.URL.Query().Get("key"); k != "" {
			return k
		}
	}
	return ""
}

func hashAPIKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}
