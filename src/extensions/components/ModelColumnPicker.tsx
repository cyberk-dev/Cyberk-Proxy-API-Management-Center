import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { SelectionCheckbox } from '@/components/ui/SelectionCheckbox';
import { IconChevronDown } from '@/components/ui/icons';
import styles from './ModelColumnPicker.module.scss';

export interface ModelColumnOption {
  name: string;
  requests: number;
}

interface ModelColumnPickerProps {
  availableModels: ModelColumnOption[];
  selected: string[];
  onChange: (next: string[]) => void;
}

function formatNumber(n: number): string {
  return n.toLocaleString('en-US');
}

export function ModelColumnPicker({
  availableModels,
  selected,
  onChange
}: ModelColumnPickerProps) {
  const { t } = useTranslation('extensions');
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const rootRef = useRef<HTMLDivElement | null>(null);

  // Close on outside click / Esc.
  useEffect(() => {
    if (!open) return;

    const onDocDown = (e: MouseEvent) => {
      if (!rootRef.current) return;
      if (!rootRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', onDocDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDocDown);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  const filteredModels = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return availableModels;
    return availableModels.filter((m) => m.name.toLowerCase().includes(q));
  }, [availableModels, query]);

  const selectedSet = useMemo(() => new Set(selected), [selected]);

  const toggleModel = useCallback(
    (name: string) => {
      if (selectedSet.has(name)) {
        onChange(selected.filter((m) => m !== name));
      } else {
        onChange([...selected, name]);
      }
    },
    [selected, selectedSet, onChange]
  );

  const clearAll = useCallback(() => onChange([]), [onChange]);

  const selectTop = useCallback(
    (n: number) => {
      onChange(availableModels.slice(0, n).map((m) => m.name));
    },
    [availableModels, onChange]
  );

  const triggerLabel = selected.length
    ? t('users.model_columns_button_count', { count: selected.length })
    : t('users.model_columns_button');

  return (
    <div ref={rootRef} className={styles.root}>
      <Button
        variant="secondary"
        size="sm"
        onClick={() => setOpen((v) => !v)}
        className={styles.trigger}
        aria-haspopup="true"
        aria-expanded={open}
      >
        <span className={styles.triggerLabel}>{triggerLabel}</span>
        <IconChevronDown size={14} />
      </Button>

      {open && (
        <div className={styles.panel} aria-label={t('users.model_columns_button')}>
          <div className={styles.searchRow}>
            <input
              className={styles.searchInput}
              type="text"
              placeholder={t('users.model_columns_search')}
              aria-label={t('users.model_columns_search')}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              autoFocus
            />
          </div>

          <div className={styles.actionsRow}>
            <button
              type="button"
              className={styles.actionBtn}
              disabled={availableModels.length === 0}
              onClick={() => selectTop(3)}
            >
              {t('users.model_columns_select_top3')}
            </button>
            <button
              type="button"
              className={styles.actionBtn}
              disabled={selected.length === 0}
              onClick={clearAll}
            >
              {t('users.model_columns_clear')}
            </button>
          </div>

          <div className={styles.list}>
            {filteredModels.length === 0 && (
              <div className={styles.empty}>{t('users.model_columns_empty')}</div>
            )}
            {filteredModels.map((m) => (
              <label key={m.name} className={styles.row}>
                <SelectionCheckbox
                  checked={selectedSet.has(m.name)}
                  onChange={() => toggleModel(m.name)}
                  ariaLabel={m.name}
                />
                <span className={styles.rowName} title={m.name}>
                  {m.name}
                </span>
                <span className={styles.rowCount}>{formatNumber(m.requests)}</span>
              </label>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
