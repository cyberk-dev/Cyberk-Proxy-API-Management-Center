/**
 * Pivot a raw CLIProxyAPI usage snapshot by top-level apiKey.
 *
 * This module is deliberately self-contained (no `@/` imports) so the test
 * suite can run under bare `node --test` without a bundler alias resolver.
 * The cost function is injected by the caller (React pages pass
 * `calculateCost` from `@/utils/usage`).
 */

export interface TokenBundle {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  cached_tokens?: number;
  cache_tokens?: number;
  reasoning_tokens?: number;
}

export interface ModelPrice {
  prompt: number;
  completion: number;
  cache: number;
}

/**
 * Signature mirrors `calculateCost` in `src/utils/usage.ts` but takes the
 * model name and token bundle directly (rather than a full UsageDetail).
 */
export type CostFn = (modelName: string, tokens: TokenBundle) => number;

export interface PerKeyModelStats {
  model: string;
  requests: number;
  successCount: number;
  failureCount: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  totalTokens: number;
  cost: number;
  lastActiveMs: number;
}

export interface PerKeyStats {
  apiKey: string;
  alias?: string;
  totalRequests: number;
  successCount: number;
  failureCount: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  totalTokens: number;
  totalCost: number;
  lastActiveMs: number;
  perModel: PerKeyModelStats[];
  /** True if the key exists in usage data but not in the current api-keys config. */
  orphan: boolean;
}

const TOKENS_PER_PRICE_UNIT = 1_000_000;

const isRecord = (v: unknown): v is Record<string, unknown> =>
  v !== null && typeof v === 'object' && !Array.isArray(v);

function safeNumber(v: unknown): number {
  const n = typeof v === 'number' ? v : Number(v);
  return Number.isFinite(n) ? Math.max(n, 0) : 0;
}

function parseTsMs(ts: unknown): number {
  if (typeof ts !== 'string' || !ts) return 0;
  const ms = Date.parse(ts);
  return Number.isFinite(ms) ? ms : 0;
}

function newModelStats(model: string): PerKeyModelStats {
  return {
    model,
    requests: 0,
    successCount: 0,
    failureCount: 0,
    inputTokens: 0,
    outputTokens: 0,
    cachedTokens: 0,
    totalTokens: 0,
    cost: 0,
    lastActiveMs: 0
  };
}

/**
 * Build a cost function from a model price map, mirroring the formula in
 * `src/utils/usage.ts::calculateCost` so runtime and tests stay in sync.
 * Exposed so tests don't need to import the upstream helper.
 */
export function makeCostFn(modelPrices: Record<string, ModelPrice>): CostFn {
  return (modelName, tokens) => {
    const price = modelPrices[modelName];
    if (!price) return 0;
    const rawInput = safeNumber(tokens.input_tokens);
    const rawOutput = safeNumber(tokens.output_tokens);
    const rawCachedPrimary = safeNumber(tokens.cached_tokens);
    const rawCachedAlternate = safeNumber(tokens.cache_tokens);
    const cachedTokens = Math.max(rawCachedPrimary, rawCachedAlternate);
    const promptTokens = Math.max(rawInput - cachedTokens, 0);
    const promptCost = (promptTokens / TOKENS_PER_PRICE_UNIT) * (Number(price.prompt) || 0);
    const cachedCost = (cachedTokens / TOKENS_PER_PRICE_UNIT) * (Number(price.cache) || 0);
    const completionCost = (rawOutput / TOKENS_PER_PRICE_UNIT) * (Number(price.completion) || 0);
    const total = promptCost + cachedCost + completionCost;
    return Number.isFinite(total) && total > 0 ? total : 0;
  };
}

export function pivotByKey(
  usage: unknown,
  knownApiKeys: string[],
  aliases: Record<string, string>,
  costFn: CostFn
): PerKeyStats[] {
  const known = new Set(knownApiKeys);
  const usageRecord = isRecord(usage) ? usage : null;
  const apisRaw = usageRecord ? usageRecord.apis : null;
  if (!isRecord(apisRaw)) return [];

  const result: PerKeyStats[] = [];

  for (const [apiKey, apiEntry] of Object.entries(apisRaw)) {
    if (!isRecord(apiEntry)) continue;
    const modelsRaw = apiEntry.models;
    const models = isRecord(modelsRaw) ? modelsRaw : null;

    const stats: PerKeyStats = {
      apiKey,
      alias: aliases[apiKey],
      totalRequests: 0,
      successCount: 0,
      failureCount: 0,
      inputTokens: 0,
      outputTokens: 0,
      cachedTokens: 0,
      totalTokens: 0,
      totalCost: 0,
      lastActiveMs: 0,
      perModel: [],
      orphan: !known.has(apiKey)
    };

    if (!models) {
      result.push(stats);
      continue;
    }

    for (const [modelName, modelEntry] of Object.entries(models)) {
      if (!isRecord(modelEntry)) continue;
      const detailsRaw = modelEntry.details;
      const details = Array.isArray(detailsRaw) ? detailsRaw : [];
      const m = newModelStats(modelName);

      for (const raw of details) {
        if (!isRecord(raw)) continue;
        const tokensRaw = (isRecord(raw.tokens) ? raw.tokens : {}) as TokenBundle;
        const inputT = safeNumber(tokensRaw.input_tokens);
        const outT = safeNumber(tokensRaw.output_tokens);
        const cachedT = Math.max(
          safeNumber(tokensRaw.cached_tokens),
          safeNumber(tokensRaw.cache_tokens)
        );
        const totalT = safeNumber(tokensRaw.total_tokens) || inputT + outT;
        const failed = raw.failed === true;
        const tsMs = parseTsMs(raw.timestamp);
        const cost = costFn(modelName, tokensRaw);

        m.requests += 1;
        if (failed) m.failureCount += 1;
        else m.successCount += 1;
        m.inputTokens += inputT;
        m.outputTokens += outT;
        m.cachedTokens += cachedT;
        m.totalTokens += totalT;
        m.cost += cost;
        if (tsMs > m.lastActiveMs) m.lastActiveMs = tsMs;

        stats.totalRequests += 1;
        if (failed) stats.failureCount += 1;
        else stats.successCount += 1;
        stats.inputTokens += inputT;
        stats.outputTokens += outT;
        stats.cachedTokens += cachedT;
        stats.totalTokens += totalT;
        stats.totalCost += cost;
        if (tsMs > stats.lastActiveMs) stats.lastActiveMs = tsMs;
      }

      if (m.requests > 0) {
        stats.perModel.push(m);
      }
    }

    stats.perModel.sort((a, b) => b.requests - a.requests);
    result.push(stats);
  }

  result.sort((a, b) => {
    if (a.orphan !== b.orphan) return a.orphan ? 1 : -1;
    return b.totalRequests - a.totalRequests;
  });
  return result;
}

export function successRate(stats: PerKeyStats | PerKeyModelStats): number {
  const total = 'totalRequests' in stats ? stats.totalRequests : stats.requests;
  if (total <= 0) return 0;
  return (stats.successCount / total) * 100;
}
