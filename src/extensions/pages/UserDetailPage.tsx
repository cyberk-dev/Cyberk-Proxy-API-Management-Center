import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useUsageData, type KeyDetailModelStats } from '../hooks/useUsageData';
import { IconCheck, IconRefreshCw, IconX } from '@/components/ui/icons';
import type { ModelPrice, UsageTimeRange } from '../utils/usageCompat';
import { getNewInputTokens } from '../utils/tokenSemantics';
import { maskApiKey } from '@/utils/format';
import { resolveDefaultModelPrice } from '@/data/defaultModelPrices';
import { useConfigStore, useNotificationStore } from '@/stores';
import { useKeyAliases } from '../hooks/useKeyAliases';
import { makeCostFn } from '../utils/keyPivot';
import { resolveKeyByIndex } from '../utils/keyIndex';
import { formatNumber, formatCost, formatLastActive } from '../utils/keyDisplay';
import styles from './UserDetailPage.module.scss';

const LOG_LIMIT = 500;

const RANGE_TO_MS: Record<Exclude<UsageTimeRange, 'all'>, number> = {
  '7h': 7 * 60 * 60 * 1000,
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000
};

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

function sinceMsFor(range: UsageTimeRange, nowMs: number = Date.now()): number | undefined {
  if (range === 'all') return undefined;
  const span = RANGE_TO_MS[range];
  return span ? nowMs - span : undefined;
}

// OpenAI-family models report `input_tokens` as including the cached slice; we
// display the new-prompt portion to match the per-detail logic used elsewhere
// (see tokenSemantics.ts). Server returns raw aggregates so we normalize here.
function newInputFromModel(stats: KeyDetailModelStats): number {
  return getNewInputTokens(
    { input_tokens: stats.input_tokens, cached_tokens: stats.cached_tokens },
    stats.model
  );
}

export function UserDetailPage() {
  const { t } = useTranslation('extensions');
  const navigate = useNavigate();
  const { index = '' } = useParams<{ index: string }>();

  const { showNotification } = useNotificationStore();
  const config = useConfigStore((s) => s.config);
  const { summary, keyUsage, loading: usageLoading, modelPrices, loadSummary, loadKeyUsage } = useUsageData();
  const { aliases, saveAlias } = useKeyAliases();

  const knownKeys = useMemo(() => config?.apiKeys || [], [config?.apiKeys]);

  useEffect(() => { void loadSummary(); }, [loadSummary]);

  const decodedKey = useMemo(
    () => resolveKeyByIndex(index, knownKeys, summary) ?? '',
    [index, knownKeys, summary]
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

  useEffect(() => {
    if (!decodedKey) return;
    void loadKeyUsage(decodedKey, { sinceMs: sinceMsFor(range), limit: LOG_LIMIT });
  }, [decodedKey, range, loadKeyUsage]);

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

  const keyMatchesPayload = !!keyUsage && keyUsage.api_key === decodedKey;

  // User-configured prices take precedence; fall back to bundled defaults so
  // unpriced-but-known models still produce a cost instead of $0.
  const effectiveModelPrices = useMemo(() => {
    const merged: Record<string, ModelPrice> = { ...modelPrices };
    if (keyMatchesPayload) {
      for (const m of keyUsage!.models) {
        if (merged[m.model]) continue;
        const def = resolveDefaultModelPrice(m.model);
        if (def) merged[m.model] = def;
      }
    }
    return merged;
  }, [modelPrices, keyMatchesPayload, keyUsage]);

  const costFn = useMemo(() => makeCostFn(effectiveModelPrices), [effectiveModelPrices]);

  // Per-model cost via the shared cost formula. Sum-then-clamp differs from
  // per-detail-then-sum only when a single detail had cached > input — a corner
  // case in practice; aggregating here keeps the math identical to the table.
  const perModelWithCost = useMemo(() => {
    if (!keyMatchesPayload) return [] as Array<KeyDetailModelStats & { cost: number; newInput: number }>;
    return keyUsage!.models.map((m) => ({
      ...m,
      newInput: newInputFromModel(m),
      cost: costFn(m.model, {
        input_tokens: m.input_tokens,
        output_tokens: m.output_tokens,
        cached_tokens: m.cached_tokens,
        reasoning_tokens: m.reasoning_tokens,
        total_tokens: m.total_tokens
      })
    }));
  }, [keyMatchesPayload, keyUsage, costFn]);

  const totalCost = useMemo(
    () => perModelWithCost.reduce((sum, m) => sum + m.cost, 0),
    [perModelWithCost]
  );

  const topStats = useMemo(() => {
    if (!keyMatchesPayload) return null;
    const u = keyUsage!;
    // Sum normalized "new" input tokens across models so the subtext matches
    // the per-model table column.
    const newInputTotal = perModelWithCost.reduce((sum, m) => sum + m.newInput, 0);
    const successRatePct = u.total_requests > 0
      ? (u.success_count / u.total_requests) * 100
      : 0;
    return {
      totalRequests: u.total_requests,
      failureCount: u.failure_count,
      successRatePct,
      totalTokens: u.total_tokens,
      newInputTotal,
      outputTokens: u.output_tokens,
      cachedTokens: u.cached_tokens
    };
  }, [keyMatchesPayload, keyUsage, perModelWithCost]);

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

  const refresh = () => {
    if (decodedKey) {
      void loadKeyUsage(decodedKey, { sinceMs: sinceMsFor(range), limit: LOG_LIMIT });
    }
  };

  // Bail to "not found" only when the fetch resolved with no matching data —
  // either the server returned 404 (keyUsage stays null) or it returned data
  // for a different key. Short-range queries where the key exists but has no
  // events in the window fall through so the range picker stays visible.
  if (!usageLoading && indexResolved && !keyMatchesPayload) {
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
              onClick={refresh}
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
            {formatNumber(topStats?.totalRequests ?? 0)}
          </span>
          <span className={styles.statMuted}>
            {topStats ? `${formatNumber(topStats.failureCount)} failed` : ''}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.success_rate')}</span>
          <span className={styles.statValue}>
            {topStats && topStats.totalRequests > 0 ? `${topStats.successRatePct.toFixed(1)}%` : '—'}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.total_tokens')}</span>
          <span className={styles.statValue}>
            {formatNumber(topStats?.totalTokens ?? 0)}
          </span>
          <span className={styles.statMuted}>
            {topStats
              ? `${formatNumber(topStats.newInputTotal)} ${t('detail.tokens_in')} / ${formatNumber(topStats.outputTokens)} ${t('detail.tokens_out')} / ${formatNumber(topStats.cachedTokens)} ${t('detail.tokens_cached')}`
              : ''}
          </span>
        </div>
        <div className={styles.statCard}>
          <span className={styles.statLabel}>{t('detail.total_cost')}</span>
          <span className={styles.statValue}>
            {formatCost(totalCost)}
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
            {perModelWithCost.map((m) => (
              <tr key={m.model}>
                <td className={styles.mono}>{m.model}</td>
                <td className={styles.numeric}>{formatNumber(m.total_requests)}</td>
                <td className={styles.numeric}>{formatNumber(m.newInput)}</td>
                <td className={styles.numeric}>{formatNumber(m.output_tokens)}</td>
                <td className={styles.numeric}>{formatNumber(m.cached_tokens)}</td>
                <td className={styles.numeric}>{formatCost(m.cost)}</td>
              </tr>
            ))}
            {perModelWithCost.length === 0 && (
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
        {keyMatchesPayload && keyUsage!.rate_limits.length === 0 && (
          <div className={styles.emptyState}>{t('detail.ratelimit_no_match')}</div>
        )}
        {keyMatchesPayload && keyUsage!.rate_limits.length > 0 && (
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
              {keyUsage!.rate_limits.map((r) => (
                <tr key={r.model}>
                  <td className={styles.mono}>{r.model}</td>
                  <td>{r.window}</td>
                  <td className={styles.numeric}>{formatNumber(r.limit)}</td>
                  <td className={styles.numeric}>
                    ≈ {formatNumber(r.used)} / {formatNumber(r.limit)}
                  </td>
                  <td>
                    {r.resets_at > 0 ? formatLastActive(r.resets_at) : '—'}
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
              {keyMatchesPayload && keyUsage!.recent_details.map((d, idx) => {
                const cached = Math.max(
                  d.tokens?.cached_tokens ?? 0,
                  d.tokens?.cache_tokens ?? 0
                );
                const newInput = getNewInputTokens(d.tokens, d.model);
                return (
                  <tr key={`${d.timestamp}-${idx}`}>
                    <td>{new Date(d.timestamp).toLocaleString()}</td>
                    <td className={styles.mono}>{d.model || '—'}</td>
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
              {(!keyMatchesPayload || keyUsage!.recent_details.length === 0) && (
                <tr>
                  <td colSpan={6} className={styles.emptyState}>
                    {t('users.empty')}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        {keyMatchesPayload && keyUsage!.recent_details.length > 0 && (
          <div className={styles.logFooter}>
            {t('detail.log_showing', {
              shown: keyUsage!.recent_details.length,
              total: keyUsage!.recent_details.length
            })}
          </div>
        )}
      </div>
    </div>
  );
}
