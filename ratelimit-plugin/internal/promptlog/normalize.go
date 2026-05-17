package promptlog

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Block is the normalized representation of one content block from a user
// message. Binary payloads (images, documents) are reduced to metadata so the
// log file stays small and free of base64 blobs; text is kept verbatim up to
// MaxTextBytes and middle-truncated past that so the user's intent (typically
// at the start and end of long pastes) survives.
//
// Tool blocks (tool_use / tool_result, plus Gemini's functionCall /
// functionResponse) are reference-only: Tool carries the tool name, Bytes /
// SHA256 cover the raw input or content body, and IsError flags tool_result
// failures. The full payload is never copied into the log — the point is to
// make agent-loop activity observable without ballooning storage.
//
// Bytes semantics depend on Type:
//   - text: byte length of the (possibly truncated) text
//   - image/document/audio: size of the decoded binary payload
//   - tool_use / tool_result: size of the JSON input / content body
type Block struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	URL       string `json:"url,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	OrigBytes int    `json:"orig_bytes,omitempty"`
	Tool      string `json:"tool,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// truncateText head+tail-truncates s when it exceeds max bytes, keeping the
// first and last halves and inserting a marker for the elided middle. The
// head and tail are where intent and conclusion typically live, so this
// preserves grep-ability of long pastes while cutting their bulk.
// Truncation respects UTF-8 rune boundaries so the log never contains
// invalid encoding. Returns the (possibly trimmed) text, a flag, and the
// original byte length.
func truncateText(s string, max int) (string, bool, int) {
	orig := len(s)
	if max <= 0 || orig <= max {
		return s, false, orig
	}
	half := max / 2
	head := truncateAtRune(s, half)
	tail := truncateAtRuneFromEnd(s, half)
	marker := fmt.Sprintf("\n...[elided %d bytes]...\n", orig-len(head)-len(tail))
	return head + marker + tail, true, orig
}

// truncateAtRune returns the longest prefix of s with byte length <= n that
// ends on a UTF-8 boundary.
func truncateAtRune(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// truncateAtRuneFromEnd returns the longest suffix of s with byte length <= n
// that starts on a UTF-8 boundary.
func truncateAtRuneFromEnd(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// toolBlock builds the normalized form of a tool_use / tool_result /
// thinking / reasoning block: tool name (when present), the original byte
// length, and the body itself head+tail-truncated at max. Hashing was
// tried first and rejected — operators reading the log want to see the
// command that was issued and its first/last output lines, not a 16-char
// hex digest. The truncation runs through the same head+tail helper as
// regular text blocks so the elision marker shape is consistent.
func toolBlock(kind, tool, raw string, max int, isError bool) Block {
	text, trunc, orig := truncateText(raw, max)
	return Block{
		Type:      kind,
		Tool:      tool,
		Text:      text,
		Bytes:     orig,
		Truncated: trunc,
		IsError:   isError,
	}
}

// hashRaw returns (byte length, short-sha256) for an arbitrary raw byte slice.
// Used for tool_use input bodies and tool_result content bodies so the log
// captures fingerprint + size without copying the payload itself.
func hashRaw(raw []byte) (int, string) {
	if len(raw) == 0 {
		return 0, ""
	}
	return len(raw), sha256First8(raw)
}

// sha256First8 returns the hex-encoded first 8 bytes of sha256(raw). Same
// truncation as maskBase64 / hashRaw so all fingerprints in the log live in
// the same 16-hex-char namespace.
func sha256First8(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// maskBase64 takes a base64-encoded payload, decodes it to measure size and
// hash, and returns (size, short-sha256). Decoding is done with the standard
// encoder; the loose URL-safe and padded-or-not variants common to API SDKs
// are handled by trying the strict decoder then falling back. On unparseable
// input, returns the size of the raw base64 string as a best-effort indicator
// with an empty hash.
func maskBase64(data string) (int, string) {
	if data == "" {
		return 0, ""
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := enc.DecodeString(data)
		if err == nil {
			sum := sha256.Sum256(decoded)
			return len(decoded), hex.EncodeToString(sum[:8])
		}
	}
	return len(data), ""
}

// parseDataURL extracts (media_type, base64_payload) from a `data:...` URL.
// Returns ok=false for non-data URLs.
func parseDataURL(u string) (mediaType, payload string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := u[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	header := rest[:comma]
	payload = rest[comma+1:]
	// header looks like "image/png;base64" or "image/png" — we only handle
	// base64-encoded data URLs; raw text URLs are not used by chat SDKs.
	if !strings.Contains(header, "base64") {
		return "", "", false
	}
	mediaType = strings.TrimSuffix(header, ";base64")
	return mediaType, payload, true
}
