# Plan: Rate-limit Plugin cho CLIProxyAPI

Plan chi tiết để implement **plugin rate-limit per API key + per model + per user** cho CLIProxyAPI, đóng gói dưới dạng 1 Go module riêng nằm trong repo UI này (không fork/patch repo gốc).

> **Review status**: đã qua oracle review round 1. Các finding P0 (multipart + websocket bug) và P1 (fsnotify filter, clock skew, body cap, head trim, structured log) đã fix trong plan. Finding defer v2: shard-by-hash mutex, Prometheus metrics, Redis multi-instance, refund on client cancel.

---

## 0. Scope & thiết kế tổng quan

### Input từ user

```yaml
ratelimit:
  window: 5h          # default window
  requests: 500       # default per-key limit
  models:
    gpt-5.4:
      window: 2h
      requests: 100
      keys:
        user_1: 50    # override cho key cụ thể
    gpt-5.4-mini:
      requests: 300   # inherit window mặc định
```

### Thuật toán đếm (sliding window log)

Mỗi `(apiKey, model)` giữ 1 list timestamp của các request đã cho qua. Mỗi lần check:

```
cutoff = now - window
hits = [t in hits[key] if t > cutoff]    # drop expired
if len(hits) >= limit: reject
else: hits.append(now); accept
```

Ưu so với fixed window: không có burst ở biên bucket (fixed window cho phép `2*limit` trong vài giây quanh ranh giới). Cửa sổ luôn dài đúng N giờ tính từ `now`.

Trade-off: tốn memory hơn (lưu mỗi timestamp thay vì 1 integer) và mỗi request phải walk list. Với `limit ~ 500` và per-request cost O(limit) dưới mutex → vẫn OK cho QPS hàng nghìn. Nếu cần tối ưu, thay `[]time.Time` bằng circular buffer / deque hoặc chuyển sang **sliding window counter** (blend 2 fixed bucket liền kề theo tỉ lệ) — giữ API, swap internal.

### Resolution limit (từ cụ thể → chung, có wildcard)

Cho request `(apiKey=X, model=M)`, thứ tự tra limit:

1. `ratelimit.models[M].keys[X]` — match model **chính xác** + key chính xác
2. `ratelimit.models[pattern].keys[X]` — match model qua **wildcard pattern** (xem dưới) + key chính xác
3. `ratelimit.models[M]` — model chính xác, default cho mọi key
4. `ratelimit.models[pattern]` — wildcard model, default cho mọi key
5. `ratelimit` (top-level) — default toàn cục

Nếu cuối cùng không có limit nào áp dụng → không rate-limit.

#### Wildcard match

Pattern syntax: dùng `*` làm free glob trong model name. Implement bằng `path.Match` của stdlib (hoặc regex nếu cần chặt hơn).

```yaml
ratelimit:
  models:
    "gpt-5.4-*":          # match gpt-5.4-mini, gpt-5.4-pro, ...
      requests: 300
    "gpt-5.4":            # exact overrides wildcard (step 1,3 chạy trước 2,4)
      requests: 100
    "claude-*-sonnet-*":  # nhiều wildcard
      requests: 200
```

Rule tie-break khi 1 model match nhiều pattern:
- **Exact match thắng wildcard** (luôn).
- Nhiều wildcard match → chọn pattern **specific nhất** = pattern có nhiều ký tự literal (không `*`) nhất. Ví dụ `gpt-5.4-*` thắng `gpt-*` cho model `gpt-5.4-mini`.
- Tie cuối cùng → chọn theo alphabetical của pattern (deterministic).

Cache kết quả lookup theo `(model, apiKey)` trong `sync.Map` để không phải walk pattern list mỗi request. Invalidate cache khi config reload (section §10).

### Extension point

**Middleware (Approach A)** — đăng ký qua `api.WithMiddleware(...)` trong SDK.

Lý do chọn middleware thay vì Access Provider:
- Cần đọc body request để biết **model** (OpenAI/Claude) hoặc URL path (Gemini). Access Provider chỉ trả về Principal, không có chỗ tự nhiên để reject theo model.
- Middleware chạy trước `AuthMiddleware` → phải tự parse API key từ header (duplicate logic với `internal/access/config_access/provider.go:55-104`, nhưng chỉ ~20 dòng).
- Viết test dễ hơn (pure function trên `*gin.Context`).

Không dùng `usage.Plugin` để **block** — plugin nhận record sau khi request hoàn tất, không kịp từ chối. Nhưng vẫn có thể register usage.Plugin để làm observability/sync counter nếu cần persist (optional).

### Repository layout

Repo này là UI (Vite/TS), nhưng user yêu cầu đặt plugin ở đây. Tạo 1 Go module độc lập:

```
Cyberk-Proxy-API-Management-Center/
├── src/                           # Existing TS UI
├── ratelimit-plugin/              # NEW — Go module
│   ├── go.mod
│   ├── go.sum
│   ├── main.go                    # Entry point: wraps cliproxy.Builder
│   ├── config.yaml.example        # Example config có phần `ratelimit:`
│   └── internal/
│       └── ratelimit/
│           ├── config.go          # Parse `ratelimit:` node từ YAML
│           ├── limiter.go         # Fixed-window counter
│           ├── limiter_test.go
│           ├── middleware.go      # Gin middleware
│           └── extract.go         # extract API key + model từ request
└── docs/
    └── ratelimit-plugin-plan.md   # File này
```

Build output: 1 binary `ratelimit-plugin` có thể thay thế `CLIProxyAPI` binary gốc. Config reuse `config.yaml` gốc + thêm section `ratelimit:`.

---

## 1. Config schema & parsing

### 1.1 Thách thức

SDK `config.LoadConfig` (`sdk/config/config.go:38`) parse đúng schema của `internalconfig.Config` — **không biết** field `ratelimit:`. Field unknown sẽ bị YAML parser bỏ qua.

→ Phải parse YAML lại ở tầng plugin **từ raw bytes**, không dùng lại output của `config.LoadConfig`.

### 1.2 Struct

`ratelimit-plugin/internal/ratelimit/config.go`:

```go
type Config struct {
    Window   time.Duration           // default window (e.g., 5h)
    Requests int                     // default per-key limit
    Models   map[string]ModelConfig  // keyed by model name (lowercase, trimmed)
}

type ModelConfig struct {
    Window   *time.Duration          // nil = inherit from top-level
    Requests *int                    // nil = inherit from top-level
    Keys     map[string]int          // per-key override (apiKey → limit)
}

// Raw YAML layout chỉ cần một lớp decoder tạm (map[string]any) rồi normalize.
```

Parse helper: đọc `config.yaml` → `yaml.Unmarshal` vào `struct { Ratelimit yaml.Node }` → walk `Node` để build `Config`. Validate:
- `window` dùng `time.ParseDuration` ("5h", "30m", "2h30m")
- `requests` >= 0
- Empty map / 0 values = disabled

### 1.3 Config resolution

```go
// Resolve trả limit, window cho (apiKey, model). ok=false → không rate-limit.
func (c *Config) Resolve(apiKey, model string) (limit int, window time.Duration, ok bool)
```

Thứ tự (file `config.go`):

```go
m, hasModel := c.Models[normalize(model)]
if hasModel {
    window := deref(m.Window, c.Window)
    if per, ok := m.Keys[apiKey]; ok {
        return per, window, per > 0
    }
    if m.Requests != nil {
        return *m.Requests, window, *m.Requests > 0
    }
}
// fallback: top-level default
if c.Requests > 0 && c.Window > 0 {
    return c.Requests, c.Window, true
}
return 0, 0, false
```

Model name normalization: lowercase + trim spaces. Nếu user muốn wildcard (`gpt-*`) thì thêm sau — v1 chỉ match exact.

---

## 2. Limiter core (sliding window log)

`ratelimit-plugin/internal/ratelimit/limiter.go`:

```go
type counterKey struct {
    apiKey string
    model  string   // "" for top-level default bucket
}

type Limiter struct {
    mu      sync.Mutex
    hits    map[counterKey][]time.Time    // timestamps đã qua, chronological
    dirty   atomic.Bool                   // for persistence (§10)
    now     func() time.Time              // injectable for test
}

func (l *Limiter) Take(apiKey, model string, limit int, window time.Duration) (allowed bool, remaining int, resetAt time.Time) {
    now := l.now()
    k := counterKey{apiKey, model}

    l.mu.Lock()
    defer l.mu.Unlock()

    list := l.hits[k]

    // Clock skew guard: nếu NTP nhảy lùi làm now < last timestamp → clamp now lên
    // để giữ list monotonic, tránh vỡ sort.Search ở lần Take sau.
    if n := len(list); n > 0 && now.Before(list[n-1]) {
        now = list[n-1].Add(time.Nanosecond)
    }
    cutoff := now.Add(-window)

    // Drop expired timestamps (list luôn sorted ascending → binary search).
    i := sort.Search(len(list), func(i int) bool { return list[i].After(cutoff) })
    list = list[i:]

    if len(list) >= limit {
        resetAt = list[0].Add(window)
        l.hits[k] = list
        return false, 0, resetAt
    }

    list = append(list, now)

    // Head-trim safety cap: cover trường hợp limit bị giảm giữa chừng
    // (hot-reload) để list không dài hơn limit mới + 1. Cũng chống config
    // typo làm list phình lên.
    if len(list) > limit+1 {
        list = list[len(list)-(limit+1):]
    }

    l.hits[k] = list
    l.dirty.Store(true)
    return true, limit - len(list), list[0].Add(window)
}
```

### Memory footprint

1 timestamp = `time.Time` (24 bytes in-memory). `1000 keys × 500 limit ≈ 12 MB`. Với scale lớn hơn thay `[]time.Time` bằng `[]int64` (Unix nano) để halve memory, hoặc circular buffer với cap = limit.

### GC strategy

`Take` tự drop timestamps expired mỗi khi gọi → inline cleanup. Thêm 1 background tick mỗi 1 phút scan map, xóa entry với `len(list) == 0` để giải phóng key đã idle lâu.

### Concurrency

`sync.Mutex` đủ cho QPS ~ vài nghìn. Muốn scale cao hơn → shard map theo `hash(apiKey) % N`, mỗi shard 1 mutex. Không cần cho v1.

### Persistence — xem §10.

---

## 3. Middleware

`ratelimit-plugin/internal/ratelimit/middleware.go`:

```go
// Max body size khi đọc để extract model (tránh OOM với payload lớn).
const maxBodyPeek = 1 << 20 // 1 MiB đủ cho JSON request, đè lên cho image body.

func Middleware(store *ConfigStore, lim *Limiter) gin.HandlerFunc {
    skipPrefixes := []string{"/v0/management", "/management.html", "/healthz"}
    skipExact := map[string]bool{"/v1/models": true, "/v1beta/models": true}

    return func(c *gin.Context) {
        path := c.Request.URL.Path
        for _, p := range skipPrefixes {
            if strings.HasPrefix(path, p) { c.Next(); return }
        }
        if skipExact[path] { c.Next(); return }

        // Skip WebSocket upgrade (1 connection = 1 slot cho cả session → sai
        // semantics cho sliding window). /v1/responses GET dùng WS, rate-limit
        // khởi tạo WS ở lớp upstream là đủ.
        if strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
            c.Next(); return
        }

        apiKey := extractAPIKey(c.Request)
        if apiKey == "" {
            // Không có key → để AuthMiddleware phía sau trả 401.
            c.Next(); return
        }

        model := extractModel(c) // không error — invalid body → empty model

        cfg := store.Get()
        limit, window, ok := cfg.Resolve(apiKey, model)
        if !ok { c.Next(); return }

        allowed, remaining, resetAt := lim.Take(apiKey, model, limit, window)
        c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
        c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
        c.Header("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

        if !allowed {
            retryAfter := int(time.Until(resetAt).Seconds())
            if retryAfter < 1 { retryAfter = 1 }
            c.Header("Retry-After", strconv.Itoa(retryAfter))

            // Structured log cho ops (hash key để không leak secret).
            log.WithFields(log.Fields{
                "event":     "ratelimit.rejected",
                "key_hash":  hashKey(apiKey),
                "model":     model,
                "limit":     limit,
                "window":    window.String(),
                "retry_s":   retryAfter,
                "path":      path,
            }).Warn("rate limit exceeded")

            c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
                "error": gin.H{
                    "type":    "rate_limit_exceeded",
                    "message": fmt.Sprintf("Quota exceeded for model %q — %d req / %s. Try again in %ds or switch model.", model, limit, window, retryAfter),
                    "model":   model,
                    "limit":   limit,
                    "window":  window.String(),
                    "reset_at": resetAt.Unix(),
                },
            })
            return
        }
        c.Next()
    }
}

func hashKey(k string) string {
    sum := sha256.Sum256([]byte(k))
    return hex.EncodeToString(sum[:6]) // 12 hex chars, non-reversible identifier
}
```

### 3.1 Extract API key

`ratelimit-plugin/internal/ratelimit/extract.go` — copy logic từ `internal/access/config_access/provider.go:62-85`:

```go
func extractAPIKey(r *http.Request) string {
    if h := r.Header.Get("Authorization"); h != "" {
        parts := strings.SplitN(h, " ", 2)
        if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
            return strings.TrimSpace(parts[1])
        }
        return strings.TrimSpace(h)
    }
    if h := r.Header.Get("X-Goog-Api-Key"); h != "" { return strings.TrimSpace(h) }
    if h := r.Header.Get("X-Api-Key"); h != "" { return strings.TrimSpace(h) }
    if r.URL != nil {
        if k := r.URL.Query().Get("key"); k != "" { return k }
        if k := r.URL.Query().Get("auth_token"); k != "" { return k }
    }
    return ""
}
```

### 3.2 Extract model — tricky

Model nằm ở các vị trí khác nhau tùy endpoint:

| Endpoint | Content-Type | Vị trí model |
|---|---|---|
| `POST /v1/chat/completions` | `application/json` | body `.model` |
| `POST /v1/completions` | `application/json` | body `.model` |
| `POST /v1/messages` | `application/json` | body `.model` |
| `POST /v1/messages/count_tokens` | `application/json` | body `.model` |
| `POST /v1/responses` | `application/json` | body `.model` |
| `POST /v1/images/generations` | `application/json` | body `.model` |
| `POST /v1/images/edits` | `multipart/form-data` | **skip body read** — rate-limit theo key-only |
| `POST /v1beta/models/:model:action` | — | URL path segment |
| `GET /v1/responses` | — (WebSocket upgrade) | **skip hoàn toàn** ở middleware |

Logic:
- Gemini path → parse URL, xong.
- Non-JSON content-type (multipart, form-urlencoded, octet-stream) → **không đọc body** → model="" → apply top-level default limit theo key.
- JSON → đọc có giới hạn `maxBodyPeek` rồi re-inject.

`:countTokens` (Gemini) và `/v1/messages/count_tokens` (Claude) hiện tính vào cùng quota với request sinh token thật — documented behavior, v1 chấp nhận. User muốn tách thì thêm config `ratelimit.exclude-count-tokens: true` sau.

```go
func extractModel(c *gin.Context) string {
    path := c.Request.URL.Path

    // Gemini: /v1beta/models/MODEL:action
    if strings.HasPrefix(path, "/v1beta/models/") {
        rest := strings.TrimPrefix(path, "/v1beta/models/")
        if i := strings.Index(rest, ":"); i >= 0 {
            return strings.ToLower(rest[:i])
        }
        return strings.ToLower(rest)
    }

    // Chỉ đọc body khi JSON. Multipart/form/binary → skip để tránh
    // slurp image/file vào memory + break ParseMultipartForm downstream.
    ct := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
    if !strings.HasPrefix(ct, "application/json") {
        return ""
    }
    if c.Request.Body == nil {
        return ""
    }

    // Body size cap (1 MiB đủ cho JSON chat request; payload > cap → bỏ qua
    // model extract, handler downstream vẫn nhận đủ body qua NopCloser vì
    // ta đã nối phần đã đọc + phần còn lại).
    limited := io.LimitReader(c.Request.Body, maxBodyPeek)
    peek, err := io.ReadAll(limited)
    if err != nil {
        // Nối lại phần đã đọc với phần còn lại để downstream không miss bytes.
        c.Request.Body = struct {
            io.Reader
            io.Closer
        }{io.MultiReader(bytes.NewReader(peek), c.Request.Body), c.Request.Body}
        return ""
    }

    // Re-inject: nếu đọc đủ body (< cap) thì dùng NopCloser bytes.NewReader;
    // nếu còn dư (body > cap) thì MultiReader nối lại.
    if int64(len(peek)) < maxBodyPeek {
        c.Request.Body = io.NopCloser(bytes.NewReader(peek))
    } else {
        orig := c.Request.Body
        c.Request.Body = struct {
            io.Reader
            io.Closer
        }{io.MultiReader(bytes.NewReader(peek), orig), orig}
    }

    m := gjson.GetBytes(peek, "model").String()
    return strings.ToLower(strings.TrimSpace(m))
}
```

**Body re-injection nuance**:
- JSON request dưới 1 MiB (case phổ biến) → đọc hết, re-inject `NopCloser(bytes.NewReader)`. Downstream `c.GetRawData()` work bình thường.
- JSON request lớn hơn 1 MiB → đọc 1 MiB đầu để peek `.model` (model luôn ở đầu object trong các SDK chuẩn), rồi `MultiReader` ghép phần đã đọc với phần còn lại để handler vẫn stream được full body.
- Non-JSON → không đọc body → `c.Request.Body` không đổi → multipart handler work bình thường.

`github.com/tidwall/gjson` đã có trong `go.sum` của SDK (transitive).

### 3.3 Empty model fallback

Nếu `extractModel` trả `""`:
- Cho qua top-level default limit nếu có (`cfg.Resolve(apiKey, "")` → chỉ match top-level)
- Model-specific limits không áp dụng (không biết model là gì)

---

## 4. Wiring vào SDK (`main.go`)

`ratelimit-plugin/main.go`:

```go
package main

import (
    "context"
    "errors"
    "flag"
    "log"
    "os"

    "github.com/router-for-me/CLIProxyAPI/v6/sdk/api"
    "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy"
    "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"

    "<module>/internal/ratelimit"
)

func main() {
    cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
    flag.Parse()

    cfg, err := config.LoadConfig(*cfgPath)
    if err != nil { log.Fatalf("load config: %v", err) }

    // Parse our custom section from the same file.
    rlCfg, err := ratelimit.LoadFromFile(*cfgPath)
    if err != nil { log.Fatalf("load ratelimit config: %v", err) }

    limiter := ratelimit.NewLimiter()
    mw := ratelimit.Middleware(rlCfg, limiter)

    svc, err := cliproxy.NewBuilder().
        WithConfig(cfg).
        WithConfigPath(*cfgPath).
        WithServerOptions(api.WithMiddleware(mw)).
        Build()
    if err != nil { log.Fatalf("build service: %v", err) }

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
        log.Fatalf("run: %v", err)
        _ = os.Stderr
    }
}
```

### 4.1 Config hot-reload — xem §10.

---

## 5. Test plan

`ratelimit-plugin/internal/ratelimit/limiter_test.go` + middleware test:

### Unit tests
- [ ] `Limiter.Take` cấp đủ N slot rồi trả false ở slot N+1
- [ ] Clock tick sang bucket mới → counter reset về 0
- [ ] Hai (apiKey, model) khác nhau không share counter
- [ ] Race test với `-race` flag

### Config tests
- [ ] Parse YAML với chỉ `window + requests` (flat)
- [ ] Parse với `models.X.requests` (inherit window)
- [ ] Parse với `models.X.keys.Y` (per-key override)
- [ ] File không có section `ratelimit:` → config rỗng, middleware no-op
- [ ] Invalid window duration → error

### Middleware integration tests
- [ ] 1 fake handler `/v1/chat/completions` + N+1 requests cùng key+model → request thứ N+1 trả 429
- [ ] Request với key khác không bị limit
- [ ] Request với model khác → dùng limit riêng
- [ ] `/v0/management/...` không bị đếm
- [ ] Gemini path `/v1beta/models/gemini-2.5-pro:generateContent` extract đúng model
- [ ] Body empty → fallback top-level default
- [ ] Handler downstream vẫn nhận được body đầy đủ (kiểm tra re-injection)

### Manual smoke test
- [ ] Start với config example, curl N+1 requests → nhận 429 với `Retry-After` header đúng
- [ ] Check `X-RateLimit-*` headers ở response thành công

---

## 6. UI integration (optional)

Repo này là UI management. Sau khi plugin xong, UI có thể thêm 1 tab hiển thị quota:

- Panel đã có `GET /v0/management/usage` → `StatisticsSnapshot.APIs[apiKey].Models[model].Details[].Timestamp`
- Plugin **không** expose endpoint riêng ở v1 (giữ scope nhỏ)
- UI tự tính: filter Details theo `Timestamp >= now - window`, count → hiển thị `count / limit`
- UI cần đọc được config `ratelimit.*` để biết limit → có 2 option:
  - **A**: Expose endpoint mới `GET /v0/ratelimit/config` (thêm qua `api.WithRouterConfigurator` trong plugin)
  - **B**: UI đọc trực tiếp `config.yaml` (đã có `/v0/management/config.yaml`) → parse section `ratelimit:` ở FE

→ Recommend Option B cho v1: zero backend code thêm, FE chỉ cần YAML parser.

---

## 7. Rollout checklist

### Phase 1 — Core plugin (1-2 ngày)
- [ ] Setup `ratelimit-plugin/` Go module, `go mod init`, `go get github.com/router-for-me/CLIProxyAPI/v6`
- [ ] Implement `Config` + loader (section 1)
- [ ] Implement `Limiter` + unit tests (section 2)
- [ ] Implement middleware + body re-injection (section 3)
- [ ] Wire `main.go` (section 4)
- [ ] Test manual 429 + headers với config example

### Phase 2 — Hardening + 3 feature thêm (2 ngày)
- [ ] Race test (`go test -race`)
- [ ] Middleware integration tests với `httptest.Server`
- [ ] **Content-type guard** — non-JSON skip body read, multipart OK (§3.2)
- [ ] **WebSocket skip** — early-return trên `Upgrade: websocket` (§3)
- [ ] **Body size cap `maxBodyPeek`** với `MultiReader` re-inject cho body > cap (§3.2)
- [ ] **Clock skew guard** trong `Take()` (§2)
- [ ] **Head-trim cap** trong `Take()` chống config giảm limit đột ngột (§2)
- [ ] **Structured reject log** với key hash (sha256 prefix) (§3)
- [ ] GC pruning cho `hits` map
- [ ] **Config hot-reload** — fsnotify watch parent dir + stat-based reload, không filter filename (§10.1)
- [ ] **Persist counter state** — snapshot JSON + load trên start (§10.2)
- [ ] **Wildcard model match** — `path.Match` + resolve cache (§0 Resolution)

### Phase 3 — Nice-to-have (không block launch)
- [ ] UI tab hiển thị quota per key+model
- [ ] Prometheus metrics cho hit rate + rejection rate
- [ ] Multi-instance support (Redis backend thay map + file)

---

## 8. Open questions (cần user confirm)

1. **Module path**: đặt tên Go module là gì? `github.com/cyberk/ratelimit-plugin`? (ảnh hưởng import ở `main.go`)
2. **Empty model behavior**: nếu request không có model field (e.g. endpoint lạ), apply top-level default hay bypass hoàn toàn? (Plan mặc định: apply top-level default)
3. **Failed requests count?**: request trả 5xx từ upstream có tính vào quota không? (Plan mặc định: có tính — vì middleware increment trước khi request chạy; tránh user abuse upstream retry để bypass)
4. **Multi-instance**: nếu deploy N instances của plugin, counter có cần share state (Redis)? V1 assume single-instance.

---

## 10. Hot-reload + Persistence + Docker

### 10.1 Config hot-reload

Plugin dùng `github.com/fsnotify/fsnotify` (đã có trong `go.sum` của repo gốc qua transitive).

```go
type ConfigStore struct {
    ptr atomic.Pointer[Config]   // swap atomically
}

func (s *ConfigStore) Get() *Config       { return s.ptr.Load() }
func (s *ConfigStore) Set(cfg *Config)    { s.ptr.Store(cfg) }

// Watch reload config khi file đổi. Watch parent dir (không watch file)
// vì nhiều cơ chế write file làm mất inode:
//   - vim/VSCode: write-via-rename
//   - K8s ConfigMap: symlink swap qua ../data/<timestamp> (event xảy ra
//     trên symlink, không phải trên file đích)
//   - Docker bind mount với file remount
// Strategy: listen MỌI event trong thư mục chứa, debounce 500ms, rồi re-stat
// file và load lại. Không filter theo filename — chi phí reload rẻ, robust.
func (s *ConfigStore) Watch(ctx context.Context, path string, onReload func(*Config)) error {
    w, err := fsnotify.NewWatcher()
    if err != nil { return err }
    if err := w.Add(filepath.Dir(path)); err != nil { w.Close(); return err }

    go func() {
        defer w.Close()
        debounce := time.NewTimer(time.Hour); debounce.Stop()
        var lastMtime time.Time

        reload := func() {
            info, err := os.Stat(path)
            if err != nil {
                log.Warnf("ratelimit stat config: %v", err)
                return
            }
            if info.ModTime().Equal(lastMtime) { return } // không đổi thật
            lastMtime = info.ModTime()

            cfg, err := LoadFromFile(path)
            if err != nil { log.Warnf("ratelimit reload: %v", err); return }
            s.Set(cfg)
            if onReload != nil { onReload(cfg) }
            log.Infof("ratelimit config reloaded")
        }

        for {
            select {
            case <-ctx.Done(): return
            case _, ok := <-w.Events:
                if !ok { return }
                debounce.Reset(500 * time.Millisecond)
            case err, ok := <-w.Errors:
                if !ok { return }
                log.Warnf("ratelimit fsnotify: %v", err)
            case <-debounce.C:
                reload()
            }
        }
    }()
    return nil
}
```

**Fallback cho filesystem không emit fsnotify events** (một số mount point trên Docker Desktop macOS qua VirtioFS/gRPC-fuse có thể lag) — thêm goroutine stat mỗi 30s làm safety net. Chỉ reload khi mtime đổi so với lần trước, overhead gần bằng 0.

**Middleware** đọc config qua `store.Get()` mỗi request (atomic pointer load → ~1ns, no lock).

**Impact khi reload**:
- Wildcard resolve cache bị invalidate (`sync.Map` → `Range` + `Delete`, hoặc replace `sync.Map` nguyên cái).
- **Counter state KHÔNG reset** — giữ `Limiter.hits` nguyên. User tăng limit giữa chừng → request đã đếm vẫn tính. User giảm limit → nếu `len(hits) >= limit mới` thì request tiếp sẽ 429 ngay.

### 10.2 Persist counter state xuống file

```go
// Snapshot serializes hits map với timestamps đã strip expired.
type snapshot struct {
    SavedAt time.Time                     `json:"saved_at"`
    Hits    map[string][]int64            `json:"hits"`   // "apiKey|model" → unix nanos
}

func (l *Limiter) Save(path string) error {
    l.mu.Lock()
    snap := snapshot{SavedAt: l.now(), Hits: make(map[string][]int64, len(l.hits))}
    for k, ts := range l.hits {
        key := k.apiKey + "\x00" + k.model
        arr := make([]int64, len(ts))
        for i, t := range ts { arr[i] = t.UnixNano() }
        snap.Hits[key] = arr
    }
    l.mu.Unlock()

    data, err := json.Marshal(snap)
    if err != nil { return err }
    tmp := path + ".tmp"
    if err := os.WriteFile(tmp, data, 0o600); err != nil { return err }
    return os.Rename(tmp, path)   // atomic trên cùng filesystem
}

func (l *Limiter) Load(path string, maxWindow time.Duration) error {
    data, err := os.ReadFile(path)
    if err != nil {
        if errors.Is(err, os.ErrNotExist) { return nil }  // first run
        return err
    }
    var snap snapshot
    if err := json.Unmarshal(data, &snap); err != nil { return err }

    cutoff := time.Now().Add(-maxWindow)
    l.mu.Lock()
    defer l.mu.Unlock()
    for key, arr := range snap.Hits {
        parts := strings.SplitN(key, "\x00", 2)
        if len(parts) != 2 { continue }
        ck := counterKey{parts[0], parts[1]}
        fresh := make([]time.Time, 0, len(arr))
        for _, ns := range arr {
            t := time.Unix(0, ns)
            if t.After(cutoff) { fresh = append(fresh, t) }
        }
        if len(fresh) > 0 { l.hits[ck] = fresh }
    }
    return nil
}
```

**Snapshot driver** — goroutine tick mỗi N giây:
```go
// trong main.go
go func() {
    t := time.NewTicker(5 * time.Second)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            _ = limiter.Save(statePath)   // final flush
            return
        case <-t.C:
            if !limiter.TakeDirty() { continue }
            if err := limiter.Save(statePath); err != nil { log.Warnf("save state: %v", err) }
        }
    }
}()
```

`TakeDirty()` = `dirty.CompareAndSwap(true, false)` — chỉ ghi file khi có thay đổi, tránh IO không cần.

**Signal handler**: bắt SIGTERM/SIGINT → `cancel()` → driver flush lần cuối.

### 10.3 Docker — persist có work không? **Work được.**

**Chiến lược đơn giản nhất: state file ở cùng thư mục với `config.yaml`** — mặc định `state.json` ngay cạnh `config.yaml`. User chỉ cần mount **1 volume chứa thư mục config**, không cần quản lý volume riêng cho state.

Path default (nếu `-state` không set):
```go
statePath := filepath.Join(filepath.Dir(cfgPath), "ratelimit-state.json")
```

Cho override qua flag `-state /path/to/state.json` nếu ai muốn tách.

```yaml
# docker-compose.yml — chỉ 1 volume mount cho cả config + state
services:
  ratelimit-plugin:
    image: ratelimit-plugin:latest
    volumes:
      - ./data:/app/data    # chứa cả config.yaml và ratelimit-state.json
    command: ["-config", "/app/data/config.yaml"]
    # state tự lưu vào /app/data/ratelimit-state.json
```

**Lưu ý khi config.yaml mount read-only** (`:ro`):
- State file ở cùng thư mục → thư mục phải RW → **không mount `:ro`** cho cả dir.
- Nếu bắt buộc muốn config read-only: mount file cụ thể `./config.yaml:/app/config.yaml:ro` + 1 thư mục khác cho state (`./state-dir:/app/state`).
- Recommend: mount nguyên thư mục RW — đơn giản, atomic rename `.tmp → state.json` vẫn work.

**Graceful shutdown**: Docker gửi SIGTERM (default 10s grace period trước SIGKILL). App phải:
1. Bắt SIGTERM → `cancel(ctx)`
2. Driver flush 1 lần cuối trong `<-ctx.Done()` branch
3. Service tự shutdown trong grace period

Nếu container bị SIGKILL (OOM, timeout) → mất ≤ 5s cuối (interval tick). Chấp nhận được với limit 5h.

**Caveats khi chạy Docker:**

| Vấn đề | Giải pháp |
|---|---|
| `os.Rename` cross-filesystem fail (nếu tmp ở `/tmp`, file ở volume) | Luôn ghi `.tmp` vào **cùng thư mục** với file đích — code `Save()` đã làm đúng (`path + ".tmp"`). |
| Volume permission (container chạy non-root user) | Dockerfile dùng `USER 1000` + `chown` volume mount point, hoặc compose `user: "1000:1000"`. |
| Hot-reload khi mount config read-only qua `docker cp` / re-mount | fsnotify trên bind mount work bình thường trên Linux. **Docker Desktop macOS/Windows** fsnotify có thể lag 1-2s (gRPC-fuse) — vẫn work, chỉ chậm. |
| Fsnotify trên file bind-mount (không phải thư mục) | Đã watch thư mục chứa (section 10.1) → tránh vấn đề inode đổi. Với bind mount file từ host, watch thư mục chứa file **trong container** vẫn nhận event. |
| Multi-instance qua Docker Swarm / K8s replicas | File-based persist **không share** giữa replicas → mỗi replica có counter riêng → user có thể đạt `N × limit` thực tế. Single-instance only ở v1, hoặc chuyển Redis backend (v2). |

**Dockerfile minimal**:

```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY ratelimit-plugin/ ./
RUN go mod download && CGO_ENABLED=0 go build -o /out/ratelimit-plugin .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/ratelimit-plugin /app/ratelimit-plugin
WORKDIR /app/data    # default cwd; user mount volume vào đây
USER nonroot:nonroot
ENTRYPOINT ["/app/ratelimit-plugin", "-config", "/app/data/config.yaml"]
```

---

## 9. Reference map

| Thứ cần biết | File | Line |
|---|---|---|
| SDK middleware option | `sdk/api/options.go` | 21 |
| SDK Builder | `sdk/cliproxy/builder.go` | 22-260 |
| SDK config loader | `sdk/config/config.go` | 38 |
| API key extract reference | `internal/access/config_access/provider.go` | 55-104 |
| Auth middleware (chạy *sau* user middleware) | `internal/api/server.go` | 1028, 1038 |
| Routes (để biết endpoint nào cần gate) | `internal/api/server.go` | 330-376 |
| Usage record struct (nếu muốn dùng plugin thay vì middleware) | `sdk/cliproxy/usage/manager.go` | 11-37 |
| Example custom provider / builder wiring | `examples/custom-provider/main.go` | 175-225 |
