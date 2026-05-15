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
  text?: string;
  media_type?: string;
  bytes?: number;
  sha256?: string;
  url?: string;
  truncated?: boolean;
  orig_bytes?: number;
}

export interface PromptMessage {
  ts: string;
  model?: string;
  provider?: string;
  status: number;
  prompt: string;
  /**
   * Hash of a registered prompt template (see /v0/management/prompts/templates).
   * When set, `prompt` holds only the SUFFIX after the template body — the
   * full prompt is `template.text + prompt`.
   */
  prompt_template?: string;
  blocks?: PromptBlock[];
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
