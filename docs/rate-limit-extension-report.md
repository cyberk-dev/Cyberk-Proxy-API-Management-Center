# Rate Limit Extension Report — CLIProxyAPI

Báo cáo kỹ thuật để implement tính năng **rate-limit per API key** (ví dụ: 1 key được dùng N request trong 5h) và **fork panel quản trị** cho CLIProxyAPI, **không cần sửa code gốc**.

---

## 1. Kiến trúc extension của CLIProxyAPI

Dự án thiết kế mở, expose SDK tại `github.com/router-for-me/CLIProxyAPI/v6/sdk/...` với các extension point chính:

| Extension | Import path | File/Line | Dùng để |
|---|---|---|---|
| **Gin middleware** | `sdk/api` | `sdk/api/options.go:21` (`api.WithMiddleware`) | Chèn logic pre-request (rate limit, metrics, logging) |
| **Router configurator** | `sdk/api` | `sdk/api/options.go:29` (`api.WithRouterConfigurator`) | Thêm route custom (ví dụ: `/v0/my-quota`) |
| **Engine configurator** | `sdk/api` | `sdk/api/options.go:24` | Cấu hình Gin engine |
| **Lifecycle hooks** | `sdk/cliproxy` | `sdk/cliproxy/builder.go:54-65` | `OnBeforeStart`, `OnAfterStart` |
| **Post-auth hook** | `sdk/cliproxy` | `sdk/cliproxy/builder.go:159` | Sau khi tạo Auth record |
| **Access Provider** | `sdk/access` | `sdk/access/registry.go:11-14` | Custom API key validation (điểm tự nhiên để từ chối quota exceeded) |
| **Usage Plugin** | `sdk/cliproxy/usage` | `sdk/cliproxy/usage/manager.go:35-37` | Nhận record mỗi request (APIKey, Model, Timestamp, Tokens) |
| **Custom executor** | `sdk/cliproxy/executor` | `examples/custom-provider/main.go` | Thêm provider AI mới |

### Request pipeline (Gin)

`internal/api/server.go:211-233`:

```
logrus → recovery → [user middleware (WithMiddleware)] → RequestLoggingMiddleware → CORS → AuthMiddleware → handler
```

**Lưu ý quan trọng**: `WithMiddleware` được append **trước** `AuthMiddleware` (`server.go:213` vs `server.go:342, 358`). Middleware user **không thấy** `c.MustGet("apiKey")` do AuthMiddleware set tại `server.go:1038`. Nếu muốn dùng API key trong middleware → phải tự parse header (copy logic `internal/access/config_access/provider.go:62-85`).

### API key extraction

Provider mặc định `config-api-key` (`internal/access/config_access/provider.go:55-104`) chấp nhận:
- `Authorization: Bearer <key>`
- `X-Goog-Api-Key: <key>`
- `X-Api-Key: <key>`
- Query param `?key=<key>` hoặc `?auth_token=<key>`

### Usage statistics có sẵn

`internal/usage/logger_plugin.go` đã đếm request theo API key + model + day/hour. SDK publish `usage.Record` cho **mọi** request (thành công lẫn thất bại) — dữ liệu sẵn sàng để bạn build rate-limit window.

---

## 2. Cơ chế `panel-github-repository`

Toàn bộ logic ở `internal/managementasset/updater.go`.

### Download flow

- Chỉ download **1 file `management.html`** (tên asset là hằng số cứng tại `updater.go:30`).
- Default: `https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest` (`updater.go:28`).
- `resolveReleaseURL()` (`updater.go:303-333`) chấp nhận cả 2 format trong config:
  - `https://github.com/org/repo` → tự map `/releases/latest`
  - `https://api.github.com/repos/org/repo/releases/latest`
- Tìm asset có tên **chính xác** `management.html` trong release (`updater.go:371`).
- Lưu vào `<config-dir>/static/management.html` (atomic rename + SHA256 verify, `updater.go:248, 266-269, 433-462`).
- Fallback mirror: `https://cpamc.router-for.me/` (không verify digest, `updater.go:29, 284-301`).
- Auto-update mỗi 3h (`updater.go:33, 74-112`).
- Tắt qua config `disable-auto-update-panel` hoặc `disable-control-panel` (`internal/config/config.go:191-194`).

### Serve

- Route: `GET /management.html` tại `internal/api/server.go:333` → handler `serveManagementControlPanel` (`server.go:654-682`).
- **Không có auth** — file HTML tĩnh; auth nằm ở các API calls từ panel.

### Management API

Group `/v0/management` tại `internal/api/server.go:487-612`:

| Endpoint | Purpose |
|---|---|
| `/v0/management/usage` | Statistics snapshot theo API key (xem schema bên dưới) |
| `/v0/management/usage/export` | Export CSV |
| `/v0/management/config` | Đọc/ghi config |
| `/v0/management/config.yaml` | Raw YAML |
| `/v0/management/api-keys` | CRUD API keys |
| `/v0/management/gemini-api-key` | CRUD Gemini keys |
| `/v0/management/codex-api-key` | CRUD Codex keys |
| `/v0/management/logs` | Logs |
| `/v0/management/api-call` | Proxy test calls |
| `/v0/management/quota-exceeded/...` | Quota config |

- Bật khi có `RemoteManagement.SecretKey` / env `MANAGEMENT_PASSWORD` / local password (`server.go:300-304`).
- Middleware: `managementAvailabilityMiddleware` + `s.mgmt.Middleware()`.

### Schema của `/v0/management/usage`

Từ `internal/usage/logger_plugin.go:110-136` — `StatisticsSnapshot` có field `APIs map[string]APISnapshot` với:
- `TotalRequests` (per API key)
- Model breakdown
- `RequestDetail.Timestamp`

→ Panel fork đã có đủ dữ liệu để vẽ UI rate-limit mà không cần thêm endpoint mới.

### Fork panel

1. Fork `router-for-me/Cli-Proxy-API-Management-Center`.
2. Build → ra 1 file `management.html` (single-file HTML hoặc có inline JS/CSS tuỳ cách build).
3. Tạo GitHub Release, upload asset tên **chính xác** `management.html`.
4. Set config:
   ```yaml
   remote-management:
     panel-github-repository: "https://github.com/<you>/<fork>"
   ```
5. Dev local: override bằng env `MANAGEMENT_STATIC_PATH` trỏ thẳng vào file local (`updater.go:135-141`) — không cần GitHub.

---

## 3. Implement rate-limit — 3 approach

### Tóm tắt

| Approach | Sửa code gốc? | Complexity | Ghi chú |
|---|---|---|---|
| **A. Middleware (khuyến nghị)** | Không | Thấp | Tự parse API key; đơn giản nhất |
| **B. Custom Access Provider** | Không | Trung bình | Tự nhiên về mặt kiến trúc; cần inject `accessManager` sớm |
| **C. Fork repo + patch** | Có | Cao | Không khuyến nghị — khó maintain với upstream |

### Cơ chế đếm chung cho cả A và B

Register `usage.Plugin` để đếm request per API key trong sliding window:

```go
type rateCounter struct {
    mu     sync.Mutex
    window time.Duration
    limit  int
    hits   map[string][]time.Time
}

func (r *rateCounter) HandleUsage(_ context.Context, rec usage.Record) {
    r.mu.Lock()
    defer r.mu.Unlock()
    cutoff := time.Now().Add(-r.window)
    kept := r.hits[rec.APIKey][:0]
    for _, t := range r.hits[rec.APIKey] {
        if t.After(cutoff) {
            kept = append(kept, t)
        }
    }
    r.hits[rec.APIKey] = append(kept, rec.RequestedAt)
}

func (r *rateCounter) Count(apiKey string) int {
    r.mu.Lock()
    defer r.mu.Unlock()
    cutoff := time.Now().Add(-r.window)
    n := 0
    for _, t := range r.hits[apiKey] {
        if t.After(cutoff) {
            n++
        }
    }
    return n
}
```

Đăng ký tại `main()`:

```go
counter := &rateCounter{
    window: 5 * time.Hour,
    limit:  1000,
    hits:   map[string][]time.Time{},
}
usage.RegisterPlugin(counter)  // sdk/cliproxy/usage/manager.go:173
```

### Approach A — Middleware (khuyến nghị)

```go
import "github.com/router-for-me/CLIProxyAPI/v6/sdk/api"

func extractAPIKey(r *http.Request) string {
    if h := r.Header.Get("Authorization"); h != "" {
        parts := strings.SplitN(h, " ", 2)
        if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
            return strings.TrimSpace(parts[1])
        }
        return h
    }
    if h := r.Header.Get("X-Goog-Api-Key"); h != "" { return h }
    if h := r.Header.Get("X-Api-Key"); h != "" { return h }
    if r.URL != nil {
        if k := r.URL.Query().Get("key"); k != "" { return k }
        if k := r.URL.Query().Get("auth_token"); k != "" { return k }
    }
    return ""
}

rlMiddleware := func(c *gin.Context) {
    if strings.HasPrefix(c.Request.URL.Path, "/v0/management") {
        c.Next()
        return
    }
    key := extractAPIKey(c.Request)
    if key != "" && counter.Count(key) >= counter.limit {
        c.AbortWithStatusJSON(429, gin.H{
            "error": "rate limit exceeded (5h window)",
        })
        return
    }
    c.Next()
}

svc, err := cliproxy.NewBuilder().
    WithConfig(cfg).
    WithConfigPath("config.yaml").
    WithServerOptions(api.WithMiddleware(rlMiddleware)).
    Build()
```

**Ưu điểm**: Đơn giản, không phụ thuộc thứ tự Build/RegisterProvider.
**Nhược điểm**: Trùng logic parse header với `config_access/provider.go`.

### Approach B — Custom Access Provider

```go
type rateLimitProvider struct {
    inner   sdkaccess.Provider
    counter *rateCounter
}

func (p *rateLimitProvider) Identifier() string { return "rate-limited-config" }

func (p *rateLimitProvider) Authenticate(ctx context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
    res, err := p.inner.Authenticate(ctx, r)
    if err != nil || res == nil {
        return res, err
    }
    if p.counter.Count(res.Principal) >= p.counter.limit {
        return nil, &sdkaccess.AuthError{
            HTTPStatusCode: 429,
            Message:        "rate limit exceeded (5h window)",
        }
    }
    return res, nil
}
```

**Lưu ý về thứ tự**: `Builder.Build()` tại `sdk/cliproxy/builder.go:201-202` chạy:
```go
configaccess.Register(&b.cfg.SDKConfig)
accessManager.SetProviders(sdkaccess.RegisteredProviders())
```

→ Provider `config-api-key` chỉ được register **lúc Build()**. Cách sạch:

```go
// Option 1: dùng custom accessManager
mgr := sdkaccess.NewManager()
svc, _ := cliproxy.NewBuilder().
    WithConfig(cfg).
    WithConfigPath("config.yaml").
    WithRequestAccessManager(mgr).  // inject
    WithHooks(cliproxy.Hooks{
        OnBeforeStart: func(c *config.Config) {
            // configaccess đã register rồi
            inner := sdkaccess.RegisteredProviders()
            mgr.SetProviders([]sdkaccess.Provider{
                &rateLimitProvider{inner: inner[0], counter: counter},
            })
        },
    }).Build()
```

### Approach C — Không khuyến nghị

Sửa trực tiếp `internal/api/server.go` hoặc `internal/access/config_access/provider.go`. Khó rebase khi upstream update.

---

## 4. Build & deploy binary riêng

### Project layout

```
~/my-proxy/
├── go.mod
├── main.go
└── config.yaml   (copy từ repo gốc, format không đổi)
```

### Setup

```bash
mkdir ~/my-proxy && cd ~/my-proxy
go mod init my-proxy
go get github.com/router-for-me/CLIProxyAPI/v6
# viết main.go (xem Section 3)
go build -o my-proxy .
./my-proxy -config config.yaml
```

### `main.go` template đầy đủ

```go
package main

import (
    "context"
    "errors"
    "net/http"
    "strings"
    "sync"
    "time"

    "github.com/gin-gonic/gin"
    "github.com/router-for-me/CLIProxyAPI/v6/sdk/api"
    "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy"
    "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
    "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// ---- rateCounter (xem Section 3) ----
type rateCounter struct { /* ... */ }
func (r *rateCounter) HandleUsage(ctx context.Context, rec usage.Record) { /* ... */ }
func (r *rateCounter) Count(apiKey string) int { /* ... */ }

// ---- extractAPIKey (xem Section 3) ----
func extractAPIKey(r *http.Request) string { /* ... */ }

func main() {
    cfg, err := config.LoadConfig("config.yaml")
    if err != nil { panic(err) }

    counter := &rateCounter{
        window: 5 * time.Hour,
        limit:  1000,
        hits:   map[string][]time.Time{},
    }
    usage.RegisterPlugin(counter)

    rlMiddleware := func(c *gin.Context) {
        if strings.HasPrefix(c.Request.URL.Path, "/v0/management") {
            c.Next()
            return
        }
        key := extractAPIKey(c.Request)
        if key != "" && counter.Count(key) >= counter.limit {
            c.AbortWithStatusJSON(429, gin.H{"error": "rate limit exceeded"})
            return
        }
        c.Next()
    }

    svc, err := cliproxy.NewBuilder().
        WithConfig(cfg).
        WithConfigPath("config.yaml").
        WithServerOptions(api.WithMiddleware(rlMiddleware)).
        Build()
    if err != nil { panic(err) }

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
        panic(err)
    }
}
```

### Reference example có sẵn

`examples/custom-provider/main.go` — full example của SDK builder pattern (customize executor). Cùng pattern, chỉ khác extension point.

---

## 5. Hiển thị quota trên panel

### Dữ liệu có sẵn

Panel đọc `GET /v0/management/usage` → `StatisticsSnapshot.APIs[apiKey]`:
- `TotalRequests`
- Model breakdown
- `RequestDetail.Timestamp` (list timestamps)

→ Panel fork tự tính số request trong 5h gần nhất từ timestamps, không cần endpoint mới.

### Nếu muốn endpoint riêng (tùy chọn)

Dùng `api.WithRouterConfigurator` trong binary custom:

```go
api.WithRouterConfigurator(func(r *gin.Engine) {
    r.GET("/v0/custom/rate-limit/:key", func(c *gin.Context) {
        key := c.Param("key")
        c.JSON(200, gin.H{
            "api_key":     key,
            "window":      "5h",
            "count":       counter.Count(key),
            "limit":       counter.limit,
            "remaining":   counter.limit - counter.Count(key),
        })
    })
})
```

**Lưu ý**: endpoint này sẽ không có management auth tự động — tự thêm middleware check nếu cần.

---

## 6. Checklist implement

### Server-side (binary custom)
- [ ] Tạo Go module mới, import `github.com/router-for-me/CLIProxyAPI/v6`
- [ ] Implement `rateCounter` với sliding window (in-memory hoặc Redis-backed)
- [ ] Register `usage.Plugin` để đếm request
- [ ] Chọn Approach A (middleware) hoặc B (access provider) để reject khi vượt quota
- [ ] Đọc limit từ config (YAML) thay vì hardcode — có thể thêm field custom vào `SDKConfig` hoặc file config riêng
- [ ] Persist counter state (nếu cần survive restart) — vd: file JSON định kỳ hoặc Redis
- [ ] Log rate-limit events cho monitoring
- [ ] Build `go build -o my-proxy .`

### Panel (fork)
- [ ] Fork `router-for-me/Cli-Proxy-API-Management-Center`
- [ ] Thêm UI: hiển thị count/limit/remaining per API key, reset time
- [ ] Gọi `GET /v0/management/usage` → tính từ `RequestDetail.Timestamp[]` trong 5h gần nhất
- [ ] (Optional) UI set/edit limit per-key nếu backend support
- [ ] Build ra file `management.html` (single file, asset name chính xác)
- [ ] Tạo GitHub Release, upload asset
- [ ] Set `panel-github-repository` trong `config.yaml` trỏ sang fork

### Test
- [ ] Gửi N+1 request với cùng API key → request thứ N+1 trả `429`
- [ ] Sau 5h, counter reset → request lại thành công
- [ ] Panel hiển thị đúng count/remaining
- [ ] Key khác không bị ảnh hưởng bởi quota của key bị limit

---

## 7. Reference map

| Chủ đề | File | Line |
|---|---|---|
| Request pipeline | `internal/api/server.go` | 211-233 |
| AuthMiddleware set `apiKey` | `internal/api/server.go` | 1038 |
| Management route registration | `internal/api/server.go` | 487-612 |
| Serve panel HTML | `internal/api/server.go` | 333, 654-682 |
| Management availability gate | `internal/api/server.go` | 300-304 |
| Panel downloader | `internal/managementasset/updater.go` | — |
| Panel URL resolver | `internal/managementasset/updater.go` | 303-333 |
| Asset name constant | `internal/managementasset/updater.go` | 30 |
| Static path override | `internal/managementasset/updater.go` | 135-141 |
| Config API key extraction | `internal/access/config_access/provider.go` | 55-104 |
| Usage logger plugin (reference) | `internal/usage/logger_plugin.go` | 22, 110-136 |
| SDK access Provider interface | `sdk/access/registry.go` | 11-14 |
| SDK access RegisterProvider | `sdk/access/registry.go` | 30 |
| SDK usage Plugin interface | `sdk/cliproxy/usage/manager.go` | 35-37 |
| SDK usage RegisterPlugin | `sdk/cliproxy/usage/manager.go` | 173 |
| SDK Builder | `sdk/cliproxy/builder.go` | 22-260 |
| SDK Hooks | `sdk/cliproxy/builder.go` | 54-65 |
| SDK api options | `sdk/api/options.go` | 21, 24, 29 |
| Example custom provider | `examples/custom-provider/main.go` | — |
