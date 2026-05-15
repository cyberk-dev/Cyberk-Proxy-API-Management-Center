import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { IconChevronDown, IconX } from '@/components/ui/icons';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useAuthStore } from '@/stores';
import { promptsApi } from '@/services/api';
import type {
  PromptDetail,
  PromptMessage,
  PromptSession,
  PromptUserSummary,
} from '@/types/prompts';
import styles from './PromptsPage.module.scss';

const DEFAULT_LIMIT = 200;

function relativeTime(iso?: string): string {
  if (!iso) return '—';
  const ts = new Date(iso).getTime();
  if (!Number.isFinite(ts)) return '—';
  const diff = Date.now() - ts;
  if (diff < 60_000) return 'just now';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`;
  if (diff < 7 * 86_400_000) return `${Math.floor(diff / 86_400_000)}d ago`;
  return new Date(iso).toLocaleDateString();
}

function timeOfDay(iso: string): string {
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return '';
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false });
}

function statusClass(status: number): string {
  if (!status) return '';
  if (status >= 200 && status < 300) return styles.statusOk;
  return styles.statusErr;
}

function getErrorMessage(err: unknown, fallback: string): string {
  if (err instanceof Error) return err.message || fallback;
  if (typeof err === 'string') return err || fallback;
  return fallback;
}

function summarizeBlock(b: { type: string; text?: string; media_type?: string; bytes?: number; sha256?: string; url?: string }): string {
  if (b.type === 'text') {
    const t = b.text ?? '';
    return t.length > 80 ? `text: ${t.slice(0, 80)}…` : `text: ${t}`;
  }
  const parts = [b.type];
  if (b.media_type) parts.push(b.media_type);
  if (typeof b.bytes === 'number') parts.push(`${b.bytes}B`);
  if (b.sha256) parts.push(`sha256=${b.sha256}`);
  if (b.url) parts.push(b.url);
  return parts.join(' · ');
}

export function PromptsPage() {
  const { t } = useTranslation();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);

  const [users, setUsers] = useState<PromptUserSummary[]>([]);
  const [usersLoading, setUsersLoading] = useState(true);
  const [usersError, setUsersError] = useState('');

  const [selectedKey, setSelectedKey] = useState<string | null>(null);
  const [detail, setDetail] = useState<PromptDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState('');

  const [expandedCWDs, setExpandedCWDs] = useState<Set<string>>(new Set());
  const [selectedMessage, setSelectedMessage] = useState<PromptMessage | null>(null);
  const [selectedSession, setSelectedSession] = useState<PromptSession | null>(null);

  const [pasteInput, setPasteInput] = useState('');

  const loadUsers = useCallback(async () => {
    setUsersLoading(true);
    setUsersError('');
    try {
      const res = await promptsApi.listUsers();
      setUsers(res.users || []);
    } catch (err) {
      setUsersError(getErrorMessage(err, t('notification.refresh_failed')));
    } finally {
      setUsersLoading(false);
    }
  }, [t]);

  const loadDetail = useCallback(async (keyOrHash: string) => {
    setDetailLoading(true);
    setDetailError('');
    setSelectedMessage(null);
    setSelectedSession(null);
    try {
      const res = await promptsApi.getDetail(keyOrHash, DEFAULT_LIMIT);
      setDetail(res);
      // Expand all CWDs by default for compact overview.
      setExpandedCWDs(new Set(res.groups.map((g) => g.cwd)));
    } catch (err) {
      setDetail(null);
      setDetailError(getErrorMessage(err, t('notification.refresh_failed')));
    } finally {
      setDetailLoading(false);
    }
  }, [t]);

  const handleRefresh = useCallback(async () => {
    await loadUsers();
    if (selectedKey) await loadDetail(selectedKey);
  }, [loadUsers, loadDetail, selectedKey]);

  useHeaderRefresh(handleRefresh);

  useEffect(() => {
    if (connectionStatus !== 'connected') return;
    loadUsers();
  }, [connectionStatus, loadUsers]);

  useEffect(() => {
    if (!selectedKey) return;
    loadDetail(selectedKey);
  }, [selectedKey, loadDetail]);

  const handleSelectUser = (u: PromptUserSummary) => {
    setSelectedKey(u.key_hash);
    setPasteInput('');
  };

  const handlePasteSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = pasteInput.trim();
    if (!trimmed) return;
    setSelectedKey(trimmed);
  };

  const toggleCWD = (cwd: string) => {
    setExpandedCWDs((prev) => {
      const next = new Set(prev);
      if (next.has(cwd)) next.delete(cwd);
      else next.add(cwd);
      return next;
    });
  };

  const handleSelectMessage = (sess: PromptSession, msg: PromptMessage) => {
    setSelectedSession(sess);
    setSelectedMessage(msg);
  };

  const closeDetail = () => {
    setSelectedMessage(null);
    setSelectedSession(null);
  };

  const messageKey = (sid: string, ts: string) => `${sid}::${ts}`;
  const selectedMessageKey = useMemo(
    () =>
      selectedSession && selectedMessage
        ? messageKey(selectedSession.session_id, selectedMessage.ts)
        : '',
    [selectedSession, selectedMessage]
  );

  const showDetailColumn = !!selectedMessage;

  return (
    <div className={styles.container}>
      <div className={styles.pageHeader}>
        <h1 className={styles.pageTitle}>{t('nav.prompts', { defaultValue: 'Prompts' })}</h1>
        <p className={styles.description}>
          {t('prompts_page.description', {
            defaultValue: 'Browse captured user prompts grouped by working directory and session.',
          })}
        </p>
      </div>

      {(usersError || detailError) && (
        <div className={styles.errorBox}>{usersError || detailError}</div>
      )}

      <div className={`${styles.layout} ${showDetailColumn ? styles.withDetail : ''}`}>
        {/* LEFT: user list */}
        <div className={styles.column}>
          <div className={styles.columnHeader}>
            <span>{t('prompts_page.users', { defaultValue: 'API keys' })}</span>
            <span className={styles.badge}>{users.length}</span>
          </div>
          <form onSubmit={handlePasteSubmit} className={styles.pasteRow}>
            <Input
              value={pasteInput}
              onChange={(e) => setPasteInput(e.target.value)}
              placeholder={t('prompts_page.paste_placeholder', {
                defaultValue: 'Paste API key or 12-char hash…',
              })}
              className={styles.pasteInput}
            />
            <span className={styles.pasteHint}>
              {t('prompts_page.paste_hint', {
                defaultValue: 'Press Enter to look up a key outside the configured list.',
              })}
            </span>
          </form>
          <div className={styles.columnBody}>
            {usersLoading ? (
              <div className={styles.loading}><LoadingSpinner /></div>
            ) : users.length === 0 ? (
              <EmptyState
                title={t('prompts_page.no_users_title', { defaultValue: 'No keys with prompt data' })}
                description={t('prompts_page.no_users_desc', {
                  defaultValue: 'Configure API keys and enable prompt_log to start capturing.',
                })}
              />
            ) : (
              <div className={styles.userList}>
                {users.map((u) => (
                  <button
                    key={u.key_hash}
                    type="button"
                    className={`${styles.userRow} ${selectedKey === u.key_hash ? styles.selected : ''}`}
                    onClick={() => handleSelectUser(u)}
                  >
                    <span className={styles.userKeyHint}>
                      {u.key_hint || u.key_hash}
                      {!u.configured && <span className={styles.badgeWarn}>orphan</span>}
                    </span>
                    <span className={styles.userMeta}>
                      <span className={styles.userMetaItem}>{u.message_count} msgs</span>
                      <span className={styles.userMetaItem}>{u.session_count} sess</span>
                      <span className={styles.userMetaItem}>{u.cwd_count} cwds</span>
                      <span className={styles.userMetaItem}>{relativeTime(u.last_seen)}</span>
                    </span>
                  </button>
                ))}
              </div>
            )}
          </div>
        </div>

        {/* MIDDLE: tree */}
        <div className={styles.column}>
          <div className={styles.columnHeader}>
            <span>
              {selectedKey
                ? detail?.key_hint || detail?.key_hash || selectedKey
                : t('prompts_page.select_user', { defaultValue: 'Select a key' })}
            </span>
            {detail && (
              <span className={styles.badge}>
                {detail.total_messages} msgs · {detail.total_sessions} sess
              </span>
            )}
          </div>
          <div className={styles.columnBody}>
            {!selectedKey ? (
              <div className={styles.placeholder}>
                {t('prompts_page.placeholder_left', {
                  defaultValue: 'Pick an API key on the left to see its prompt history.',
                })}
              </div>
            ) : detailLoading ? (
              <div className={styles.loading}><LoadingSpinner /></div>
            ) : !detail || detail.groups.length === 0 ? (
              <EmptyState
                title={t('prompts_page.no_prompts_title', { defaultValue: 'No prompts captured' })}
                description={t('prompts_page.no_prompts_desc', {
                  defaultValue: 'This key has no logged messages yet.',
                })}
              />
            ) : (
              <div className={styles.tree}>
                {detail.groups.map((group) => {
                  const open = expandedCWDs.has(group.cwd);
                  return (
                    <div key={group.cwd} className={styles.cwdGroup}>
                      <button
                        type="button"
                        className={styles.cwdHeader}
                        onClick={() => toggleCWD(group.cwd)}
                      >
                        <span className={`${styles.chevron} ${open ? styles.chevronOpen : ''}`}>
                          <IconChevronDown size={12} />
                        </span>
                        <span className={styles.cwdPath}>{group.cwd}</span>
                        <span className={styles.badge}>
                          {group.message_count}m · {group.sessions.length}s
                        </span>
                      </button>
                      {open && (
                        <div className={styles.sessionList}>
                          {group.sessions.map((sess) => (
                            <div key={sess.session_id} className={styles.sessionCard}>
                              <div className={styles.sessionHead}>
                                <span className={styles.badge}>{sess.client || 'unknown'}</span>
                                {sess.client_version && (
                                  <span className={styles.badge}>{sess.client_version}</span>
                                )}
                                {sess.models.map((m) => (
                                  <span key={m} className={styles.badge}>{m}</span>
                                ))}
                                <span className={styles.sessionId} title={sess.session_id}>
                                  {sess.session_id}
                                </span>
                                <span className={styles.sessionTime}>
                                  {relativeTime(sess.last_seen)}
                                </span>
                              </div>
                              <div className={styles.messageList}>
                                {sess.messages.map((msg) => {
                                  const key = messageKey(sess.session_id, msg.ts);
                                  const isSelected = key === selectedMessageKey;
                                  const preview = msg.prompt ?? '';
                                  return (
                                    <button
                                      key={key}
                                      type="button"
                                      className={`${styles.messageRow} ${isSelected ? styles.selectedMsg : ''}`}
                                      onClick={() => handleSelectMessage(sess, msg)}
                                    >
                                      <span className={styles.msgTime}>{timeOfDay(msg.ts)}</span>
                                      <span className={styles.msgModel}>{msg.model || '—'}</span>
                                      <span className={styles.msgText}>
                                        {preview.replace(/\s+/g, ' ').slice(0, 200) || '(empty)'}
                                      </span>
                                      <span className={`${styles.msgStatus} ${statusClass(msg.status)}`}>
                                        {msg.status || ''}
                                      </span>
                                    </button>
                                  );
                                })}
                              </div>
                              {sess.truncated && (
                                <div className={styles.truncBanner}>
                                  {t('prompts_page.session_truncated', {
                                    defaultValue: 'Older messages truncated — increase limit to see more.',
                                  })}
                                </div>
                              )}
                            </div>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        </div>

        {/* RIGHT: message detail */}
        {showDetailColumn && selectedMessage && selectedSession && (
          <div className={styles.column}>
            <div className={styles.detailHeader}>
              <h3 className={styles.detailTitle}>
                {t('prompts_page.detail_title', { defaultValue: 'Message detail' })}
              </h3>
              <Button variant="ghost" size="sm" onClick={closeDetail} aria-label="Close">
                <IconX size={16} />
              </Button>
            </div>
            <div className={styles.detailBody}>
              <div className={styles.detailMeta}>
                <span className={styles.detailMetaKey}>Time</span>
                <span className={styles.detailMetaVal}>{new Date(selectedMessage.ts).toLocaleString()}</span>
                <span className={styles.detailMetaKey}>Model</span>
                <span className={styles.detailMetaVal}>{selectedMessage.model || '—'}</span>
                <span className={styles.detailMetaKey}>Provider</span>
                <span className={styles.detailMetaVal}>{selectedMessage.provider || '—'}</span>
                <span className={styles.detailMetaKey}>Status</span>
                <span className={`${styles.detailMetaVal} ${statusClass(selectedMessage.status)}`}>
                  {selectedMessage.status || '—'}
                </span>
                <span className={styles.detailMetaKey}>Client</span>
                <span className={styles.detailMetaVal}>
                  {selectedSession.client}
                  {selectedSession.client_version ? ` ${selectedSession.client_version}` : ''}
                </span>
                <span className={styles.detailMetaKey}>Session</span>
                <span className={styles.detailMetaVal}>{selectedSession.session_id}</span>
              </div>
              <div>
                <div className={styles.detailMetaKey} style={{ marginBottom: 4 }}>Prompt</div>
                <div className={styles.detailPrompt}>{selectedMessage.prompt || '(empty)'}</div>
              </div>
              {selectedMessage.blocks && selectedMessage.blocks.length > 0 && (
                <div>
                  <div className={styles.detailMetaKey} style={{ marginBottom: 4 }}>Blocks</div>
                  <div className={styles.detailBlocks}>
                    {selectedMessage.blocks.map((b, i) => (
                      <div key={i} className={styles.detailBlock}>{summarizeBlock(b)}</div>
                    ))}
                  </div>
                </div>
              )}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
