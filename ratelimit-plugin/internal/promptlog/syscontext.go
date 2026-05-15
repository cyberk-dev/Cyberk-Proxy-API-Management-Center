package promptlog

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// extractSystemText returns the concatenated system-prompt text for a
// request, in whatever shape the provider's schema uses. The output is a flat
// string used as input to CWD detection — formatting / ordering is preserved
// only enough that regex anchors work, not enough to round-trip the original.
func extractSystemText(peek []byte, provider string) string {
	switch provider {
	case ProviderAnthropic:
		return anthropicSystemText(peek)
	case ProviderOpenAIChat:
		return openAIChatSystemText(peek)
	case ProviderOpenAIResponses:
		return openAIResponsesSystemText(peek)
	case ProviderGemini:
		return geminiSystemText(peek)
	}
	return ""
}

func anthropicSystemText(peek []byte) string {
	sys := gjson.GetBytes(peek, "system")
	if sys.Type == gjson.String {
		return sys.String()
	}
	if !sys.IsArray() {
		return ""
	}
	var sb strings.Builder
	sys.ForEach(func(_, item gjson.Result) bool {
		if t := item.Get("text"); t.Exists() {
			sb.WriteString(t.String())
			sb.WriteByte('\n')
		}
		return true
	})
	return sb.String()
}

func openAIChatSystemText(peek []byte) string {
	var sb strings.Builder
	gjson.GetBytes(peek, "messages").ForEach(func(_, msg gjson.Result) bool {
		// "developer" is the Responses-era replacement of "system" that some
		// chat-completions wrappers also accept; treat both as system.
		role := msg.Get("role").String()
		if role != "system" && role != "developer" {
			return true
		}
		appendContentText(&sb, msg.Get("content"))
		return true
	})
	return sb.String()
}

func openAIResponsesSystemText(peek []byte) string {
	var sb strings.Builder
	if inst := gjson.GetBytes(peek, "instructions"); inst.Exists() && inst.Type == gjson.String {
		sb.WriteString(inst.String())
		sb.WriteByte('\n')
	}
	input := gjson.GetBytes(peek, "input")
	if input.IsArray() {
		input.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			if role != "system" && role != "developer" {
				return true
			}
			appendContentText(&sb, msg.Get("content"))
			return true
		})
	}
	return sb.String()
}

func geminiSystemText(peek []byte) string {
	si := gjson.GetBytes(peek, "systemInstruction")
	if !si.Exists() {
		return ""
	}
	// systemInstruction may be {role, parts} or just {parts: [...]}.
	parts := si.Get("parts")
	if !parts.IsArray() {
		// Fallback: a plain string under systemInstruction.text.
		if t := si.Get("text"); t.Exists() {
			return t.String()
		}
		return ""
	}
	var sb strings.Builder
	parts.ForEach(func(_, part gjson.Result) bool {
		if t := part.Get("text"); t.Exists() {
			sb.WriteString(t.String())
			sb.WriteByte('\n')
		}
		return true
	})
	return sb.String()
}

// appendContentText handles the OpenAI dual-shape content field (string or
// array of typed blocks) used in both chat and responses inputs.
func appendContentText(sb *strings.Builder, content gjson.Result) {
	if content.Type == gjson.String {
		sb.WriteString(content.String())
		sb.WriteByte('\n')
		return
	}
	if !content.IsArray() {
		return
	}
	content.ForEach(func(_, item gjson.Result) bool {
		if t := item.Get("text"); t.Exists() {
			sb.WriteString(t.String())
			sb.WriteByte('\n')
		}
		return true
	})
}

// cwdPatterns are tried in order against the system text. They cover the
// dialects observed across Claude Code (old + new), Amp, and opencode. New
// clients can be supported by appending a pattern without touching call
// sites. Anchored to start of line so they don't match prose like
// "...working directory: /tmp..." in unrelated docs.
var cwdPatterns = []*regexp.Regexp{
	// Claude Code v2.1.97+: " - Primary working directory: /path"
	regexp.MustCompile(`(?m)^\s*-?\s*Primary working directory:\s*(\S.*?)\s*$`),
	// Claude Code v2.1.63 and opencode <env> block: "Working directory: /path"
	regexp.MustCompile(`(?m)^\s*-?\s*Working directory:\s*(\S.*?)\s*$`),
	// Amp + older Claude Code: "Workspace root folder: /path"
	regexp.MustCompile(`(?m)^\s*-?\s*Workspace root folder:\s*(\S.*?)\s*$`),
}

// extractCWD scans systemText for the first matching directory hint. Returns
// an empty string when none of the patterns match — log analysts treat empty
// CWD as "unknown / non-CLI client", which is the right default.
func extractCWD(systemText string) string {
	if systemText == "" {
		return ""
	}
	for _, re := range cwdPatterns {
		if m := re.FindStringSubmatch(systemText); m != nil {
			// Strip trailing punctuation/quotes that some clients append.
			return strings.Trim(strings.TrimSpace(m[1]), `"',;`)
		}
	}
	return ""
}
