/**
 * Time-range filter for the Users page.
 *
 * Mirrors the shape of `filterUsageByTimeRange` in `src/utils/usage.ts` but
 * supports a different set of windows (`1h` / `5h` / `24h` / `7d` / `all`) so
 * the Users page can offer finer granularity without touching shared code.
 *
 * Self-contained (no `@/` imports) so the extension test suite can run under
 * bare `node --test`.
 */

export type UsersTimeRange = '1h' | '5h' | '24h' | '7d' | 'all';

export const USERS_TIME_RANGE_OPTIONS: readonly UsersTimeRange[] = [
  '1h',
  '5h',
  '24h',
  '7d',
  'all'
];

const HOUR_MS = 60 * 60 * 1000;
const WINDOW_MS: Record<Exclude<UsersTimeRange, 'all'>, number> = {
  '1h': 1 * HOUR_MS,
  '5h': 5 * HOUR_MS,
  '24h': 24 * HOUR_MS,
  '7d': 7 * 24 * HOUR_MS
};
// Tolerance for client/server clock drift. Without this a request whose
// timestamp lands a few seconds ahead of our `Date.now()` would be dropped
// entirely — users see an empty table right after activity.
const CLOCK_SKEW_MS = 60_000;

export function isUsersTimeRange(value: unknown): value is UsersTimeRange {
  return (
    value === '1h' ||
    value === '5h' ||
    value === '24h' ||
    value === '7d' ||
    value === 'all'
  );
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  v !== null && typeof v === 'object' && !Array.isArray(v);

function parseTsMs(v: unknown): number {
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (v instanceof Date) return v.getTime();
  if (typeof v !== 'string') return Number.NaN;
  const trimmed = v.trim();
  if (!trimmed) return Number.NaN;
  return Date.parse(trimmed);
}

function extractTotalTokens(detail: Record<string, unknown>): number {
  const tokensRaw = detail.tokens;
  if (!isRecord(tokensRaw)) return 0;
  if (typeof tokensRaw.total_tokens === 'number') return tokensRaw.total_tokens;
  // Fallback when upstream didn't emit `total_tokens`. Keep the formula
  // consistent with `keyPivot.ts` (rawInput + output): do NOT add `cached` or
  // `reasoning`, since for Codex `input_tokens` already includes the cached
  // slice and `output_tokens` already includes `reasoning_tokens` — adding
  // them again silently double/triple-counts gpt-* traffic.
  const input = typeof tokensRaw.input_tokens === 'number' ? tokensRaw.input_tokens : 0;
  const output = typeof tokensRaw.output_tokens === 'number' ? tokensRaw.output_tokens : 0;
  return input + output;
}

interface Summary {
  totalRequests: number;
  successCount: number;
  failureCount: number;
  totalTokens: number;
}
const newSummary = (): Summary => ({
  totalRequests: 0,
  successCount: 0,
  failureCount: 0,
  totalTokens: 0
});
const toSummaryFields = (s: Summary) => ({
  total_requests: s.totalRequests,
  success_count: s.successCount,
  failure_count: s.failureCount,
  total_tokens: s.totalTokens
});

/**
 * Returns a deep-filtered copy of `usage` where every `details[]` entry
 * outside the window is dropped, and the per-model / per-api / top-level
 * rollup counters are recomputed from the kept details.
 *
 * `range === 'all'` returns the input unchanged (reference-equal).
 */
export function filterUsageByUsersTimeRange<T>(
  usage: T,
  range: UsersTimeRange,
  nowMs: number = Date.now()
): T {
  if (range === 'all') return usage;

  const root = isRecord(usage) ? usage : null;
  const apisRaw = root ? root.apis : null;
  if (!root || !isRecord(apisRaw)) return usage;

  const windowMs = WINDOW_MS[range];
  if (!Number.isFinite(windowMs) || windowMs <= 0) return usage;

  const windowStart = nowMs - windowMs;
  const filteredApis: Record<string, unknown> = {};
  const totalSummary = newSummary();

  for (const [apiName, apiEntry] of Object.entries(apisRaw)) {
    if (!isRecord(apiEntry)) continue;
    const modelsRaw = apiEntry.models;
    if (!isRecord(modelsRaw)) continue;

    const filteredModels: Record<string, unknown> = {};
    const apiSummary = newSummary();
    let hasModelData = false;

    for (const [modelName, modelEntry] of Object.entries(modelsRaw)) {
      if (!isRecord(modelEntry)) continue;

      const detailsRaw = Array.isArray(modelEntry.details) ? modelEntry.details : [];
      const modelSummary = newSummary();
      const keptDetails: unknown[] = [];

      for (const detail of detailsRaw) {
        if (!isRecord(detail)) continue;
        const tsMs = parseTsMs(detail.timestamp);
        if (
          !Number.isFinite(tsMs) ||
          tsMs < windowStart ||
          tsMs > nowMs + CLOCK_SKEW_MS
        )
          continue;

        keptDetails.push(detail);
        modelSummary.totalRequests += 1;
        if (detail.failed === true) {
          modelSummary.failureCount += 1;
        } else {
          modelSummary.successCount += 1;
        }
        modelSummary.totalTokens += extractTotalTokens(detail);
      }

      if (keptDetails.length === 0) continue;

      filteredModels[modelName] = {
        ...modelEntry,
        ...toSummaryFields(modelSummary),
        details: keptDetails
      };
      hasModelData = true;
      apiSummary.totalRequests += modelSummary.totalRequests;
      apiSummary.successCount += modelSummary.successCount;
      apiSummary.failureCount += modelSummary.failureCount;
      apiSummary.totalTokens += modelSummary.totalTokens;
    }

    if (!hasModelData) continue;

    filteredApis[apiName] = {
      ...apiEntry,
      ...toSummaryFields(apiSummary),
      models: filteredModels
    };
    totalSummary.totalRequests += apiSummary.totalRequests;
    totalSummary.successCount += apiSummary.successCount;
    totalSummary.failureCount += apiSummary.failureCount;
    totalSummary.totalTokens += apiSummary.totalTokens;
  }

  return {
    ...root,
    ...toSummaryFields(totalSummary),
    apis: filteredApis
  } as T;
}
