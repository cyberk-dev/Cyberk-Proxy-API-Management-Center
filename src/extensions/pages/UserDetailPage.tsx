import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useUsageData } from '../hooks/useUsageData';
import { IconCheck, IconRefreshCw, IconX } from '@/components/ui/icons';
import {
  collectUsageDetails,
  filterUsageByTimeRange,
  type ModelPrice,
  type UsageDetail,
  type UsageTimeRange
} from '../utils/usageCompat';
import { getNewInputTokens } from '../utils/tokenSemantics';
import { maskApiKey } from '@/utils/format';
import { resolveDefaultModelPrice } from '@/data/defaultModelPrices';
import { useConfigStore, useNotificationStore } from '@/stores';
import { configFileApi } from '@/services/api';
import { useKeyAliases } from '../hooks/useKeyAliases';
import {
  pivotByKey,
  successRate,
  makeCostFn,
  type PerKeyStats
} from '../utils/keyPivot';
import { resolveKeyByIndex } from '../utils/keyIndex';
import {
  formatNumber,
  formatCost,
  formatLastActive
} from '../utils/keyDisplay';
import {
  parseRateLimitFromYaml,
  resolveRateLimit,
  type RateLimitConfig
} from '../services/ratelimitConfig';
import styles from './UserDetailPage.module.scss';

const LOG_CAP = 500;

function IconPencil({ size = 14 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M17 3a2.85 2.85 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z" />
    </svg>
  );
}

const RANGE_OPTIONS: ReadonlyArray<{ value: UsageTimeRange; labelKey: string }> = [
  { value: '7h', labelKey: 'detail.range_7h' },
  { value: '24h', labelKey: 'detail.range_24h' },
  { value: '7d', labelKey: 'detail.range_7d' },
  { value: 'all', labelKey: 'detail.range_all' }
];

const RANGE_VALUES: ReadonlySet<string> = new Set(['7h', '24h', '7d', 'all']);
const DEFAULT_RANGE: UsageTimeRange = '24h';
const RANGE_QUERY_KEY = 'range';

function parseRange(raw: string | null): UsageTimeRange {
  return raw && RANGE_VALUES.has(raw) ? (raw as UsageTimeRange) : DEFAULT_RANGE;
}

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
  const { index = '' } = useParams<{ index: string }>();

  const { showNotification } = useNotificationStore();
  const config = useConfigStore((s) => s.config);
  const { usage, loading: usageLoading, modelPrices, loadUsage } = useUsageData();
  const { aliases, saveAlias } = useKeyAliases();

  const knownKeys = useMemo(() => config?.apiKeys || [], [config?.apiKeys]);
  const decodedKey = useMemo(
    () => resolveKeyByIndex(index, knownKeys, usage) ?? '',
    [index, knownKeys, usage]
  );
  const indexResolved = decodedKey !== '';

  const [searchParams, setSearchParams] = useSearchParams();
  const range = parseRange(searchParams.get(RANGE_QUERY_KEY));
  const setRange = useCallback(
    (value: UsageTimeRange) => {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          if (value === DEFAULT_RANGE) {
            next.delete(RANGE_QUERY_KEY);
          } else {
            next.set(RANGE_QUERY_KEY, value);
          }
          return next;
        },
        { replace: true }
      );
    },
    [setSearchParams]
  );

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

  const isOrphan =
    indexResolved && knownKeys.length > 0 && !knownKeys.includes(decodedKey);

  const currentAlias = aliases[decodedKey];
  const [editingAlias, setEditingAlias] = useState(false);
  const [aliasDraft, setAliasDraft] = useState('');
  const [aliasSaving, setAliasSaving] = useState(false);
  const aliasInputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (editingAlias && aliasInputRef.current) {
      aliasInputRef.current.focus();
      aliasInputRef.current.select();
    }
  }, [editingAlias]);


  const singleUsage = useMemo(() => isolateApiKey(usage, decodedKey), [usage, decodedKey]);

  // Whether the decoded key has any data in the raw usage export (ignoring the
  // current time-range filter). Used to decide between "truly not found" and
  // "found, but current range is empty" so we can still show the range picker
  // in the latter case.
  const keyHasAnyUsage = useMemo(() => {
    if (!decodedKey) return false;
    if (!singleUsage || typeof singleUsage !== 'object') return false;
    const apis = (singleUsage as { apis?: unknown }).apis;
    if (!apis || typeof apis !== 'object' || Array.isArray(apis)) return false;
    return Object.prototype.hasOwnProperty.call(apis, decodedKey);
  }, [singleUsage, decodedKey]);

  const filteredUsage = useMemo(
    () => filterUsageByTimeRange(singleUsage, range),
    [singleUsage, range]
  );

  // User-configured prices take precedence; fall back to bundled defaults so
  // unpriced-but-known models still produce a cost instead of $0.
  const effectiveModelPrices = useMemo(() => {
    const merged: Record<string, ModelPrice> = { ...modelPrices };
    for (const d of collectUsageDetails(singleUsage)) {
      const name = d.__modelName;
      if (!name || merged[name]) continue;
      const def = resolveDefaultModelPrice(name);
      if (def) merged[name] = def;
    }
    return merged;
  }, [modelPrices, singleUsage]);

  const costFn = useMemo(() => makeCostFn(effectiveModelPrices), [effectiveModelPrices]);

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

  const maskedKey = useMemo(() => maskApiKey(decodedKey), [decodedKey]);

  const beginAliasEdit = () => {
    setAliasDraft(currentAlias ?? '');
    setEditingAlias(true);
  };

  const cancelAliasEdit = () => {
    setEditingAlias(false);
    setAliasDraft('');
  };

  const commitAliasEdit = async () => {
    if (aliasSaving) return;
    setAliasSaving(true);
    try {
      await saveAlias(decodedKey, aliasDraft);
      showNotification(t('users.save_success'), 'success');
      setEditingAlias(false);
    } catch (e) {
      const msg = e instanceof Error ? e.message : t('users.save_failed');
      showNotification(`${t('users.save_failed')}: ${msg}`, 'error');
    } finally {
      setAliasSaving(false);
    }
  };

  const handleAliasKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      void commitAliasEdit();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelAliasEdit();
    }
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
      if (used <= 0) continue;
      out.push({
        model,
        window: rule.window || '—',
        limit: rule.requests,
        windowMs,
        used,
        resetsAt: earliest + windowMs
      });
    }
    out.sort((a, b) => a.model.localeCompare(b.model));
    return out;
  }, [rlConfig, singleUsage, decodedKey]);

  const rlEnabled = rlConfig !== null && (rlConfig.default || Object.keys(rlConfig.models).length > 0);

  // Only bail when the key itself cannot be found in the usage export. If the
  // key exists but the selected time range filters out everything, fall
  // through so the page still renders with the range picker (otherwise the
  // user gets stuck on a blank "not found" screen and can't switch ranges).
  if (!usageLoading && !keyHasAnyUsage) {
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
          <div className={styles.identityBlock}>
            {editingAlias ? (
              <div className={styles.titleEditRow}>
                <input
                  ref={aliasInputRef}
                  className={styles.titleInput}
                  type="text"
                  value={aliasDraft}
                  onChange={(e) => setAliasDraft(e.target.value)}
                  onKeyDown={handleAliasKeyDown}
                  placeholder={t('detail.alias_placeholder')}
                  disabled={aliasSaving}
                  maxLength={64}
                />
                <button
                  className={`${styles.iconBtn} ${styles.iconBtnPrimary}`}
                  onClick={() => void commitAliasEdit()}
                  disabled={aliasSaving}
                  type="button"
                  aria-label={t('users.save')}
                  title={t('users.save')}
                >
                  <IconCheck size={14} />
                </button>
                <button
                  className={styles.iconBtn}
                  onClick={cancelAliasEdit}
                  disabled={aliasSaving}
                  type="button"
                  aria-label={t('users.cancel')}
                  title={t('users.cancel')}
                >
                  <IconX size={14} />
                </button>
              </div>
            ) : (
              <div className={styles.titleRow}>
                <h1
                  className={`${styles.title} ${currentAlias ? '' : styles.titleMono}`}
                >
                  {currentAlias || maskedKey}
                </h1>
                {isOrphan && (
                  <span className={styles.orphanBadge}>{t('users.orphan_badge')}</span>
                )}
                {!isOrphan && indexResolved && (
                  <button
                    className={styles.iconBtn}
                    onClick={beginAliasEdit}
                    type="button"
                    aria-label={t('users.edit_alias')}
                    title={t('users.edit_alias')}
                  >
                    <IconPencil size={13} />
                  </button>
                )}
              </div>
            )}
            {currentAlias && !editingAlias && (
              <div className={styles.keyLine}>{maskedKey}</div>
            )}
          </div>
          <div className={styles.headerActions}>
            <button className={styles.btn} onClick={handleCopy} type="button">
              {t('detail.copy_key')}
            </button>
            <button
              type="button"
              className={styles.iconBtn}
              onClick={() => void loadUsage()}
              disabled={usageLoading}
              aria-label={t('users.refresh')}
              title={t('users.refresh')}
            >
              <IconRefreshCw
                size={14}
                className={usageLoading ? styles.spin : undefined}
              />
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
              ? `${formatNumber(keyStats.inputTokens)} ${t('detail.tokens_in')} / ${formatNumber(keyStats.outputTokens)} ${t('detail.tokens_out')} / ${formatNumber(keyStats.cachedTokens)} ${t('detail.tokens_cached')}`
              : ''}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.total_cost')}</span>
          <span className={styles.statValue}>
            {formatCost(keyStats?.totalCost ?? 0)}
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
              <th className={styles.numeric}>{t('detail.col_input')}</th>
              <th className={styles.numeric}>{t('detail.col_output')}</th>
              <th className={styles.numeric}>{t('detail.col_cached')}</th>
              <th className={styles.numeric}>{t('detail.cost')}</th>
            </tr>
          </thead>
          <tbody>
            {(keyStats?.perModel ?? []).map((m) => (
              <tr key={m.model}>
                <td className={styles.mono}>{m.model}</td>
                <td className={styles.numeric}>{formatNumber(m.requests)}</td>
                <td className={styles.numeric}>{formatNumber(m.inputTokens)}</td>
                <td className={styles.numeric}>{formatNumber(m.outputTokens)}</td>
                <td className={styles.numeric}>{formatNumber(m.cachedTokens)}</td>
                <td className={styles.numeric}>{formatCost(m.cost)}</td>
              </tr>
            ))}
            {(!keyStats || keyStats.perModel.length === 0) && (
              <tr>
                <td colSpan={6} className={styles.emptyState}>
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
                <th className={styles.numeric}>{t('detail.log_tokens_in')}</th>
                <th className={styles.numeric}>{t('detail.log_tokens_out')}</th>
                <th className={styles.numeric}>{t('detail.log_tokens_cached')}</th>
                <th>{t('detail.log_status')}</th>
              </tr>
            </thead>
            <tbody>
              {logVisible.map((d, idx) => {
                const cached = Math.max(
                  d.tokens?.cached_tokens ?? 0,
                  d.tokens?.cache_tokens ?? 0
                );
                const newInput = getNewInputTokens(d.tokens, d.__modelName);
                return (
                  <tr key={`${d.timestamp}-${idx}`}>
                    <td>{new Date(d.timestamp).toLocaleString()}</td>
                    <td className={styles.mono}>{d.__modelName ?? '—'}</td>
                    <td className={styles.numeric}>
                      {formatNumber(newInput)}
                    </td>
                    <td className={styles.numeric}>
                      {formatNumber(d.tokens?.output_tokens ?? 0)}
                    </td>
                    <td className={styles.numeric}>{formatNumber(cached)}</td>
                    <td>
                      <span className={d.failed ? styles.badgeFailed : styles.badgeOk}>
                        {d.failed ? t('detail.log_failed') : t('detail.log_ok')}
                      </span>
                    </td>
                  </tr>
                );
              })}
              {logVisible.length === 0 && (
                <tr>
                  <td colSpan={6} className={styles.emptyState}>
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
