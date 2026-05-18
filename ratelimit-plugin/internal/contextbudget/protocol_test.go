package contextbudget

import "testing"

func TestDetectProtocol(t *testing.T) {
	cases := []struct {
		path string
		want Protocol
	}{
		{"/v1/messages", ProtoClaude},
		{"/api/v1/messages", ProtoClaude},
		{"/v1/messages/count_tokens", ProtoUnknown},
		{"/v1/chat/completions", ProtoOpenAIChat},
		{"/v1/completions", ProtoOpenAIChat},
		{"/v1/responses", ProtoOpenAIResponses},
		{"/v1beta/models/gemini-2.5-pro:generateContent", ProtoGemini},
		{"/v1beta/models/gemini-2.5-pro:streamGenerateContent", ProtoGemini},
		{"/v1beta/models/gemini-2.5-pro:countTokens", ProtoUnknown},
		{"/v1beta/models", ProtoUnknown},
		{"/v1/models", ProtoUnknown},
		{"/healthz", ProtoUnknown},
		{"", ProtoUnknown},
	}
	for _, tc := range cases {
		if got := DetectProtocol(tc.path); got != tc.want {
			t.Errorf("DetectProtocol(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestIsStreamingPath(t *testing.T) {
	if !IsStreamingPath("/v1beta/models/gemini-2.5-pro:streamGenerateContent") {
		t.Error("streamGenerateContent should be detected as streaming")
	}
	if IsStreamingPath("/v1beta/models/gemini-2.5-pro:generateContent") {
		t.Error("generateContent (non-stream) should not be detected as streaming")
	}
	if IsStreamingPath("/v1/messages") {
		t.Error("anthropic stream is decided via body, not path")
	}
}
