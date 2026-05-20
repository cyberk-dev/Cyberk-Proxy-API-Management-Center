import { apiClient } from './client';
import type {
  PromptDetail,
  PromptSearchResponse,
  PromptTemplate,
  PromptTemplatesResponse,
  PromptUsersResponse,
} from '@/types/prompts';

const PROMPTS_TIMEOUT_MS = 30 * 1000;

export interface SessionCursor {
  ts: string;
  sid: string;
}

export const promptsApi = {
  listUsers: () =>
    apiClient.get<PromptUsersResponse>('/prompts/users', {
      timeout: PROMPTS_TIMEOUT_MS,
    }),

  /**
   * Fetch prompt detail. Pass `inlineTemplates: true` to have the server
   * splice template bodies back into each message's `prompt` (no need for
   * a second round-trip). Default false keeps the response small.
   *
   * `sessionLimit` and `initialCwds` control pagination: only the first
   * `initialCwds` groups get sessions inlined (others come back lazy with
   * `has_more=true`), and each inlined group caps at `sessionLimit`
   * sessions. Defaults (200 and 20) match the backend defaults.
   */
  getDetail: (
    keyOrHash: string,
    opts?: { limit?: number; inlineTemplates?: boolean; sessionLimit?: number; initialCwds?: number }
  ) =>
    apiClient.get<PromptDetail>(`/prompts/users/${encodeURIComponent(keyOrHash)}`, {
      timeout: PROMPTS_TIMEOUT_MS,
      params: {
        ...(opts?.limit ? { limit: opts.limit } : {}),
        ...(opts?.inlineTemplates ? { inline_templates: 1 } : {}),
        ...(opts?.sessionLimit ? { session_limit: opts.sessionLimit } : {}),
        ...(opts?.initialCwds !== undefined ? { initial_cwds: opts.initialCwds } : {}),
      },
    }),

  /**
   * Fetch a page of sessions for a single CWD. Pass `before` to resume
   * from a previous response (typically the oldest already-loaded session's
   * `{ ts: last_seen, sid: session_id }`). Without `before`, returns the
   * first page (top `limit` most recent).
   *
   * Backend response shape is a full `PromptDetail`, but only `groups[0]`
   * is meaningful — callers should treat anything else as a no-match.
   */
  loadCWDSessions: (
    keyOrHash: string,
    cwd: string,
    before?: SessionCursor,
    limit?: number
  ) =>
    apiClient.get<PromptDetail>(`/prompts/users/${encodeURIComponent(keyOrHash)}`, {
      timeout: PROMPTS_TIMEOUT_MS,
      params: {
        cwd,
        ...(before ? { session_before: `${before.ts}|${before.sid}` } : {}),
        ...(limit ? { session_limit: limit } : {}),
      },
    }),

  /**
   * Fetch one page of older messages for a single session inside a CWD.
   * `before` is the timestamp of the oldest message currently in the
   * client's window — server returns messages strictly older than that,
   * capped at `limit`. The session is the only one populated in the
   * response's `groups[0].sessions[0]`; CWD-level meta still reflects
   * the full CWD so "1 of N sessions" stays accurate.
   *
   * Tied per-session timestamps are a documented limitation: if two
   * messages share the same `ts` exactly, the older one may not
   * surface on the page boundary. Rare in practice.
   */
  loadOlderMessages: (
    keyOrHash: string,
    cwd: string,
    sessionId: string,
    before: string,
    limit?: number,
  ) =>
    apiClient.get<PromptDetail>(`/prompts/users/${encodeURIComponent(keyOrHash)}`, {
      timeout: PROMPTS_TIMEOUT_MS,
      params: {
        cwd,
        session_id: sessionId,
        message_before: before,
        ...(limit ? { limit } : {}),
      },
    }),

  /**
   * Fetch every CWD group's meta WITHOUT touching the sessions arrays.
   * Used by the refresh button so a refresh can update message counts and
   * last_seen without clobbering already-loaded session pages in the UI
   * state. The response's `groups[*].sessions` is always `[]`.
   */
  refreshHeaders: (keyOrHash: string) =>
    apiClient.get<PromptDetail>(`/prompts/users/${encodeURIComponent(keyOrHash)}`, {
      timeout: PROMPTS_TIMEOUT_MS,
      params: { headers_only: 1 },
    }),

  /**
   * Case-insensitive substring search over Message.prompt for one key.
   * Returns flat hits (no tree shape) so the UI can render a single list
   * with [cwd] [time] [excerpt] rows. Subagent hits carry their parent's
   * `cwd` / `session_id` so clicking deep-links into the parent's reading
   * pane the same way the tree does.
   *
   * Caller must pass q with at least 2 chars after trim — the server
   * rejects shorter queries with 400 to avoid useless renders.
   */
  searchMessages: (keyOrHash: string, q: string, limit?: number) =>
    apiClient.get<PromptSearchResponse>(
      `/prompts/users/${encodeURIComponent(keyOrHash)}/search`,
      {
        timeout: PROMPTS_TIMEOUT_MS,
        params: {
          q,
          ...(limit ? { limit } : {}),
        },
      },
    ),

  listTemplates: () =>
    apiClient.get<PromptTemplatesResponse>('/prompts/templates', {
      timeout: PROMPTS_TIMEOUT_MS,
    }),

  getTemplate: (hash: string) =>
    apiClient.get<PromptTemplate>(`/prompts/templates/${encodeURIComponent(hash)}`, {
      timeout: PROMPTS_TIMEOUT_MS,
    }),
};
