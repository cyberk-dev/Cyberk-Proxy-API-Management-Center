# ratelimit-plugin

Rate-limit plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI),
implemented as a standalone Go binary that wraps the upstream SDK via
`cliproxy.Builder` + `api.WithMiddleware`. No fork of the upstream repo required.

## Features

- **Per-API-key + per-model limits** with sliding-window-log algorithm
- **Wildcard model matching** (`gpt-5.4-*`, `claude-*-sonnet-*`) via `path.Match`
- **Per-(key, model) overrides** for VIP / restricted tenants
- **Request-shape policy** — block OpenAI `service_tier=priority` (and other
  configured values) globally, before rate-limit accounting
- **Weighted routing for Codex** — distribute traffic across upstream accounts
  proportionally to each account's `plan_type` (Pro / Plus / Free / …)
- **Prompt logging** — capture each request's final user message (with file /
  image masking) to daily-rotated JSONL, grouped by cwd + session + client
- **Hot-reload** config via fsnotify (watch parent dir, K8s ConfigMap compatible)
- **State persistence** — JSON snapshot on graceful shutdown + every 5s, survives
  container restart
- **Content-type aware** — skips multipart body (image edits) and WebSocket upgrades
- Structured rejection logs with `sha256[:6]` API-key hash (no secret leak)

## Quick start

```bash
cd ratelimit-plugin
go build -o ratelimit-plugin .
./ratelimit-plugin -config config.yaml
```

State file defaults to `<config-dir>/ratelimit-state.json`. Override with
`-state /path/to/state.json`.

## Config

See `config.yaml.example` for full syntax. Minimum viable:

```yaml
ratelimit:
  window: 5h
  requests: 500
```

## Policy: block OpenAI fast-mode (`service_tier=priority`)

OpenAI's `service_tier: "priority"` runs requests on a faster pool but bills at
roughly twice the quota of `default`. Operators sharing an upstream key across
tenants frequently want to forbid this tier entirely. Opt-in with:

```yaml
policy:
  block_service_tiers:
    - priority
```

Behavior:

- **Global.** Applies to every API key and every model — no per-key whitelist.
- **OpenAI-only in practice.** The check looks at the JSON field `service_tier`
  on the request body. Anthropic `/v1/messages` and Gemini `/v1beta/...` do not
  carry the field and pass through unchanged.
- **Reject, don't strip.** Matching requests are aborted with `400
  invalid_request_error` (mirroring the rate-limit error shape) so clients see
  a clear failure rather than silently downgraded responses.
- **Runs before rate-limit.** A blocked request never consumes per-key quota.
- **Case-insensitive.** `priority`, `Priority`, `PRIORITY` all match.
- **Hot-reloadable.** Edits to `config.yaml` propagate via fsnotify (same path
  as the `ratelimit:` section).

Omit the block — or leave the list empty — to disable rejection entirely.

### Silent strip of `priority` (default-on)

Separately from the reject list, the proxy **silently strips**
`service_tier: "priority"` from request bodies so callers can't obtain fast-mode
processing on Codex/OpenAI upstreams. This is **on by default** — including when
there is no `policy:` section at all — and is controlled by a single flag:

```yaml
policy:
  strip_priority_service_tier: false  # default: true (strip)
```

- **Strip, don't reject.** The field is removed and the request proceeds at the
  upstream's default tier — the client gets a normal `200`, not a `400`. This is
  the difference from `block_service_tiers`, which fails the call loudly.
- **Priority only.** Other tiers (`auto`, `default`, `flex`) pass through
  untouched; only `priority` (case-insensitive) is removed.
- **Block wins.** If `priority` is also listed in `block_service_tiers`, the
  request is rejected with `400` instead of stripped — an explicit block beats
  the silent default.
- **OpenAI/Codex-only in practice.** Anthropic and Gemini requests don't carry
  `service_tier`, so they're unaffected.
- **Default-on, even on load failure.** If the `policy:` section is missing or
  fails to parse, stripping stays enabled (priority denied by default).
- **Hot-reloadable.** Same fsnotify path as the rest of the section.

Set `strip_priority_service_tier: false` to let callers keep `priority`.

## Weighted routing (Codex)

Codex subscription tiers have very different upstream quotas — a Pro account
absorbs roughly 10× the traffic of a Plus account before throttling. The plugin
can pick between Codex accounts using Smooth Weighted Round Robin based on each
auth's `plan_type` (extracted from the JWT by the upstream SDK).

Opt-in by adding a `codex_weights:` block to `config.yaml`:

```yaml
codex_weights:
  pro: 10
  prolite: 5
  plus: 1
  free: 1
  team: 1
  business: 1
  go: 1
```

Behavior:

- **Codex only.** Claude / Gemini / other providers keep default round-robin.
- **Deterministic.** Over any N requests the distribution matches the weight
  ratio exactly (no random spikes).
- **Cooldown-aware.** Accounts in cooldown or disabled are skipped; their weight
  redistributes to survivors.
- **Priority-aware.** `auth.Attributes["priority"]` still wins — only the
  highest-priority bucket participates in the weighted pick.
- **Session affinity wins.** If `routing.session-affinity: true`, sticky
  sessions return to their bound auth regardless of weight.
- **Unknown plans get weight 1** — a future ChatGPT tier won't silently drop to
  zero traffic.

Omit the block to disable the feature entirely (no `WithCoreAuthManager` call,
zero behavior change vs. the SDK default).

## Prompt logging

The proxy can capture the **last user message** of every chat-completion
request (Anthropic `/v1/messages`, OpenAI `/v1/chat/completions` +
`/v1/responses`, Gemini `:generateContent` + `:streamGenerateContent`) and
append it to a daily-rotated JSONL file for offline review. Conversation
history is **not** duplicated per request — the previous turn's log already
contains it, so logging only the newest turn keeps files small while still
preserving full user intent across a session.

Opt-in:

```yaml
prompt_log:
  enabled: true
  dir: prompts            # relative → <config-dir>/prompts/
  max_text_bytes: 51200   # middle-truncate text blocks longer than this
  queue_size: 1024        # async write queue; overflow drops with metric
```

Output: `<dir>/prompts-YYYY-MM-DD.jsonl` (UTC date). One JSON object per line:

```json
{
  "ts": "2026-05-15T07:26:50Z",
  "provider": "anthropic",
  "path": "/v1/messages",
  "status": 200,
  "model": "claude-opus-4-7",
  "key_hash": "d6f3e1a2b4c5",
  "client": "claude_code",
  "client_version": "2.1.141",
  "session_id": "d4dac6da-0bdd-4f7f-8d7d-84857a73be29",
  "cwd": "/Users/huybuidac/Projects/cyberk/cyberk-skills",
  "prompt": "ok xử lý ddi",
  "blocks": [{ "type": "text", "text": "ok xử lý ddi" }]
}
```

### What gets captured

- **Last user message only.** Tool-loop rounds are skipped so a single
  user-typed prompt is logged exactly once even when the model fans out into
  many tool calls. Detection differs by schema:
    - Anthropic / Gemini: the last message is still `role: "user"` but its
      content is purely `tool_result` / `function_response` blocks → filtered
      out → empty → dropped.
    - OpenAI Chat / Responses: the last message is `role: "tool"` (Chat) or a
      typed `function_call_output` item (Responses) → not user-role → skipped
      before extraction.
- **Both successful and rejected requests.** The middleware runs *before*
  policy + rate-limit, so blocked attempts still appear with their rejection
  status (400 / 429 / 403). Analyzing failed attempts is one of the main
  reasons to enable this.
- **Multiple providers, one log.** The 4 chat schemas are all normalized into
  the same flat `blocks` shape. Routes that aren't chat completions
  (`/v1/models`, management endpoints, health checks, websocket upgrades) are
  skipped — no body cost, no log clutter.

### Content normalization

The raw API request is dominated by hook noise, base64 attachments, and long
pastes. Captured content is reduced before write:

- `<system-reminder>`, `<local-command-*>`, `<command-*>` wrapper blocks are
  **dropped** — they are Claude Code CLI artifacts, not user-authored text.
- **Synthetic CLI prompts** are **dropped entirely**: ghost-text autocomplete
  (`[SUGGESTION MODE: …]`), skill body injection (`Base directory for this
  skill: …`), compaction summary (`This session is being continued from a
  previous conversation …`), and subagent dispatch (`CRITICAL: Respond with
  TEXT ONLY. Do NOT call any tools …`). All four are sent by Claude Code as
  `role:"user"` messages but the content is machine-generated. See
  `syntheticCLIPrefixes` in `extract.go` to extend.
- **Claude Code subagents are dropped at the middleware layer**, even when the
  prompt prefix is novel. Subagents (Task-tool dispatches: web search, Explore,
  Plan, custom agent types) reuse the parent's `claude-cli/X.Y.Z` UA and
  session id but ship their own system prompt without the env block — so cwd
  extraction returns `""`. The middleware drops any entry where
  `client = claude_code` AND `cwd = ""`, since real parent CLI requests always
  carry the `Primary working directory:` line. This catches every subagent
  type generically rather than chasing each new prefix.
- **Images / documents / audio** with inline base64 are **masked** to
  `{ media_type, bytes, sha256[:16] }`. The hash is stable across re-encodings
  so you can dedupe attachments without storing pixels.
- **Long text blocks** (> `max_text_bytes`) are **middle-truncated** keeping
  the first and last halves plus a `[truncated N bytes]` marker, so paste-bomb
  intent at the start and end of a wall of text still survives.
- **`prompt` field** is a flat join of the kept text blocks — easy to `grep`
  and dashboard without parsing `blocks`.

### Client / session / cwd identification

| Client | Detection | Session ID | CWD source (in system prompt) |
|---|---|---|---|
| Claude Code (CLI / VSCode) | `User-Agent: claude-cli/X.Y.Z` | `X-Claude-Code-Session-Id` header (v2.1.97+) | `Primary working directory:` (new) or `Working directory:` (old) |
| Amp (Sourcegraph) | any `X-Amp-*` header (UA is minified) | `X-Amp-Thread-Id` header | `Workspace root folder:` |
| opencode | `User-Agent: opencode/X.Y.Z` | *(none — opencode does not send one)* | `<env>` block with `Working directory:` |
| Vercel AI SDK | `User-Agent: ai/...` or `ai-sdk/...` | none | depends on host app |
| OpenAI / Google SDK | `User-Agent: AsyncOpenAI/Python`, `google-genai-sdk/...` | none | depends on host |
| LiteLLM, curl, node, Bun | UA prefix | none | none |
| anything else | fallback | none | none |

Detection priority is fixed in `internal/promptlog/client.go` — Amp's
auxiliary headers are checked *before* the `claude-cli/` UA so Amp installs
that proxy a Claude-Code-flavored system prompt still group under their own
`X-Amp-Thread-Id`.

### Body size ceiling

`internal/ratelimit/extract.go` caches up to **16 MiB** of the request body
once per request (shared between rate-limit, policy, and promptlog). Bodies
larger than that flow through to the upstream in full, but the cached prefix
is what each middleware reads — so an attached image plus a 100-turn history
fits comfortably; anything beyond is flagged via `body_truncated: true` on the
log entry.

### Disable / re-enable

Omit the `prompt_log:` block entirely (or set `enabled: false`) to turn the
feature off. With it disabled, the read endpoints below return `503` (not
`404`) so the UI can distinguish "feature off" from "wrong URL".

## Prompts management API

Two read-only endpoints under `/v0/management/prompts/`, auth-gated by the
same `X-Management-Key` / `Authorization: Bearer …` header as the rest of the
management plane.

### `GET /v0/management/prompts/users`

Aggregated per-key summary across **all** JSONL files, unioned with keys from
`api-keys:` so brand-new keys appear even before they have activity. Sort
order: most-recent activity first, configured-but-empty keys at the bottom.

```json
{
  "users": [
    {
      "key_hash": "d6f3e1a2b4c5",
      "key_hint": "alic...key1",
      "configured": true,
      "message_count": 42,
      "session_count": 3,
      "cwd_count": 2,
      "first_seen": "2026-05-12T08:15:00Z",
      "last_seen": "2026-05-15T07:26:50Z",
      "clients": ["claude_code"],
      "models": ["claude-haiku-4-5", "claude-opus-4-7"]
    }
  ]
}
```

`configured: false` rows are **orphan keys** — they appear in the log but no
longer in `api-keys:`. Useful for spotting rotated / revoked tokens that are
still being used.

### `GET /v0/management/prompts/users/:key?limit=200`

`:key` accepts either the raw API key (server hashes it) or the 12-char hex
hash directly. Returns a tree grouped by `cwd → session_id → messages`.
`limit` caps messages **per session** (default 200, max 2000); sessions over
the cap include `truncated: true`.

```json
{
  "key_hash": "d6f3e1a2b4c5",
  "key_hint": "alic...key1",
  "configured": true,
  "total_messages": 42,
  "total_sessions": 3,
  "total_cwds": 2,
  "groups": [
    {
      "cwd": "/Users/huybuidac/Projects/cyberk/cyberk-skills",
      "message_count": 28,
      "last_seen": "2026-05-15T07:26:50Z",
      "sessions": [
        {
          "session_id": "d4dac6da-0bdd-4f7f-8d7d-84857a73be29",
          "client": "claude_code",
          "client_version": "2.1.141",
          "models": ["claude-opus-4-7"],
          "first_seen": "2026-05-15T06:00:00Z",
          "last_seen": "2026-05-15T07:26:50Z",
          "message_count": 12,
          "truncated": false,
          "messages": [
            { "ts": "2026-05-15T07:26:50Z", "model": "claude-opus-4-7", "status": 200, "prompt": "ok xử lý ddi" }
          ]
        }
      ]
    }
  ]
}
```

### Templates (dynamic prefix dedup)

Long prefixes that recur across many prompts — Claude Code's compaction
summary, opencode's auto-summarize, custom slash-command bodies, skill
markdown — are detected automatically and stored once in
`<dir>/templates.jsonl`. Subsequent matching entries are encoded as
`prompt_template: <hash>` plus the suffix only.

Defaults: a prefix of ≥ 200 runes shared by ≥ 3 distinct prompts within the
last 5000 entries qualifies; the detector runs every 5 minutes (or sooner if
window/5 new prompts have arrived). Tune via `prompt_log.templates.*` —
disable with `enabled: false`.

Two read endpoints expose the catalog:

- `GET /v0/management/prompts/templates` — list every registered template
  (hash, length, source, occurrences, first/last seen, full text).
- `GET /v0/management/prompts/templates/:hash` — fetch one by hash.

The detail endpoint accepts `?inline_templates=1` to splice template bodies
back into each message's `prompt` server-side, useful for `jq` workflows that
want self-contained data.

### Quick analysis via `jq`

If you'd rather work straight from the JSONL files:

```bash
# All prompts from one session
jq -c 'select(.session_id=="d4dac6da-0bdd-4f7f-8d7d-84857a73be29")' prompts-*.jsonl

# Group by working directory
jq -c '[.cwd, .prompt]' prompts-*.jsonl | sort -u

# Only Claude Code, just model + prompt
jq 'select(.client=="claude_code") | {model, prompt}' prompts-*.jsonl

# Which keys have orphan activity (in JSONL but not in config.yaml)
curl -s -H "X-Management-Key: $MGMT_KEY" /v0/management/prompts/users \
  | jq '.users[] | select(.configured == false)'
```

### UI

`/prompts` in the management web UI exposes the same data with a 3-column
layout: **API keys** (sidebar with paste-field for ad-hoc lookup) →
**cwd / session tree** (expandable, compact 1-line message rows; templated
prompts show a `📋 <hash> · Nc` chip) → **message detail** side-panel (full
text, blocks, metadata, "Show template" expand for templated entries) when a
row is clicked. A header toggle "Show templates inline" flips the page into
self-contained mode (server splices template bodies back into prompts).
Refresh via the header refresh button reloads both the user list
and the currently selected key's tree.

## Docker

```bash
docker build -t ratelimit-plugin .
docker run -v $(pwd)/data:/app/data -p 8317:8317 ratelimit-plugin
```

Mount a directory (not just the file) so the plugin can write the state file
alongside `config.yaml`.

## Tests

```bash
go test -race -cover ./...
```

Current coverage (by package):

| Package | Coverage |
|---|---|
| `internal/ratelimit` | 74% |
| `internal/promptlog` | 77% |
| `internal/policy` | 47% |
| `internal/weightedselector` | 85% |
| `internal/usagestore` | 51% |
| `internal/usagepush` | 86% |
