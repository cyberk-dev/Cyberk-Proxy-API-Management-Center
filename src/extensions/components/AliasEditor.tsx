import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import { useTranslation } from 'react-i18next';
import styles from '../pages/UsersPage.module.scss';

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
          className={`${styles.miniBtn} ${styles.miniBtnPrimary}`}
          onClick={() => void commit()}
          disabled={saving}
          type="button"
        >
          {saving ? '…' : t('users.save')}
        </button>
        <button
          className={styles.miniBtn}
          onClick={cancel}
          disabled={saving}
          type="button"
        >
          {t('users.cancel')}
        </button>
      </div>
    );
  }

  return (
    <div className={styles.aliasView} onClick={handleRowClick}>
      {value ? (
        <span className={styles.aliasText}>{value}</span>
      ) : (
        <span className={styles.aliasMuted}>{t('users.no_alias')}</span>
      )}
      {!disabled && (
        <button
          className={styles.aliasEditBtn}
          onClick={beginEdit}
          type="button"
          aria-label={t('users.edit_alias')}
        >
          {t('users.edit_alias')}
        </button>
      )}
    </div>
  );
}
