package promptlog

import (
	"net/http"
	"strings"

	"github.com/cyberk/ratelimit-plugin/internal/contextbudget"
)

// Client is the metadata we extract from request headers to identify which
// developer tool / SDK / proxy generated the request. The identity is purely
// for log enrichment and session grouping — body parsing is provider-driven
// (see extract.go) and never branches on Client.
type Client struct {
	Name      string
	Version   string
	SessionID string
}

const (
	ClientClaudeCode = "claude_code"
	ClientAmp        = "amp"
	ClientOpencode   = "opencode"
	ClientAISDK      = "ai_sdk"
	ClientOpenAISDK  = "openai_sdk"
	ClientGoogleSDK  = "google_sdk"
	ClientLiteLLM    = "litellm"
	ClientCurl       = "curl"
	ClientGeneric    = "generic"
	ClientUnknown    = "unknown"
)

type detectFn func(http.Header) (Client, bool)

// detectors run in order. The first match wins, so place specific checks
// (Amp's auxiliary headers, vendor-prefixed UAs) before generic ones (raw
// "node" or "Bun/..." which any HTTP client could send).
var detectors = []detectFn{
	detectAmp,
	detectClaudeCode,
	detectOpencode,
	detectAISDK,
	detectOpenAISDK,
	detectGoogleSDK,
	detectLiteLLM,
	detectCurl,
	detectGeneric,
}

// IdentifyClient runs the detector chain. Always returns a non-empty Name —
// unmatched requests get ClientUnknown so analysts can still group "other
// traffic" without losing entries.
//
// SessionID resolution is two-tier: detectors set it from a client-specific
// header when available (Amp's X-Amp-Thread-Id, Claude Code's
// X-Claude-Code-Session-Id), and any remaining gap is filled by the shared
// contextbudget.HeaderSessionID lookup. The shared fallback covers clients
// whose detection signal is the User-Agent only (opencode 1.15+, raw SDK
// host apps that opt into Session_id / X-Session-Id), and prevents future
// drift between the rate-limit tracker and the prompt-log UI bucketing.
func IdentifyClient(h http.Header) Client {
	for _, d := range detectors {
		if c, ok := d(h); ok {
			if c.SessionID == "" {
				c.SessionID = contextbudget.HeaderSessionID(h)
			}
			return c
		}
	}
	return Client{Name: ClientUnknown, SessionID: contextbudget.HeaderSessionID(h)}
}

// detectAmp keys off the X-Amp-* sidecar headers because Amp's User-Agent is
// minified ("Ap/JS 0.74.0", "HK/JS 0.71.2", etc.) and would otherwise collide
// with random JS clients. X-Amp-Thread-Id is the session identifier.
func detectAmp(h http.Header) (Client, bool) {
	threadID := h.Get("X-Amp-Thread-Id")
	if threadID == "" && h.Get("X-Amp-Client-Type") == "" && h.Get("X-Amp-Client-Application") == "" {
		return Client{}, false
	}
	return Client{
		Name:      ClientAmp,
		Version:   h.Get("X-Amp-Client-Version"),
		SessionID: threadID,
	}, true
}

// detectClaudeCode matches both the terminal CLI and the VSCode extension —
// they share the "claude-cli/X.Y.Z (external, ...)" UA. The Session-Id header
// is absent on older versions (< 2.1.97); the resulting empty SessionID is
// accepted and just means we can't group those entries.
func detectClaudeCode(h http.Header) (Client, bool) {
	ua := h.Get("User-Agent")
	if !strings.HasPrefix(ua, "claude-cli/") {
		return Client{}, false
	}
	return Client{
		Name:      ClientClaudeCode,
		Version:   versionFromUA(ua, "claude-cli/"),
		SessionID: h.Get("X-Claude-Code-Session-Id"),
	}, true
}

func detectOpencode(h http.Header) (Client, bool) {
	ua := h.Get("User-Agent")
	if !strings.HasPrefix(ua, "opencode/") {
		return Client{}, false
	}
	// SessionID is filled by the shared IdentifyClient fallback. Opencode
	// 1.15+ sends `Session_id` (and `X-Session-Affinity` as the upstream
	// affinity hint, same value); older versions send neither, in which
	// case SessionID stays empty.
	return Client{
		Name:    ClientOpencode,
		Version: versionFromUA(ua, "opencode/"),
	}, true
}

// detectAISDK catches the Vercel AI SDK and its provider sub-packages. The
// canonical UA looks like "ai/6.0.79 ai-sdk/provider-utils/4.0.15 ..." or
// "ai-sdk/openai/3.0.53 ...". We do NOT try to subtype here — host apps using
// the AI SDK rarely send a session header, so version is the only useful bit.
func detectAISDK(h http.Header) (Client, bool) {
	ua := h.Get("User-Agent")
	switch {
	case strings.HasPrefix(ua, "ai/"):
		return Client{Name: ClientAISDK, Version: versionFromUA(ua, "ai/")}, true
	case strings.HasPrefix(ua, "ai-sdk/"):
		return Client{Name: ClientAISDK, Version: firstToken(strings.TrimPrefix(ua, "ai-sdk/"))}, true
	}
	return Client{}, false
}

func detectOpenAISDK(h http.Header) (Client, bool) {
	ua := h.Get("User-Agent")
	for _, p := range []string{"OpenAI/Python", "AsyncOpenAI/Python", "OpenAI/JS"} {
		if strings.HasPrefix(ua, p) {
			return Client{Name: ClientOpenAISDK, Version: firstToken(strings.TrimPrefix(ua, p+" "))}, true
		}
	}
	return Client{}, false
}

func detectGoogleSDK(h http.Header) (Client, bool) {
	ua := h.Get("User-Agent")
	if strings.HasPrefix(ua, "google-genai-sdk/") {
		return Client{Name: ClientGoogleSDK, Version: versionFromUA(ua, "google-genai-sdk/")}, true
	}
	return Client{}, false
}

func detectLiteLLM(h http.Header) (Client, bool) {
	ua := h.Get("User-Agent")
	if strings.HasPrefix(ua, "litellm/") {
		return Client{Name: ClientLiteLLM, Version: versionFromUA(ua, "litellm/")}, true
	}
	return Client{}, false
}

func detectCurl(h http.Header) (Client, bool) {
	ua := h.Get("User-Agent")
	if strings.HasPrefix(ua, "curl/") {
		return Client{Name: ClientCurl, Version: versionFromUA(ua, "curl/")}, true
	}
	return Client{}, false
}

// detectGeneric catches raw runtime UAs (node, Bun/X.Y.Z, Go-http-client) that
// almost always indicate a hand-rolled HTTP call with no app context.
func detectGeneric(h http.Header) (Client, bool) {
	ua := h.Get("User-Agent")
	if ua == "" {
		return Client{}, false
	}
	switch {
	case ua == "node":
		return Client{Name: ClientGeneric, Version: "node"}, true
	case strings.HasPrefix(ua, "Bun/"):
		return Client{Name: ClientGeneric, Version: "bun/" + versionFromUA(ua, "Bun/")}, true
	case strings.HasPrefix(ua, "Go-http-client/"):
		return Client{Name: ClientGeneric, Version: "go/" + versionFromUA(ua, "Go-http-client/")}, true
	}
	return Client{}, false
}

// versionFromUA returns the version token immediately after prefix, stripping
// trailing parens or extra UA components.
func versionFromUA(ua, prefix string) string {
	return firstToken(strings.TrimPrefix(ua, prefix))
}

// firstToken returns the first whitespace/paren-delimited token from s.
func firstToken(s string) string {
	for i, r := range s {
		if r == ' ' || r == '\t' || r == '(' {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}
