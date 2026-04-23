# ratelimit-plugin

Rate-limit plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI),
implemented as a standalone Go binary that wraps the upstream SDK via
`cliproxy.Builder` + `api.WithMiddleware`. No fork of the upstream repo required.

## Features

- **Per-API-key + per-model limits** with sliding-window-log algorithm
- **Wildcard model matching** (`gpt-5.4-*`, `claude-*-sonnet-*`) via `path.Match`
- **Per-(key, model) overrides** for VIP / restricted tenants
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
