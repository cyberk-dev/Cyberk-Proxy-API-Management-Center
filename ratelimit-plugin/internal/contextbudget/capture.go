package contextbudget

import (
	"bytes"
	"regexp"
	"strconv"

	"github.com/gin-gonic/gin"
)

// usageCapturingWriter wraps a gin.ResponseWriter so we can scan the
// upstream response bytes for the token-usage fields *as they stream by*,
// without ever buffering the body. The captured numbers are then handed
// to the Tracker.Record call site (synchronous inside the same request
// goroutine).
//
// We took this route after discovering that the obvious approach —
// registering a coreusage.Plugin and reading the session key from the
// gin.Context inside HandleUsage — fails in production because Gin
// recycles *gin.Context back to its sync.Pool the instant the handler
// chain returns, while HandleUsage runs ASYNCHRONOUSLY off a queue. By
// the time HandleUsage reads `ctx.Value("gin").(*gin.Context).Keys`,
// the keys map has either been wiped by c.reset() or — worse — replaced
// by an unrelated request's state. The fix is to never depend on the
// gin.Context surviving past `c.Next()` return.
//
// The scanner deliberately uses regex over JSON parsing. Streaming SSE
// responses arrive in many small chunks; assembling them just to JSON-
// decode would defeat the point. The usage values we need are stable
// integer fields ("input_tokens":N, "cache_read_input_tokens":M, etc.)
// that the regex matches on a per-chunk basis without context.
type usageCapturingWriter struct {
	gin.ResponseWriter
	protocol Protocol

	// captured fields — refined by max-of-all-matches across chunks.
	// Anthropic's message_start chunk carries input + cache fields up
	// front; later message_delta chunks only emit output_tokens, which
	// we ignore. OpenAI Responses streaming emits usage in the final
	// `response.completed` event.
	inputTokens         int
	cacheReadTokens     int
	cacheCreationTokens int
	openAICachedTokens  int
	geminiCachedTokens  int
	geminiPromptCount   int
	openAIPromptTokens  int
}

func newUsageCapturingWriter(w gin.ResponseWriter, p Protocol) *usageCapturingWriter {
	return &usageCapturingWriter{ResponseWriter: w, protocol: p}
}

// Write scans the chunk for usage fields *before* forwarding to the
// underlying writer. We never modify the bytes — gin.ResponseWriter's
// Write contract requires the byte count returned to match what would
// have been written had we never wrapped it.
func (w *usageCapturingWriter) Write(b []byte) (int, error) {
	w.scan(b)
	return w.ResponseWriter.Write(b)
}

// WriteString mirrors Write for code paths that prefer string emission
// (gin-contrib/sse asserts the WriteString interface on the underlying
// writer when emitting SSE events). Without this override the scan
// would miss SSE chunks.
func (w *usageCapturingWriter) WriteString(s string) (int, error) {
	w.scan([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

// EffectiveInputTokens collapses the captured fields into the single
// number the middleware uses for soft/hard checks on the NEXT turn.
// Returns 0 when no usage was seen (translator-emitted fake responses,
// errors, etc.) — caller treats that as "don't record".
//
// The wrapper is only ever installed by middleware.go when the request's
// protocol is one of the four known values; ProtoUnknown short-circuits
// earlier in the chain. So there's no `default` case here — an unknown
// protocol reaching this method indicates a wiring bug, not user input.
func (w *usageCapturingWriter) EffectiveInputTokens() int {
	switch w.protocol {
	case ProtoClaude:
		// Anthropic: input + cache_read + cache_creation are disjoint
		// partitions that sum to true context size. We capture all
		// three directly from the response (CLIProxyAPI's executor
		// parses only one cache field into Detail.CachedTokens — by
		// reading the raw response we sidestep that lossy step).
		return w.inputTokens + w.cacheReadTokens + w.cacheCreationTokens
	case ProtoOpenAIChat, ProtoOpenAIResponses:
		// prompt_tokens (Chat) / input_tokens (Responses) ALREADY
		// include cached_tokens per OpenAI's documented accounting —
		// don't double-count.
		return w.openAIPromptTokens
	case ProtoGemini:
		// promptTokenCount ALREADY includes cachedContentTokenCount.
		return w.geminiPromptCount
	}
	return 0
}

// Regexes are package-level so they compile once. Each captures an
// integer value to group 1. Whitespace between key/colon/value is
// permitted because some serializers emit `"k" : N` rather than `"k":N`.
var (
	reInputTokens         = regexp.MustCompile(`"input_tokens"\s*:\s*(\d+)`)
	reCacheReadTokens     = regexp.MustCompile(`"cache_read_input_tokens"\s*:\s*(\d+)`)
	reCacheCreationTokens = regexp.MustCompile(`"cache_creation_input_tokens"\s*:\s*(\d+)`)
	rePromptTokens        = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)
	reOpenAICachedTokens  = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)
	rePromptTokenCount    = regexp.MustCompile(`"promptTokenCount"\s*:\s*(\d+)`)
	reGeminiCachedTokens  = regexp.MustCompile(`"cachedContentTokenCount"\s*:\s*(\d+)`)
)

// scan extracts the highest-value occurrence of each usage field.
// "Highest-value" because Anthropic's SSE emits initial usage in
// message_start and may refine cache totals in later events; taking
// max-of-all-matches is safe because usage fields are monotonically
// non-decreasing within one response cycle.
//
// LLM-generated text that *looks* like a usage field is not a false-
// positive risk: any such characters live inside JSON-string values
// (`"text": "...\"input_tokens\":1234..."`) where the leading `"` is
// JSON-escaped to `\"`, breaking the regex prefix. Verified for
// Anthropic SSE, OpenAI Chat/Responses SSE, and Gemini NDJSON which
// all wrap model output in JSON strings.
//
// The bytes.Contains pre-filter avoids running regex on chunks that
// don't even mention the field name — the vast majority of streaming
// content_block_delta events contain just model text, never usage.
func (w *usageCapturingWriter) scan(b []byte) {
	if len(b) == 0 {
		return
	}
	switch w.protocol {
	case ProtoClaude:
		if bytes.Contains(b, claudeUsageNeedle) {
			w.inputTokens = maxIntFromRegex(reInputTokens, b, w.inputTokens)
			w.cacheReadTokens = maxIntFromRegex(reCacheReadTokens, b, w.cacheReadTokens)
			w.cacheCreationTokens = maxIntFromRegex(reCacheCreationTokens, b, w.cacheCreationTokens)
		}
	case ProtoOpenAIChat, ProtoOpenAIResponses:
		// Chat API emits `prompt_tokens`; Responses API emits
		// `input_tokens` (different field name). Try the Chat field
		// first, then fall back to the Responses field — the fallback
		// is REQUIRED, not optional, for Responses streaming.
		if bytes.Contains(b, openaiUsageNeedle) || bytes.Contains(b, openaiInputUsageNeedle) {
			w.openAIPromptTokens = maxIntFromRegex(rePromptTokens, b, w.openAIPromptTokens)
			if w.openAIPromptTokens == 0 {
				w.openAIPromptTokens = maxIntFromRegex(reInputTokens, b, w.openAIPromptTokens)
			}
			w.openAICachedTokens = maxIntFromRegex(reOpenAICachedTokens, b, w.openAICachedTokens)
		}
	case ProtoGemini:
		if bytes.Contains(b, geminiUsageNeedle) {
			w.geminiPromptCount = maxIntFromRegex(rePromptTokenCount, b, w.geminiPromptCount)
			w.geminiCachedTokens = maxIntFromRegex(reGeminiCachedTokens, b, w.geminiCachedTokens)
		}
	}
}

// Pre-filter needles. bytes.Contains is a vectorized SIMD search on
// modern Go runtimes — orders of magnitude faster than the regex
// engine for the common case of "chunk doesn't mention this field".
var (
	claudeUsageNeedle      = []byte(`"input_tokens"`)
	openaiUsageNeedle      = []byte(`"prompt_tokens"`)
	openaiInputUsageNeedle = []byte(`"input_tokens"`)
	geminiUsageNeedle      = []byte(`"promptTokenCount"`)
)

// maxIntFromRegex finds all matches and returns max(matches..., seed).
// Using max over all matches lets us pick up the final value when
// providers emit progressively-refined usage in streaming chunks.
func maxIntFromRegex(re *regexp.Regexp, b []byte, seed int) int {
	matches := re.FindAllSubmatch(b, -1)
	best := seed
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			continue
		}
		if n > best {
			best = n
		}
	}
	return best
}
