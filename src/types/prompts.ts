export interface PromptUserSummary {
  key_hash: string;
  key_hint?: string;
  configured: boolean;
  message_count: number;
  session_count: number;
  cwd_count: number;
  first_seen?: string;
  last_seen?: string;
  clients?: string[];
  models?: string[];
}

export interface PromptUsersResponse {
  users: PromptUserSummary[];
}

export interface PromptBlock {
  type: string;
  /**
   * Only present on legacy entries written before 2026-05-17. Newer entries
   * carry text content in {@link PromptMessage.prompt} and leave this empty;
   * use {@link bytes} for length and {@link truncated} / {@link orig_bytes}
   * for head+tail-elided originals.
   */
  text?: string;
  media_type?: string;
  bytes?: number;
  sha256?: string;
  url?: string;
  truncated?: boolean;
  orig_bytes?: number;
  /** Name of the tool for tool_use / tool_result reference blocks. */
  tool?: string;
  /** True when a tool_result was returned with is_error=true. */
  is_error?: boolean;
}

export interface PromptMessage {
  ts: string;
  model?: string;
  provider?: string;
  status: number;
  /**
   * Authoring role: `"user"` for a captured prompt, `"assistant"` for the
   * upstream model reply. Absent on legacy entries written before
   * assistant-side logging existed — treat empty as `"user"`.
   */
  role?: 'user' | 'assistant';
  prompt: string;
  /**
   * Hash of a registered prompt template (see /v0/management/prompts/templates).
   * When set, `prompt` holds only the SUFFIX after the template body — the
   * full prompt is `template.text + prompt`.
   */
  prompt_template?: string;
  blocks?: PromptBlock[];
  /**
   * Set by the reader when this message was logged inside a subagent
   * dispatch (Claude Code Task tool or opencode child session) and the
   * parent session was found in the same scan. The UI renders these rows
   * indented under the dispatching parent's row so the conversation reads
   * as one thread. Orphan subagent entries — parent rolled out of
   * retention — render as ordinary messages (this flag stays false).
   */
  is_subagent?: boolean;
  /**
   * Short identifier of the subagent run, displayed in the indent chip.
   * - Claude Code: first 8 chars of `X-Claude-Code-Agent-Id`.
   * - opencode: last 8 chars of the subagent's `Session_id` (after `ses_`).
   */
  subagent_id?: string;
}

export interface PromptTemplate {
  hash: string;
  len: number;
  source?: string;
  label?: string;
  first_seen: string;
  last_seen: string;
  occurrences: number;
  text: string;
}

export interface PromptTemplatesResponse {
  templates: PromptTemplate[];
}

export interface PromptSession {
  session_id: string;
  client: string;
  client_version?: string;
  models: string[];
  first_seen: string;
  last_seen: string;
  message_count: number;
  truncated?: boolean;
  messages: PromptMessage[];
}

export interface PromptCWDGroup {
  cwd: string;
  message_count: number;
  last_seen: string;
  /**
   * Total sessions in this CWD, independent of `session_before` and the
   * `session_limit` cap on the response. Stable across initial-load and
   * load-more responses so the UI can show "X loaded of Y total" without
   * tracking it locally.
   */
  session_count: number;
  /**
   * True when the server has more sessions older than the last entry in
   * `sessions`. False once the cursor has walked to the end, or when the
   * group is lazy (overview past `initial_cwds`) with `session_count==0`.
   */
  has_more: boolean;
  /**
   * Always a non-nil array. Empty when the group is lazy (overview past
   * `initial_cwds`) or when the response was `headers_only=1`; the client
   * auto-fetches the first page on expand in those cases.
   */
  sessions: PromptSession[];
}

export interface PromptDetail {
  key_hash: string;
  key_hint?: string;
  configured: boolean;
  total_messages: number;
  total_sessions: number;
  total_cwds: number;
  groups: PromptCWDGroup[];
}

/**
 * One row of the `/users/:key/search` response. `cwd` and `session_id` are
 * the render-time bucket (subagent → parent merged), matching the tree, so
 * clicking a hit can deep-link into the parent's reading pane without the
 * client redoing that resolution.
 *
 * `excerpt` is a clipped window of the original prompt with whitespace
 * collapsed to single spaces. The client re-locates matches with a
 * case-insensitive indexOf on this string to render highlights — the server
 * deliberately does NOT return byte offsets because JS strings index by
 * UTF-16 code units while the backend speaks UTF-8, and translating between
 * the two for highlight ranges is error-prone.
 */
export interface PromptSearchHit {
  cwd: string;
  session_id: string;
  ts: string;
  role?: 'user' | 'assistant';
  excerpt: string;
  is_subagent?: boolean;
  subagent_id?: string;
}

export interface PromptSearchResponse {
  matches: PromptSearchHit[];
  total_matches: number;
  truncated: boolean;
}
