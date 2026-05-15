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
