package contextbudget

import "strings"

// Protocol identifies which provider-format the request follows, which
// determines where in the JSON body the conversation history lives.
type Protocol int

const (
	ProtoUnknown Protocol = iota
	// ProtoClaude is Anthropic's Messages API: top-level `messages[]`,
	// each item has `role` and `content` (string OR array of blocks).
	ProtoClaude
	// ProtoOpenAIChat is OpenAI Chat Completions: top-level `messages[]`,
	// each item has `role` and `content` (string OR array of blocks).
	ProtoOpenAIChat
	// ProtoOpenAIResponses is OpenAI Responses API: top-level `input[]`
	// (or `input` as a string), each item has `role` and `content`
	// (array of typed blocks).
	ProtoOpenAIResponses
	// ProtoGemini is Google's Gemini API: top-level `contents[]`,
	// each item has `role` and `parts[]` (each part has `text`, `inlineData`,
	// `functionCall`, etc.).
	ProtoGemini
)

func (p Protocol) String() string {
	switch p {
	case ProtoClaude:
		return "claude"
	case ProtoOpenAIChat:
		return "openai_chat"
	case ProtoOpenAIResponses:
		return "openai_responses"
	case ProtoGemini:
		return "gemini"
	default:
		return "unknown"
	}
}

// DetectProtocol picks a Protocol based on the request URL path. Returns
// ProtoUnknown when the route is something the middleware should not act on
// (model lists, count_tokens, management, etc.) — callers use Unknown as
// "skip this request".
func DetectProtocol(urlPath string) Protocol {
	if urlPath == "" {
		return ProtoUnknown
	}
	// Anthropic.
	if urlPath == "/v1/messages" || strings.HasSuffix(urlPath, "/v1/messages") {
		return ProtoClaude
	}
	// Anthropic count-tokens endpoint is metadata-only; do not warn/block.
	if strings.HasSuffix(urlPath, "/v1/messages/count_tokens") {
		return ProtoUnknown
	}
	// OpenAI Responses.
	if urlPath == "/v1/responses" || strings.HasSuffix(urlPath, "/v1/responses") {
		return ProtoOpenAIResponses
	}
	// OpenAI Chat Completions / Completions.
	if urlPath == "/v1/chat/completions" || strings.HasSuffix(urlPath, "/v1/chat/completions") {
		return ProtoOpenAIChat
	}
	if urlPath == "/v1/completions" || strings.HasSuffix(urlPath, "/v1/completions") {
		return ProtoOpenAIChat
	}
	// Gemini action routes — only generateContent variants count.
	if strings.HasPrefix(urlPath, "/v1beta/models/") || strings.HasPrefix(urlPath, "/v1/models/") {
		// e.g. /v1beta/models/gemini-2.5-pro:generateContent
		if i := strings.LastIndex(urlPath, ":"); i >= 0 {
			action := urlPath[i+1:]
			switch action {
			case "generateContent", "streamGenerateContent":
				return ProtoGemini
			}
		}
		return ProtoUnknown
	}
	return ProtoUnknown
}

// IsStreamingPath reports whether the URL itself signals a streaming response
// (Gemini distinguishes streaming via the `:streamGenerateContent` suffix).
// For protocols that signal stream via the request body, callers must inspect
// the body separately.
func IsStreamingPath(urlPath string) bool {
	return strings.HasSuffix(urlPath, ":streamGenerateContent")
}
