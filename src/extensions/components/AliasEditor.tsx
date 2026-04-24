import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { IconCheck, IconX } from '@/components/ui/icons';
import styles from '../pages/UsersPage.module.scss';

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

interface AliasEditorProps {
  apiKey: string;
  value: string | undefined;
  disabled?: boolean;
  onSave: (apiKey: string, alias: string) => Promise<void>;
  onError?: (msg: string) => void;
  onSuccess?: () => void;
}

export function AliasEditor({
  apiKey,
  value,
  disabled,
  onSave,
  onError,
  onSuccess
}: AliasEditorProps) {
  const { t } = useTranslation('extensions');
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [saving, setSaving] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (editing && inputRef.current) {
      inputRef.current.focus();
      inputRef.current.select();
    }
  }, [editing]);

  const beginEdit = () => {
    setDraft(value ?? '');
    setEditing(true);
  };

  const cancel = () => {
    setEditing(false);
    setDraft('');
  };

  const commit = async () => {
    if (saving) return;
    setSaving(true);
    try {
      await onSave(apiKey, draft);
      setEditing(false);
      onSuccess?.();
    } catch (e) {
      const msg = e instanceof Error ? e.message : t('users.save_failed');
      onError?.(msg);
    } finally {
      setSaving(false);
    }
  };

  const handleKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      void commit();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancel();
    }
  };

  const handleRowClick = (e: React.MouseEvent) => {
    // Prevent row navigation when interacting with editor.
    e.stopPropagation();
  };

  if (editing) {
    return (
      <div className={styles.aliasEditor} onClick={handleRowClick}>
        <div className={styles.aliasEditorRow}>
          <input
            ref={inputRef}
            className={styles.aliasInput}
            type="text"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={handleKey}
            placeholder={t('users.set_alias')}
            disabled={saving}
            maxLength={64}
          />
          <button
            className={`${styles.iconBtn} ${styles.iconBtnPrimary}`}
            onClick={() => void commit()}
            disabled={saving}
            type="button"
            aria-label={t('users.save')}
            title={t('users.save')}
          >
            <IconCheck size={14} />
          </button>
          <button
            className={styles.iconBtn}
            onClick={cancel}
            disabled={saving}
            type="button"
            aria-label={t('users.cancel')}
            title={t('users.cancel')}
          >
            <IconX size={14} />
          </button>
        </div>
        <div className={styles.aliasEditorHint} title={apiKey}>
          {apiKey}
        </div>
      </div>
    );
  }

  return (
    <div className={styles.aliasView} onClick={handleRowClick}>
      <span className={value ? styles.aliasText : styles.aliasKeyFallback} title={apiKey}>
        {value ?? apiKey}
      </span>
      {!disabled && (
        <button
          className={styles.iconBtn}
          onClick={beginEdit}
          type="button"
          aria-label={t('users.edit_alias')}
          title={t('users.edit_alias')}
        >
          <IconPencil size={13} />
        </button>
      )}
    </div>
  );
}
