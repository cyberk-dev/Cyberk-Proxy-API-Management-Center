# ratelimit-plugin

Rate-limit plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI),
implemented as a standalone Go binary that wraps the upstream SDK via
`cliproxy.Builder` + `api.WithMiddleware`. No fork of the upstream repo required.

## Features

- **Per-API-key + per-model limits** with sliding-window-log algorithm
- **Wildcard model matching** (`gpt-5.4-*`, `claude-*-sonnet-*`) via `path.Match`
- **Per-(key, model) overrides** for VIP / restricted tenants
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
