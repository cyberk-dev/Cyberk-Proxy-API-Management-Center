import {
  Fragment,
  useCallback,
  useDeferredValue,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import {
  IconChevronDown,
  IconChevronLeft,
  IconSearch,
  IconX,
} from '@/components/ui/icons';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useAuthStore } from '@/stores';
import { promptsApi } from '@/services/api';
import { formatCwdLabel } from '@/utils/cwdLabel';
import type {
  PromptCWDGroup,
  PromptDetail,
  PromptMessage,
  PromptSearchHit,
  PromptSearchResponse,
  PromptSession,
  PromptTemplate,
  PromptUserSummary,
} from '@/types/prompts';
import styles from './PromptsPage.module.scss';

const DEFAULT_LIMIT = 200;
const DEFAULT_SESSION_LIMIT = 200;
const DEFAULT_INITIAL_CWDS = 20;
const SESSION_LIMIT_MIN = 1;
const SESSION_LIMIT_MAX = 500;
const SEARCH_MIN_CHARS = 2;
const SEARCH_LIMIT = 200;
const INLINE_TEMPLATES_STORAGE_KEY = 'prompts.inlineTemplates';
const KEYS_PANEL_COLLAPSED_STORAGE_KEY = 'prompts.keysPanelCollapsed';
const SESSION_LIMIT_STORAGE_KEY = 'prompts.sessionLimit';

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

function getErrorMessage(err: unknown, fallback: string): string {
  if (err instanceof Error) return err.message || fallback;
  if (typeof err === 'string') return err || fallback;
  return fallback;
}

function summarizeBlock(b: {
  type: string;
  text?: string;
  media_type?: string;
  bytes?: number;
  sha256?: string;
  url?: string;
  truncated?: boolean;
  orig_bytes?: number;
  tool?: string;
  is_error?: boolean;
}): string {
  if (b.type === 'text') {
    if (b.text) {
      return b.text.length > 80 ? `text: ${b.text.slice(0, 80)}…` : `text: ${b.text}`;
    }
    const size = typeof b.bytes === 'number' ? `${b.bytes}B` : '?';
    return b.truncated && b.orig_bytes
      ? `text · ${size} (head+tail of ${b.orig_bytes}B)`
      : `text · ${size}`;
  }
  const parts = [b.type];
  if (b.tool) parts.push(b.tool);
  if (b.media_type) parts.push(b.media_type);
  if (typeof b.bytes === 'number') parts.push(`${b.bytes}B`);
  if (b.sha256) parts.push(`sha256=${b.sha256}`);
  if (b.url) parts.push(b.url);
  if (b.is_error) parts.push('error');
  return parts.join(' · ');
}

// Render a string with every case-insensitive occurrence of `query` wrapped
// in <mark>. The server's excerpt is already whitespace-collapsed, so a
// straight indexOf walk is enough. Empty query → plain text. Note: this
// matches NON-OVERLAPPING occurrences only — query "aa" against text "aaaa"
// highlights [0..2] and [2..4], skipping [1..3]. That matches the server's
// first-match excerpt window so the user never sees a missed mid-overlap
// match in practice.
function HighlightedExcerpt({ text, query }: { text: string; query: string }) {
  const q = query.trim();
  if (q.length < SEARCH_MIN_CHARS) return <>{text}</>;
  const lowText = text.toLowerCase();
  const lowQ = q.toLowerCase();
  const parts: React.ReactNode[] = [];
  let i = 0;
  while (i < text.length) {
    const idx = lowText.indexOf(lowQ, i);
    if (idx < 0) {
      parts.push(text.slice(i));
      break;
    }
    if (idx > i) parts.push(text.slice(i, idx));
    parts.push(
      <mark key={parts.length} className={styles.searchHighlight}>
        {text.slice(idx, idx + lowQ.length)}
      </mark>,
    );
    i = idx + lowQ.length;
  }
  return <>{parts.map((p, j) => <Fragment key={j}>{p}</Fragment>)}</>;
}

type SessionRef = { cwd: string; sid: string };

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

  // Keys section: when a key is selected, collapse the list down to a single
  // "Selected: X · Change" row so the CWD/sessions tree below has the bulk of
  // the column. Clicking Change brings the full list back. Default true so a
  // freshly-loaded page with no key selected shows the list.
  const [keysListExpanded, setKeysListExpanded] = useState(true);
  const [keysFilter, setKeysFilter] = useState('');

  const [expandedCWDs, setExpandedCWDs] = useState<Set<string>>(new Set());

  // Selection lives as identifiers, not direct refs. detail.groups is rebuilt
  // on every load-more / refresh, so a captured session object would go stale
  // and the messages list would re-render with the OLD messages array even
  // after the user paged more in. Resolving via memoized find() keeps the
  // current view bound to the latest detail.
  const [selectedSessionRef, setSelectedSessionRef] = useState<SessionRef | null>(null);
  const [selectedMessageTs, setSelectedMessageTs] = useState<string | null>(null);

  const selectedSession = useMemo<PromptSession | null>(() => {
    if (!detail || !selectedSessionRef) return null;
    const group = detail.groups.find((g) => g.cwd === selectedSessionRef.cwd);
    if (!group) return null;
    return group.sessions.find((s) => s.session_id === selectedSessionRef.sid) ?? null;
  }, [detail, selectedSessionRef]);

  const selectedMessage = useMemo<PromptMessage | null>(() => {
    if (!selectedSession || !selectedMessageTs) return null;
    return selectedSession.messages.find((m) => m.ts === selectedMessageTs) ?? null;
  }, [selectedSession, selectedMessageTs]);

  const [loadingMoreCWDs, setLoadingMoreCWDs] = useState<Set<string>>(new Set());
  const [cwdLoadErrors, setCwdLoadErrors] = useState<Record<string, string>>({});
  const [loadingMoreMessages, setLoadingMoreMessages] = useState<Set<string>>(new Set());
  const [messageLoadErrors, setMessageLoadErrors] = useState<Record<string, string>>({});

  const inFlightCWDsRef = useRef<Set<string>>(new Set());
  const inFlightMessagesRef = useRef<Set<string>>(new Set());

  const [pasteInput, setPasteInput] = useState('');

  const [inlineTemplates, setInlineTemplates] = useState<boolean>(() => {
    try {
      return localStorage.getItem(INLINE_TEMPLATES_STORAGE_KEY) === '1';
    } catch {
      return false;
    }
  });
  const [templateCache, setTemplateCache] = useState<Record<string, PromptTemplate>>({});
  const [templateLoading, setTemplateLoading] = useState<Set<string>>(new Set());
  const [expandedTemplateInDetail, setExpandedTemplateInDetail] = useState(false);

  const persistInlineTemplates = useCallback((v: boolean) => {
    setInlineTemplates(v);
    try {
      localStorage.setItem(INLINE_TEMPLATES_STORAGE_KEY, v ? '1' : '0');
    } catch {
      /* localStorage unavailable */
    }
  }, []);

  const [sessionLimit, setSessionLimit] = useState<number>(() => {
    try {
      const raw = localStorage.getItem(SESSION_LIMIT_STORAGE_KEY);
      if (raw) {
        const n = parseInt(raw, 10);
        if (Number.isFinite(n) && n >= SESSION_LIMIT_MIN && n <= SESSION_LIMIT_MAX) return n;
      }
    } catch {
      /* localStorage unavailable */
    }
    return DEFAULT_SESSION_LIMIT;
  });
  const [sessionLimitDraft, setSessionLimitDraft] = useState<string>(String(sessionLimit));
  const commitSessionLimit = useCallback((raw: string) => {
    const n = parseInt(raw, 10);
    if (!Number.isFinite(n) || n < SESSION_LIMIT_MIN || n > SESSION_LIMIT_MAX) {
      setSessionLimitDraft(String(sessionLimit));
      return;
    }
    if (n === sessionLimit) return;
    setSessionLimit(n);
    setSessionLimitDraft(String(n));
    try {
      localStorage.setItem(SESSION_LIMIT_STORAGE_KEY, String(n));
    } catch {
      /* localStorage unavailable */
    }
  }, [sessionLimit]);

  const [keysCollapsed, setKeysCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem(KEYS_PANEL_COLLAPSED_STORAGE_KEY) === '1';
    } catch {
      return false;
    }
  });
  const toggleKeysCollapsed = useCallback(() => {
    setKeysCollapsed((prev) => {
      const next = !prev;
      try {
        localStorage.setItem(KEYS_PANEL_COLLAPSED_STORAGE_KEY, next ? '1' : '0');
      } catch {
        /* localStorage unavailable */
      }
      return next;
    });
  }, []);

  // Search state. searchQuery is the controlled input value (immediate),
  // deferredQuery is what drives the fetch effect — useDeferredValue debounces
  // implicitly during transitions so the user doesn't see a request per
  // keystroke.
  const [searchQuery, setSearchQuery] = useState('');
  const deferredQuery = useDeferredValue(searchQuery);
  const [searchResults, setSearchResults] = useState<PromptSearchResponse | null>(null);
  const [searchLoading, setSearchLoading] = useState(false);
  const [searchError, setSearchError] = useState('');

  // Scroll memory + first-select-bottom flag. Keyed by `keyHash::session_id`
  // so the maps survive across session switches but reset on key change.
  // sessionsSeenRef tracks "has the bottom-scroll already fired for this
  // session" — without it, every refresh that re-creates the session object
  // would re-trigger the scroll-to-bottom and lose the user's reading
  // position.
  const messagesScrollRef = useRef<HTMLDivElement | null>(null);
  const scrollPosByKey = useRef<Map<string, number>>(new Map());
  const sessionsSeenRef = useRef<Set<string>>(new Set());

  // Pagination anchor for "load older messages". Capture scrollHeight BEFORE
  // dispatching the fetch; after the new messages render, restore scrollTop
  // by (newHeight - oldHeight) so the user's current read position stays put
  // instead of jumping to the top of the freshly-prepended block.
  const pendingScrollAnchor = useRef<number | null>(null);

  // Monotonic sequence for search requests. The deferred-vs-current race is
  // real: user types "abc" (fetch fires, in-flight), backspaces to "ab" (the
  // onChange synchronously clears searchResults), but the "abc" promise
  // resolves later and would overwrite cleared state without this guard.
  // Bumped on every fire AND on every onChange-driven clear so stale
  // responses can detect that they're no longer the current request.
  const searchSeqRef = useRef(0);

  const fetchTemplate = useCallback(async (hash: string): Promise<PromptTemplate | null> => {
    if (templateCache[hash]) return templateCache[hash];
    if (templateLoading.has(hash)) return null;
    setTemplateLoading((prev) => new Set(prev).add(hash));
    try {
      const tpl = await promptsApi.getTemplate(hash);
      setTemplateCache((prev) => ({ ...prev, [hash]: tpl }));
      return tpl;
    } catch {
      return null;
    } finally {
      setTemplateLoading((prev) => {
        const next = new Set(prev);
        next.delete(hash);
        return next;
      });
    }
  }, [templateCache, templateLoading]);

  const prewarmTemplates = useCallback((sessions: PromptSession[]) => {
    const hashes = new Set<string>();
    for (const s of sessions) {
      for (const m of s.messages) {
        if (m.prompt_template) hashes.add(m.prompt_template);
      }
    }
    for (const h of hashes) void fetchTemplate(h);
  }, [fetchTemplate]);

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
    setSelectedMessageTs(null);
    setSelectedSessionRef(null);
    setExpandedTemplateInDetail(false);
    setLoadingMoreCWDs(new Set());
    setCwdLoadErrors({});
    setLoadingMoreMessages(new Set());
    setMessageLoadErrors({});
    // Search + scroll memory belong to the previous key — purge here so a
    // sessionLimit-triggered reload also resets them. Doing this in
    // loadDetail rather than a selectedKey-change effect keeps the resets
    // OFF the cascading-render lint path (state writes from inside effects
    // are flagged for re-render churn).
    setSearchQuery('');
    setSearchResults(null);
    setSearchError('');
    setSearchLoading(false);
    // Bump so any in-flight search from the old key bails on resolution.
    searchSeqRef.current++;
    scrollPosByKey.current.clear();
    sessionsSeenRef.current.clear();
    pendingScrollAnchor.current = null;
    inFlightCWDsRef.current.clear();
    inFlightMessagesRef.current.clear();
    try {
      const res = await promptsApi.getDetail(keyOrHash, {
        limit: DEFAULT_LIMIT,
        inlineTemplates,
        sessionLimit,
        initialCwds: DEFAULT_INITIAL_CWDS,
      });
      setDetail(res);
      // Expand every CWD so the user can browse session titles immediately;
      // sessions are no longer nested-expanded (the reading pane is panel 2).
      setExpandedCWDs(new Set(res.groups.map((g) => g.cwd)));
      // Auto-select the most-recent session of the most-recent CWD so panel
      // 2 has content right after load. If the key has zero activity, leave
      // selection null — the empty-state placeholder in panel 2 takes over.
      const firstGroup = res.groups.find((g) => g.sessions.length > 0);
      const firstSession = firstGroup?.sessions[0];
      if (firstGroup && firstSession) {
        setSelectedSessionRef({ cwd: firstGroup.cwd, sid: firstSession.session_id });
      }
      const populated: PromptSession[] = [];
      for (const g of res.groups) populated.push(...g.sessions);
      prewarmTemplates(populated);
    } catch (err) {
      setDetail(null);
      setDetailError(getErrorMessage(err, t('notification.refresh_failed')));
    } finally {
      setDetailLoading(false);
    }
  }, [t, inlineTemplates, sessionLimit, prewarmTemplates]);

  const fetchCWDPage = useCallback(async (
    keyOrHash: string,
    cwd: string,
    before?: { ts: string; sid: string },
  ) => {
    if (inFlightCWDsRef.current.has(cwd)) return;
    inFlightCWDsRef.current.add(cwd);
    setLoadingMoreCWDs((prev) => {
      if (prev.has(cwd)) return prev;
      const next = new Set(prev);
      next.add(cwd);
      return next;
    });
    setCwdLoadErrors((prev) => {
      if (!prev[cwd]) return prev;
      const next: Record<string, string> = {};
      for (const k of Object.keys(prev)) {
        if (k !== cwd) next[k] = prev[k];
      }
      return next;
    });
    try {
      const res = await promptsApi.loadCWDSessions(keyOrHash, cwd, before, sessionLimit);
      const got: PromptCWDGroup | undefined = res.groups[0];
      setDetail((prev) => {
        if (!prev) return prev;
        if (res.key_hash !== prev.key_hash) return prev;
        const idx = prev.groups.findIndex((g) => g.cwd === cwd);
        if (idx < 0) return prev;
        const existing = prev.groups[idx];
        const seen = new Set(existing.sessions.map((s) => s.session_id));
        const incoming = got ? got.sessions.filter((s) => !seen.has(s.session_id)) : [];
        const mergedSessions = before ? [...existing.sessions, ...incoming] : incoming;
        const merged: PromptCWDGroup = {
          ...existing,
          sessions: mergedSessions,
          has_more: got ? got.has_more : false,
          session_count: got ? got.session_count : existing.session_count,
          last_seen: got ? got.last_seen : existing.last_seen,
          message_count: got ? got.message_count : existing.message_count,
        };
        const groups = prev.groups.slice();
        groups[idx] = merged;
        return { ...prev, groups };
      });
      if (got) prewarmTemplates(got.sessions);
    } catch (err) {
      const msg = getErrorMessage(err, t('notification.refresh_failed'));
      setCwdLoadErrors((prev) => ({ ...prev, [cwd]: msg }));
    } finally {
      inFlightCWDsRef.current.delete(cwd);
      setLoadingMoreCWDs((prev) => {
        if (!prev.has(cwd)) return prev;
        const next = new Set(prev);
        next.delete(cwd);
        return next;
      });
    }
  }, [t, sessionLimit, prewarmTemplates]);

  const fetchOlderMessages = useCallback(async (cwd: string, sid: string) => {
    if (!selectedKey) return;
    const skey = `${cwd}::${sid}`;
    if (inFlightMessagesRef.current.has(skey)) return;

    const group = detail?.groups.find((g) => g.cwd === cwd);
    const sess = group?.sessions.find((s) => s.session_id === sid);
    if (!sess || sess.messages.length === 0) return;
    const oldestTs = sess.messages[0].ts;

    inFlightMessagesRef.current.add(skey);
    setLoadingMoreMessages((prev) => {
      if (prev.has(skey)) return prev;
      const next = new Set(prev);
      next.add(skey);
      return next;
    });
    setMessageLoadErrors((prev) => {
      if (!prev[skey]) return prev;
      const next: Record<string, string> = {};
      for (const k of Object.keys(prev)) {
        if (k !== skey) next[k] = prev[k];
      }
      return next;
    });

    // Capture scrollHeight BEFORE the fetch — useLayoutEffect below adjusts
    // scrollTop by the delta after the new messages render, which keeps the
    // user pinned to the same logical row instead of jumping to the top of
    // the prepended block. Only captures when we're paging the currently-
    // selected session; otherwise the user can't see the list anyway.
    const isCurrent =
      selectedSessionRef && selectedSessionRef.cwd === cwd && selectedSessionRef.sid === sid;
    if (isCurrent && messagesScrollRef.current) {
      pendingScrollAnchor.current = messagesScrollRef.current.scrollHeight;
    }

    try {
      const res = await promptsApi.loadOlderMessages(selectedKey, cwd, sid, oldestTs, DEFAULT_LIMIT);
      const got = res.groups[0]?.sessions[0];
      // Track whether the merge will actually grow messages.length, since
      // the scroll-anchor consumer is the layout effect on that length.
      // If no growth happens (server returned nothing, or every message
      // was already in the window via dedup) the layout effect never
      // fires, and a leftover anchor would mis-adjust the NEXT load-more
      // using a scrollHeight from the wrong moment in time.
      let addedCount = 0;
      setDetail((prev) => {
        if (!prev) return prev;
        if (res.key_hash !== prev.key_hash) return prev;
        const gIdx = prev.groups.findIndex((g) => g.cwd === cwd);
        if (gIdx < 0) return prev;
        const sIdx = prev.groups[gIdx].sessions.findIndex((s) => s.session_id === sid);
        if (sIdx < 0) return prev;
        const existingSess = prev.groups[gIdx].sessions[sIdx];
        if (!got) {
          const sessions = prev.groups[gIdx].sessions.slice();
          sessions[sIdx] = { ...existingSess, truncated: false };
          const groups = prev.groups.slice();
          groups[gIdx] = { ...prev.groups[gIdx], sessions };
          return { ...prev, groups };
        }
        const dedupKey = (m: PromptMessage) =>
          `${m.ts}|${m.role ?? ''}|${(m.prompt ?? '').slice(0, 32)}`;
        const seen = new Set(existingSess.messages.map(dedupKey));
        const older = got.messages.filter((m) => !seen.has(dedupKey(m)));
        addedCount = older.length;
        const sessions = prev.groups[gIdx].sessions.slice();
        sessions[sIdx] = {
          ...existingSess,
          messages: [...older, ...existingSess.messages],
          truncated: got.truncated ?? false,
        };
        const groups = prev.groups.slice();
        groups[gIdx] = { ...prev.groups[gIdx], sessions };
        return { ...prev, groups };
      });
      if (addedCount === 0) {
        // No length change → layout effect won't fire → anchor would leak.
        pendingScrollAnchor.current = null;
      }
      if (got) prewarmTemplates([got]);
    } catch (err) {
      pendingScrollAnchor.current = null;
      const msg = getErrorMessage(err, t('notification.refresh_failed'));
      setMessageLoadErrors((prev) => ({ ...prev, [skey]: msg }));
    } finally {
      inFlightMessagesRef.current.delete(skey);
      setLoadingMoreMessages((prev) => {
        if (!prev.has(skey)) return prev;
        const next = new Set(prev);
        next.delete(skey);
        return next;
      });
    }
  }, [selectedKey, detail, selectedSessionRef, t, prewarmTemplates]);

  const handleRefresh = useCallback(async () => {
    await loadUsers();
    if (!selectedKey) return;
    try {
      const res = await promptsApi.refreshHeaders(selectedKey);
      setDetail((prev) => {
        if (!prev) return res;
        if (res.key_hash !== prev.key_hash) return prev;
        const respCwds = new Set(res.groups.map((g) => g.cwd));
        const merged: PromptCWDGroup[] = res.groups.map((g) => {
          const existing = prev.groups.find((p) => p.cwd === g.cwd);
          if (!existing) return g;
          return {
            ...existing,
            message_count: g.message_count,
            last_seen: g.last_seen,
            session_count: g.session_count,
            has_more: g.has_more,
          };
        });
        for (const existing of prev.groups) {
          if (!respCwds.has(existing.cwd)) merged.push(existing);
        }
        return { ...res, groups: merged };
      });
    } catch (err) {
      setDetailError(getErrorMessage(err, t('notification.refresh_failed')));
    }
  }, [loadUsers, selectedKey, t]);

  useHeaderRefresh(handleRefresh);

  useEffect(() => {
    if (connectionStatus !== 'connected') return;
    loadUsers();
  }, [connectionStatus, loadUsers]);

  useEffect(() => {
    if (!selectedKey) return;
    loadDetail(selectedKey);
  }, [selectedKey, loadDetail]);

  // Search effect. Triggered on deferredQuery so rapid typing collapses to a
  // single fetch. Below-min-chars is a no-op here because the input's
  // onChange already cleared state synchronously AND bumped searchSeqRef so
  // any in-flight older request bails when it resolves.
  //
  // setSearchLoading(true) is called synchronously from the effect body —
  // tripping react-hooks/set-state-in-effect, matched by the rest of the
  // file's loadUsers/loadDetail pattern. The earlier microtask wrapper was
  // cargo-culted around the lint rule without solving anything; explicit
  // suppression with rationale is honest.
  useEffect(() => {
    const q = deferredQuery.trim();
    if (!selectedKey || q.length < SEARCH_MIN_CHARS) return;
    const seq = ++searchSeqRef.current;
    // eslint-disable-next-line react-hooks/set-state-in-effect -- visible spinner during async fetch
    setSearchLoading(true);
    promptsApi
      .searchMessages(selectedKey, q, SEARCH_LIMIT)
      .then((res) => {
        if (searchSeqRef.current !== seq) return;
        setSearchResults(res);
        setSearchError('');
      })
      .catch((err) => {
        if (searchSeqRef.current !== seq) return;
        setSearchResults(null);
        setSearchError(getErrorMessage(err, t('notification.refresh_failed')));
      })
      .finally(() => {
        if (searchSeqRef.current !== seq) return;
        setSearchLoading(false);
      });
  }, [deferredQuery, selectedKey, t]);

  // Scroll memory: on session change, restore saved scrollTop if we've seen
  // this session before, else jump to bottom (latest message). Skipped while
  // search mode is active because the list is search hits, not chronological.
  useLayoutEffect(() => {
    const el = messagesScrollRef.current;
    if (!el || !selectedSession || searchResults) return;
    const key = `${detail?.key_hash ?? ''}::${selectedSession.session_id}`;
    if (sessionsSeenRef.current.has(key)) {
      const saved = scrollPosByKey.current.get(key);
      if (saved !== undefined) el.scrollTop = saved;
    } else {
      el.scrollTop = el.scrollHeight;
      sessionsSeenRef.current.add(key);
    }
    // Effect deps: when search closes (searchResults goes null) we want to
    // restore the saved scroll position too, so include searchResults.
  }, [selectedSession, searchResults, detail?.key_hash]);

  // Pagination anchor: after older messages are prepended, the messages list
  // grows but the user expects to stay at their previous row. Adjust scrollTop
  // by the height delta. Runs only when an anchor was captured (i.e. user
  // triggered load-older). selectedSession.messages.length is the trigger.
  const currentMessagesLen = selectedSession?.messages.length ?? 0;
  useLayoutEffect(() => {
    const el = messagesScrollRef.current;
    if (!el || pendingScrollAnchor.current === null) return;
    const oldHeight = pendingScrollAnchor.current;
    pendingScrollAnchor.current = null;
    const delta = el.scrollHeight - oldHeight;
    if (delta > 0) el.scrollTop += delta;
  }, [currentMessagesLen]);

  // Persist scrollTop on scroll so re-selecting the session restores position.
  // Uses rAF coalescing so a fast scroll doesn't fire 60 writes per second.
  const handleMessagesScroll = useCallback(() => {
    const el = messagesScrollRef.current;
    if (!el || !selectedSession || searchResults) return;
    const key = `${detail?.key_hash ?? ''}::${selectedSession.session_id}`;
    scrollPosByKey.current.set(key, el.scrollTop);
  }, [selectedSession, searchResults, detail?.key_hash]);

  const handleSelectUser = (u: PromptUserSummary) => {
    setSelectedKey(u.key_hash);
    setPasteInput('');
    // Auto-collapse the keys list once a key is picked so the CWD/sessions
    // tree gets the vertical space. The user can re-expand via Change.
    setKeysListExpanded(false);
  };

  const handlePasteSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = pasteInput.trim();
    if (!trimmed) return;
    setSelectedKey(trimmed);
    setKeysListExpanded(false);
  };

  const toggleCWD = (cwd: string) => {
    const willOpen = !expandedCWDs.has(cwd);
    setExpandedCWDs((prev) => {
      const next = new Set(prev);
      if (willOpen) next.add(cwd);
      else next.delete(cwd);
      return next;
    });
    if (willOpen && selectedKey) {
      const group = detail?.groups.find((g) => g.cwd === cwd);
      if (group && group.sessions.length === 0 && group.has_more && !loadingMoreCWDs.has(cwd)) {
        void fetchCWDPage(selectedKey, cwd);
      }
    }
  };

  const cursorOf = (s: PromptSession) => ({ ts: s.last_seen, sid: s.session_id });

  const handleLoadOlderSessions = (cwd: string) => {
    if (!selectedKey) return;
    const group = detail?.groups.find((g) => g.cwd === cwd);
    if (!group || group.sessions.length === 0) return;
    if (loadingMoreCWDs.has(cwd)) return;
    const last = group.sessions[group.sessions.length - 1];
    void fetchCWDPage(selectedKey, cwd, cursorOf(last));
  };

  const handleRetryCWD = (cwd: string) => {
    if (!selectedKey) return;
    const group = detail?.groups.find((g) => g.cwd === cwd);
    if (!group) return;
    const cursor = group.sessions.length > 0
      ? cursorOf(group.sessions[group.sessions.length - 1])
      : undefined;
    void fetchCWDPage(selectedKey, cwd, cursor);
  };

  const handleSelectSession = (cwd: string, sid: string) => {
    setSelectedSessionRef({ cwd, sid });
    setSelectedMessageTs(null);
    setExpandedTemplateInDetail(false);
  };

  const handleSelectMessage = (msg: PromptMessage) => {
    setSelectedMessageTs(msg.ts);
    setExpandedTemplateInDetail(false);
    if (msg.prompt_template) void fetchTemplate(msg.prompt_template);
  };

  const closeDetail = () => {
    setSelectedMessageTs(null);
  };

  // Search-hit click: deep-link into the session referenced by the hit,
  // then select the matching message. If the session isn't in the loaded
  // detail (CWD not expanded yet, or session past the loaded page), fire a
  // CWD fetch and expand it — the user may need to click again. PR1 keeps
  // it best-effort; chasing arbitrary-depth pagination from a search jump
  // is out of scope.
  const handleSelectSearchHit = (hit: PromptSearchHit) => {
    if (!detail) return;
    setExpandedCWDs((prev) => {
      if (prev.has(hit.cwd)) return prev;
      const next = new Set(prev);
      next.add(hit.cwd);
      return next;
    });
    const group = detail.groups.find((g) => g.cwd === hit.cwd);
    if (!group) return;
    const sess = group.sessions.find((s) => s.session_id === hit.session_id);
    if (!sess) {
      // Session not loaded — surface that to the user via the per-CWD
      // error slot (instead of silently no-op'ing) and kick off a fetch
      // so the next click can land. Two-click is acceptable for the
      // edge case; the visible feedback is the important part.
      if (selectedKey && group.has_more) {
        setCwdLoadErrors((prev) => ({
          ...prev,
          [hit.cwd]: t('prompts_page.search_jump_needs_load', {
            defaultValue: 'Session not loaded — fetching more sessions, click the search hit again afterwards.',
          }),
        }));
        void fetchCWDPage(selectedKey, hit.cwd);
      }
      return;
    }
    setSelectedSessionRef({ cwd: hit.cwd, sid: hit.session_id });
    // Mark seen so the bottom-scroll-on-first-select path doesn't fire —
    // we want to land on the matched message, not the bottom.
    const key = `${detail.key_hash}::${sess.session_id}`;
    sessionsSeenRef.current.add(key);
    // Close search; bump the seq so any in-flight fetch is muted.
    searchSeqRef.current++;
    setSearchQuery('');
    setSearchResults(null);
    setSearchError('');
    setSearchLoading(false);

    // Select the message if it's in the loaded window. If not (older than
    // currently-paged), just switch to the session — user can Load older.
    const target = sess.messages.find((m) => m.ts === hit.ts);
    if (target) {
      setSelectedMessageTs(target.ts);
      if (target.prompt_template) void fetchTemplate(target.prompt_template);
      // Scroll into view AFTER the layout pass. requestAnimationFrame ensures
      // the new selectedSession has rendered its rows so the data-attr query
      // finds something.
      requestAnimationFrame(() => {
        const el = document.querySelector<HTMLElement>(
          `[data-message-key="${CSS.escape(sess.session_id)}::${CSS.escape(target.ts)}"]`,
        );
        el?.scrollIntoView({ block: 'center', behavior: 'smooth' });
      });
    }
  };

  const messageKey = (sid: string, ts: string) => `${sid}::${ts}`;
  const selectedMessageKey = useMemo(
    () =>
      selectedSession && selectedMessage
        ? messageKey(selectedSession.session_id, selectedMessage.ts)
        : '',
    [selectedSession, selectedMessage],
  );

  const showDetailColumn = !!selectedMessage;

  // Filter keys by hint substring. Lowercased so the filter is case-
  // insensitive. Empty filter → full list.
  const filteredUsers = useMemo(() => {
    const q = keysFilter.trim().toLowerCase();
    if (!q) return users;
    return users.filter((u) => {
      const hint = (u.key_hint || u.key_hash).toLowerCase();
      return hint.includes(q);
    });
  }, [users, keysFilter]);

  const selectedUser = useMemo(
    () => users.find((u) => u.key_hash === selectedKey) ?? null,
    [users, selectedKey],
  );

  return (
    <div className={styles.container}>
      <div className={styles.pageHeader}>
        <h1 className={styles.pageTitle}>{t('nav.prompts', { defaultValue: 'Prompts' })}</h1>
        <p className={styles.description}>
          {t('prompts_page.description', {
            defaultValue: 'Browse captured user prompts grouped by working directory and session.',
          })}
        </p>
        <div className={styles.headerActions}>
          <label className={styles.toggleLabel}>
            <input
              type="checkbox"
              checked={inlineTemplates}
              onChange={(e) => persistInlineTemplates(e.target.checked)}
            />
            {t('prompts_page.inline_templates', {
              defaultValue: 'Show templates inline (server-side splice)',
            })}
          </label>
          <label className={styles.toggleLabel} title={t('prompts_page.sessions_per_cwd_hint', {
            defaultValue: 'Changing this reloads detail and resets any loaded extra pages.',
          })}>
            {t('prompts_page.sessions_per_cwd', { defaultValue: 'Sessions per CWD' })}
            <input
              type="number"
              min={SESSION_LIMIT_MIN}
              max={SESSION_LIMIT_MAX}
              value={sessionLimitDraft}
              onChange={(e) => setSessionLimitDraft(e.target.value)}
              onBlur={(e) => commitSessionLimit(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  commitSessionLimit((e.target as HTMLInputElement).value);
                  (e.target as HTMLInputElement).blur();
                }
              }}
              className={styles.limitInput}
            />
            <span className={styles.limitHint}>
              {t('prompts_page.sessions_per_cwd_hint_short', {
                defaultValue: '(reloads on change)',
              })}
            </span>
          </label>
        </div>
      </div>

      {(usersError || detailError) && (
        <div className={styles.errorBox}>{usersError || detailError}</div>
      )}

      <div className={`${styles.layout} ${showDetailColumn ? styles.withDetail : ''} ${keysCollapsed ? styles.collapsedKeys : ''}`}>
        {/* LEFT: keys list (filterable, collapsible to selected-key chip)
            stacked over CWD/sessions tree of the active key. */}
        {keysCollapsed ? (
          <div className={styles.collapsedColumn}>
            <button
              type="button"
              className={styles.collapseToggle}
              onClick={toggleKeysCollapsed}
              aria-label={t('prompts_page.keys_expand', { defaultValue: 'Show navigation panel' })}
              title={t('prompts_page.keys_expand', { defaultValue: 'Show navigation panel' })}
            >
              <IconChevronLeft size={14} style={{ transform: 'rotate(180deg)' }} />
            </button>
          </div>
        ) : (
        <div className={styles.column}>
          {/* Keys section. Collapses to a compact bar once a key is selected
              so the tree below has room to breathe. */}
          {keysListExpanded || !selectedKey ? (
            <>
              <div className={styles.columnHeader}>
                <span>{t('prompts_page.users', { defaultValue: 'API keys' })}</span>
                <span className={styles.badge}>{users.length}</span>
                <button
                  type="button"
                  className={styles.collapseToggle}
                  onClick={toggleKeysCollapsed}
                  aria-label={t('prompts_page.keys_collapse', { defaultValue: 'Hide navigation panel' })}
                  title={t('prompts_page.keys_collapse', { defaultValue: 'Hide navigation panel' })}
                >
                  <IconChevronLeft size={14} />
                </button>
              </div>
              <div className={styles.keysFilterRow}>
                <Input
                  value={keysFilter}
                  onChange={(e) => setKeysFilter(e.target.value)}
                  placeholder={t('prompts_page.keys_filter_placeholder', {
                    defaultValue: 'Filter keys…',
                  })}
                  className={styles.keysFilterInput}
                  rightElement={
                    keysFilter ? (
                      <button
                        type="button"
                        className={styles.searchClear}
                        onClick={() => setKeysFilter('')}
                        aria-label="Clear filter"
                      >
                        <IconX size={12} />
                      </button>
                    ) : (
                      <IconSearch size={12} />
                    )
                  }
                />
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
              <div className={styles.keysListBody}>
                {usersLoading ? (
                  <div className={styles.loading}><LoadingSpinner /></div>
                ) : filteredUsers.length === 0 ? (
                  users.length === 0 ? (
                    <EmptyState
                      title={t('prompts_page.no_users_title', { defaultValue: 'No keys with prompt data' })}
                      description={t('prompts_page.no_users_desc', {
                        defaultValue: 'Configure API keys and enable prompt_log to start capturing.',
                      })}
                    />
                  ) : (
                    <div className={styles.placeholder}>
                      {t('prompts_page.keys_filter_empty', {
                        defaultValue: 'No keys match the filter.',
                      })}
                    </div>
                  )
                ) : (
                  <div className={styles.userList}>
                    {filteredUsers.map((u) => (
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
            </>
          ) : (
            <div className={styles.keysCollapsedBar}>
              <div className={styles.keysCollapsedSummary}>
                <span className={styles.keysCollapsedHint}>
                  {selectedUser?.key_hint || selectedUser?.key_hash || selectedKey}
                  {selectedUser && !selectedUser.configured && (
                    <span className={styles.badgeWarn}>orphan</span>
                  )}
                </span>
                {selectedUser && (
                  <span className={styles.keysCollapsedMeta}>
                    {selectedUser.message_count} msgs · {selectedUser.session_count} sess
                  </span>
                )}
              </div>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setKeysListExpanded(true)}
              >
                {t('prompts_page.change_key', { defaultValue: 'Change' })}
              </Button>
            </div>
          )}

          {/* CWD / sessions tree of the active key. Hidden until a key is
              selected (the keys list takes the whole column then). */}
          {selectedKey && !keysListExpanded && (
            <div className={styles.treeBody}>
              {detailLoading ? (
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
                    const label = formatCwdLabel(group.cwd);
                    return (
                      <div key={group.cwd} className={styles.cwdGroup}>
                        <button
                          type="button"
                          className={styles.cwdHeader}
                          onClick={() => toggleCWD(group.cwd)}
                          title={label.full}
                        >
                          <span className={`${styles.chevron} ${open ? styles.chevronOpen : ''}`}>
                            <IconChevronDown size={12} />
                          </span>
                          <span className={styles.cwdPath}>{label.short}</span>
                          <span className={styles.badge}>
                            {group.message_count}m · {group.session_count}s
                          </span>
                        </button>
                        {open && (
                          <div className={styles.sessionList}>
                            {group.sessions.map((sess) => {
                              const firstMsg = sess.messages[0];
                              const firstTplHash = firstMsg?.prompt_template;
                              const firstTpl = firstTplHash ? templateCache[firstTplHash] : undefined;
                              const firstSuffix = (firstMsg?.prompt ?? '').replace(/\s+/g, ' ').trim();
                              const titleText =
                                firstSuffix ||
                                (firstTplHash ? `📋 ${firstTpl?.label || firstTplHash}` : '(no messages)');
                              const isActive =
                                selectedSessionRef?.cwd === group.cwd &&
                                selectedSessionRef?.sid === sess.session_id;
                              return (
                                <button
                                  key={sess.session_id}
                                  type="button"
                                  className={`${styles.sessionRow} ${isActive ? styles.sessionRowActive : ''}`}
                                  onClick={() => handleSelectSession(group.cwd, sess.session_id)}
                                  title={sess.session_id}
                                >
                                  <span className={styles.sessionTitle}>{titleText}</span>
                                  <span className={styles.sessionMeta}>
                                    <span className={styles.badge}>{sess.message_count}m</span>
                                    <span className={styles.sessionTime}>
                                      {relativeTime(sess.last_seen)}
                                    </span>
                                  </span>
                                </button>
                              );
                            })}
                            {loadingMoreCWDs.has(group.cwd) && (
                              <div className={styles.cwdLoading}>
                                <LoadingSpinner />
                              </div>
                            )}
                            {!loadingMoreCWDs.has(group.cwd) && cwdLoadErrors[group.cwd] && (
                              <div className={styles.cwdError}>
                                <span>{cwdLoadErrors[group.cwd]}</span>
                                <Button variant="ghost" size="sm" onClick={() => handleRetryCWD(group.cwd)}>
                                  {t('prompts_page.retry', { defaultValue: 'Retry' })}
                                </Button>
                              </div>
                            )}
                            {!loadingMoreCWDs.has(group.cwd)
                              && !cwdLoadErrors[group.cwd]
                              && group.sessions.length > 0
                              && group.has_more && (
                              <button
                                type="button"
                                className={styles.loadMoreRow}
                                onClick={() => handleLoadOlderSessions(group.cwd)}
                              >
                                {t('prompts_page.load_more_sessions', {
                                  defaultValue: 'Load {{n}} more sessions',
                                  n: Math.max(0, group.session_count - group.sessions.length),
                                })}
                              </button>
                            )}
                          </div>
                        )}
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          )}
        </div>
        )}

        {/* MIDDLE: reading pane for the selected session, or search results
            when the search input has content. */}
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
          {selectedKey && (
            <div className={styles.searchRow}>
              <Input
                value={searchQuery}
                onChange={(e) => {
                  const v = e.target.value;
                  setSearchQuery(v);
                  // When the input shrinks below the min, drop stale
                  // results immediately so the reading pane returns to
                  // session mode without waiting for the deferred effect
                  // to fire. Also bump searchSeqRef so any in-flight
                  // older-query response bails on resolution instead of
                  // re-populating the now-cleared state under a query
                  // the user has already moved past.
                  if (v.trim().length < SEARCH_MIN_CHARS) {
                    searchSeqRef.current++;
                    setSearchResults(null);
                    setSearchError('');
                    setSearchLoading(false);
                  }
                }}
                placeholder={t('prompts_page.search_placeholder', {
                  defaultValue: 'Search messages in this key…',
                })}
                className={styles.searchInput}
                rightElement={
                  searchQuery ? (
                    <button
                      type="button"
                      className={styles.searchClear}
                      onClick={() => {
                        searchSeqRef.current++;
                        setSearchQuery('');
                        setSearchResults(null);
                        setSearchError('');
                        setSearchLoading(false);
                      }}
                      aria-label="Clear search"
                    >
                      <IconX size={12} />
                    </button>
                  ) : (
                    <IconSearch size={12} />
                  )
                }
              />
            </div>
          )}
          <div
            className={styles.readingPane}
            ref={messagesScrollRef}
            onScroll={handleMessagesScroll}
          >
            {!selectedKey ? (
              <div className={styles.placeholder}>
                {t('prompts_page.placeholder_left', {
                  defaultValue: 'Pick an API key on the left to see its prompt history.',
                })}
              </div>
            ) : detailLoading ? (
              <div className={styles.loading}><LoadingSpinner /></div>
            ) : searchResults || searchLoading || searchError ? (
              <SearchResultsView
                loading={searchLoading}
                error={searchError}
                results={searchResults}
                query={deferredQuery}
                onSelectHit={handleSelectSearchHit}
              />
            ) : !selectedSession ? (
              <div className={styles.placeholder}>
                {t('prompts_page.no_session_selected', {
                  defaultValue: 'Select a session to view its messages.',
                })}
              </div>
            ) : (
              <SessionReadingPane
                session={selectedSession}
                cwd={selectedSessionRef?.cwd ?? '(unknown)'}
                templateCache={templateCache}
                selectedMessageKey={selectedMessageKey}
                onSelectMessage={handleSelectMessage}
                onLoadOlder={fetchOlderMessages}
                loadingOlder={loadingMoreMessages.has(
                  `${selectedSessionRef?.cwd ?? ''}::${selectedSessionRef?.sid ?? ''}`,
                )}
                loadError={
                  messageLoadErrors[
                    `${selectedSessionRef?.cwd ?? ''}::${selectedSessionRef?.sid ?? ''}`
                  ]
                }
              />
            )}
          </div>
        </div>

        {/* RIGHT: message detail (unchanged from previous layout). */}
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
                {(() => {
                  const tplHash = selectedMessage.prompt_template;
                  if (!tplHash) {
                    return <div className={styles.detailPrompt}>{selectedMessage.prompt || '(empty)'}</div>;
                  }
                  const tpl = templateCache[tplHash];
                  const loading = templateLoading.has(tplHash);
                  return (
                    <div>
                      <div className={styles.tplBanner}>
                        <span className={styles.tplChip} title={tpl?.text}>
                          📋 {tpl?.label || tplHash}
                          {tpl?.len ? ` · ${tpl.len}c` : ''}
                          {tpl?.occurrences ? ` · ×${tpl.occurrences}` : ''}
                        </span>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => {
                            if (!templateCache[tplHash]) void fetchTemplate(tplHash);
                            setExpandedTemplateInDetail((v) => !v);
                          }}
                          disabled={loading}
                        >
                          {expandedTemplateInDetail ? 'Hide template' : 'Show template'}
                        </Button>
                      </div>
                      {expandedTemplateInDetail && (
                        <div className={styles.detailPrompt}>
                          {loading
                            ? '(loading template…)'
                            : tpl?.text || '(template not found)'}
                          {selectedMessage.prompt}
                        </div>
                      )}
                      {!expandedTemplateInDetail && (
                        <div className={styles.detailPrompt}>{selectedMessage.prompt || '(suffix is empty)'}</div>
                      )}
                    </div>
                  );
                })()}
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

// -- Subcomponents ----------------------------------------------------------

interface SessionReadingPaneProps {
  session: PromptSession;
  cwd: string;
  templateCache: Record<string, PromptTemplate>;
  selectedMessageKey: string;
  onSelectMessage: (msg: PromptMessage) => void;
  onLoadOlder: (cwd: string, sid: string) => void;
  loadingOlder: boolean;
  loadError?: string;
}

function SessionReadingPane({
  session,
  cwd,
  templateCache,
  selectedMessageKey,
  onSelectMessage,
  onLoadOlder,
  loadingOlder,
  loadError,
}: SessionReadingPaneProps) {
  const { t } = useTranslation();
  const cwdLabel = formatCwdLabel(cwd);

  return (
    <div className={styles.sessionPane}>
      <div className={styles.sessionPaneHeader}>
        <div className={styles.sessionPaneTitle} title={cwdLabel.full}>
          {cwdLabel.short}
          <span className={styles.sessionPaneSeparator}>·</span>
          <span className={styles.sessionId} title={session.session_id}>
            {session.session_id}
          </span>
        </div>
        <div className={styles.sessionPaneMeta}>
          <span className={styles.badge}>{session.client || 'unknown'}</span>
          {session.client_version && (
            <span className={styles.badge}>{session.client_version}</span>
          )}
          {session.models.map((m) => (
            <span key={m} className={styles.badge}>{m}</span>
          ))}
          <span className={styles.badge}>{session.message_count}m</span>
        </div>
      </div>
      <div className={styles.messageList}>
        {session.truncated && (
          <>
            {loadingOlder ? (
              <div className={styles.cwdLoading}><LoadingSpinner /></div>
            ) : loadError ? (
              <div className={styles.cwdError}>
                <span>{loadError}</span>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => onLoadOlder(cwd, session.session_id)}
                >
                  {t('prompts_page.retry', { defaultValue: 'Retry' })}
                </Button>
              </div>
            ) : (
              <button
                type="button"
                className={styles.loadMoreRow}
                onClick={() => onLoadOlder(cwd, session.session_id)}
              >
                {t('prompts_page.load_more_messages', {
                  defaultValue: 'Load {{n}} more messages',
                  n: Math.max(0, session.message_count - session.messages.length),
                })}
              </button>
            )}
          </>
        )}
        {session.messages.map((msg) => {
          const key = `${session.session_id}::${msg.ts}`;
          const isSelected = key === selectedMessageKey;
          const tplHash = msg.prompt_template;
          const tpl = tplHash ? templateCache[tplHash] : undefined;
          const tplLabel = tpl?.label;
          const tplLen = tpl?.len;
          const suffix = (msg.prompt ?? '').replace(/\s+/g, ' ').trim();
          const isAssistant = msg.role === 'assistant';
          const roleClass = isAssistant ? styles.assistantMsg : styles.userMsg;
          const isSub = msg.is_subagent === true;
          const subID = msg.subagent_id ?? '';
          const rowClass = [
            styles.messageRow,
            roleClass,
            isSub ? styles.subagentMsg : '',
            isSelected ? styles.selectedMsg : '',
          ]
            .filter(Boolean)
            .join(' ');
          return (
            <button
              key={key}
              type="button"
              data-message-key={key}
              className={rowClass}
              onClick={() => onSelectMessage(msg)}
            >
              <div className={styles.msgHeader}>
                <span className={styles.msgTime}>{timeOfDay(msg.ts)}</span>
                {isSub && (
                  <span
                    className={styles.subagentChip}
                    title={`Subagent ${subID || '(no id)'}`}
                  >
                    ↳ subagent{subID ? ` · ${subID.slice(0, 6)}` : ''}
                  </span>
                )}
                {tplHash && (
                  <span className={styles.tplChip} title={tpl?.text || `template ${tplHash}`}>
                    {tplLabel ? `📋 ${tplLabel}` : `📋 ${tplHash}`}
                    {tplLen ? ` · ${tplLen}c` : ''}
                  </span>
                )}
              </div>
              <div className={styles.msgBody}>
                {suffix || (tplHash ? '' : '(empty)')}
              </div>
            </button>
          );
        })}
      </div>
    </div>
  );
}

interface SearchResultsViewProps {
  loading: boolean;
  error: string;
  results: PromptSearchResponse | null;
  query: string;
  onSelectHit: (hit: PromptSearchHit) => void;
}

function SearchResultsView({
  loading,
  error,
  results,
  query,
  onSelectHit,
}: SearchResultsViewProps) {
  const { t } = useTranslation();
  if (loading) return <div className={styles.loading}><LoadingSpinner /></div>;
  if (error) return <div className={styles.errorBox}>{error}</div>;
  if (!results) return null;
  if (results.matches.length === 0) {
    return (
      <div className={styles.placeholder}>
        {t('prompts_page.search_no_results', {
          defaultValue: "No matches for '{{q}}'",
          q: query,
        })}
      </div>
    );
  }
  return (
    <div className={styles.searchResults}>
      {results.truncated && (
        <div className={styles.searchTruncatedBanner}>
          {t('prompts_page.search_truncated', {
            defaultValue:
              'Showing {{shown}} of {{total}} matches — refine the query for older results.',
            shown: results.matches.length,
            total: results.total_matches,
          })}
        </div>
      )}
      {results.matches.map((hit) => {
        const label = formatCwdLabel(hit.cwd);
        return (
          <button
            key={`${hit.session_id}::${hit.ts}`}
            type="button"
            className={styles.searchHitRow}
            onClick={() => onSelectHit(hit)}
            title={`${hit.cwd}\n${hit.session_id}\n${hit.ts}`}
          >
            <div className={styles.searchHitHead}>
              <span className={styles.searchHitCwd}>{label.short}</span>
              <span className={styles.searchHitTime}>
                {new Date(hit.ts).toLocaleString()}
              </span>
              {hit.is_subagent && (
                <span className={styles.subagentChip}>
                  ↳ sub{hit.subagent_id ? ` · ${hit.subagent_id.slice(0, 6)}` : ''}
                </span>
              )}
              {hit.role === 'assistant' && (
                <span className={styles.badge}>assistant</span>
              )}
            </div>
            <div className={styles.searchHitExcerpt}>
              <HighlightedExcerpt text={hit.excerpt} query={query} />
            </div>
          </button>
        );
      })}
    </div>
  );
}
