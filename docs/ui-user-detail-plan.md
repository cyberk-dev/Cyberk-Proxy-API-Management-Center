# Plan — User Detail Usage + Access-Key Alias (isolated custom screens)

**Mục tiêu:** Thêm 2 tính năng vào UI panel **theo cách cực kỳ isolate**, để khi pull code upstream (`router-for-me`) về merge không bị conflict (hoặc conflict cực nhỏ và dễ giải quyết trong 5 giây).

1. **User Detail Usage** — drill-down xem usage chi tiết của từng API key.
2. **Access-Key Alias** — đặt tên thân thiện cho từng api-key (vd `sk-abc123…` → `"Alice laptop"`).

Câu hỏi của user: *có cần thêm code vào plugin không?*
**→ Không cần.** Alias là concept thuần UI; dữ liệu alias được lưu vào chính `config.yaml` dưới 1 top-level key mới (upstream ignore, giống hệt cách `ratelimit:` đang hoạt động). Plugin không phải đổi 1 dòng.

---

## 0. Chiến lược isolate (quan trọng nhất)

Nguyên tắc: **tất cả code mới nằm trong `src/extensions/` — 1 thư mục upstream sẽ không bao giờ đụng tới**. File upstream hiện tại chỉ bị touch ở **đúng 2 chỗ**, mỗi chỗ đúng **1 dòng spread**.

```
src/
├── extensions/                    ← thư mục của riêng tao, upstream không touch
│   ├── index.ts                   ← entry: export routes + navItems + i18n
│   ├── routes.tsx                 ← mảng route cho React Router
│   ├── nav.ts                     ← mảng nav item cho sidebar
│   ├── i18n/                      ← chuỗi dịch riêng (không đụng vào locale upstream)
│   │   ├── en.ts
│   │   └── vi.ts
│   ├── pages/
│   │   ├── UsersPage.tsx          ← list users (api-key) + alias editor
│   │   └── UserDetailPage.tsx     ← drill-down usage của 1 key
│   ├── components/
│   │   ├── UserTable.tsx
│   │   ├── AliasEditor.tsx
│   │   ├── UserUsageCharts.tsx
│   │   └── RequestDetailList.tsx
│   ├── hooks/
│   │   ├── useKeyAliases.ts       ← đọc/ghi alias từ config.yaml
│   │   └── usePerKeyUsage.ts      ← pivot usage theo apiKey
│   ├── services/
│   │   └── aliasStore.ts          ← YAML read/write alias section
│   └── utils/
│       └── keyPivot.ts            ← tổng hợp usage per-key từ snapshot
```

### Điểm chạm upstream (2 file, 2 dòng)

**File 1 — `src/router/MainRoutes.tsx`** (chèn marker comment cho merge dễ):
```ts
import { extensionRoutes } from '@/extensions';
// ...
const mainRoutes = [
  /* ...upstream routes giữ nguyên... */
  // --- extensions: do not remove ---
  ...extensionRoutes,
  // --- /extensions ---
  { path: '*', element: <Navigate to="/" replace /> },
];
```

**File 2 — `src/components/layout/MainLayout.tsx`**:
```ts
import { extensionNavItems } from '@/extensions';
// ...
const navItems = [
  /* ...upstream navItems giữ nguyên... */
  { path: '/system', label: t('nav.system_info'), icon: sidebarIcons.system },
  // --- extensions: do not remove ---
  ...extensionNavItems(t),
  // --- /extensions ---
];
```

**Note về `navOrder` / `getRouteOrder`** (MainLayout.tsx:434-469): nav items cuối cùng được ánh xạ vào `navOrder` để xác định animation direction khi chuyển trang. Việc append `...extensionNavItems(t)` vào cuối KHÔNG phá logic này — upstream `getRouteOrder` dùng `navOrder.indexOf(normalizedPath)` fallback cho các route không phải ai-providers/auth-files, nên route của extensions sẽ được match chính xác. Marker comment giúp merge 3-way khi upstream refactor block này.

### i18n isolate

Locale upstream nằm ở `src/i18n/locales/*.ts`. Nếu tao nhét string vào đấy sẽ đụng khi upstream thêm key.
→ Tạo namespace riêng **`extensions`**, load song song với namespace default.

Trong `src/extensions/i18n/vi.ts`:
```ts
export default {
  users: {
    title: 'Người dùng',
    alias: 'Tên gọi',
    no_alias: '(chưa đặt)',
    total_requests: 'Tổng request',
    ...
  },
};
```

**[UPDATED]** Import từ central init (`@/i18n`) thay vì trực tiếp `i18next`, vì i18n init là synchronous ở import-time trong `src/i18n/index.ts`:

```ts
import i18n from '@/i18n';   // central instance đã init rồi
import enExt from './i18n/en';
import zhCNExt from './i18n/zh-CN';
i18n.addResourceBundle('en', 'extensions', enExt, true, true);
i18n.addResourceBundle('zh-CN', 'extensions', zhCNExt, true, true);
// zh-TW, ru fallback về zh-CN → không cần thêm trừ khi muốn dịch hoàn chỉnh
```

Trong page dùng: `useTranslation('extensions')`. i18n fallbackLng là `zh-CN` nên nếu user chọn locale không có bundle extensions, t() sẽ fallback sang zh-CN của extensions (nếu ns đã register), hoặc trả key.

### Style isolate

Dùng CSS Modules (`*.module.scss`) trong `src/extensions/` — không đụng vào `src/styles/` upstream, không đụng vào biến SCSS global (dùng CSS custom properties chung mà upstream đã expose như `--color-bg`, `--color-fg`, v.v.).

---

## 1. Feature A — Access-Key Alias

### 1.0. VERIFIED — sửa config có được không, extend `api-keys` có được không?

Đã đọc source upstream tại `/Users/huybuidac/Projects/ai-oss/CLIProxyAPI`. Kết luận:

**(a) Thêm top-level key lạ (vd `ui-aliases:`) vào config.yaml → AN TOÀN ✓**

Evidence:
- `internal/config/config.go:611` — load dùng `yaml.Unmarshal(data, &cfg)`, **không** dùng `yaml.NewDecoder(r).KnownFields(true)`. yaml.v3 ở chế độ non-strict **silently ignore** field không match struct tag. → Thêm `ui-aliases:` không gây parse error.
- `internal/api/handlers/management/config_basic.go:111-162` (`PutConfigYAML`):
  1. Unmarshal body để validate (non-strict → unknown keys OK).
  2. Gọi `LoadConfigOptional` validate lần 2 (cũng non-strict).
  3. `WriteConfig(h.configFilePath, body)` — **ghi raw bytes** từ request body xuống disk (line 151). → **Giữ nguyên comment, format, và mọi custom key**.
  4. `LoadConfig` reload vào memory (runtime struct không thấy `ui-aliases:` nhưng file trên disk vẫn còn nguyên).
- `GetConfigYAML` (line 167-182) — `os.ReadFile` + write raw bytes. → Panel đọc lại chính xác bytes đã ghi.

→ **Round-trip GET → edit → PUT giữ nguyên comment, giữ nguyên custom key, không làm hỏng function chính của CLIProxyAPI.**

**(b) Extend INLINE `api-keys:` từ `[]string` sang `[]object` → KHÔNG ĐƯỢC ✗**

Evidence:
- `internal/config/sdk_config.go:25` — `APIKeys []string \`yaml:"api-keys"\``. Strict Go type.
- Nếu gửi:
  ```yaml
  api-keys:
    - key: sk-alice
      alias: Alice
  ```
  thì `yaml.Unmarshal` trả error `cannot unmarshal !!map into string`. → `PutConfigYAML` trả HTTP 400 `invalid_yaml`. Request bị reject. Restart binary cũng fail khởi động (line 611-616 trả error nếu không optional).

→ **Không thể đổi shape của `api-keys:` inline. Phải dùng section riêng.**

### 1.1. Lưu ở đâu (kết luận)

Dùng top-level key riêng — đã verified an toàn ở §1.0(a):

```yaml
# ...
api-keys:
  - sk-alice-xxx
  - sk-bob-yyy

# mới — plugin + upstream đều ignore
ui-aliases:
  sk-alice-xxx: "Alice laptop"
  sk-bob-yyy:   "Bob iOS app"
```

Tại sao `config.yaml` chứ không phải file riêng / localStorage:
- **Shared**: team chung config → chung alias. localStorage chỉ per-browser.
- **Backup-able**: theo đúng pattern config-as-source-of-truth.
- **Không cần endpoint mới**: `/v0/management/config.yaml` GET/PUT sẵn có (xem `src/services/api/configFile.ts`), handler ghi raw bytes → bảo toàn custom keys.
- **Upstream tương thích**: verified ở §1.0(a).

### 1.2. API lớp trong UI

**[UPDATED per oracle review]** Repo đã có sẵn `yaml@2.8.2` (`package.json`) và pattern `parseDocument` được dùng khắp `useVisualConfig.ts` + `ConfigPage.tsx`. Dùng luôn — comment preservation built-in, handle tốt anchors/flow/multi-line scalars/CRLF.

`src/extensions/services/aliasStore.ts`:
```ts
import { parseDocument, isMap, YAMLMap } from 'yaml';
import { configFileApi } from '@/services/api';

export type AliasMap = Record<string, string>;
const ALIAS_KEY = 'ui-aliases';

export async function readAliases(): Promise<AliasMap> {
  const raw = await configFileApi.fetchConfigYaml();
  const doc = parseDocument(raw);
  const node = doc.get(ALIAS_KEY);
  if (!isMap(node)) return {};
  const out: AliasMap = {};
  for (const item of node.items) {
    const k = String(item.key);
    const v = item.value == null ? '' : String((item.value as { value?: unknown }).value ?? item.value);
    if (k && v) out[k] = v;
  }
  return out;
}

export async function writeAlias(apiKey: string, alias: string): Promise<void> {
  // Always read-modify-write server-latest to avoid clobbering concurrent ConfigPage edits.
  const raw = await configFileApi.fetchConfigYaml();
  const doc = parseDocument(raw);
  if (doc.errors.length > 0) {
    throw new Error(`config.yaml parse error: ${doc.errors[0].message}`);
  }
  let node = doc.get(ALIAS_KEY);
  if (!isMap(node)) {
    node = new YAMLMap();
    doc.set(ALIAS_KEY, node);
  }
  const trimmed = alias.trim();
  if (trimmed) (node as YAMLMap).set(apiKey, trimmed);
  else (node as YAMLMap).delete(apiKey);
  await configFileApi.saveConfigYaml(doc.toString({ lineWidth: 120 }));
}
```

**Why not regex**: breaks on block scalars, anchors, merge keys, CRLF. Not worth.
**Why not js-yaml**: loses comments + adds a dep. `yaml@2` via `Document` AST preserves comments natively (`doc.commentBefore`, node-level `.comment`).

### 1.3. Hook `useKeyAliases`

```ts
// src/extensions/hooks/useKeyAliases.ts
export function useKeyAliases() {
  const [aliases, setAliases] = useState<AliasMap>({});
  const [loading, setLoading] = useState(false);

  const reload = useCallback(async () => {
    setLoading(true);
    try { setAliases(await readAliases()); }
    finally { setLoading(false); }
  }, []);

  const save = useCallback(async (k: string, v: string) => {
    await writeAlias(k, v);
    setAliases(prev => {
      const next = { ...prev };
      if (v.trim()) next[k] = v.trim(); else delete next[k];
      return next;
    });
  }, []);

  useEffect(() => { void reload(); }, [reload]);
  return { aliases, loading, reload, save };
}
```

### 1.4. UI — inline alias editor
Component `AliasEditor.tsx`: click cell alias → input inline → Enter save / Esc cancel. Dùng trong cột đầu tiên của `UserTable`.

---

## 2. Feature B — User Detail Usage

### 2.1. Nguồn dữ liệu

Tận dụng endpoint sẵn có: `GET /v0/management/usage` → shape:
```
{ apis: { [apiKey]: { models: { [modelName]: { details: UsageDetail[] } } } } }
```

Tất cả đã có cache/helper trong `src/utils/usage.ts` (`collectUsageDetails`, `getApiStats`, `filterUsageByTimeRange`). **Tận dụng hết — không viết lại.**

### 2.2. Trang list — `/custom/users` (namespaced để tránh va chạm upstream)

`UsersPage.tsx`:

Cột:
| Alias | API key (masked) | Total req | Success % | Tokens | Cost | Last active |
|---|---|---|---|---|---|---|

- Data source: reuse `useUsageData()` hook hoặc đọc từ `useUsageStatsStore`.
- Pivot: `Object.entries(usage.apis).map(([key, entry]) => aggregate(entry))`.
- Masking: `sk-abc…xyz` (giữ prefix 4 + suffix 4).
- Sort mặc định: Total req giảm dần.
- Search box: filter theo alias / key substring.
- Row click → `navigate('/custom/users/' + encodeURIComponent(key))`.
- **Orphan handling**: usage có thể chứa apiKey đã bị xóa khỏi `api-keys:` list hiện tại. Group orphans ở cuối bảng, badge "(removed)", disable alias edit cho các row này (không lưu được vì readAliases chỉ đọc key thật — thực ra vẫn lưu được, nhưng UX kỳ; nên disable).

Route path: `/custom/users` (namespace để tránh upstream collision).

### 2.3. Trang detail — `/custom/users/:apiKey`

`UserDetailPage.tsx`:

**Header**: alias (editable ngay ở đây), api-key masked, button copy full key (với confirmation).

**Summary cards**: Total req / Total tokens / Total cost / Success rate / RPM / TPM — dùng `StatCards` upstream nếu có prop feed được subset usage, nếu không thì clone 1 bản thu gọn vào extensions.

**Time range filter**: giống `UsagePage` (`7h / 24h / 7d / all`).

**Charts**:
- Requests trend (line)
- Token breakdown (stacked: input / output / cache)
- Cost trend
- Model distribution (pie/bar: request count per model)

**Rate-limit status panel** *(bonus, nếu dễ)*:
- Đọc `ratelimit:` section từ `config.yaml`, resolve limit cho từng model của user này.
- Hiển thị bảng: `model | window | limit | approx usage | reset at`.
- **Label rõ là "approx" / "ước tính client-side"**: client-side count từ `details[]` sẽ drift so với plugin counter (usage cache TTL, sliding-window boundary, in-flight requests). Không dùng để drive quyết định; chỉ dashboard hint. Plugin vẫn là source of truth khi enforce.

**Request log table**: list `details[]` gần nhất. Cột: timestamp, model, latency, tokens in/out, status (failed/ok), source. **Virtualize từ 200 rows** (react-window) — row dense với 6-8 cell dễ jank trên laptop tầm trung, lib đã trong deps hoặc cheap thêm. Không cap upper bound; virtualize handle được.

### 2.4. Pivot helper

`src/extensions/utils/keyPivot.ts`:

```ts
export interface PerKeyStats {
  apiKey: string;
  alias?: string;
  totalRequests: number;
  failedRequests: number;
  totalInputTokens: number;
  totalOutputTokens: number;
  totalCost: number;           // needs modelPrices
  lastActiveMs: number;
  perModel: Array<{ model: string; requests: number; tokens: number; cost: number }>;
}

export function pivotByKey(usage: unknown, aliases: AliasMap, modelPrices: ModelPriceMap): PerKeyStats[];
```

Implement = iterate `apis` entries, chạy logic tương tự `getApiStats` nhưng grouped theo top-level apiKey thay vì flatten.

---

## 3. Task breakdown (thứ tự làm)

| # | Task | File | Est |
|---|------|------|-----|
| 1 | Scaffold `src/extensions/` + `index.ts` với i18n register | `src/extensions/{index,i18n/*}.ts` | 20m |
| 2 | Patch 2 điểm chạm upstream (routes + nav) | `MainRoutes.tsx`, `MainLayout.tsx` | 10m |
| 3 | `aliasStore.ts` với surgical YAML edit + unit test | `services/aliasStore.ts` | 1h |
| 4 | `useKeyAliases` hook | `hooks/useKeyAliases.ts` | 20m |
| 5 | `keyPivot.ts` (+ test) | `utils/keyPivot.ts` | 1h |
| 6 | `UsersPage` + `UserTable` + `AliasEditor` | `pages/UsersPage.tsx` | 2-3h |
| 7 | `UserDetailPage` summary cards + charts | `pages/UserDetailPage.tsx` | 3-4h |
| 8 | Request log table trong detail page | `components/RequestDetailList.tsx` | 1-2h |
| 9 | Rate-limit status panel (nếu có thời gian) | `components/RateLimitStatus.tsx` | 1-2h |
| 10 | i18n vi + en strings | `i18n/*.ts` | 30m |
| 11 | Smoke test trên browser với docker-compose đang chạy | — | 30m |

Total: ~11-16h. MVP (1-7 + 10) ~8h.

---

## 4. Những thứ **không** làm (out of scope v1)

- Không build per-user quota/limit editor trong UI này (đã có trong plan ratelimit-plugin riêng).
- Không edit config.yaml ngoài `ui-aliases:` section. Mọi config khác vẫn qua `ConfigPage` upstream.
- Không tạo endpoint backend mới. **Tuyệt đối không đụng vào `ratelimit-plugin/`.**
- Không lưu alias trong localStorage (đã bàn ở §1.1).
- Không role-based access: tất cả user có management-key đều thấy & edit alias.

---

## 5. Rủi ro & mitigate

| Rủi ro | Mitigate |
|---|---|
| YAML round-trip làm mất comment | Surgical edit chỉ block `ui-aliases:`; giữ nguyên mọi thứ khác. Có fallback full-dump nếu regex fail + warn user. |
| Upstream 1 ngày đó thêm field `ui-aliases` vào main config | Rename key local thành `_ui-aliases` hoặc `custom-ui.aliases` (nested). Xác suất thấp. |
| Upstream chuyển sang `yaml.KnownFields(true)` (strict) | Phải migrate sang sidecar file `aliases.yaml` cạnh `config.yaml`, đọc/ghi qua static file server hoặc add endpoint. Xác suất thấp (breaking change cho cả user community). |
| PUT ghi raw bytes → nếu UI gửi YAML syntactically broken sẽ reject với 400; nếu semantically sai (vd đổi port sang string) sẽ reject với 422 | Surgical edit chỉ block `ui-aliases:`, không động vào phần khác → không thể làm hỏng phần config khác. Test trước khi PUT bằng `yaml.load` client-side. |
| Conflict merge khi upstream reorder mảng `mainRoutes` | Hai điểm chạm ở cuối mảng, conflict trivial. Nếu upstream refactor lớn → revisit (hiếm). |
| Usage data không có apiKey (request anonymous) | Group thành row "(unknown)" riêng. |
| `js-yaml` chưa có trong deps | Check `package.json`; thêm nếu thiếu (`npm i js-yaml @types/js-yaml`). |

---

## 6. Câu hỏi cần user confirm trước khi code

1. **Tên top-level key** trong config.yaml: `ui-aliases` / `key-aliases` / `access-key-aliases`? (tao vote `ui-aliases` — ngắn, rõ là của UI).
2. **Surgical YAML edit** (giữ comment, phức tạp hơn) hay **full round-trip dump** (mất comment, đơn giản)? Tao recommend surgical.
3. **Route path**: `/users` có OK không, hay muốn `/extensions/users` cho rõ là custom? (`/users` ngắn hơn, nhưng có rủi ro upstream xài sau này).
4. Rate-limit status panel trong user detail có cần làm MVP không, hay để v2?
5. Có muốn thêm nút **export per-user usage to CSV/JSON** trong detail page không?

---

## 7. Reference

- UI entry: `src/router/MainRoutes.tsx:24-80` (mảng `mainRoutes`)
- Sidebar nav: `src/components/layout/MainLayout.tsx:421-433` (mảng `navItems`)
- Config YAML API: `src/services/api/configFile.ts`
- Usage API + cache: `src/services/api/usage.ts`, `src/utils/usage.ts` (`collectUsageDetails`, `getApiStats`)
- Upstream config shape: `src/services/api/transformers.ts:424-426` (`api-keys` parsing)
- Existing usage page reference: `src/pages/UsagePage.tsx`
- Plugin doc (liên quan `ratelimit:` key pattern): `docs/ratelimit-plugin-plan.md`
