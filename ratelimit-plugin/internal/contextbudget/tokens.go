package contextbudget

import (
	"strings"

	"github.com/tidwall/gjson"
)

// charsPerToken is the bytes-to-tokens ratio used for char-based estimation.
// 4 is the OpenAI rule of thumb for English; CJK comes in lower (more tokens
// per char) but downstream thresholds bake in headroom for that drift.
const charsPerToken = 4

// EstimateTokens returns a char-based estimate of the input tokens that the
// upstream provider will count for rawJSON. The estimate is intentionally
// rough — it walks every text-bearing field for the given protocol and sums
// rune counts / charsPerToken. Base64 image / inline-data payloads are
// skipped because their effective tokenization is provider-specific and they
// would otherwise dominate the count.
//
// Returns 0 for ProtoUnknown or empty input so the middleware can fall
// through without imposing limits on unrecognized routes.
func EstimateTokens(rawJSON []byte, p Protocol) int {
	if len(rawJSON) == 0 || p == ProtoUnknown {
		return 0
	}
	chars := 0
	switch p {
	case ProtoClaude:
		chars += claudeChars(rawJSON)
	case ProtoOpenAIChat:
		chars += openaiChatChars(rawJSON)
	case ProtoOpenAIResponses:
		chars += openaiResponsesChars(rawJSON)
	case ProtoGemini:
		chars += geminiChars(rawJSON)
	}
	if chars <= 0 {
		return 0
	}
	return chars / charsPerToken
}

// claudeChars walks Anthropic Messages API: top-level `system` (string or
// array), `tools[].description` / `input_schema`, and `messages[].content`
// which is either a string or an array of typed blocks.
func claudeChars(raw []byte) int {
	n := 0

	// system: string or array of {type:text, text:...}
	sys := gjson.GetBytes(raw, "system")
	if sys.Type == gjson.String {
		n += len(sys.String())
	} else if sys.IsArray() {
		sys.ForEach(func(_, block gjson.Result) bool {
			n += len(block.Get("text").String())
			return true
		})
	}

	// tools: schemas are part of the prompt budget
	gjson.GetBytes(raw, "tools").ForEach(func(_, tool gjson.Result) bool {
		n += len(tool.Get("name").String())
		n += len(tool.Get("description").String())
		n += len(tool.Get("input_schema").Raw)
		return true
	})

	gjson.GetBytes(raw, "messages").ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.Type == gjson.String {
			n += len(content.String())
			return true
		}
		if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				n += claudeBlockChars(block)
				return true
			})
		}
		return true
	})
	return n
}

// claudeBlockChars handles one item from a Claude content array. The shapes
// here track the public Anthropic Messages SDK: text, image, document,
// tool_use, tool_result, thinking/reasoning blocks.
func claudeBlockChars(block gjson.Result) int {
	typ := block.Get("type").String()
	switch typ {
	case "text":
		return len(block.Get("text").String())
	case "thinking", "redacted_thinking":
		return len(block.Get("thinking").String()) + len(block.Get("data").String())
	case "tool_use":
		return len(block.Get("name").String()) + len(block.Get("input").Raw)
	case "tool_result":
		// content can be string or nested array
		inner := block.Get("content")
		if inner.Type == gjson.String {
			return len(inner.String())
		}
		if inner.IsArray() {
			n := 0
			inner.ForEach(func(_, sub gjson.Result) bool {
				n += claudeBlockChars(sub)
				return true
			})
			return n
		}
	case "image", "document":
		// base64 source.data is excluded; only metadata counts
		return 0
	}
	// Unknown block types: best-effort count of any embedded text fields.
	return len(block.Get("text").String())
}

// openaiChatChars walks OpenAI Chat Completions: top-level `messages[]`,
// optional `tools[]`. Content is string OR array of typed blocks
// ({type:"text",text:...}, {type:"image_url",...}).
func openaiChatChars(raw []byte) int {
	n := 0

	// Top-level instructions / system on Responses-shaped payloads sneaking
	// into the chat endpoint — handled via the openai_responses path; here
	// we only walk `messages` to avoid double-counting.
	gjson.GetBytes(raw, "messages").ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.Type == gjson.String {
			n += len(content.String())
			return true
		}
		if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				n += openaiChatBlockChars(block)
				return true
			})
		}
		// tool_calls live on the message itself
		msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			n += len(tc.Get("function.name").String())
			n += len(tc.Get("function.arguments").String())
			return true
		})
		return true
	})

	gjson.GetBytes(raw, "tools").ForEach(func(_, tool gjson.Result) bool {
		n += len(tool.Get("function.name").String())
		n += len(tool.Get("function.description").String())
		n += len(tool.Get("function.parameters").Raw)
		return true
	})

	return n
}

func openaiChatBlockChars(block gjson.Result) int {
	typ := block.Get("type").String()
	switch typ {
	case "text", "input_text", "output_text":
		return len(block.Get("text").String())
	case "image_url":
		// Skip data: URLs entirely. http(s) URLs are negligible.
		url := block.Get("image_url.url").String()
		if strings.HasPrefix(url, "data:") {
			return 0
		}
		return len(url)
	}
	return len(block.Get("text").String())
}

// openaiResponsesChars walks OpenAI Responses API: top-level `instructions`
// (string), `input` (string OR array), `tools[]`.
func openaiResponsesChars(raw []byte) int {
	n := 0

	n += len(gjson.GetBytes(raw, "instructions").String())

	input := gjson.GetBytes(raw, "input")
	if input.Type == gjson.String {
		n += len(input.String())
	} else if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			n += responsesItemChars(item)
			return true
		})
	}

	gjson.GetBytes(raw, "tools").ForEach(func(_, tool gjson.Result) bool {
		n += len(tool.Get("name").String())
		n += len(tool.Get("description").String())
		n += len(tool.Get("parameters").Raw)
		return true
	})

	return n
}

func responsesItemChars(item gjson.Result) int {
	// Items may be message/system/developer/user role messages, or function
	// call / function call output, or reasoning blocks.
	typ := item.Get("type").String()
	n := 0
	switch typ {
	case "function_call":
		n += len(item.Get("name").String())
		n += len(item.Get("arguments").String())
	case "function_call_output":
		n += len(item.Get("output").String())
	case "reasoning":
		item.Get("summary").ForEach(func(_, s gjson.Result) bool {
			n += len(s.Get("text").String())
			return true
		})
	default:
		// Standard message item: walk content blocks.
		content := item.Get("content")
		if content.Type == gjson.String {
			n += len(content.String())
		} else if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				n += responsesBlockChars(block)
				return true
			})
		}
	}
	return n
}

func responsesBlockChars(block gjson.Result) int {
	typ := block.Get("type").String()
	switch typ {
	case "input_text", "output_text", "text":
		return len(block.Get("text").String())
	case "input_image":
		url := block.Get("image_url").String()
		if strings.HasPrefix(url, "data:") {
			return 0
		}
		return len(url)
	}
	return len(block.Get("text").String())
}

// geminiChars walks Gemini generateContent: top-level `systemInstruction`,
// `tools[].functionDeclarations[]`, `contents[].parts[]`.
func geminiChars(raw []byte) int {
	n := 0

	gjson.GetBytes(raw, "systemInstruction.parts").ForEach(func(_, part gjson.Result) bool {
		n += len(part.Get("text").String())
		return true
	})

	gjson.GetBytes(raw, "tools").ForEach(func(_, tool gjson.Result) bool {
		tool.Get("functionDeclarations").ForEach(func(_, fn gjson.Result) bool {
			n += len(fn.Get("name").String())
			n += len(fn.Get("description").String())
			n += len(fn.Get("parameters").Raw)
			return true
		})
		return true
	})

	gjson.GetBytes(raw, "contents").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("parts").ForEach(func(_, part gjson.Result) bool {
			n += geminiPartChars(part)
			return true
		})
		return true
	})

	return n
}

func geminiPartChars(part gjson.Result) int {
	if t := part.Get("text"); t.Exists() {
		return len(t.String())
	}
	if fc := part.Get("functionCall"); fc.Exists() {
		return len(fc.Get("name").String()) + len(fc.Get("args").Raw)
	}
	if fr := part.Get("functionResponse"); fr.Exists() {
		return len(fr.Get("name").String()) + len(fr.Get("response").Raw)
	}
	// inlineData / fileData are skipped (base64 / URI not counted).
	return 0
}
