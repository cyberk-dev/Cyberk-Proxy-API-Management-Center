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

Omit the block — or leave the list empty — to disable the feature entirely.

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

Current coverage: ~83% of statements in `internal/ratelimit`.
