import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useUsageData } from '@/components/usage';
import { useConfigStore, useNotificationStore } from '@/stores';
import { Select } from '@/components/ui/Select';
import { useKeyAliases } from '../hooks/useKeyAliases';
import { pivotByKey, type PerKeyStats } from '../utils/keyPivot';
import { buildKeyList } from '../utils/keyIndex';
import {
  filterUsageByUsersTimeRange,
  isUsersTimeRange,
  USERS_TIME_RANGE_OPTIONS,
  type UsersTimeRange
} from '../utils/timeRangeFilter';
import { formatNumber } from '../utils/keyDisplay';
import { AliasEditor } from '../components/AliasEditor';
import { ModelColumnPicker } from '../components/ModelColumnPicker';
import styles from './UsersPage.module.scss';

const TIME_RANGE_STORAGE_KEY = 'cli-proxy-users-time-range-v1';
const SELECTED_MODELS_STORAGE_KEY = 'cli-proxy-users-selected-models-v1';
const DEFAULT_TIME_RANGE: UsersTimeRange = '24h';
const DEFAULT_TOP_N = 3;

// Trim a vendor prefix from a model name for compact column headers.
// Keeps the full name in the `title` attribute so hover still discloses it.
// Falls back to the full name if stripping would leave an empty label.
function displayModelName(model: string): string {
  if (model.startsWith('claude-')) {
    const tail = model.slice('claude-'.length);
    if (tail.length > 0) return tail;
  }
  return model;
}

function loadTimeRange(): UsersTimeRange {
  try {
    if (typeof localStorage === 'undefined') return DEFAULT_TIME_RANGE;
    const raw = localStorage.getItem(TIME_RANGE_STORAGE_KEY);
    return isUsersTimeRange(raw) ? raw : DEFAULT_TIME_RANGE;
  } catch {
    return DEFAULT_TIME_RANGE;
  }
}

// An empty stored array is treated as "not stored" so that clearing all
// models doesn't leave the user with a permanently blank table. Next visit
// will re-seed the top-N default instead.
function loadSelectedModels(): { value: string[]; stored: boolean } {
  try {
    if (typeof localStorage === 'undefined') return { value: [], stored: false };
    const raw = localStorage.getItem(SELECTED_MODELS_STORAGE_KEY);
    if (raw === null) return { value: [], stored: false };
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return { value: [], stored: false };
    const value = parsed
      .filter((item): item is string => typeof item === 'string')
      .map((s) => s.trim())
      .filter(Boolean);
    return { value, stored: value.length > 0 };
  } catch {
    return { value: [], stored: false };
  }
}

export function UsersPage() {
  const { t } = useTranslation('extensions');
  const navigate = useNavigate();
  const { showNotification } = useNotificationStore();
  const config = useConfigStore((s) => s.config);
  const { usage, loading: usageLoading, error: usageError } = useUsageData();
  const { aliases, loading: aliasesLoading, error: aliasesError, saveAlias } = useKeyAliases();

  const [filter, setFilter] = useState('');

  const [timeRange, setTimeRange] = useState<UsersTimeRange>(loadTimeRange);

  // On first visit (no localStorage entry) we seed with top-N by requests.
  // `selectionStored` tracks whether the current selection is user-authored
  // or still awaiting seeding — see `loadSelectedModels` for the "empty = not
  // stored" rule.
  const [initialSelectionState] = useState(loadSelectedModels);
  const [selectedModels, setSelectedModels] = useState<string[]>(
    initialSelectionState.value
  );
  const [selectionStored, setSelectionStored] = useState<boolean>(
    initialSelectionState.stored
  );

  // Persist time range.
  useEffect(() => {
    try {
      localStorage.setItem(TIME_RANGE_STORAGE_KEY, timeRange);
    } catch {
      /* ignore */
    }
  }, [timeRange]);

  // Persist selected models. Empty arrays are intentionally NOT persisted so
  // that clearing all models falls back to the auto-seeded top-N next visit.
  useEffect(() => {
    if (!selectionStored) return;
    try {
      if (selectedModels.length === 0) {
        localStorage.removeItem(SELECTED_MODELS_STORAGE_KEY);
      } else {
        localStorage.setItem(SELECTED_MODELS_STORAGE_KEY, JSON.stringify(selectedModels));
      }
    } catch {
      /* ignore */
    }
  }, [selectedModels, selectionStored]);

  const loading = usageLoading || aliasesLoading;
  const error = usageError || aliasesError;

  const knownKeys = useMemo(() => config?.apiKeys || [], [config?.apiKeys]);

  // `pivotByKey` takes a cost function but we don't display cost anywhere —
  // return 0 to keep the signature satisfied without pulling price data.
  const costFn = useMemo(() => () => 0, []);

  const filteredUsage = useMemo(
    () => filterUsageByUsersTimeRange(usage, timeRange),
    [usage, timeRange]
  );

  const rows = useMemo<PerKeyStats[]>(() => {
    const pivoted = pivotByKey(filteredUsage, knownKeys, aliases, costFn);
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
  }, [filteredUsage, knownKeys, aliases, costFn]);

  // Aggregate model counts directly from usage data (not from `rows`) so the
  // picker's list doesn't rebuild on every alias keystroke.
  const availableModels = useMemo(() => {
    const root = filteredUsage as { apis?: unknown } | null | undefined;
    const apis =
      root && typeof root === 'object' && root.apis && typeof root.apis === 'object'
        ? (root.apis as Record<string, unknown>)
        : null;
    if (!apis) return [];
    const map = new Map<string, number>();
    for (const apiEntry of Object.values(apis)) {
      if (!apiEntry || typeof apiEntry !== 'object') continue;
      const models = (apiEntry as { models?: unknown }).models;
      if (!models || typeof models !== 'object') continue;
      for (const [modelName, modelEntry] of Object.entries(models as Record<string, unknown>)) {
        if (!modelEntry || typeof modelEntry !== 'object') continue;
        const tr = (modelEntry as { total_requests?: unknown }).total_requests;
        const n = typeof tr === 'number' && tr > 0 ? tr : 0;
        if (n > 0) {
          map.set(modelName, (map.get(modelName) ?? 0) + n);
        }
      }
    }
    return Array.from(map, ([name, requests]) => ({ name, requests })).sort(
      (a, b) => b.requests - a.requests
    );
  }, [filteredUsage]);

  // On first visit (localStorage empty), show top-N recomputed from the
  // current time range's data instead of an empty table. Once the user picks
  // anything, `selectionStored` flips true and we honor their choice forever.
  const effectiveSelectedModels = useMemo<string[]>(() => {
    if (selectionStored) return selectedModels;
    return availableModels.slice(0, DEFAULT_TOP_N).map((m) => m.name);
  }, [selectionStored, selectedModels, availableModels]);

  const onSelectedModelsChange = (next: string[]) => {
    setSelectedModels(next);
    // Clearing everything reverts to the auto-seeded top-N; don't flip the
    // stored flag in that case so re-opening the page restores a sensible
    // default instead of showing a blank table forever.
    setSelectionStored(next.length > 0);
  };

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

  const timeRangeOptions = useMemo(
    () =>
      USERS_TIME_RANGE_OPTIONS.map((value) => ({
        value,
        label: t(`users.range_${value}`)
      })),
    [t]
  );

  const totalColumns = 2 + effectiveSelectedModels.length;

  // Stable index lookup: route params carry the position in this canonical
  // list so API keys never appear in the URL / browser history / referer.
  const keyIndexMap = useMemo(() => {
    const list = buildKeyList(knownKeys, usage);
    const map = new Map<string, number>();
    list.forEach((k, i) => map.set(k, i));
    return map;
  }, [knownKeys, usage]);

  const renderRow = (r: PerKeyStats) => {
    const perModelMap = new Map<string, number>(
      r.perModel.map((m) => [m.model, m.requests])
    );
    const idx = keyIndexMap.get(r.apiKey);
    return (
      <tr
        key={r.apiKey}
        className={r.orphan ? styles.orphan : undefined}
        onClick={() => {
          if (idx !== undefined) navigate(`/custom/users/${idx}`);
        }}
      >
        <td className={styles.aliasCell}>
          <AliasEditor
            apiKey={r.apiKey}
            value={r.alias}
            disabled={r.orphan}
            onSave={onSaveAlias}
            onError={onSaveAliasError}
          />
          {r.orphan && (
            <span className={styles.orphanBadge}>{t('users.orphan_badge')}</span>
          )}
        </td>
        <td className={styles.numeric}>{formatNumber(r.totalRequests)}</td>
        {effectiveSelectedModels.map((m) => (
          <td key={m} className={styles.numeric}>
            {formatNumber(perModelMap.get(m) ?? 0)}
          </td>
        ))}
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
          <div className={styles.timeRangeGroup}>
            <span className={styles.timeRangeLabel}>{t('users.range_filter')}</span>
            <Select
              value={timeRange}
              options={timeRangeOptions}
              onChange={(value) => setTimeRange(value as UsersTimeRange)}
              className={styles.timeRangeSelectControl}
              ariaLabel={t('users.range_filter')}
              fullWidth={false}
            />
          </div>
          <ModelColumnPicker
            availableModels={availableModels}
            selected={effectiveSelectedModels}
            onChange={onSelectedModelsChange}
          />
        </div>
      </div>

      {error && <div className={styles.errorBox}>{error}</div>}

      <div className={styles.tableCard}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>{t('users.col_alias')}</th>
              <th className={styles.numeric}>{t('users.col_requests')}</th>
              {effectiveSelectedModels.map((m) => (
                <th key={m} className={styles.numeric} title={m}>
                  {displayModelName(m)}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {loading && rows.length === 0 && (
              <tr>
                <td colSpan={totalColumns} className={styles.loadingState}>
                  {t('users.loading')}
                </td>
              </tr>
            )}
            {!loading && filtered.length === 0 && (
              <tr>
                <td colSpan={totalColumns} className={styles.emptyState}>
                  {t('users.empty')}
                </td>
              </tr>
            )}
            {nonOrphan.map(renderRow)}
            {orphans.length > 0 && (
              <tr>
                <td colSpan={totalColumns} className={styles.sectionLabel}>
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
