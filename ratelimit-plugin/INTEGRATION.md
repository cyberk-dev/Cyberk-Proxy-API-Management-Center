# Integration guide — swap CLIProxyAPI with ratelimit-plugin

Plugin này là **drop-in replacement** cho binary gốc — nó embed toàn bộ CLIProxyAPI qua SDK (`cliproxy.Builder`), expose đúng các port/route/handler như upstream. Chỉ cần swap binary (hoặc image), không cần đổi config gốc, không cần sửa UI management.

## Kiến trúc

```
┌────────────────────────────────────────────────────┐
│  Container: cyberk/cli-proxy-api-ratelimit         │
│                                                    │
│  ./CLIProxyAPI (plugin binary)                     │
│  ├── rate-limit middleware (Gin)                   │
│  │   └── sliding window log + config hot-reload    │
│  └── embed CLIProxyAPI SDK                         │
│      ├── /v1/chat/completions                      │
│      ├── /v1/messages                              │
│      ├── /v1beta/models/...                        │
│      ├── /v0/management/...                        │
│      └── ... (all upstream endpoints unchanged)    │
└────────────────────────────────────────────────────┘
```

---

## Bước 1: thêm section `ratelimit:` vào config.yaml hiện tại

File `config.yaml` mày đang mount vào container (ở `${CLI_PROXY_CONFIG_PATH:-./config.yaml}` trên host). Giữ nguyên mọi field hiện có, append thêm:

```yaml
# ... existing fields: port, auth-dir, api-keys, remote-management, ... ...

ratelimit:
  window: 5h          # default window
  requests: 500       # default per-key limit
  models:
    gpt-5.4:
      window: 2h
      requests: 100
      keys:
        alice-key: 50    # per-key override
    "gpt-5.4-*":        # wildcard (path.Match syntax)
      requests: 300
```

Plugin tự parse section `ratelimit:` độc lập; upstream CLIProxyAPI bỏ qua field này như field unknown → compatible 2 chiều.

**Nếu không có section `ratelimit:`** → plugin chạy như proxy thường, không rate-limit gì (no-op).

---

## Bước 2: build image từ source

Có 2 cách, chọn cái hợp workflow:

### Cách A — build image riêng, docker-compose swap (khuyến nghị)

```bash
cd /Users/huybuidac/Projects/cyberk/Cyberk-Proxy-API-Management-Center/ratelimit-plugin
docker build -t cyberk/cli-proxy-api-ratelimit:latest .
```

Rồi trong thư mục chạy CLIProxyAPI hiện tại (`/Users/huybuidac/Projects/ai-oss/CLIProxyAPI/`), sửa `docker-compose.yml`:

**Before:**
```yaml
services:
  cli-proxy-api:
    image: ${CLI_PROXY_IMAGE:-eceasy/cli-proxy-api:latest}
    pull_policy: always
    build:
      context: .
      dockerfile: Dockerfile
```

**After:**
```yaml
services:
  cli-proxy-api:
    image: cyberk/cli-proxy-api-ratelimit:latest   # ← swap image
    pull_policy: build                              # ← đừng pull từ docker hub
    # bỏ luôn block build: ... vì context không còn đúng
    command: ["./CLIProxyAPI", "-config", "/CLIProxyAPI/config.yaml", "-state", "/CLIProxyAPI/state/ratelimit-state.json"]
    # ... giữ nguyên ports, environment, volumes (thêm 1 volume mới phía dưới)
    volumes:
      - ${CLI_PROXY_CONFIG_PATH:-./config.yaml}:/CLIProxyAPI/config.yaml
      - ${CLI_PROXY_AUTH_PATH:-./auths}:/root/.cli-proxy-api
      - ${CLI_PROXY_LOG_PATH:-./logs}:/CLIProxyAPI/logs
      - ${CLI_PROXY_STATE_PATH:-./state}:/CLIProxyAPI/state   # ← NEW
```

Tạo thư mục state trước (compose tự tạo được nhưng owner có thể là root):
```bash
mkdir -p /Users/huybuidac/Projects/ai-oss/CLIProxyAPI/state
```

Chạy:
```bash
cd /Users/huybuidac/Projects/ai-oss/CLIProxyAPI
docker compose down
docker compose up -d
docker compose logs -f
```

### Cách B — point `build.context` trực tiếp sang plugin source

Trong `docker-compose.yml` của CLIProxyAPI gốc, thay `build.context` trỏ sang thư mục plugin:

```yaml
services:
  cli-proxy-api:
    build:
      context: /Users/huybuidac/Projects/cyberk/Cyberk-Proxy-API-Management-Center/ratelimit-plugin
      dockerfile: Dockerfile
    command: ["./CLIProxyAPI", "-config", "/CLIProxyAPI/config.yaml", "-state", "/CLIProxyAPI/state/ratelimit-state.json"]
    volumes:
      # ... giữ 3 volume cũ + thêm:
      - ./state:/CLIProxyAPI/state
```

Rồi `docker compose up -d --build`. Cách này workflow đơn — nhưng nếu đổi code plugin phải `--build` lại mỗi lần.

### Cách C — dùng luôn docker-compose của plugin

Trong `ratelimit-plugin/` đã có sẵn `docker-compose.yml` template. Copy config + auth của mày vào:

```bash
cd ratelimit-plugin
cp /Users/huybuidac/Projects/ai-oss/CLIProxyAPI/config.yaml ./config.yaml
# thêm section `ratelimit:` vào config.yaml
ln -s /Users/huybuidac/Projects/ai-oss/CLIProxyAPI/auths ./auths
mkdir -p ./state ./logs
docker compose up -d --build
```

---

## Bước 3: verify

```bash
# Health check — như upstream
curl http://localhost:8317/healthz
# → {"status":"ok"}

# Gửi request với key → check rate-limit headers
curl -i -X POST http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}'

# Response có:
#   X-RateLimit-Limit: 500
#   X-RateLimit-Remaining: 499
#   X-RateLimit-Reset: <unix-timestamp>

# Spam tới khi bị reject:
for i in $(seq 1 600); do
  curl -s -o /dev/null -w "%{http_code}\n" -X POST ... ;
done | sort | uniq -c
#   500 200
#   100 400   ← bị rate-limit, trả 400 invalid_request_error để Claude Code không retry
```

Check logs:
```bash
docker compose logs cli-proxy-api | grep ratelimit
# time="..." level=info msg="ratelimit-plugin starting (config=... state=...)"
# time="..." level=warning msg="rate limit exceeded" event=ratelimit.rejected key_hash=... model=gpt-4 limit=500 window=5h0m0s retry_s=17942 path=/v1/chat/completions
```

Check state file persist:
```bash
ls -la state/ratelimit-state.json
# Restart container, check counter không bị reset:
docker compose restart
curl -i ... # X-RateLimit-Remaining không về 499
```

---

## Bước 4: hot-reload test

Đang chạy, thử đổi limit trong `config.yaml`:

```bash
# Sửa requests: 500 → 50
sed -i '' 's/requests: 500/requests: 50/' config.yaml
# (trên Linux bỏ '')

# Check log — nên thấy trong vòng 1-2s:
# time="..." level=info msg="ratelimit: config reloaded (mtime=...)"
# time="..." level=info msg="ratelimit: config swapped (top=50req/5h0m0s models=...)"
```

Không cần restart container.

---

## Troubleshooting

| Vấn đề | Chẩn đoán |
|---|---|
| `docker compose up` fail "context not found" | `build.context` phải trỏ đúng dir plugin. Dùng Cách A hoặc absolute path. |
| Container restart về 0 request | `state/` thiếu permission write. `chmod 777 state` hoặc chạy container với `user: "$(id -u):$(id -g)"`. |
| Hot-reload không trigger trên Docker Desktop macOS | Bind mount qua VirtioFS đôi khi mất fsnotify event — plugin có fallback 30s stat-based. Đợi 30s là reload. |
| Config sai syntax → không reload | Log sẽ in `ratelimit: reload config: invalid ...`. Config cũ vẫn giữ, không crash. |
| Bị reject (400) ngay từ request đầu | State file cũ còn counter chưa expire. Xóa `state/ratelimit-state.json` rồi restart. |
| Upstream panel (`/management.html`) không rate-limit | Đúng — plugin skip `/v0/management`, `/management.html`, `/healthz`, `/`, `/v1/models`, `/v1beta/models` by design. |

---

## Upgrade plugin (khi upstream CLIProxyAPI có version mới)

```bash
cd ratelimit-plugin
go get -u github.com/router-for-me/CLIProxyAPI/v6
go mod tidy
go test -race ./...
docker build -t cyberk/cli-proxy-api-ratelimit:latest .
docker compose up -d   # pick up new image
```

Nếu upstream đổi SDK interface → test sẽ fail, fix rồi build lại.

---

## Rollback về upstream

Nếu cần quay lại binary gốc ngay:
```yaml
# docker-compose.yml
services:
  cli-proxy-api:
    image: eceasy/cli-proxy-api:latest   # upstream
    # bỏ command: override nếu có
    # bỏ volume state nếu muốn (hoặc giữ — upstream không đụng tới)
```

Section `ratelimit:` trong config.yaml sẽ bị upstream bỏ qua — không break gì.
