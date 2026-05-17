package promptlog

import (
	"bufio"
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// parseAssistantResponse returns the assistant-side blocks reconstructed
// from the upstream response body. body may be either a single JSON object
// (non-streaming reply), a sequence of SSE frames (`event: ... \n data:
// {...}` per the four providers' streaming spec), or a JSON array of stream
// chunks (Gemini's stream form). Truncated bodies — produced when the
// response writer's buffer cap is hit — still parse: the partial body
// yields whatever blocks survived, plus the caller marks BodyTruncated on
// the entry so consumers know to expect missing tail content.
//
// All tool_use blocks emitted here are reference-only (Tool, Bytes,
// SHA256), matching the user-side extractor. Thinking / reasoning content
// is captured as a reference-only block of type "thinking" so the log shows
// "the model thought N bytes" without ballooning storage with raw chain of
// thought.
func parseAssistantResponse(body []byte, provider string, maxText int) []Block {
	if len(body) == 0 {
		return nil
	}
	if isSSE(body) {
		return parseSSE(body, provider, maxText)
	}
	// Some Gemini streaming variants return a top-level JSON array of
	// chunks rather than SSE. Treat that as a sequence of full responses
	// and merge their blocks.
	if provider == ProviderGemini && body[0] == '[' {
		return parseGeminiStreamArray(body, maxText)
	}
	return parseAssistantJSON(body, provider, maxText)
}

// isSSE sniffs the first non-blank line for an SSE marker. Cheap (does not
// allocate when the prefix mismatches) and unambiguous: JSON bodies start
// with `{` or `[`, never `event:` / `data:`.
func isSSE(body []byte) bool {
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		rest := body[i:]
		return bytes.HasPrefix(rest, []byte("event:")) || bytes.HasPrefix(rest, []byte("data:"))
	}
	return false
}

// parseAssistantJSON handles a single non-streaming response body for each
// provider. Returns nil when the shape doesn't match (e.g. an error
// response).
func parseAssistantJSON(body []byte, provider string, maxText int) []Block {
	switch provider {
	case ProviderAnthropic:
		return parseAnthropicMessage(body, maxText)
	case ProviderOpenAIChat:
		return parseOpenAIChatChoice(body, maxText)
	case ProviderOpenAIResponses:
		return parseOpenAIResponsesOutput(body, maxText)
	case ProviderGemini:
		return parseGeminiCandidate(body, maxText)
	}
	return nil
}

// parseAnthropicMessage walks the top-level `content[]` array, the same
// shape returned by `/v1/messages` non-streaming. tool_use carries the full
// `input` object; we hash it and record size, matching user-side semantics.
func parseAnthropicMessage(body []byte, maxText int) []Block {
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return nil
	}
	var blocks []Block
	content.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "text":
			text, trunc, orig := truncateText(item.Get("text").String(), maxText)
			blocks = append(blocks, textBlock(text, trunc, orig))
		case "tool_use":
			blocks = append(blocks, toolBlock("tool_use", item.Get("name").String(), item.Get("input").Raw, maxText, false))
		case "thinking":
			// Anthropic extended-thinking block. Head+tail-truncated so a
			// long chain of thought still shows its opening framing and
			// final conclusion without saving the entire interior.
			blocks = append(blocks, toolBlock("thinking", "", item.Get("thinking").String(), maxText, false))
		}
		return true
	})
	return blocks
}

// parseOpenAIChatChoice handles `/v1/chat/completions` non-streaming
// responses. Only choices[0] is examined — n>1 is rare in production proxy
// traffic, and capturing only the first keeps storage predictable.
func parseOpenAIChatChoice(body []byte, maxText int) []Block {
	msg := gjson.GetBytes(body, "choices.0.message")
	if !msg.Exists() {
		return nil
	}
	var blocks []Block
	if c := msg.Get("content"); c.Type == gjson.String && c.String() != "" {
		text, trunc, orig := truncateText(c.String(), maxText)
		blocks = append(blocks, textBlock(text, trunc, orig))
	} else if c.IsArray() {
		// Some models stream multi-part content; collect each text part.
		c.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				text, trunc, orig := truncateText(part.Get("text").String(), maxText)
				blocks = append(blocks, textBlock(text, trunc, orig))
			}
			return true
		})
	}
	// reasoning_content (o1 / o3) — head+tail-truncated so the reasoning
	// preamble and conclusion stay legible. Some SDKs name the field
	// "reasoning" instead; check both.
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if r := msg.Get(key); r.Exists() && r.String() != "" {
			blocks = append(blocks, toolBlock("thinking", "", r.String(), maxText, false))
			break
		}
	}
	msg.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
		args := tc.Get("function.arguments").String()
		blocks = append(blocks, toolBlock("tool_use", tc.Get("function.name").String(), args, maxText, false))
		return true
	})
	return blocks
}

// parseOpenAIResponsesOutput handles `/v1/responses` non-streaming. The
// top-level `output` array carries typed items: `message` (with nested
// content[] of output_text / refusal), `function_call`, and `reasoning`.
func parseOpenAIResponsesOutput(body []byte, maxText int) []Block {
	out := gjson.GetBytes(body, "output")
	if !out.IsArray() {
		return nil
	}
	var blocks []Block
	out.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "message":
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "output_text", "text":
					text, trunc, orig := truncateText(part.Get("text").String(), maxText)
					blocks = append(blocks, textBlock(text, trunc, orig))
				case "refusal":
					text, trunc, orig := truncateText(part.Get("refusal").String(), maxText)
					b := textBlock(text, trunc, orig)
					b.Type = "refusal"
					blocks = append(blocks, b)
				}
				return true
			})
		case "function_call":
			blocks = append(blocks, toolBlock("tool_use", item.Get("name").String(), item.Get("arguments").String(), maxText, false))
		case "reasoning":
			// Reasoning items expose a `summary` array of `summary_text`
			// parts. Concatenate then head+tail-truncate so the surviving
			// snippet covers the start and end of the reasoning trace.
			var sb strings.Builder
			item.Get("summary").ForEach(func(_, sum gjson.Result) bool {
				sb.WriteString(sum.Get("text").String())
				return true
			})
			t := sb.String()
			if t == "" {
				return true
			}
			blocks = append(blocks, toolBlock("thinking", "", t, maxText, false))
		}
		return true
	})
	return blocks
}

// parseGeminiCandidate handles a single `:generateContent` response.
// candidates[0].content.parts mirrors the request-side parts shape.
func parseGeminiCandidate(body []byte, maxText int) []Block {
	parts := gjson.GetBytes(body, "candidates.0.content.parts")
	if !parts.IsArray() {
		return nil
	}
	var blocks []Block
	parts.ForEach(func(_, part gjson.Result) bool {
		if t := part.Get("text"); t.Exists() && t.String() != "" {
			if part.Get("thought").Bool() {
				blocks = append(blocks, toolBlock("thinking", "", t.String(), maxText, false))
				return true
			}
			text, trunc, orig := truncateText(t.String(), maxText)
			blocks = append(blocks, textBlock(text, trunc, orig))
			return true
		}
		if fc := part.Get("functionCall"); fc.Exists() {
			blocks = append(blocks, toolBlock("tool_use", fc.Get("name").String(), fc.Get("args").Raw, maxText, false))
		}
		return true
	})
	return blocks
}

// parseGeminiStreamArray handles the `:streamGenerateContent` JSON-array
// form (`[{chunk1}, {chunk2}, ...]`). Each element is a candidate-shaped
// chunk; merging is the same as concatenating their parts. Text and
// thinking deltas are joined into single blocks to keep block count low.
func parseGeminiStreamArray(body []byte, maxText int) []Block {
	arr := gjson.ParseBytes(body)
	if !arr.IsArray() {
		return nil
	}
	var textSB, thinkSB strings.Builder
	var toolBlocks []Block
	arr.ForEach(func(_, chunk gjson.Result) bool {
		chunk.Get("candidates.0.content.parts").ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text"); t.Exists() {
				if part.Get("thought").Bool() {
					thinkSB.WriteString(t.String())
					return true
				}
				textSB.WriteString(t.String())
				return true
			}
			if fc := part.Get("functionCall"); fc.Exists() {
				toolBlocks = append(toolBlocks, toolBlock("tool_use", fc.Get("name").String(), fc.Get("args").Raw, maxText, false))
			}
			return true
		})
		return true
	})
	return finishAssembled(textSB.String(), thinkSB.String(), toolBlocks, maxText)
}

// parseSSE assembles a single response from the streamed SSE frames. The
// frame format ("event: T\ndata: J\n\n") is consistent across providers;
// only the JSON payload shapes differ, so we dispatch per-provider after
// extracting each data line. Some providers (Anthropic) carry the event
// name on a separate line; we ignore it and rely on the payload's `type`.
func parseSSE(body []byte, provider string, maxText int) []Block {
	switch provider {
	case ProviderAnthropic:
		return parseAnthropicSSE(body, maxText)
	case ProviderOpenAIChat:
		return parseOpenAIChatSSE(body, maxText)
	case ProviderOpenAIResponses:
		return parseOpenAIResponsesSSE(body, maxText)
	case ProviderGemini:
		return parseGeminiSSE(body, maxText)
	}
	return nil
}

// sseDataLines yields each `data: <json>` payload from body in order,
// skipping `event:`, `id:`, `retry:`, comments, and the trailing `[DONE]`
// sentinel emitted by OpenAI-compatible streams.
func sseDataLines(body []byte, fn func(payload []byte) bool) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if !fn(payload) {
			return
		}
	}
}

// parseAnthropicSSE walks message_start / content_block_* / message_delta
// frames and assembles per-index content. tool_use input is delivered as
// `input_json_delta.partial_json` strings that must be concatenated before
// hashing — the final hash + size mirror what a non-streaming response
// would have produced.
func parseAnthropicSSE(body []byte, maxText int) []Block {
	type acc struct {
		kind     string // "text" | "tool_use" | "thinking"
		text     strings.Builder
		toolName string
		toolJSON strings.Builder
	}
	byIndex := map[int64]*acc{}
	var order []int64
	sseDataLines(body, func(payload []byte) bool {
		j := gjson.ParseBytes(payload)
		switch j.Get("type").String() {
		case "content_block_start":
			idx := j.Get("index").Int()
			block := j.Get("content_block")
			a := &acc{kind: block.Get("type").String()}
			if a.kind == "tool_use" {
				a.toolName = block.Get("name").String()
			}
			if _, ok := byIndex[idx]; !ok {
				order = append(order, idx)
			}
			byIndex[idx] = a
		case "content_block_delta":
			idx := j.Get("index").Int()
			a, ok := byIndex[idx]
			if !ok {
				return true
			}
			delta := j.Get("delta")
			switch delta.Get("type").String() {
			case "text_delta":
				a.text.WriteString(delta.Get("text").String())
			case "thinking_delta":
				a.text.WriteString(delta.Get("thinking").String())
			case "input_json_delta":
				a.toolJSON.WriteString(delta.Get("partial_json").String())
			}
		}
		return true
	})
	var blocks []Block
	for _, idx := range order {
		a := byIndex[idx]
		switch a.kind {
		case "text":
			if a.text.Len() == 0 {
				continue
			}
			text, trunc, orig := truncateText(a.text.String(), maxText)
			blocks = append(blocks, textBlock(text, trunc, orig))
		case "thinking":
			t := a.text.String()
			if t == "" {
				continue
			}
			blocks = append(blocks, toolBlock("thinking", "", t, maxText, false))
		case "tool_use":
			blocks = append(blocks, toolBlock("tool_use", a.toolName, a.toolJSON.String(), maxText, false))
		}
	}
	return blocks
}

// parseOpenAIChatSSE folds delta.content + delta.tool_calls[] deltas into
// a single text block and one tool_use per tool_calls index. tool_calls
// deltas use stable indexes that identify the same call across chunks.
func parseOpenAIChatSSE(body []byte, maxText int) []Block {
	var textSB strings.Builder
	var thinkSB strings.Builder
	type tc struct {
		name string
		args strings.Builder
	}
	tools := map[int64]*tc{}
	var toolOrder []int64
	sseDataLines(body, func(payload []byte) bool {
		delta := gjson.GetBytes(payload, "choices.0.delta")
		if c := delta.Get("content"); c.Exists() && c.Type == gjson.String {
			textSB.WriteString(c.String())
		}
		for _, key := range []string{"reasoning_content", "reasoning"} {
			if r := delta.Get(key); r.Exists() && r.Type == gjson.String {
				thinkSB.WriteString(r.String())
				break
			}
		}
		delta.Get("tool_calls").ForEach(func(_, t gjson.Result) bool {
			idx := t.Get("index").Int()
			cur, ok := tools[idx]
			if !ok {
				cur = &tc{}
				tools[idx] = cur
				toolOrder = append(toolOrder, idx)
			}
			if name := t.Get("function.name").String(); name != "" {
				cur.name = name
			}
			cur.args.WriteString(t.Get("function.arguments").String())
			return true
		})
		return true
	})
	var blocks []Block
	if textSB.Len() > 0 {
		text, trunc, orig := truncateText(textSB.String(), maxText)
		blocks = append(blocks, textBlock(text, trunc, orig))
	}
	if thinkSB.Len() > 0 {
		blocks = append(blocks, toolBlock("thinking", "", thinkSB.String(), maxText, false))
	}
	for _, idx := range toolOrder {
		t := tools[idx]
		blocks = append(blocks, toolBlock("tool_use", t.name, t.args.String(), maxText, false))
	}
	return blocks
}

// parseOpenAIResponsesSSE follows the `response.*` event family. Text
// deltas land on `response.output_text.delta`; function-call args on
// `response.function_call_arguments.delta`; reasoning on
// `response.reasoning.delta` (rare) or via a final `response.completed`
// event whose payload mirrors the non-streaming output[].
func parseOpenAIResponsesSSE(body []byte, maxText int) []Block {
	var textSB strings.Builder
	var thinkSB strings.Builder
	type fc struct {
		name string
		args strings.Builder
	}
	fcs := map[string]*fc{}
	var fcOrder []string
	var final []byte
	sseDataLines(body, func(payload []byte) bool {
		j := gjson.ParseBytes(payload)
		switch j.Get("type").String() {
		case "response.output_text.delta":
			textSB.WriteString(j.Get("delta").String())
		case "response.reasoning_summary_text.delta", "response.reasoning.delta":
			thinkSB.WriteString(j.Get("delta").String())
		case "response.function_call_arguments.delta":
			id := j.Get("item_id").String()
			cur, ok := fcs[id]
			if !ok {
				cur = &fc{name: j.Get("name").String()}
				fcs[id] = cur
				fcOrder = append(fcOrder, id)
			}
			cur.args.WriteString(j.Get("delta").String())
		case "response.output_item.added":
			item := j.Get("item")
			if item.Get("type").String() == "function_call" {
				id := item.Get("id").String()
				if id == "" {
					id = item.Get("call_id").String()
				}
				if _, ok := fcs[id]; !ok {
					fcs[id] = &fc{name: item.Get("name").String()}
					fcOrder = append(fcOrder, id)
				}
			}
		case "response.completed":
			// Final snapshot — useful as a fallback when deltas were missed.
			final = []byte(j.Get("response").Raw)
		}
		return true
	})
	var blocks []Block
	if textSB.Len() > 0 {
		text, trunc, orig := truncateText(textSB.String(), maxText)
		blocks = append(blocks, textBlock(text, trunc, orig))
	}
	if thinkSB.Len() > 0 {
		blocks = append(blocks, toolBlock("thinking", "", thinkSB.String(), maxText, false))
	}
	for _, id := range fcOrder {
		f := fcs[id]
		blocks = append(blocks, toolBlock("tool_use", f.name, f.args.String(), maxText, false))
	}
	// When the SSE buffer was truncated mid-stream, a tail of
	// `response.function_call_arguments.delta` events may have been dropped
	// — we'd then return text without the tool calls that actually fired.
	// `response.completed` (always the last event) carries the FULL final
	// output snapshot, so we union its tool_use blocks into ours, keyed by
	// (tool name + arg hash) to dedupe with what deltas already produced.
	// SSE-assembled text remains canonical: deltas are reliable in order
	// and arrived before the cap was hit.
	if len(final) > 0 {
		finalBlocks := parseOpenAIResponsesOutput(final, maxText)
		// If we got nothing at all from SSE, the final snapshot is the
		// authoritative reply — return it as-is.
		if len(blocks) == 0 {
			return finalBlocks
		}
		// Dedup key combines tool name with the truncated content prefix.
		// Two calls that share the same head+tail snippet must have come
		// from the same arguments (truncation is deterministic), so this
		// catches the legitimate duplicate without sha256 in the block.
		dedupKey := func(b Block) string { return b.Tool + "|" + b.Text }
		seen := make(map[string]struct{}, len(blocks))
		for _, b := range blocks {
			if b.Type == "tool_use" {
				seen[dedupKey(b)] = struct{}{}
			}
		}
		for _, b := range finalBlocks {
			if b.Type != "tool_use" {
				continue
			}
			if _, ok := seen[dedupKey(b)]; ok {
				continue
			}
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// parseGeminiSSE walks Gemini's SSE-form stream. Each `data:` payload is a
// full candidate chunk — same shape as a non-streaming response — so we
// concatenate their `parts[].text`. functionCall parts arrive whole.
func parseGeminiSSE(body []byte, maxText int) []Block {
	var textSB, thinkSB strings.Builder
	var toolBlocks []Block
	sseDataLines(body, func(payload []byte) bool {
		gjson.GetBytes(payload, "candidates.0.content.parts").ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text"); t.Exists() {
				if part.Get("thought").Bool() {
					thinkSB.WriteString(t.String())
					return true
				}
				textSB.WriteString(t.String())
				return true
			}
			if fc := part.Get("functionCall"); fc.Exists() {
				toolBlocks = append(toolBlocks, toolBlock("tool_use", fc.Get("name").String(), fc.Get("args").Raw, maxText, false))
			}
			return true
		})
		return true
	})
	return finishAssembled(textSB.String(), thinkSB.String(), toolBlocks, maxText)
}

// finishAssembled is the common tail for stream-merge parsers: build a
// single text block + a thinking block + the per-tool blocks. Each
// content type goes through head+tail truncation so a verbose model reply
// or chain of thought still leaves readable snippets in the log.
func finishAssembled(text, thinkText string, tools []Block, maxText int) []Block {
	var blocks []Block
	if text != "" {
		t, trunc, orig := truncateText(text, maxText)
		blocks = append(blocks, textBlock(t, trunc, orig))
	}
	if thinkText != "" {
		blocks = append(blocks, toolBlock("thinking", "", thinkText, maxText, false))
	}
	blocks = append(blocks, tools...)
	return blocks
}
