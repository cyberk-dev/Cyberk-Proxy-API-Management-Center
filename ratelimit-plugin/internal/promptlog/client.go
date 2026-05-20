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

// Kind tags the role this request plays in the agent loop. Defaults to
// KindParent — every request is a normal user turn unless a detector
// recognizes a specific sub-call signal.
//
// Adding a new kind = one constant + one switch case in the detector that
// observes the signal. The middleware switches on Kind exactly once and
// never branches on Client.Name, so the matrix stays inside detectors.
type Kind int

const (
	// KindParent is the default — a top-level user turn. Clients without
	// subagent / title-gen signals land here.
	KindParent Kind = iota
	// KindSubagent is a Task-tool dispatch (Claude Code) or child session
	// (opencode). Recorded with AgentID / ParentSessionID populated so the
	// reader can group it under its parent at render time.
	KindSubagent
	// KindTitleGen is the synthetic title-generation sub-call that some
	// CLIs run in parallel with each turn. Pure noise — dropped at the
	// middleware layer.
	KindTitleGen
)

// detectResult is the return shape of every detectFn. It bundles the legacy
// Client metadata with the routing kind and the per-kind extras (AgentID
// for Claude Code, ParentSessionID for opencode). Detectors that observe
// no sub-call signals return Kind: KindParent and leave the extras at zero
// values — Entry's omitempty tags then keep JSONL lines byte-identical to
// what the writer produced before this change for the parent-only path.
type detectResult struct {
	Client          Client
	Kind            Kind
	AgentID         string // Claude Code: X-Claude-Code-Agent-Id (subagent only)
	ParentSessionID string // opencode: X-Parent-Session-Id (subagent only)
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

// detectFn examines headers + a normalized prefix of the system prompt
// (sysHead) and returns a structured detectResult on match.
//
// sysHead contract — the middleware always passes a value that has been:
//   - lowercased (so detectors compare against lowercase literals)
//   - capped at ~256 bytes
//   - had any leading Claude Code "x-anthropic-billing-header" preamble
//     stripped (see middleware.sysHead)
//
// sysHead is only consulted by detectors that need it (Claude Code +
// opencode for title-gen); the rest ignore it. Callers that don't yet
// have the body (e.g. early header-only flows) pass "" — kind detection
// then degrades gracefully to KindParent, never to KindTitleGen.
type detectFn func(h http.Header, sysHead string) (detectResult, bool)

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

// identifyRequest runs the detector chain and returns the full
// detectResult (client + kind + sub-call extras). This is the entry point
// the promptlog middleware uses; the public IdentifyClient below is the
// thinner wrapper kept for external callers (contextbudget) that only
// care about the client identity.
//
// SessionID resolution is two-tier: detectors set it from a client-specific
// header when available (Amp's X-Amp-Thread-Id, Claude Code's
// X-Claude-Code-Session-Id), and any remaining gap is filled by the shared
// contextbudget.HeaderSessionID lookup. The shared fallback covers clients
// whose detection signal is the User-Agent only (opencode 1.15+, raw SDK
// host apps that opt into Session_id / X-Session-Id), and prevents future
// drift between the rate-limit tracker and the prompt-log UI bucketing.
func identifyRequest(h http.Header, sysHead string) detectResult {
	for _, d := range detectors {
		if r, ok := d(h, sysHead); ok {
			if r.Client.SessionID == "" {
				r.Client.SessionID = contextbudget.HeaderSessionID(h)
			}
			return r
		}
	}
	return detectResult{
		Client: Client{Name: ClientUnknown, SessionID: contextbudget.HeaderSessionID(h)},
		Kind:   KindParent,
	}
}

// IdentifyClient is the legacy entry point kept for callers that only need
// the Client identity (contextbudget, etc.). New promptlog code should call
// identifyRequest to get Kind / AgentID / ParentSessionID as well.
func IdentifyClient(h http.Header) Client {
	return identifyRequest(h, "").Client
}

// detectAmp keys off the X-Amp-* sidecar headers because Amp's User-Agent is
// minified ("Ap/JS 0.74.0", "HK/JS 0.71.2", etc.) and would otherwise collide
// with random JS clients. X-Amp-Thread-Id is the session identifier. Amp
// currently exposes no subagent / title-gen signals — request always lands
// in KindParent.
func detectAmp(h http.Header, _ string) (detectResult, bool) {
	threadID := h.Get("X-Amp-Thread-Id")
	if threadID == "" && h.Get("X-Amp-Client-Type") == "" && h.Get("X-Amp-Client-Application") == "" {
		return detectResult{}, false
	}
	return detectResult{
		Client: Client{
			Name:      ClientAmp,
			Version:   h.Get("X-Amp-Client-Version"),
			SessionID: threadID,
		},
		Kind: KindParent,
	}, true
}

// detectClaudeCode matches both the terminal CLI and the VSCode extension —
// they share the "claude-cli/X.Y.Z (external, ...)" UA. The Session-Id
// header is absent on older versions (< 2.1.97); the resulting empty
// SessionID is accepted and just means we can't group those entries.
//
// Sub-call signals (in priority order):
//   - X-Claude-Code-Agent-Id present → KindSubagent (Task-tool dispatch).
//     Stable across all turns of the same subagent instance — verified
//     against a 28-turn run in the user's demo logs.
//   - sysHead starts with "generate a concise" → KindTitleGen (synthetic
//     title-generation sub-call that runs in parallel to every new
//     conversation turn). Drops at the middleware layer.
func detectClaudeCode(h http.Header, sysHead string) (detectResult, bool) {
	ua := h.Get("User-Agent")
	if !strings.HasPrefix(ua, "claude-cli/") {
		return detectResult{}, false
	}
	r := detectResult{
		Client: Client{
			Name:      ClientClaudeCode,
			Version:   versionFromUA(ua, "claude-cli/"),
			SessionID: h.Get("X-Claude-Code-Session-Id"),
		},
		Kind: KindParent,
	}
	switch {
	case h.Get("X-Claude-Code-Agent-Id") != "":
		r.Kind = KindSubagent
		r.AgentID = h.Get("X-Claude-Code-Agent-Id")
	case strings.HasPrefix(sysHead, "generate a concise"):
		r.Kind = KindTitleGen
	}
	return r, true
}

// detectOpencode matches the opencode CLI. SessionID is filled by the shared
// identifyRequest fallback. Opencode 1.15+ sends `Session_id` (and
// `X-Session-Affinity` as the upstream affinity hint); older versions send
// neither, in which case SessionID stays empty.
//
// Sub-call signals:
//   - X-Parent-Session-Id present → KindSubagent. Subagent has its own
//     Session_id (different from parent); ParentSessionID points to the
//     spawning session. The reader merges the subagent's messages back
//     into the parent session card at render time.
//   - sysHead starts with "you are a title generator" → KindTitleGen
//     (gpt-5-nano sub-call per conversation turn).
func detectOpencode(h http.Header, sysHead string) (detectResult, bool) {
	ua := h.Get("User-Agent")
	if !strings.HasPrefix(ua, "opencode/") {
		return detectResult{}, false
	}
	r := detectResult{
		Client: Client{
			Name:    ClientOpencode,
			Version: versionFromUA(ua, "opencode/"),
		},
		Kind: KindParent,
	}
	switch {
	case h.Get("X-Parent-Session-Id") != "":
		r.Kind = KindSubagent
		r.ParentSessionID = h.Get("X-Parent-Session-Id")
	case strings.HasPrefix(sysHead, "you are a title generator"):
		r.Kind = KindTitleGen
	}
	return r, true
}

// detectAISDK catches the Vercel AI SDK and its provider sub-packages. The
// canonical UA looks like "ai/6.0.79 ai-sdk/provider-utils/4.0.15 ..." or
// "ai-sdk/openai/3.0.53 ...". We do NOT try to subtype here — host apps using
// the AI SDK rarely send a session header, so version is the only useful bit.
// No subagent / title-gen signals exposed → always KindParent.
func detectAISDK(h http.Header, _ string) (detectResult, bool) {
	ua := h.Get("User-Agent")
	switch {
	case strings.HasPrefix(ua, "ai/"):
		return detectResult{Client: Client{Name: ClientAISDK, Version: versionFromUA(ua, "ai/")}, Kind: KindParent}, true
	case strings.HasPrefix(ua, "ai-sdk/"):
		return detectResult{Client: Client{Name: ClientAISDK, Version: firstToken(strings.TrimPrefix(ua, "ai-sdk/"))}, Kind: KindParent}, true
	}
	return detectResult{}, false
}

func detectOpenAISDK(h http.Header, _ string) (detectResult, bool) {
	ua := h.Get("User-Agent")
	for _, p := range []string{"OpenAI/Python", "AsyncOpenAI/Python", "OpenAI/JS"} {
		if strings.HasPrefix(ua, p) {
			return detectResult{Client: Client{Name: ClientOpenAISDK, Version: firstToken(strings.TrimPrefix(ua, p+" "))}, Kind: KindParent}, true
		}
	}
	return detectResult{}, false
}

func detectGoogleSDK(h http.Header, _ string) (detectResult, bool) {
	ua := h.Get("User-Agent")
	if strings.HasPrefix(ua, "google-genai-sdk/") {
		return detectResult{Client: Client{Name: ClientGoogleSDK, Version: versionFromUA(ua, "google-genai-sdk/")}, Kind: KindParent}, true
	}
	return detectResult{}, false
}

func detectLiteLLM(h http.Header, _ string) (detectResult, bool) {
	ua := h.Get("User-Agent")
	if strings.HasPrefix(ua, "litellm/") {
		return detectResult{Client: Client{Name: ClientLiteLLM, Version: versionFromUA(ua, "litellm/")}, Kind: KindParent}, true
	}
	return detectResult{}, false
}

func detectCurl(h http.Header, _ string) (detectResult, bool) {
	ua := h.Get("User-Agent")
	if strings.HasPrefix(ua, "curl/") {
		return detectResult{Client: Client{Name: ClientCurl, Version: versionFromUA(ua, "curl/")}, Kind: KindParent}, true
	}
	return detectResult{}, false
}

// detectGeneric catches raw runtime UAs (node, Bun/X.Y.Z, Go-http-client) that
// almost always indicate a hand-rolled HTTP call with no app context.
func detectGeneric(h http.Header, _ string) (detectResult, bool) {
	ua := h.Get("User-Agent")
	if ua == "" {
		return detectResult{}, false
	}
	switch {
	case ua == "node":
		return detectResult{Client: Client{Name: ClientGeneric, Version: "node"}, Kind: KindParent}, true
	case strings.HasPrefix(ua, "Bun/"):
		return detectResult{Client: Client{Name: ClientGeneric, Version: "bun/" + versionFromUA(ua, "Bun/")}, Kind: KindParent}, true
	case strings.HasPrefix(ua, "Go-http-client/"):
		return detectResult{Client: Client{Name: ClientGeneric, Version: "go/" + versionFromUA(ua, "Go-http-client/")}, Kind: KindParent}, true
	}
	return detectResult{}, false
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
