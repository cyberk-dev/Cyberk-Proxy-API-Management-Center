package contextbudget

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// reminderMarker is included verbatim in every injected reminder so the
// middleware can recognize and skip already-injected requests on retries.
//
// Detection has to tolerate both raw and JSON-unicode-escaped forms because
// sjson's typed setters route map values through encoding/json, which
// escapes `<` and `>` to `<` / `>` by default. Raw string setters
// (e.g. sjson.SetBytes(path, "literal")) pass the bytes through verbatim.
// Both appear depending on which code path produced the body, so the
// marker check considers either.
const (
	reminderMarker    = "<system-reminder>"
	reminderMarkerEnd = "</system-reminder>"
	// reminderMarkerEscaped is the form that ends up in the byte stream when
	// encoding/json serializes the opening tag with default settings — the
	// raw `<` becomes the JSON unicode escape sequence (six characters
	// backslash-u-0-0-3-c). Spelled here as a double-quoted Go string with
	// a doubled backslash so the source file is portable across editors.
	reminderMarkerEscaped = "\\u003csystem-reminder\\u003e"
)

// wrapReminder surrounds the operator-provided body text with the recognized
// marker tags. The tags are stable so detection of prior injection across
// retries is reliable.
func wrapReminder(body string) string {
	return reminderMarker + "\n" + body + "\n" + reminderMarkerEnd
}

// InjectSystemReminder appends a *new* user-authored message to the
// conversation that carries the system-reminder text. We deliberately do NOT
// mutate the existing last user message — that would alter the
// prompt-cached prefix and force the upstream provider to re-cache the
// entire conversation. Appending a fresh trailing message preserves every
// cache breakpoint set on earlier turns; both Anthropic and the OpenAI APIs
// merge consecutive same-role turns server-side so the alternation
// invariant is not violated.
//
// Returns the original bytes when injection is not possible (malformed
// body, missing top-level array, or unknown protocol) — callers should
// treat that as best-effort.
func InjectSystemReminder(rawJSON []byte, p Protocol, reminderBody string) []byte {
	if len(rawJSON) == 0 || reminderBody == "" {
		return rawJSON
	}
	if alreadyInjected(rawJSON) {
		return rawJSON
	}
	reminder := wrapReminder(reminderBody)
	switch p {
	case ProtoClaude:
		return appendClaude(rawJSON, reminder)
	case ProtoOpenAIChat:
		return appendOpenAIChat(rawJSON, reminder)
	case ProtoOpenAIResponses:
		return appendOpenAIResponses(rawJSON, reminder)
	case ProtoGemini:
		return appendGemini(rawJSON, reminder)
	}
	return rawJSON
}

// alreadyInjected does a substring check for the system-reminder marker
// in either its raw or JSON-unicode-escaped form. Costs O(n) but n is
// bounded by the body-peek cap; cheap relative to downstream gjson walks.
func alreadyInjected(raw []byte) bool {
	s := string(raw)
	return strings.Contains(s, reminderMarker) || strings.Contains(s, reminderMarkerEscaped)
}

// appendClaude pushes a new user message onto messages[]. The model sees
// the reminder as if the user had sent a follow-up turn; consecutive user
// turns are merged by the Anthropic server per the documented behavior, so
// alternation is preserved without us touching the original turns.
func appendClaude(raw []byte, reminder string) []byte {
	msg := map[string]any{
		"role": "user",
		"content": []map[string]any{
			{"type": "text", "text": reminder},
		},
	}
	out, err := sjson.SetBytes(raw, "messages.-1", msg)
	if err != nil {
		return raw
	}
	return out
}

// appendOpenAIChat pushes a new user message onto messages[]. OpenAI's
// chat-completions endpoint also tolerates consecutive same-role turns;
// the reminder lands as the final user message before the assistant
// reply is sampled.
func appendOpenAIChat(raw []byte, reminder string) []byte {
	msg := map[string]any{
		"role":    "user",
		"content": reminder,
	}
	out, err := sjson.SetBytes(raw, "messages.-1", msg)
	if err != nil {
		return raw
	}
	return out
}

// appendOpenAIResponses pushes a new message item onto input[]. Responses
// requests may send `input` as a string for the simplest case; we promote
// that to an array first so the appended item has a place to live.
func appendOpenAIResponses(raw []byte, reminder string) []byte {
	input := gjson.GetBytes(raw, "input")
	if !input.Exists() {
		// No conversation context at all; nothing to append to.
		return raw
	}
	switch {
	case input.Type == gjson.String:
		// Promote string input into an array form so the new message has a
		// place to live alongside the original user turn.
		wrapped := []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": input.String()},
				},
			},
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": reminder},
				},
			},
		}
		out, err := sjson.SetBytes(raw, "input", wrapped)
		if err != nil {
			return raw
		}
		return out
	case input.IsArray():
		item := map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": reminder},
			},
		}
		out, err := sjson.SetBytes(raw, "input.-1", item)
		if err != nil {
			return raw
		}
		return out
	}
	return raw
}

// appendGemini pushes a new user content entry onto contents[]. Gemini's
// API also merges consecutive same-role turns, so this is safe regardless
// of whether the prior turn was user-authored or model-authored.
func appendGemini(raw []byte, reminder string) []byte {
	entry := map[string]any{
		"role":  "user",
		"parts": []map[string]any{{"text": reminder}},
	}
	out, err := sjson.SetBytes(raw, "contents.-1", entry)
	if err != nil {
		return raw
	}
	return out
}
