/**
 * Provider-aware token normalization.
 *
 * Different upstream providers report `input_tokens` with different semantics:
 *   - OpenAI Responses / Chat Completions (`gpt-*` surfaced by the Codex
 *     executor): `input_tokens` is the TOTAL prompt size, cached tokens
 *     INCLUDED. `cached_tokens` is a subset of `input_tokens`.
 *   - Anthropic Messages (`claude-*`): `input_tokens` is the NEW prompt size,
 *     cached tokens EXCLUDED. `cache_read_input_tokens` /
 *     `cache_creation_input_tokens` are reported separately.
 *
 * These helpers let extension code treat "new input tokens" (the slice that
 * is actually billed at the full prompt rate) uniformly across providers.
 */

// IMPORTANT: `./keyPivot.ts` inlines a copy of this list to honor its
// "self-contained, no cross-file imports" invariant. Update both together.
const INPUT_INCLUDES_CACHED_PREFIXES = ['gpt-'];

export function inputIncludesCached(modelName: string | undefined | null): boolean {
  if (!modelName) return false;
  return INPUT_INCLUDES_CACHED_PREFIXES.some((prefix) => modelName.startsWith(prefix));
}

export interface TokenCountsLike {
  input_tokens?: number | null;
  cached_tokens?: number | null;
  cache_tokens?: number | null;
}

const toNonNegative = (value: unknown): number => {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? n : 0;
};

const readCached = (tokens: TokenCountsLike): number =>
  Math.max(toNonNegative(tokens.cached_tokens), toNonNegative(tokens.cache_tokens));

/**
 * Return the "new" input tokens (non-cached) for the given usage detail.
 * For OpenAI-style providers this subtracts the cached slice; for Claude it
 * returns `input_tokens` unchanged.
 */
export function getNewInputTokens(
  tokens: TokenCountsLike | null | undefined,
  modelName: string | undefined | null
): number {
  if (!tokens) return 0;
  const input = toNonNegative(tokens.input_tokens);
  if (!inputIncludesCached(modelName)) return input;
  return Math.max(input - readCached(tokens), 0);
}
