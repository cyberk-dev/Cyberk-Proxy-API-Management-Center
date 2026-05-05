export interface ModelPrice {
  prompt: number;
  completion: number;
  cache: number;
}

// Prices in USD per 1M tokens. Source: docs/model-pricing.csv (2026-04-24).
// Keep `completion` (output) and `cache` (cache read) aligned with how
// `calculateCost` splits tokens: prompt × (input - cached) + cache × cached +
// completion × output.
//
// Models the CSV could not price (e.g. research-preview-only) are omitted, so
// they still render cost=0 until the user supplies a manual override.
export const DEFAULT_MODEL_PRICES: Record<string, ModelPrice> = {
  'gpt-5.5': { prompt: 5.0, completion: 30.0, cache: 0.5 },
  'gpt-5.4': { prompt: 2.5, completion: 15.0, cache: 0.25 },
  'gpt-5.4-mini': { prompt: 0.75, completion: 4.5, cache: 0.075 },
  'gpt-5.3-codex': { prompt: 1.75, completion: 14.0, cache: 0.175 },
  'gpt-5.2': { prompt: 1.75, completion: 14.0, cache: 0.175 },
  'gpt-image-2': { prompt: 8.0, completion: 30.0, cache: 2.0 },

  'claude-haiku-4-5-20251001': { prompt: 1.0, completion: 5.0, cache: 0.1 },
  'claude-sonnet-4-6': { prompt: 3.0, completion: 15.0, cache: 0.3 },
  'claude-sonnet-4-5': { prompt: 3.0, completion: 15.0, cache: 0.3 },
  'claude-opus-4-7': { prompt: 5.0, completion: 25.0, cache: 0.5 },
  'claude-opus-4-6': { prompt: 5.0, completion: 25.0, cache: 0.5 },
  'claude-opus-4-5': { prompt: 5.0, completion: 25.0, cache: 0.5 },

  'kimi-k2.5': { prompt: 0.6, completion: 3.0, cache: 0.1 },
  'kimi-k2.6': { prompt: 0.95, completion: 4.0, cache: 0.16 },

  'qwen3.5-plus': { prompt: 0.4, completion: 2.4, cache: 0.08 },
  'qwen3.6-plus': { prompt: 0.5, completion: 3.0, cache: 0.1 },
  'qwen3-coder-next': { prompt: 0.3, completion: 1.5, cache: 0 },
  'qwen3-max-2026-01-23': { prompt: 1.2, completion: 6.0, cache: 0.24 },

  'glm-5': { prompt: 1.0, completion: 3.2, cache: 0.2 },
  'glm-5.1': { prompt: 1.4, completion: 4.4, cache: 0.26 },

  'MiniMax-M2.5': { prompt: 0.3, completion: 1.2, cache: 0.03 },
  'MiniMax-M2.7': { prompt: 0.3, completion: 1.2, cache: 0.06 }
};

// Keys sorted by length desc so the first prefix match is the most specific
// one (e.g. `gpt-5.4-mini` wins over `gpt-5.4` when the model name starts
// with `gpt-5.4-mini-…`).
const DEFAULT_KEYS_BY_LENGTH = Object.keys(DEFAULT_MODEL_PRICES).sort(
  (a, b) => b.length - a.length
);

/**
 * Look up a built-in default price for `modelName`. Matches exact first, then
 * falls back to the longest configured prefix where the next character is a
 * `-` (or end-of-string) to avoid `gpt-5.4` matching `gpt-5.40`.
 */
export function resolveDefaultModelPrice(modelName: string): ModelPrice | undefined {
  if (!modelName) return undefined;
  const exact = DEFAULT_MODEL_PRICES[modelName];
  if (exact) return exact;
  for (const key of DEFAULT_KEYS_BY_LENGTH) {
    if (modelName.length <= key.length) continue;
    if (modelName.startsWith(key) && modelName.charAt(key.length) === '-') {
      return DEFAULT_MODEL_PRICES[key];
    }
  }
  return undefined;
}

/**
 * Resolve the effective price for a model: user-configured override wins,
 * otherwise fall back to the bundled default.
 */
export function resolveModelPrice(
  modelName: string,
  userPrices: Record<string, ModelPrice> | undefined | null
): ModelPrice | undefined {
  if (userPrices && userPrices[modelName]) return userPrices[modelName];
  return resolveDefaultModelPrice(modelName);
}
