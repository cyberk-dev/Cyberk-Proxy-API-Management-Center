package promptlog

import (
	"net/http"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestIdentifyClient_ClaudeCode(t *testing.T) {
	c := IdentifyClient(hdr(
		"User-Agent", "claude-cli/2.1.141 (external, cli)",
		"X-Claude-Code-Session-Id", "d4dac6da-0bdd-4f7f-8d7d-84857a73be29",
	))
	if c.Name != ClientClaudeCode {
		t.Errorf("name=%q", c.Name)
	}
	if c.Version != "2.1.141" {
		t.Errorf("version=%q", c.Version)
	}
	if c.SessionID != "d4dac6da-0bdd-4f7f-8d7d-84857a73be29" {
		t.Errorf("session=%q", c.SessionID)
	}
}

func TestIdentifyClient_ClaudeCodeOldNoSession(t *testing.T) {
	c := IdentifyClient(hdr("User-Agent", "claude-cli/2.1.63 (external, cli)"))
	if c.Name != ClientClaudeCode {
		t.Errorf("name=%q", c.Name)
	}
	if c.SessionID != "" {
		t.Errorf("expected empty session for old version")
	}
}

func TestIdentifyClient_Amp_MinifiedUA(t *testing.T) {
	// Amp ships minified class names as the UA. Detection must rely on
	// X-Amp-* sidecar headers.
	c := IdentifyClient(hdr(
		"User-Agent", "Ap/JS 0.74.0",
		"X-Amp-Thread-Id", "T-019c9d5b-7f80-776c-b301-3e08a162b8ed",
		"X-Amp-Client-Version", "0.0.1772158255-g7676d5",
		"X-Amp-Client-Application", "VS Code CLI",
	))
	if c.Name != ClientAmp {
		t.Errorf("name=%q", c.Name)
	}
	if c.Version != "0.0.1772158255-g7676d5" {
		t.Errorf("version=%q", c.Version)
	}
	if c.SessionID != "T-019c9d5b-7f80-776c-b301-3e08a162b8ed" {
		t.Errorf("session=%q", c.SessionID)
	}
}

func TestIdentifyClient_Amp_OnlyClientHeader(t *testing.T) {
	// Some Amp requests omit Thread-Id but still have X-Amp-Client-Type.
	c := IdentifyClient(hdr(
		"User-Agent", "HK/JS 0.71.2",
		"X-Amp-Client-Type", "cli",
	))
	if c.Name != ClientAmp {
		t.Errorf("name=%q", c.Name)
	}
}

func TestIdentifyClient_Opencode(t *testing.T) {
	c := IdentifyClient(hdr("User-Agent", "opencode/1.14.41 (darwin 23.6.0; arm64) ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13"))
	if c.Name != ClientOpencode || c.Version != "1.14.41" {
		t.Errorf("got %+v", c)
	}
	if c.SessionID != "" {
		t.Errorf("old opencode without session header should have empty SessionID, got %q", c.SessionID)
	}
}

func TestIdentifyClient_Opencode_SessionIdHeader(t *testing.T) {
	// opencode 1.15+ sends `Session_id: ses_xxx`. Before the shared fallback
	// landed, this was dropped at the per-detector layer and every opencode
	// conversation collapsed into the "(no-session)" bucket in the UI.
	c := IdentifyClient(hdr(
		"User-Agent", "opencode/1.15.4 (darwin 25.4.0; arm64) ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13",
		"Session_id", "ses_1c42ebcccffeJx1dYBsGz4Lj8s",
	))
	if c.Name != ClientOpencode {
		t.Errorf("name=%q want=%q", c.Name, ClientOpencode)
	}
	if c.SessionID != "ses_1c42ebcccffeJx1dYBsGz4Lj8s" {
		t.Errorf("session=%q want=%q", c.SessionID, "ses_1c42ebcccffeJx1dYBsGz4Lj8s")
	}
}

func TestIdentifyClient_Unknown_PicksUpSessionHeader(t *testing.T) {
	// Generic / unknown clients that bother to send a session header should
	// still get bucketed correctly. This guards against the same drift that
	// historically affected opencode: a new client appears, the detector
	// chain falls through to ClientUnknown, but session bucketing should
	// still work via the shared header lookup.
	c := IdentifyClient(hdr(
		"User-Agent", "MyCustomAgent/1.0",
		"X-Session-Id", "custom-session-42",
	))
	if c.Name != ClientUnknown {
		t.Errorf("name=%q want=%q", c.Name, ClientUnknown)
	}
	if c.SessionID != "custom-session-42" {
		t.Errorf("session=%q want=%q", c.SessionID, "custom-session-42")
	}
}

func TestIdentifyClient_AISDK(t *testing.T) {
	cases := map[string]string{
		"ai/6.0.79 ai-sdk/provider-utils/4.0.15 runtime/bun/1.3.10":         "6.0.79",
		"ai-sdk/openai/3.0.53 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.5": "openai/3.0.53",
	}
	for ua, wantVer := range cases {
		c := IdentifyClient(hdr("User-Agent", ua))
		if c.Name != ClientAISDK {
			t.Errorf("ua=%q name=%q", ua, c.Name)
		}
		if c.Version != wantVer {
			t.Errorf("ua=%q version=%q want=%q", ua, c.Version, wantVer)
		}
	}
}

func TestIdentifyClient_OpenAISDK(t *testing.T) {
	c := IdentifyClient(hdr("User-Agent", "AsyncOpenAI/Python 2.24.0"))
	if c.Name != ClientOpenAISDK || c.Version != "2.24.0" {
		t.Errorf("got %+v", c)
	}
}

func TestIdentifyClient_GoogleSDK(t *testing.T) {
	c := IdentifyClient(hdr("User-Agent", "google-genai-sdk/1.40.0 gl-node/v24.3.0"))
	if c.Name != ClientGoogleSDK || c.Version != "1.40.0" {
		t.Errorf("got %+v", c)
	}
}

func TestIdentifyClient_LiteLLM(t *testing.T) {
	c := IdentifyClient(hdr("User-Agent", "litellm/1.82.3"))
	if c.Name != ClientLiteLLM {
		t.Errorf("got %+v", c)
	}
}

func TestIdentifyClient_Curl(t *testing.T) {
	c := IdentifyClient(hdr("User-Agent", "curl/8.7.1"))
	if c.Name != ClientCurl || c.Version != "8.7.1" {
		t.Errorf("got %+v", c)
	}
}

func TestIdentifyClient_Generic(t *testing.T) {
	cases := []string{"node", "Bun/1.3.11", "Go-http-client/2.0"}
	for _, ua := range cases {
		c := IdentifyClient(hdr("User-Agent", ua))
		if c.Name != ClientGeneric {
			t.Errorf("ua=%q name=%q", ua, c.Name)
		}
	}
}

func TestIdentifyClient_Unknown(t *testing.T) {
	c := IdentifyClient(hdr("User-Agent", "MyCustomAgent/1.0"))
	if c.Name != ClientUnknown {
		t.Errorf("name=%q", c.Name)
	}
}

func TestIdentifyClient_AmpBeatsClaudeCodeWhenBothPresent(t *testing.T) {
	// Amp sometimes proxies a Claude-Code-flavored system prompt and even
	// passes through a claude-cli UA. The Amp detector must win because
	// its session ID is the meaningful one for grouping.
	c := IdentifyClient(hdr(
		"User-Agent", "claude-cli/2.1.141 (external, cli)",
		"X-Amp-Thread-Id", "T-xxx",
	))
	if c.Name != ClientAmp {
		t.Errorf("expected amp to win, got %q", c.Name)
	}
}

// Subagent / title-gen detection via identifyRequest. Table-driven so
// adding a new (client, kind) combo is one row, no extra functions.
func TestIdentifyRequest_KindDetection(t *testing.T) {
	cases := []struct {
		name                string
		headers             http.Header
		sysHead             string
		wantClient          string
		wantKind            Kind
		wantAgentID         string
		wantParentSessionID string
	}{
		{
			name: "claude_code parent",
			headers: hdr(
				"User-Agent", "claude-cli/2.1.143 (external, cli)",
				"X-Claude-Code-Session-Id", "2507c625-b5da-46ad-b93b-9fffe88c3e6b",
			),
			sysHead:    "you are claude code, anthropic's official cli for claude.",
			wantClient: ClientClaudeCode,
			wantKind:   KindParent,
		},
		{
			name: "claude_code subagent (X-Claude-Code-Agent-Id present)",
			headers: hdr(
				"User-Agent", "claude-cli/2.1.143 (external, cli)",
				"X-Claude-Code-Session-Id", "2507c625-b5da-46ad-b93b-9fffe88c3e6b",
				"X-Claude-Code-Agent-Id", "a84564f0326e0281b",
			),
			// Subagent system prompts also start with the "You are Claude Code"
			// preamble — but the X-Claude-Code-Agent-Id header takes priority
			// over any prompt fingerprint, so even when sysHead lies, the
			// classification stays correct.
			sysHead:     "you are claude code, anthropic's official cli for claude.",
			wantClient:  ClientClaudeCode,
			wantKind:    KindSubagent,
			wantAgentID: "a84564f0326e0281b",
		},
		{
			name: "claude_code title-gen (sysHead fingerprint)",
			headers: hdr(
				"User-Agent", "claude-cli/2.1.143 (external, cli)",
				"X-Claude-Code-Session-Id", "2507c625-b5da-46ad-b93b-9fffe88c3e6b",
			),
			// Real prefix observed in 2026-05-20T102520-f3185b26.log.
			sysHead:    "generate a concise, sentence-case title (3-7 words)",
			wantClient: ClientClaudeCode,
			wantKind:   KindTitleGen,
		},
		{
			name: "opencode parent",
			headers: hdr(
				"User-Agent", "opencode/1.15.5 (darwin 25.4.0; arm64) ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
				"Session_id", "ses_1bc967092ffeGtKzXWe1lpywr4",
			),
			sysHead:    "you are opencode, you and the user share the same workspace",
			wantClient: ClientOpencode,
			wantKind:   KindParent,
		},
		{
			name: "opencode subagent (X-Parent-Session-Id present)",
			headers: hdr(
				"User-Agent", "opencode/1.15.5 (darwin 25.4.0; arm64) ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
				"Session_id", "ses_1bc965ecaffeYsbNVmshuHo4aT",
				"X-Parent-Session-Id", "ses_1bc967092ffeGtKzXWe1lpywr4",
			),
			sysHead:             "you are opencode, you and the user share the same workspace",
			wantClient:          ClientOpencode,
			wantKind:            KindSubagent,
			wantParentSessionID: "ses_1bc967092ffeGtKzXWe1lpywr4",
		},
		{
			name: "opencode title-gen (sysHead fingerprint)",
			headers: hdr(
				"User-Agent", "opencode/1.15.5 (darwin 25.4.0; arm64) ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
				"Session_id", "ses_1bc967092ffeGtKzXWe1lpywr4",
			),
			// Real prefix observed in 2026-05-20T102414-04181a73.log.
			sysHead:    "you are a title generator. you output only a thread title.",
			wantClient: ClientOpencode,
			wantKind:   KindTitleGen,
		},
		{
			name:       "amp has no subagent signal — stays parent",
			headers:    hdr("User-Agent", "Ap/JS 0.74.0", "X-Amp-Thread-Id", "T-abc"),
			sysHead:    "",
			wantClient: ClientAmp,
			wantKind:   KindParent,
		},
		{
			name:       "curl with no kind hints stays parent",
			headers:    hdr("User-Agent", "curl/8.7.1"),
			sysHead:    "",
			wantClient: ClientCurl,
			wantKind:   KindParent,
		},
		{
			name:       "unknown ua falls through to parent",
			headers:    hdr("User-Agent", "MyCustomAgent/1.0"),
			sysHead:    "",
			wantClient: ClientUnknown,
			wantKind:   KindParent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := identifyRequest(tc.headers, tc.sysHead)
			if r.Client.Name != tc.wantClient {
				t.Errorf("client=%q want=%q", r.Client.Name, tc.wantClient)
			}
			if r.Kind != tc.wantKind {
				t.Errorf("kind=%d want=%d", r.Kind, tc.wantKind)
			}
			if r.AgentID != tc.wantAgentID {
				t.Errorf("agent_id=%q want=%q", r.AgentID, tc.wantAgentID)
			}
			if r.ParentSessionID != tc.wantParentSessionID {
				t.Errorf("parent_session_id=%q want=%q", r.ParentSessionID, tc.wantParentSessionID)
			}
		})
	}
}

// Claude Code subagent header beats title-gen sysHead fingerprint — header
// is the structural signal, prompt is a fallback. Without this ordering a
// hypothetical subagent that happens to ALSO generate a title (custom
// agent that opens with "Generate a concise…") would mis-route to drop.
func TestIdentifyRequest_AgentIdBeatsTitleGenFingerprint(t *testing.T) {
	r := identifyRequest(hdr(
		"User-Agent", "claude-cli/2.1.143 (external, cli)",
		"X-Claude-Code-Session-Id", "parent-1",
		"X-Claude-Code-Agent-Id", "agent-deadbeef",
	), "generate a concise summary of the file")
	if r.Kind != KindSubagent {
		t.Fatalf("expected KindSubagent when both signals present, got %d", r.Kind)
	}
	if r.AgentID != "agent-deadbeef" {
		t.Errorf("agent_id=%q", r.AgentID)
	}
}
