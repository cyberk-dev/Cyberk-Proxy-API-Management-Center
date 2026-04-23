import { useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useUsageData } from '@/components/usage';
import { useConfigStore, useNotificationStore } from '@/stores';
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
  formatLastActive
} from '../utils/keyDisplay';
import { AliasEditor } from '../components/AliasEditor';
import styles from './UsersPage.module.scss';

export function UsersPage() {
  const { t } = useTranslation('extensions');
  const navigate = useNavigate();
  const { showNotification } = useNotificationStore();
  const config = useConfigStore((s) => s.config);
  const { usage, loading: usageLoading, error: usageError, modelPrices } = useUsageData();
  const { aliases, loading: aliasesLoading, error: aliasesError, saveAlias } = useKeyAliases();

  const [filter, setFilter] = useState('');

  const loading = usageLoading || aliasesLoading;
  const error = usageError || aliasesError;

  const knownKeys = useMemo(() => config?.apiKeys || [], [config?.apiKeys]);

  const costFn = useMemo(() => makeCostFn(modelPrices), [modelPrices]);

  const rows = useMemo<PerKeyStats[]>(() => {
    const pivoted = pivotByKey(usage, knownKeys, aliases, costFn);
    // Include "known but no usage" keys so user can still set alias on a fresh key.
    const seen = new Set(pivoted.map((r) => r.apiKey));
    for (const k of knownKeys) {
      if (!seen.has(k)) {
        pivoted.push({
          apiKey: k,
          alias: aliases[k],
          totalRequests: 0,
          successCount: 0,
          failureCount: 0,
          inputTokens: 0,
          outputTokens: 0,
          totalTokens: 0,
          totalCost: 0,
          lastActiveMs: 0,
          perModel: [],
          orphan: false
        });
      }
    }
    // Re-sort after merge.
    pivoted.sort((a, b) => {
      if (a.orphan !== b.orphan) return a.orphan ? 1 : -1;
      return b.totalRequests - a.totalRequests;
    });
    return pivoted;
  }, [usage, knownKeys, aliases, costFn]);

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return rows;
    return rows.filter(
      (r) =>
        r.apiKey.toLowerCase().includes(q) ||
        (r.alias ?? '').toLowerCase().includes(q)
    );
  }, [rows, filter]);

  const [nonOrphan, orphans] = useMemo(() => {
    const a: PerKeyStats[] = [];
    const b: PerKeyStats[] = [];
    for (const r of filtered) {
      (r.orphan ? b : a).push(r);
    }
    return [a, b];
  }, [filtered]);

  const onSaveAlias = async (apiKey: string, alias: string) => {
    await saveAlias(apiKey, alias);
    showNotification(t('users.save_success'), 'success');
  };

  const onSaveAliasError = (msg: string) => {
    showNotification(`${t('users.save_failed')}: ${msg}`, 'error');
  };

  const renderRow = (r: PerKeyStats) => {
    const sr = successRate(r);
    const topModels = r.perModel
      .slice(0, 3)
      .map((m) => m.model)
      .join(', ');
    const moreModels = r.perModel.length > 3 ? ` +${r.perModel.length - 3}` : '';
    return (
      <tr
        key={r.apiKey}
        className={r.orphan ? styles.orphan : undefined}
        onClick={() => navigate(`/custom/users/${encodeURIComponent(r.apiKey)}`)}
      >
        <td className={styles.aliasCell}>
          <AliasEditor
            apiKey={r.apiKey}
            value={r.alias}
            disabled={r.orphan}
            onSave={onSaveAlias}
            onError={onSaveAliasError}
          />
        </td>
        <td className={styles.keyCell}>
          {maskKey(r.apiKey)}
          {r.orphan && (
            <span className={styles.orphanBadge}>{t('users.orphan_badge')}</span>
          )}
        </td>
        <td className={styles.numeric}>{formatNumber(r.totalRequests)}</td>
        <td className={styles.numeric}>
          {r.totalRequests > 0 ? `${sr.toFixed(1)}%` : '—'}
        </td>
        <td className={styles.numeric}>{formatNumber(r.totalTokens)}</td>
        <td className={styles.numeric}>{formatCost(r.totalCost)}</td>
        <td>{formatLastActive(r.lastActiveMs)}</td>
        <td className={styles.modelsCell}>
          {topModels || '—'}
          <span className={styles.modelsCellMore}>{moreModels}</span>
        </td>
      </tr>
    );
  };

  return (
    <div className={styles.container}>
      <div className={styles.header}>
        <div>
          <h1 className={styles.pageTitle}>{t('users.title')}</h1>
          <p className={styles.description}>{t('users.subtitle')}</p>
        </div>
        <div className={styles.headerActions}>
          <input
            className={styles.searchInput}
            type="text"
            placeholder={t('users.search_placeholder')}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
        </div>
      </div>

      {error && <div className={styles.errorBox}>{error}</div>}

      <div className={styles.tableCard}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>{t('users.col_alias')}</th>
              <th>{t('users.col_key')}</th>
              <th className={styles.numeric}>{t('users.col_requests')}</th>
              <th className={styles.numeric}>{t('users.col_success_rate')}</th>
              <th className={styles.numeric}>{t('users.col_tokens')}</th>
              <th className={styles.numeric}>{t('users.col_cost')}</th>
              <th>{t('users.col_last_active')}</th>
              <th>{t('users.col_models')}</th>
            </tr>
          </thead>
          <tbody>
            {loading && rows.length === 0 && (
              <tr>
                <td colSpan={8} className={styles.loadingState}>
                  {t('users.loading')}
                </td>
              </tr>
            )}
            {!loading && filtered.length === 0 && (
              <tr>
                <td colSpan={8} className={styles.emptyState}>
                  {t('users.empty')}
                </td>
              </tr>
            )}
            {nonOrphan.map(renderRow)}
            {orphans.length > 0 && (
              <tr>
                <td colSpan={8} className={styles.sectionLabel}>
                  {t('users.orphan_group')}
                </td>
              </tr>
            )}
            {orphans.map(renderRow)}
          </tbody>
        </table>
      </div>
    </div>
  );
}
