import { useEffect, useState, useCallback, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { useNotificationStore } from '@/stores';
import { apiClient } from '@/services/api';
import type { ModelPrice } from '../utils/usageCompat';

const MODEL_PRICE_STORAGE_KEY = 'cli-proxy-model-prices-v2';
const USAGE_TIMEOUT_MS = 30_000;

export interface UsagePayload {
  total_requests?: number;
  success_count?: number;
  failure_count?: number;
  total_tokens?: number;
  apis?: Record<string, unknown>;
  [key: string]: unknown;
}

export interface KeyDetailModelStats {
  model: string;
  total_requests: number;
  success_count: number;
  failure_count: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  reasoning_tokens: number;
  total_tokens: number;
  last_active?: string;
}

export interface KeyRateLimitWindow {
  model: string;
  window: string;
  window_ms: number;
  limit: number;
  used: number;
  resets_at: number;
}

export interface KeyRecentDetail {
  timestamp: string;
  latency_ms?: number;
  source?: string;
  auth_index?: string;
  tokens: {
    input_tokens?: number;
    output_tokens?: number;
    reasoning_tokens?: number;
    cached_tokens?: number;
    cache_tokens?: number;
    total_tokens?: number;
  };
  failed: boolean;
  model: string;
}

export interface KeyDetailPayload {
  api_key: string;
  total_requests: number;
  success_count: number;
  failure_count: number;
  input_tokens: number;
  output_tokens: number;
  cached_tokens: number;
  reasoning_tokens: number;
  total_tokens: number;
  models: KeyDetailModelStats[];
  recent_details: KeyRecentDetail[];
  rate_limits: KeyRateLimitWindow[];
}

export interface LoadKeyUsageOptions {
  sinceMs?: number;
  limit?: number;
}

export interface UseUsageDataReturn {
  usage: UsagePayload | null;
  summary: UsagePayload | null;
  keyUsage: KeyDetailPayload | null;
  loading: boolean;
  error: string;
  lastRefreshedAt: Date | null;
  modelPrices: Record<string, ModelPrice>;
  setModelPrices: (prices: Record<string, ModelPrice>) => void;
  loadUsage: () => Promise<void>;
  loadSummary: (sinceMs?: number) => Promise<void>;
  loadKeyUsage: (apiKey: string, opts?: LoadKeyUsageOptions) => Promise<void>;
  handleExport: () => Promise<void>;
  handleImport: () => void;
  handleImportChange: (event: React.ChangeEvent<HTMLInputElement>) => Promise<void>;
  importInputRef: React.RefObject<HTMLInputElement | null>;
  exporting: boolean;
  importing: boolean;
}

function loadModelPrices(): Record<string, ModelPrice> {
  try {
    const raw = localStorage.getItem(MODEL_PRICE_STORAGE_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const result: Record<string, ModelPrice> = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (v && typeof v === 'object' && 'prompt' in v && 'completion' in v && 'cache' in v) {
        result[k] = v as ModelPrice;
      }
    }
    return result;
  } catch {
    return {};
  }
}

function saveModelPrices(prices: Record<string, ModelPrice>): void {
  try {
    localStorage.setItem(MODEL_PRICE_STORAGE_KEY, JSON.stringify(prices));
  } catch { /* ignore */ }
}

function downloadBlob(filename: string, blob: Blob) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

export function useUsageData(): UseUsageDataReturn {
  const { t } = useTranslation();
  const { showNotification } = useNotificationStore();

  const [usage, setUsage] = useState<UsagePayload | null>(null);
  const [summary, setSummary] = useState<UsagePayload | null>(null);
  const [keyUsage, setKeyUsage] = useState<KeyDetailPayload | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [lastRefreshedAt, setLastRefreshedAt] = useState<Date | null>(null);
  const [modelPrices, setModelPricesState] = useState<Record<string, ModelPrice>>({});
  const [exporting, setExporting] = useState(false);
  const [importing, setImporting] = useState(false);
  const importInputRef = useRef<HTMLInputElement | null>(null);

  const loadUsage = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const data = await apiClient.get<Record<string, unknown>>('/usage', { timeout: USAGE_TIMEOUT_MS });
      setUsage(data as UsagePayload);
      setLastRefreshedAt(new Date());
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : 'Unknown error';
      setError(message);
    } finally {
      setLoading(false);
    }
  }, []);

  const loadSummary = useCallback(async (sinceMs?: number) => {
    setLoading(true);
    setError('');
    try {
      const url = sinceMs ? `/usage/summary?since=${sinceMs}` : '/usage/summary';
      const data = await apiClient.get<Record<string, unknown>>(url, { timeout: USAGE_TIMEOUT_MS });
      setSummary(data as UsagePayload);
      setLastRefreshedAt(new Date());
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : 'Unknown error';
      setError(message);
    } finally {
      setLoading(false);
    }
  }, []);

  const loadKeyUsage = useCallback(async (apiKey: string, opts?: LoadKeyUsageOptions) => {
    setLoading(true);
    setError('');
    try {
      const params = new URLSearchParams();
      if (opts?.sinceMs && opts.sinceMs > 0) params.set('since', String(opts.sinceMs));
      if (opts?.limit && opts.limit > 0) params.set('limit', String(opts.limit));
      const qs = params.toString();
      const url = qs
        ? `/usage/keys/${encodeURIComponent(apiKey)}?${qs}`
        : `/usage/keys/${encodeURIComponent(apiKey)}`;
      const data = await apiClient.get<KeyDetailPayload>(url, { timeout: USAGE_TIMEOUT_MS });
      setKeyUsage(data);
      setLastRefreshedAt(new Date());
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : 'Unknown error';
      setError(message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    setModelPricesState(loadModelPrices());
  }, []);

  const handleExport = async () => {
    setExporting(true);
    try {
      const data = await apiClient.get<Record<string, unknown>>('/usage/export', { timeout: USAGE_TIMEOUT_MS });
      const filename = `usage-export-${new Date().toISOString().replace(/[:.]/g, '-')}.json`;
      downloadBlob(filename, new Blob([JSON.stringify(data ?? {}, null, 2)], { type: 'application/json' }));
      showNotification(t('usage_stats.export_success'), 'success');
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : '';
      showNotification(`${t('notification.download_failed')}${message ? `: ${message}` : ''}`, 'error');
    } finally {
      setExporting(false);
    }
  };

  const handleImport = () => { importInputRef.current?.click(); };

  const handleImportChange = async (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (!file) return;
    setImporting(true);
    try {
      const text = await file.text();
      let payload: unknown;
      try { payload = JSON.parse(text); } catch {
        showNotification(t('usage_stats.import_invalid'), 'error');
        return;
      }
      const result = await apiClient.post<Record<string, unknown>>('/usage/import', payload, { timeout: USAGE_TIMEOUT_MS });
      showNotification(
        t('usage_stats.import_success', {
          added: (result as Record<string, number>)?.added ?? 0,
          skipped: (result as Record<string, number>)?.skipped ?? 0,
          total: (result as Record<string, number>)?.total_requests ?? 0,
          failed: (result as Record<string, number>)?.failed_requests ?? 0,
        }),
        'success'
      );
      await loadUsage();
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : '';
      showNotification(`${t('notification.upload_failed')}${message ? `: ${message}` : ''}`, 'error');
    } finally {
      setImporting(false);
    }
  };

  const setModelPrices = useCallback((prices: Record<string, ModelPrice>) => {
    setModelPricesState(prices);
    saveModelPrices(prices);
  }, []);

  return {
    usage, summary, keyUsage, loading, error, lastRefreshedAt, modelPrices, setModelPrices,
    loadUsage, loadSummary, loadKeyUsage,
    handleExport, handleImport, handleImportChange, importInputRef,
    exporting, importing,
  };
}
