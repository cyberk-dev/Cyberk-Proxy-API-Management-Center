package contextbudget

import (
	"net/http"
	"testing"
)

// Helper: build a request with the given headers + body. We use httptest
// internally elsewhere; here we only need r.Header so a bare *http.Request
// is enough.
func makeReq(headers map[string]string) *http.Request {
	r, _ := http.NewRequest("POST", "http://x/v1/messages", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestExtractSession_ClaudeCodeHeader(t *testing.T) {
	r := makeReq(map[string]string{
		"Authorization":            "Bearer alice",
		"X-Claude-Code-Session-Id": "abc-123",
	})
	key := ExtractSession(r, []byte(`{"messages":[]}`), ProtoClaude)
	if key.Source != SessionFromHeader {
		t.Fatalf("source = %v, want header", key.Source)
	}
	if key.ID != "abc-123" {
		t.Errorf("id = %q, want abc-123", key.ID)
	}
	if key.APIKeyHash == "" {
		t.Error("APIKeyHash should be derived from Authorization")
	}
}

func TestExtractSession_OpencodeUnderscoreHeader(t *testing.T) {
	// opencode sends `Session_id: ses_xxx` (underscore, non-standard case).
	// Go's net/http normalizes via CanonicalMIMEHeaderKey: `Session_Id`.
	// Our lookup uses Header.Get which already does the canonicalization.
	r := makeReq(map[string]string{
		"Authorization": "Bearer alice",
		"Session_id":    "ses_xyz",
	})
	key := ExtractSession(r, []byte(`{"input":[]}`), ProtoOpenAIResponses)
	if key.Source != SessionFromHeader {
		t.Fatalf("source = %v, want header", key.Source)
	}
	if key.ID != "ses_xyz" {
		t.Errorf("id = %q, want ses_xyz", key.ID)
	}
}

func TestExtractSession_BodyHashFallback_Claude(t *testing.T) {
	// No session header → fallback to body fingerprint. Two requests in
	// the same conversation MUST hash identically (the first user message
	// is the same).
	body := []byte(`{"messages":[{"role":"user","content":"first turn"}]}`)
	body2 := []byte(`{"messages":[{"role":"user","content":"first turn"},{"role":"assistant","content":"hi"},{"role":"user","content":"second turn"}]}`)

	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	k1 := ExtractSession(r, body, ProtoClaude)
	k2 := ExtractSession(r, body2, ProtoClaude)
	if k1.Source != SessionFromBodyHash || k2.Source != SessionFromBodyHash {
		t.Fatalf("expected body_hash source, got %v / %v", k1.Source, k2.Source)
	}
	if k1.ID != k2.ID {
		t.Errorf("same conversation must hash identically; got %q vs %q", k1.ID, k2.ID)
	}
}

func TestExtractSession_BodyHashFallback_Gemini(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"first turn"}]}]}`)
	body2 := []byte(`{"contents":[{"role":"user","parts":[{"text":"first turn"}]},{"role":"model","parts":[{"text":"reply"}]}]}`)
	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	k1 := ExtractSession(r, body, ProtoGemini)
	k2 := ExtractSession(r, body2, ProtoGemini)
	if k1.ID != k2.ID {
		t.Errorf("gemini same first user turn must collide; got %q vs %q", k1.ID, k2.ID)
	}
}

func TestExtractSession_DifferentConversationsDontCollide(t *testing.T) {
	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	k1 := ExtractSession(r, []byte(`{"messages":[{"role":"user","content":"task A"}]}`), ProtoClaude)
	k2 := ExtractSession(r, []byte(`{"messages":[{"role":"user","content":"task B"}]}`), ProtoClaude)
	if k1.ID == k2.ID {
		t.Errorf("different first messages must not collide; got %q", k1.ID)
	}
}

func TestExtractSession_DifferentKeysIsolate(t *testing.T) {
	// Same body, different API key → different composite SessionKey
	// because the API key hash is mixed into the String() form.
	body := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
	r1 := makeReq(map[string]string{"Authorization": "Bearer alice"})
	r2 := makeReq(map[string]string{"Authorization": "Bearer bob"})
	k1 := ExtractSession(r1, body, ProtoClaude)
	k2 := ExtractSession(r2, body, ProtoClaude)
	if k1.ID != k2.ID {
		t.Error("body hash should be identical regardless of key")
	}
	if k1.String() == k2.String() {
		t.Error("composite key must differ across API keys")
	}
}

func TestExtractSession_NoBodyNoHeader(t *testing.T) {
	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	k := ExtractSession(r, nil, ProtoClaude)
	if k.ID != "" {
		t.Errorf("empty body + no header should yield no ID, got %q", k.ID)
	}
}

func TestExtractSession_CodexCLIConversationIDHeader(t *testing.T) {
	// Codex CLI sends both `session_id` and `conversation_id` headers
	// carrying the same UUID. We should pick either up. Test the
	// alt header in isolation to verify it's not skipped.
	r := makeReq(map[string]string{
		"Authorization":   "Bearer alice",
		"Conversation_id": "01928374-1234-...",
	})
	key := ExtractSession(r, []byte(`{}`), ProtoOpenAIResponses)
	if key.Source != SessionFromHeader {
		t.Fatalf("source = %v, want header", key.Source)
	}
	if key.ID != "01928374-1234-..." {
		t.Errorf("id = %q, want 01928374-1234-...", key.ID)
	}
}

func TestExtractSession_HyphenSessionIdHeader(t *testing.T) {
	// Codex CLI #11732 (Envoy-compat rename) and other clients may use
	// hyphen instead of underscore. Go's CanonicalMIMEHeaderKey treats
	// these as separate canonical forms, so we must list both.
	r := makeReq(map[string]string{
		"Authorization": "Bearer alice",
		"Session-Id":    "ses_hyphen",
	})
	key := ExtractSession(r, []byte(`{}`), ProtoClaude)
	if key.Source != SessionFromHeader {
		t.Fatalf("source = %v, want header", key.Source)
	}
	if key.ID != "ses_hyphen" {
		t.Errorf("id = %q, want ses_hyphen", key.ID)
	}
}

func TestExtractSession_FingerprintSkipsDeveloperBlock_Responses(t *testing.T) {
	// opencode emits a long fixed `developer` system block as input[0]
	// followed by the actual user turn at input[1+]. Two DIFFERENT
	// conversations from the same client/version share input[0] but
	// differ on the user message. Without the skip, both would hash
	// identically and the tracker would merge them.
	devBlock := `{"role":"developer","content":[{"type":"input_text","text":"You are opencode v1.0 — long fixed system prompt"}]}`
	bodyA := []byte(`{"input":[` + devBlock + `,{"role":"user","content":[{"type":"input_text","text":"task A"}]}]}`)
	bodyB := []byte(`{"input":[` + devBlock + `,{"role":"user","content":[{"type":"input_text","text":"task B"}]}]}`)

	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	kA := ExtractSession(r, bodyA, ProtoOpenAIResponses)
	kB := ExtractSession(r, bodyB, ProtoOpenAIResponses)
	if kA.Source != SessionFromBodyHash || kB.Source != SessionFromBodyHash {
		t.Fatalf("expected body_hash source, got %v / %v", kA.Source, kB.Source)
	}
	if kA.ID == kB.ID {
		t.Errorf("different user prompts must not collide; both hashed to %q", kA.ID)
	}
}

func TestExtractSession_FingerprintSkipsSystem_Claude(t *testing.T) {
	// Same hazard on the Claude shape: if a client (or middleware) puts
	// a `system` role as messages[0], distinct user turns must still
	// produce distinct hashes.
	prelude := `{"role":"system","content":"long system prompt"},`
	bodyA := []byte(`{"messages":[` + prelude + `{"role":"user","content":"task A"}]}`)
	bodyB := []byte(`{"messages":[` + prelude + `{"role":"user","content":"task B"}]}`)
	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	kA := ExtractSession(r, bodyA, ProtoClaude)
	kB := ExtractSession(r, bodyB, ProtoClaude)
	if kA.ID == kB.ID {
		t.Errorf("must not collide; both hashed to %q", kA.ID)
	}
}

func TestExtractSession_FingerprintStableAcrossTurns_ResponsesWithDeveloper(t *testing.T) {
	// Stability check: same conversation across turns must still hash
	// identically even when a developer block is present.
	devBlock := `{"role":"developer","content":[{"type":"input_text","text":"sys"}]}`
	bodyT1 := []byte(`{"input":[` + devBlock + `,{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	bodyT2 := []byte(`{"input":[` + devBlock + `,{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"role":"assistant","content":[{"type":"output_text","text":"hi"}]},{"role":"user","content":[{"type":"input_text","text":"follow-up"}]}]}`)
	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	k1 := ExtractSession(r, bodyT1, ProtoOpenAIResponses)
	k2 := ExtractSession(r, bodyT2, ProtoOpenAIResponses)
	if k1.ID == "" || k2.ID == "" {
		t.Fatal("both turns should produce a fingerprint")
	}
	if k1.ID != k2.ID {
		t.Errorf("turn 1 and turn 2 of the same conversation must collide; got %q vs %q", k1.ID, k2.ID)
	}
}

func TestExtractSession_FingerprintToolResultFirstTurn_Claude(t *testing.T) {
	// Edge case: a request whose first user turn carries a tool_result
	// block (rather than a plain text block). The improved firstText
	// recurses into the tool_result's inner content so we still produce
	// a fingerprint.
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"42"}]}]}`)
	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	k := ExtractSession(r, body, ProtoClaude)
	if k.ID == "" {
		t.Error("tool_result first turn should still produce a fingerprint")
	}
}

func TestExtractSession_FingerprintGeminiMultiPartUser(t *testing.T) {
	// Gemini's user role with multiple text parts — concat all parts so
	// the seed reflects the whole turn rather than just parts[0].
	bodyA := []byte(`{"contents":[{"role":"user","parts":[{"text":"intro"},{"text":"detail A"}]}]}`)
	bodyB := []byte(`{"contents":[{"role":"user","parts":[{"text":"intro"},{"text":"detail B"}]}]}`)
	r := makeReq(map[string]string{"Authorization": "Bearer alice"})
	kA := ExtractSession(r, bodyA, ProtoGemini)
	kB := ExtractSession(r, bodyB, ProtoGemini)
	if kA.ID == kB.ID {
		t.Error("multi-part user turns with different content must not collide")
	}
}
