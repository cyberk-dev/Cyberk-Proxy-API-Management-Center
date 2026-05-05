import { parseTimestampMs } from '@/utils/timestamp';

export interface ModelPrice {
  prompt: number;
  completion: number;
  cache: number;
}

export interface UsageDetail {
  timestamp: string;
  source: string;
  auth_index: string | number | null;
  latency_ms?: number;
  tokens: {
    input_tokens: number;
    output_tokens: number;
    reasoning_tokens: number;
    cached_tokens: number;
    cache_tokens?: number;
    total_tokens: number;
  };
  failed: boolean;
  __modelName?: string;
  __timestampMs?: number;
}

export type UsageTimeRange = '7h' | '24h' | '7d' | 'all';

const USAGE_TIME_RANGE_MS: Record<Exclude<UsageTimeRange, 'all'>, number> = {
  '7h': 7 * 60 * 60 * 1000,
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000,
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value);

const getApisRecord = (usageData: unknown): Record<string, unknown> | null => {
  const usageRecord = isRecord(usageData) ? usageData : null;
  const apisRaw = usageRecord ? usageRecord.apis : null;
  return isRecord(apisRaw) ? apisRaw : null;
};

function extractTotalTokens(detail: unknown): number {
  const record = isRecord(detail) ? detail : null;
  const tokensRaw = record?.tokens;
  const tokens = isRecord(tokensRaw) ? tokensRaw : {};
  if (typeof tokens.total_tokens === 'number') return tokens.total_tokens;
  const inputTokens = typeof tokens.input_tokens === 'number' ? tokens.input_tokens : 0;
  const outputTokens = typeof tokens.output_tokens === 'number' ? tokens.output_tokens : 0;
  const reasoningTokens = typeof tokens.reasoning_tokens === 'number' ? tokens.reasoning_tokens : 0;
  const cachedTokens = Math.max(
    typeof tokens.cached_tokens === 'number' ? Math.max(tokens.cached_tokens, 0) : 0,
    typeof tokens.cache_tokens === 'number' ? Math.max(tokens.cache_tokens, 0) : 0
  );
  return inputTokens + outputTokens + reasoningTokens + cachedTokens;
}

const usageDetailsCache = new WeakMap<object, UsageDetail[]>();

export function collectUsageDetails(usageData: unknown): UsageDetail[] {
  const cacheKey = isRecord(usageData) ? (usageData as object) : null;
  if (cacheKey) {
    const cached = usageDetailsCache.get(cacheKey);
    if (cached) return cached;
  }

  const apis = getApisRecord(usageData);
  if (!apis) return [];
  const details: UsageDetail[] = [];

  Object.values(apis).forEach((apiEntry) => {
    if (!isRecord(apiEntry)) return;
    const models = isRecord(apiEntry.models) ? apiEntry.models : null;
    if (!models) return;

    Object.entries(models).forEach(([modelName, modelEntry]) => {
      if (!isRecord(modelEntry)) return;
      const modelDetails = Array.isArray(modelEntry.details) ? modelEntry.details : [];

      modelDetails.forEach((detailRaw) => {
        if (!isRecord(detailRaw) || typeof detailRaw.timestamp !== 'string') return;
        const timestamp = detailRaw.timestamp;
        const timestampMs = parseTimestampMs(timestamp);
        const tokensRaw = isRecord(detailRaw.tokens) ? detailRaw.tokens : {};
        details.push({
          timestamp,
          source: typeof detailRaw.source === 'string' ? detailRaw.source : '',
          auth_index:
            (detailRaw?.auth_index ??
              detailRaw?.authIndex ??
              detailRaw?.AuthIndex ??
              null) as UsageDetail['auth_index'],
          tokens: tokensRaw as unknown as UsageDetail['tokens'],
          failed: detailRaw.failed === true,
          __modelName: modelName,
          __timestampMs: Number.isNaN(timestampMs) ? 0 : timestampMs,
        });
      });
    });
  });

  if (cacheKey) usageDetailsCache.set(cacheKey, details);
  return details;
}

interface UsageSummary {
  totalRequests: number;
  successCount: number;
  failureCount: number;
  totalTokens: number;
}

const createUsageSummary = (): UsageSummary => ({
  totalRequests: 0, successCount: 0, failureCount: 0, totalTokens: 0,
});

const toUsageSummaryFields = (s: UsageSummary) => ({
  total_requests: s.totalRequests,
  success_count: s.successCount,
  failure_count: s.failureCount,
  total_tokens: s.totalTokens,
});

export function filterUsageByTimeRange<T>(
  usageData: T,
  range: UsageTimeRange,
  nowMs: number = Date.now()
): T {
  if (range === 'all') return usageData;

  const usageRecord = isRecord(usageData) ? usageData : null;
  const apis = getApisRecord(usageData);
  if (!usageRecord || !apis) return usageData;

  const rangeMs = USAGE_TIME_RANGE_MS[range];
  if (!Number.isFinite(rangeMs) || rangeMs <= 0) return usageData;

  const windowStart = nowMs - rangeMs;
  const filteredApis: Record<string, unknown> = {};
  const totalSummary = createUsageSummary();

  Object.entries(apis).forEach(([apiName, apiEntry]) => {
    if (!isRecord(apiEntry)) return;
    const models = isRecord(apiEntry.models) ? apiEntry.models : null;
    if (!models) return;

    const filteredModels: Record<string, unknown> = {};
    const apiSummary = createUsageSummary();
    let hasModelData = false;

    Object.entries(models).forEach(([modelName, modelEntry]) => {
      if (!isRecord(modelEntry)) return;
      const detailsRaw = Array.isArray(modelEntry.details) ? modelEntry.details : [];
      const modelSummary = createUsageSummary();
      const filteredDetails: unknown[] = [];

      detailsRaw.forEach((detail) => {
        const detailRecord = isRecord(detail) ? detail : null;
        if (!detailRecord || typeof detailRecord.timestamp !== 'string') return;
        const timestamp = parseTimestampMs(detailRecord.timestamp);
        if (Number.isNaN(timestamp) || timestamp < windowStart || timestamp > nowMs) return;

        filteredDetails.push(detail);
        modelSummary.totalRequests += 1;
        if (detailRecord.failed === true) {
          modelSummary.failureCount += 1;
        } else {
          modelSummary.successCount += 1;
        }
        modelSummary.totalTokens += extractTotalTokens(detailRecord);
      });

      if (!filteredDetails.length) return;
      filteredModels[modelName] = { ...modelEntry, ...toUsageSummaryFields(modelSummary), details: filteredDetails };
      hasModelData = true;
      apiSummary.totalRequests += modelSummary.totalRequests;
      apiSummary.successCount += modelSummary.successCount;
      apiSummary.failureCount += modelSummary.failureCount;
      apiSummary.totalTokens += modelSummary.totalTokens;
    });

    if (!hasModelData) return;
    filteredApis[apiName] = { ...apiEntry, ...toUsageSummaryFields(apiSummary), models: filteredModels };
    totalSummary.totalRequests += apiSummary.totalRequests;
    totalSummary.successCount += apiSummary.successCount;
    totalSummary.failureCount += apiSummary.failureCount;
    totalSummary.totalTokens += apiSummary.totalTokens;
  });

  return { ...usageRecord, ...toUsageSummaryFields(totalSummary), apis: filteredApis } as T;
}
