package promptlog

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Provider identifies which request schema the JSON body follows. The path is
// the sole signal — content-type alone is insufficient because all four
// upstreams use application/json.
const (
	ProviderAnthropic       = "anthropic"
	ProviderOpenAIChat      = "openai_chat"
	ProviderOpenAIResponses = "openai_responses"
	ProviderGemini          = "gemini"
)

// detectProvider returns "" when the path is not a known chat-completion
// endpoint, signalling the middleware to skip extraction. We deliberately
// exclude /v1/models, /healthz, etc. — those have no user prompt to extract.
func detectProvider(path string) string {
	switch path {
	case "/v1/messages":
		return ProviderAnthropic
	case "/v1/chat/completions":
		return ProviderOpenAIChat
	case "/v1/responses":
		return ProviderOpenAIResponses
	}
	if strings.HasPrefix(path, "/v1beta/models/") {
		// Gemini exposes both unary (`:generateContent`) and streaming
		// (`:streamGenerateContent`) variants on the same body shape.
		if strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent") {
			return ProviderGemini
		}
	}
	return ""
}

// extractBlocks parses the last user message from the request body and
// returns its normalized content blocks. Returns nil when no user message is
// found (e.g. tool-only round-trip, malformed body) AND when the user
// message exists but contributes no useful blocks after filtering — callers
// treat both as "nothing to log."
func extractBlocks(peek []byte, provider string, maxText int) []Block {
	var blocks []Block
	switch provider {
	case ProviderAnthropic:
		blocks = extractFromMessages(peek, "messages", extractAnthropicBlock, maxText)
	case ProviderOpenAIChat:
		blocks = extractFromMessages(peek, "messages", extractOpenAIChatBlock, maxText)
	case ProviderOpenAIResponses:
		blocks = extractOpenAIResponses(peek, maxText)
	case ProviderGemini:
		blocks = extractGemini(peek, maxText)
	default:
		return nil
	}
	if blocks == nil {
		return nil
	}
	filtered := dropWrapperBlocks(blocks)
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// wrapperTags are XML-like markers that Claude Code (and similar CLIs) wrap
// around hook output, slash-command artifacts, and system reminders. Blocks
// whose entire text is enclosed by one of these tags are CLI noise, not
// user-authored content, and must be dropped before logging. The list is
// derived from samples across ~170 captured request bodies.
var wrapperTags = []string{
	"system-reminder",
	"local-command-caveat",
	"local-command-stdout",
	"local-command-stderr",
	"command-name",
	"command-message",
	"command-args",
	"command-stdout",
	"command-stderr",
}

// dropWrapperBlocks removes text blocks that are entirely a wrapper-tagged
// CLI artifact. Non-text blocks (image, document, audio) pass through
// untouched because masking already neutralized their payload.
func dropWrapperBlocks(in []Block) []Block {
	out := in[:0]
	for _, b := range in {
		if b.Type == "text" && isWrapperOnly(b.Text) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// isWrapperOnly reports whether the text is entirely a single wrapper-tagged
// block (allowing surrounding whitespace). Conservative: requires both open
// and close tags so prose that legitimately mentions "<system-reminder>" is
// not silently dropped.
func isWrapperOnly(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, tag := range wrapperTags {
		open := "<" + tag + ">"
		closing := "</" + tag + ">"
		if strings.HasPrefix(trimmed, open) && strings.HasSuffix(trimmed, closing) {
			return true
		}
	}
	return false
}

// joinPromptText concatenates the human-readable text blocks into a single
// convenience string, suitable for grep / jq / dashboarding. Non-text blocks
// are summarized as a one-line marker so the prompt string still describes
// the request shape (e.g. "[image:image/png 12345B sha256=abc]").
func joinPromptText(blocks []Block) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
			continue
		}
		parts = append(parts, summarizeNonText(b))
	}
	return strings.Join(parts, "\n\n")
}

func summarizeNonText(b Block) string {
	var sb strings.Builder
	sb.WriteByte('[')
	sb.WriteString(b.Type)
	if b.MediaType != "" {
		sb.WriteByte(':')
		sb.WriteString(b.MediaType)
	}
	if b.Bytes > 0 {
		sb.WriteByte(' ')
		sb.WriteString(formatBytes(b.Bytes))
	}
	if b.SHA256 != "" {
		sb.WriteString(" sha256=")
		sb.WriteString(b.SHA256)
	}
	if b.URL != "" {
		sb.WriteByte(' ')
		sb.WriteString(b.URL)
	}
	sb.WriteByte(']')
	return sb.String()
}

func formatBytes(n int) string {
	const (
		kib = 1024
		mib = 1024 * kib
	)
	switch {
	case n >= mib:
		return formatScaled(n, mib, "MiB")
	case n >= kib:
		return formatScaled(n, kib, "KiB")
	default:
		return formatScaled(n, 1, "B")
	}
}

func formatScaled(n, unit int, suffix string) string {
	if unit == 1 {
		return itoa(n) + suffix
	}
	whole := n / unit
	frac := (n % unit) * 10 / unit
	if frac == 0 {
		return itoa(whole) + suffix
	}
	return itoa(whole) + "." + itoa(frac) + suffix
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// extractFromMessages walks `messages` (or `input`) forward, keeping the
// content of the latest entry whose role is "user". Forward iteration is
// chosen because gjson does not support cheap reverse traversal, and the
// memory cost of holding one gjson.Result reference is constant.
func extractFromMessages(peek []byte, arrayKey string, blockFn func(gjson.Result, int) (Block, bool), maxText int) []Block {
	var lastContent gjson.Result
	var found bool
	gjson.GetBytes(peek, arrayKey).ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" {
			return true
		}
		lastContent = msg.Get("content")
		found = true
		return true
	})
	if !found {
		return nil
	}
	return walkContent(lastContent, blockFn, maxText)
}

// walkContent handles the dual-shape `content` field shared by Anthropic and
// OpenAI chat: a bare string is shorthand for a single text block, an array
// is a list of typed blocks. Returns an empty slice (not nil) when the user
// message has only tool blocks, distinguishing "request seen, no prompt
// text" from "no user message at all" in the caller.
func walkContent(content gjson.Result, blockFn func(gjson.Result, int) (Block, bool), maxText int) []Block {
	if content.Type == gjson.String {
		text, trunc, orig := truncateText(content.String(), maxText)
		return []Block{textBlock(text, trunc, orig)}
	}
	if !content.IsArray() {
		return []Block{}
	}
	blocks := make([]Block, 0, 4)
	content.ForEach(func(_, item gjson.Result) bool {
		if b, ok := blockFn(item, maxText); ok {
			blocks = append(blocks, b)
		}
		return true
	})
	return blocks
}

func textBlock(text string, truncated bool, orig int) Block {
	b := Block{Type: "text", Text: text, Truncated: truncated}
	if truncated {
		b.OrigBytes = orig
	}
	return b
}

func extractAnthropicBlock(item gjson.Result, maxText int) (Block, bool) {
	t := item.Get("type").String()
	switch t {
	case "text":
		text, trunc, orig := truncateText(item.Get("text").String(), maxText)
		return textBlock(text, trunc, orig), true
	case "image":
		return maskAnthropicSource(item.Get("source"), "image"), true
	case "document":
		return maskAnthropicSource(item.Get("source"), "document"), true
	case "tool_use", "tool_result":
		// These appear in the user-role array for tool round-trips. They
		// represent model/tool exchanges, not user-authored content.
		return Block{}, false
	case "":
		return Block{}, false
	default:
		return Block{Type: t}, true
	}
}

// maskAnthropicSource handles {source: {type, media_type, data | url}}.
// `type: "base64"` carries inline binary; `type: "url"` references a remote
// asset and is safe to keep as-is (no PII in the URL itself for chat APIs).
func maskAnthropicSource(src gjson.Result, kind string) Block {
	b := Block{Type: kind, MediaType: src.Get("media_type").String()}
	switch src.Get("type").String() {
	case "base64":
		n, sha := maskBase64(src.Get("data").String())
		b.Bytes = n
		b.SHA256 = sha
	case "url":
		b.URL = src.Get("url").String()
	}
	return b
}

func extractOpenAIChatBlock(item gjson.Result, maxText int) (Block, bool) {
	t := item.Get("type").String()
	switch t {
	case "text":
		text, trunc, orig := truncateText(item.Get("text").String(), maxText)
		return textBlock(text, trunc, orig), true
	case "image_url":
		return maskOpenAIImageURL(item.Get("image_url.url").String()), true
	case "input_audio":
		// {"type": "input_audio", "input_audio": {"data": "...", "format": "wav"}}
		n, sha := maskBase64(item.Get("input_audio.data").String())
		return Block{
			Type:      "audio",
			MediaType: "audio/" + item.Get("input_audio.format").String(),
			Bytes:     n,
			SHA256:    sha,
		}, true
	case "":
		return Block{}, false
	default:
		return Block{Type: t}, true
	}
}

func maskOpenAIImageURL(url string) Block {
	if mt, payload, ok := parseDataURL(url); ok {
		n, sha := maskBase64(payload)
		return Block{Type: "image", MediaType: mt, Bytes: n, SHA256: sha}
	}
	return Block{Type: "image", URL: url}
}

// extractOpenAIResponses handles the Responses API (POST /v1/responses), whose
// `input` field can be either a bare string (shorthand) or an array of typed
// items. The item types are prefixed `input_*` to distinguish from chat's
// untyped `text`/`image_url`.
func extractOpenAIResponses(peek []byte, maxText int) []Block {
	input := gjson.GetBytes(peek, "input")
	if input.Type == gjson.String {
		text, trunc, orig := truncateText(input.String(), maxText)
		return []Block{textBlock(text, trunc, orig)}
	}
	if !input.IsArray() {
		return nil
	}
	var lastContent gjson.Result
	var found bool
	input.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" {
			return true
		}
		lastContent = msg.Get("content")
		found = true
		return true
	})
	if !found {
		return nil
	}
	return walkContent(lastContent, extractOpenAIResponsesBlock, maxText)
}

func extractOpenAIResponsesBlock(item gjson.Result, maxText int) (Block, bool) {
	t := item.Get("type").String()
	switch t {
	case "input_text", "text":
		text, trunc, orig := truncateText(item.Get("text").String(), maxText)
		return textBlock(text, trunc, orig), true
	case "input_image":
		return maskOpenAIImageURL(item.Get("image_url").String()), true
	case "input_file":
		// {"file_data": "base64...", "filename": "..."} OR {"file_id": "..."}
		if data := item.Get("file_data").String(); data != "" {
			n, sha := maskBase64(data)
			return Block{
				Type:   "document",
				Bytes:  n,
				SHA256: sha,
				URL:    item.Get("filename").String(),
			}, true
		}
		return Block{Type: "document", URL: item.Get("file_id").String()}, true
	case "":
		return Block{}, false
	default:
		return Block{Type: t}, true
	}
}

// extractGemini walks `contents` for the last user-role entry and returns its
// normalized `parts`. Gemini's role values are exactly "user" / "model";
// systemInstruction lives in a sibling field and is intentionally ignored.
func extractGemini(peek []byte, maxText int) []Block {
	var lastParts gjson.Result
	var found bool
	gjson.GetBytes(peek, "contents").ForEach(func(_, content gjson.Result) bool {
		role := content.Get("role").String()
		// Gemini sometimes omits role on user turns when there is only one
		// content; treat empty role as user to avoid silently dropping
		// single-shot prompts.
		if role != "" && role != "user" {
			return true
		}
		lastParts = content.Get("parts")
		found = true
		return true
	})
	if !found {
		return nil
	}
	if !lastParts.IsArray() {
		return []Block{}
	}
	blocks := make([]Block, 0, 4)
	lastParts.ForEach(func(_, part gjson.Result) bool {
		if b, ok := extractGeminiPart(part, maxText); ok {
			blocks = append(blocks, b)
		}
		return true
	})
	return blocks
}

func extractGeminiPart(part gjson.Result, maxText int) (Block, bool) {
	if t := part.Get("text"); t.Exists() {
		text, trunc, orig := truncateText(t.String(), maxText)
		return textBlock(text, trunc, orig), true
	}
	if inline := part.Get("inlineData"); inline.Exists() {
		n, sha := maskBase64(inline.Get("data").String())
		return Block{
			Type:      kindFromMime(inline.Get("mimeType").String()),
			MediaType: inline.Get("mimeType").String(),
			Bytes:     n,
			SHA256:    sha,
		}, true
	}
	if file := part.Get("fileData"); file.Exists() {
		return Block{
			Type:      kindFromMime(file.Get("mimeType").String()),
			MediaType: file.Get("mimeType").String(),
			URL:       file.Get("fileUri").String(),
		}, true
	}
	// Tool / function-call parts: drop, same reasoning as Anthropic tool_use.
	if part.Get("functionCall").Exists() || part.Get("functionResponse").Exists() {
		return Block{}, false
	}
	return Block{}, false
}

func kindFromMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	default:
		return "document"
	}
}
