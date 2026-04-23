import { useEffect, useMemo, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useUsageData } from '@/components/usage';
import {
  collectUsageDetails,
  filterUsageByTimeRange,
  type UsageDetail,
  type UsageTimeRange
} from '@/utils/usage';
import { useConfigStore, useNotificationStore } from '@/stores';
import { configFileApi } from '@/services/api';
import { useKeyAliases } from '../hooks/useKeyAliases';
import {
  pivotByKey,
  successRate,
  makeCostFn,
  type PerKeyStats
} from '../utils/keyPivot';
import {
  maskKey,
  formatNumber,
  formatCost,
  formatLastActive,
  formatLatency
} from '../utils/keyDisplay';
import {
  parseRateLimitFromYaml,
  resolveRateLimit,
  type RateLimitConfig
} from '../services/ratelimitConfig';
import { AliasEditor } from '../components/AliasEditor';
import styles from './UserDetailPage.module.scss';

const LOG_CAP = 500;

const RANGE_OPTIONS: ReadonlyArray<{ value: UsageTimeRange; labelKey: string }> = [
  { value: '7h', labelKey: 'detail.range_7h' },
  { value: '24h', labelKey: 'detail.range_24h' },
  { value: '7d', labelKey: 'detail.range_7d' },
  { value: 'all', labelKey: 'detail.range_all' }
];

function isolateApiKey(usage: unknown, apiKey: string): unknown {
  if (!usage || typeof usage !== 'object' || Array.isArray(usage)) return usage;
  const u = usage as Record<string, unknown>;
  const apis = u.apis;
  if (!apis || typeof apis !== 'object' || Array.isArray(apis)) return usage;
  const subset = (apis as Record<string, unknown>)[apiKey];
  return { ...u, apis: subset ? { [apiKey]: subset } : {} };
}

export function UserDetailPage() {
  const { t } = useTranslation('extensions');
  const navigate = useNavigate();
  const { apiKey = '' } = useParams<{ apiKey: string }>();
  const decodedKey = useMemo(() => {
    try {
      return decodeURIComponent(apiKey);
    } catch {
      return apiKey;
    }
  }, [apiKey]);

  const { showNotification } = useNotificationStore();
  const config = useConfigStore((s) => s.config);
  const { usage, loading: usageLoading, modelPrices } = useUsageData();
  const { aliases, saveAlias } = useKeyAliases();

  const [range, setRange] = useState<UsageTimeRange>('24h');
  const [rlConfig, setRlConfig] = useState<RateLimitConfig | null>(null);

  useEffect(() => {
    let cancelled = false;
    configFileApi
      .fetchConfigYaml()
      .then((raw) => {
        if (!cancelled) setRlConfig(parseRateLimitFromYaml(raw));
      })
      .catch(() => {
        if (!cancelled) setRlConfig({ default: null, models: {}, keyOverrides: {} });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const knownKeys = useMemo(() => config?.apiKeys || [], [config?.apiKeys]);
  const isOrphan = knownKeys.length > 0 && !knownKeys.includes(decodedKey);

  const singleUsage = useMemo(() => isolateApiKey(usage, decodedKey), [usage, decodedKey]);

  const filteredUsage = useMemo(
    () => filterUsageByTimeRange(singleUsage, range),
    [singleUsage, range]
  );

  const costFn = useMemo(() => makeCostFn(modelPrices), [modelPrices]);

  const keyStats = useMemo<PerKeyStats | null>(() => {
    const all = pivotByKey(filteredUsage, knownKeys, aliases, costFn);
    return all.find((r) => r.apiKey === decodedKey) ?? null;
  }, [filteredUsage, knownKeys, aliases, costFn, decodedKey]);

  const logEntries = useMemo<UsageDetail[]>(() => {
    const all = collectUsageDetails(filteredUsage);
    all.sort((a, b) => (b.__timestampMs ?? 0) - (a.__timestampMs ?? 0));
    return all;
  }, [filteredUsage]);

  const logVisible = useMemo(() => logEntries.slice(0, LOG_CAP), [logEntries]);

  const onSaveAlias = async (key: string, alias: string) => {
    await saveAlias(key, alias);
    showNotification(t('users.save_success'), 'success');
  };

  const onSaveAliasError = (msg: string) => {
    showNotification(`${t('users.save_failed')}: ${msg}`, 'error');
  };

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(decodedKey);
      showNotification(t('detail.copied'), 'success');
    } catch {
      /* ignore */
    }
  };

  const sr = keyStats ? successRate(keyStats) : 0;

  // Rate-limit panel: compute approx usage per matched model.
  type RlRow = {
    model: string;
    window: string;
    limit: number;
    windowMs: number;
    used: number;
    resetsAt: number;
  };
  const rlRows = useMemo<RlRow[]>(() => {
    if (!rlConfig) return [];
    const nowMs = Date.now();
    const out: RlRow[] = [];
    // Use unfiltered singleUsage so models touched outside the detail-page time
    // range still show up in the limit panel (plugin window may be > filter).
    const allDetails = collectUsageDetails(singleUsage);
    const modelsTouched = new Set<string>();
    for (const d of allDetails) {
      if (d.__modelName) modelsTouched.add(d.__modelName);
    }
    for (const model of modelsTouched) {
      const rule = resolveRateLimit(rlConfig, decodedKey, model);
      if (!rule || rule.requests <= 0) continue;
      const windowMs = rule.windowMs;
      if (!windowMs) continue;
      const cutoff = nowMs - windowMs;
      let used = 0;
      let earliest = nowMs;
      for (const d of allDetails) {
        if (d.__modelName !== model) continue;
        const ts = d.__timestampMs ?? 0;
        if (ts >= cutoff) {
          used += 1;
          if (ts < earliest) earliest = ts;
        }
      }
      out.push({
        model,
        window: rule.window || '—',
        limit: rule.requests,
        windowMs,
        used,
        resetsAt: used > 0 ? earliest + windowMs : 0
      });
    }
    out.sort((a, b) => a.model.localeCompare(b.model));
    return out;
  }, [rlConfig, singleUsage, decodedKey]);

  const rlEnabled = rlConfig !== null && (rlConfig.default || Object.keys(rlConfig.models).length > 0);

  if (!usageLoading && !keyStats) {
    return (
      <div className={styles.container}>
        <button className={styles.backLink} onClick={() => navigate('/custom/users')}>
          {t('detail.back')}
        </button>
        <div className={styles.sectionCard}>
          <div className={styles.emptyState}>{t('detail.not_found')}</div>
        </div>
      </div>
    );
  }

  return (
    <div className={styles.container}>
      <button className={styles.backLink} onClick={() => navigate('/custom/users')}>
        {t('detail.back')}
      </button>

      <div className={styles.pageHeader}>
        <div className={styles.headerRow}>
          <div className={styles.aliasBlock}>
            <div className={styles.aliasInline}>
              <h1 className={styles.aliasInlineTitle}>
                {aliases[decodedKey] || t('users.no_alias')}
              </h1>
              {isOrphan && (
                <span className={styles.orphanBadge}>{t('users.orphan_badge')}</span>
              )}
            </div>
            <div className={styles.keyLabel}>{maskKey(decodedKey)}</div>
            <AliasEditor
              apiKey={decodedKey}
              value={aliases[decodedKey]}
              onSave={onSaveAlias}
              onError={onSaveAliasError}
              disabled={isOrphan}
            />
          </div>
          <div className={styles.headerActions}>
            <button className={styles.btn} onClick={handleCopy} type="button">
              {t('detail.copy_key')}
            </button>
          </div>
        </div>

        <div className={styles.rangeGroup}>
          <span>{t('detail.range_filter')}:</span>
          {RANGE_OPTIONS.map((opt) => (
            <button
              key={opt.value}
              className={`${styles.rangeBtn} ${range === opt.value ? styles.rangeBtnActive : ''}`}
              onClick={() => setRange(opt.value)}
              type="button"
            >
              {t(opt.labelKey)}
            </button>
          ))}
        </div>
      </div>

      <div className={styles.statGrid}>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.total_requests')}</span>
          <span className={styles.statValue}>
            {formatNumber(keyStats?.totalRequests ?? 0)}
          </span>
          <span className={styles.statMuted}>
            {keyStats ? `${formatNumber(keyStats.failureCount)} failed` : ''}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.success_rate')}</span>
          <span className={styles.statValue}>
            {keyStats && keyStats.totalRequests > 0 ? `${sr.toFixed(1)}%` : '—'}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.total_tokens')}</span>
          <span className={styles.statValue}>
            {formatNumber(keyStats?.totalTokens ?? 0)}
          </span>
          <span className={styles.statMuted}>
            {keyStats
              ? `${formatNumber(keyStats.inputTokens)} in / ${formatNumber(keyStats.outputTokens)} out`
              : ''}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.total_cost')}</span>
          <span className={styles.statValue}>
            {formatCost(keyStats?.totalCost ?? 0)}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.last_active')}</span>
          <span className={styles.statValue} style={{ fontSize: '1rem' }}>
            {formatLastActive(keyStats?.lastActiveMs ?? 0)}
          </span>
        </div>
      </div>

      {/* Per-model breakdown */}
      <div className={styles.sectionCard}>
        <div className={styles.sectionHeader}>
          <h2 className={styles.sectionTitle}>{t('detail.per_model')}</h2>
        </div>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>{t('detail.model')}</th>
              <th className={styles.numeric}>{t('detail.requests')}</th>
              <th className={styles.numeric}>{t('detail.tokens')}</th>
              <th className={styles.numeric}>{t('detail.cost')}</th>
              <th>{t('detail.last_active')}</th>
            </tr>
          </thead>
          <tbody>
            {(keyStats?.perModel ?? []).map((m) => (
              <tr key={m.model}>
                <td className={styles.mono}>{m.model}</td>
                <td className={styles.numeric}>{formatNumber(m.requests)}</td>
                <td className={styles.numeric}>{formatNumber(m.totalTokens)}</td>
                <td className={styles.numeric}>{formatCost(m.cost)}</td>
                <td>{formatLastActive(m.lastActiveMs)}</td>
              </tr>
            ))}
            {(!keyStats || keyStats.perModel.length === 0) && (
              <tr>
                <td colSpan={5} className={styles.emptyState}>
                  {t('users.empty')}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {/* Rate-limit estimate */}
      <div className={styles.sectionCard}>
        <div className={styles.sectionHeader}>
          <h2 className={styles.sectionTitle}>{t('detail.ratelimit_panel')}</h2>
        </div>
        <div className={styles.sectionNote}>{t('detail.ratelimit_note')}</div>
        {!rlEnabled && (
          <div className={styles.emptyState}>{t('detail.ratelimit_disabled')}</div>
        )}
        {rlEnabled && rlRows.length === 0 && (
          <div className={styles.emptyState}>{t('detail.ratelimit_no_match')}</div>
        )}
        {rlEnabled && rlRows.length > 0 && (
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('detail.model')}</th>
                <th>{t('detail.rl_window')}</th>
                <th className={styles.numeric}>{t('detail.rl_limit')}</th>
                <th className={styles.numeric}>{t('detail.rl_used')}</th>
                <th>{t('detail.rl_resets_around')}</th>
              </tr>
            </thead>
            <tbody>
              {rlRows.map((r) => (
                <tr key={r.model}>
                  <td className={styles.mono}>{r.model}</td>
                  <td>{r.window}</td>
                  <td className={styles.numeric}>{formatNumber(r.limit)}</td>
                  <td className={styles.numeric}>
                    ≈ {formatNumber(r.used)} / {formatNumber(r.limit)}
                  </td>
                  <td>
                    {r.resetsAt > 0 ? formatLastActive(r.resetsAt) : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Request log */}
      <div className={styles.sectionCard}>
        <div className={styles.sectionHeader}>
          <h2 className={styles.sectionTitle}>{t('detail.request_log')}</h2>
        </div>
        <div className={styles.logTableWrap}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>{t('detail.log_timestamp')}</th>
                <th>{t('detail.log_model')}</th>
                <th className={styles.numeric}>{t('detail.log_latency')}</th>
                <th className={styles.numeric}>{t('detail.log_tokens_in')}</th>
                <th className={styles.numeric}>{t('detail.log_tokens_out')}</th>
                <th>{t('detail.log_status')}</th>
                <th>{t('detail.log_source')}</th>
              </tr>
            </thead>
            <tbody>
              {logVisible.map((d, idx) => (
                <tr key={`${d.timestamp}-${idx}`}>
                  <td>{new Date(d.timestamp).toLocaleString()}</td>
                  <td className={styles.mono}>{d.__modelName ?? '—'}</td>
                  <td className={styles.numeric}>{formatLatency(d.latency_ms)}</td>
                  <td className={styles.numeric}>
                    {formatNumber(d.tokens?.input_tokens ?? 0)}
                  </td>
                  <td className={styles.numeric}>
                    {formatNumber(d.tokens?.output_tokens ?? 0)}
                  </td>
                  <td>
                    <span className={d.failed ? styles.badgeFailed : styles.badgeOk}>
                      {d.failed ? t('detail.log_failed') : t('detail.log_ok')}
                    </span>
                  </td>
                  <td className={styles.mono}>{d.source || '—'}</td>
                </tr>
              ))}
              {logVisible.length === 0 && (
                <tr>
                  <td colSpan={7} className={styles.emptyState}>
                    {t('users.empty')}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        {logEntries.length > 0 && (
          <div className={styles.logFooter}>
            {t('detail.log_showing', {
              shown: logVisible.length,
              total: logEntries.length
            })}
          </div>
        )}
      </div>
    </div>
  );
}
