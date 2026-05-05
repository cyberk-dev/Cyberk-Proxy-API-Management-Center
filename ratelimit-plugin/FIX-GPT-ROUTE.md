# Plan: Fix gpt-5.4 routing failure when ratelimit-plugin is active

## Context for the agent picking this up

The user runs `ratelimit-plugin` which embeds the `CLIProxyAPI` SDK (`github.com/router-for-me/CLIProxyAPI/v6`) and adds:

1. A Gin middleware for per-API-key rate limiting (`internal/ratelimit/middleware.go`)
2. A custom `coreauth.Selector` named `WeightedSelector` (`internal/weightedselector/selector.go`) that does SWRR over Codex auths weighted by `Attributes["plan_type"]`. It is plugged in via `cliproxy.Builder.WithCoreAuthManager(...)` in `main.go` when `codex_weights:` block is present in `config.yaml`.

The SDK source is read-only at `/Users/mac/Documents/cyberk/CLIProxyAPI` (sample copy; the real binary uses the published module). Do NOT edit SDK source as part of the fix — fix lives in the plugin.

## Symptom

When the plugin is running (binary reports `Version: dev`), requests from Claude Code (`POST /v1/messages?beta=true`) with `model: gpt-5.4` fail intermittently:

- HTTP 400 `{"detail":"Store must be set to false"}`
- HTTP 400 `{"detail":"Unsupported parameter: messages"}`

When swapped to the stock `cli-proxy-api` binary (`v6.9.37`), the same requests succeed. The plugin is the only difference.

Evidence: `./logs/error-v1-messages-2026-04-25T*.log` — all 5 `gpt-5.4` failures recorded with `Version: dev`. No `gpt-5.4` failures with `Version: v6.9.37`.

## User config (relevant excerpt)

`config.yaml`:
```yaml
routing:
  strategy: round-robin
oauth-model-alias:
  codex:
    - name: gpt-5.4
      alias: gpt-5.4-high-fast
      fork: true
    - name: gpt-5.4
      alias: gpt-5.4-fast
      fork: true
payload:
  override:
    - models: [{ name: gpt-5.4-high-fast, protocol: codex }]
      params: { "reasoning.effort": high, service_tier: priority }
    - models: [{ name: gpt-5.4-fast, protocol: codex }]
      params: { "reasoning.effort": medium, service_tier: priority }
codex_weights:
  pro: 10
  prolite: 5
  plus: 1
  free: 1
  team: 1
  business: 1
  go: 1
```

The `codex_weights` block enables the plugin's custom selector. Removing the block disables it (binary then behaves identically to stock).

The error response style (`{"detail":...}` Pydantic-shape, headers `X-Ratelimit-Limit: 800/1500`, `Access-Control-Allow-Origin: *`) is from a **custom upstream gateway** sitting in front of the real ChatGPT Codex API, not chatgpt.com itself. This gateway strictly validates the OpenAI Responses API schema (requires `input` field, requires explicit `store: false`).

## Root cause

The SDK splits auth selection into two paths controlled by `useSchedulerFastPath()` in `sdk/cliproxy/auth/conductor.go`:

```go
// conductor.go:~2637
func (m *Manager) useSchedulerFastPath() bool {
    return isBuiltInSelector(m.selector)
}

// conductor.go:~205
func isBuiltInSelector(selector Selector) bool {
    switch selector.(type) {
    case *RoundRobinSelector, *FillFirstSelector:
        return true
    default:
        return false
    }
}
```

| Setup | `m.selector` runtime type | `useSchedulerFastPath()` | Routing path |
|---|---|---|---|
| Stock binary, `routing.strategy: round-robin` | `*coreauth.RoundRobinSelector` | true | `m.scheduler.pickMixed` (fast) |
| Plugin (this repo), `codex_weights` set | `*weightedselector.Selector` (or `*SessionAffinitySelector` wrapping it) | **false** | `m.pickNextMixedLegacy` |

The two paths are not strictly equivalent for Codex with OAuth model aliases:

- **Fast path** (`scheduler.pickMixed`) uses pre-built per-(provider, model) shards that incorporate model-alias resolution via `selectionModelKeyForAuth` and per-model state.
- **Legacy path** (`pickNextMixedLegacy`) filters `m.auths` by provider only, then defers entirely to `m.selector.Pick(ctx, "mixed", model, opts, available)`.

The user's config has TWO codex aliases that fork additional client-visible models (`gpt-5.4-high-fast`, `gpt-5.4-fast`). The fast path scheduler reconciles these aliases differently from the legacy path. Combined with `payload.override` for `service_tier: priority` on the alias variants, and a strict custom upstream gateway, the legacy path produces an outgoing request that the gateway rejects.

Empirically confirmed by the version split in error logs: every failure is on `Version: dev` (plugin), no failures on stock `v6.9.37`.

## What is NOT yet 100% confirmed

The user's config did not enable `request-log: true`, so the error logs do not contain the **outgoing upstream request body**. We cannot directly see whether:

1. The body arriving at the upstream gateway is the raw Anthropic Messages payload (no translation), OR
2. The body is partially translated (e.g., Claude → OpenAI Chat Completions, which would still contain `messages`), OR
3. The body is correctly Codex-formatted but routed to a wrong upstream URL (a different auth's `BaseURL`).

All three would produce the observed error message from a Pydantic-shaped Responses-API gateway.

## Verification step (do this FIRST before coding any fix)

Add a debug log inside `WeightedSelector.Pick` and rerun a failing case to capture which auth is being picked and on which path. Then enable `request-log: true` to capture the outgoing body and URL.

### Step 1 — add diagnostic logging

In `internal/weightedselector/selector.go`, at the start of `Pick(...)` and at every return site, log:
- `provider` (the parameter, lowercased)
- `model`
- `len(auths)` and the IDs of every auth in `auths`
- whether `allCodexAuths(auths)` is true
- the chosen auth ID (if any)
- which return branch fired (`base.Pick passthrough`, `weighted pick`, `fallback to base because no positive weights`, `fallback to base because pool returned empty`, etc.)

Use `log` (logrus) the same way `main.go` does. Example:

```go
log.WithFields(log.Fields{
    "event":     "weighted.pick.enter",
    "provider":  providerLower,
    "model":     model,
    "auth_ids":  authIDs(auths),
    "all_codex": allCodexAuths(auths),
}).Info("weighted selector pick")
```

Add a tiny helper:

```go
func authIDs(auths []*coreauth.Auth) []string {
    out := make([]string, 0, len(auths))
    for _, a := range auths {
        if a != nil {
            out = append(out, a.ID)
        }
    }
    return out
}
```

### Step 2 — enable upstream request logging

Append to `config.yaml`:
```yaml
request-log: true
```

This makes `RecordAPIRequest` (in the SDK at `internal/runtime/executor/helps/logging_helpers.go`) write a `=== API REQUEST 1 ===` block into the error log files containing the actual upstream URL, headers, and body.

### Step 3 — reproduce the failure

Restart the plugin binary, send a `POST /v1/messages?beta=true` with `{"model":"gpt-5.4", ...}` from Claude Code. When it fails, the new error log file under `./logs/error-v1-messages-*.log` will contain both the upstream request and the plugin's `weighted.pick.*` lines in `main.log`.

Tag both the request_id and the chosen auth ID. From there:

- If outgoing body has `"messages"` (no `"input"`) → translator did not run → fix at the routing level.
- If outgoing body has `"input"` but no `"store"` → `store: false` is being dropped somewhere → fix in `payload.override` or translator path.
- If outgoing URL differs across success and failure runs → wrong auth selected → fix in `WeightedSelector` filtering.

## Fix (apply once Step 3 confirms which scenario)

Below are the three fixes ordered by likelihood. All are local to the plugin (`internal/weightedselector/`).

### Fix A — Force codex auths only when picking for a Codex route (most likely needed)

Right now `Pick(...)` enters the codex weighted branch when `provider == "mixed"` and `allCodexAuths(auths)` is true. If the candidate set ever contains a non-codex auth (because `oauth-model-alias` makes a non-codex auth advertise `gpt-5.4`), the selector falls through to the wrapped `base` (`RoundRobin`), which can pick a non-codex auth → wrong executor → wrong format → upstream rejects.

In `internal/weightedselector/selector.go`, change the codex branch entry condition to always **filter** to codex auths first when the route is recognizable as codex (registered codex provider OR `model` matches a known codex model name pattern), then run weighted pick on that subset:

```go
func (s *Selector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
    if !s.cfg.Enabled {
        return s.base.Pick(ctx, provider, model, opts, auths)
    }
    providerLower := strings.ToLower(strings.TrimSpace(provider))

    // Force codex-only filtering when ANY codex auth exists in the candidate set
    // for a "mixed" route. Prevents accidental fallback to non-codex auths from
    // RR-base when oauth-model-alias makes other providers advertise codex models.
    var codexAuths []*coreauth.Auth
    if providerLower == codexProvider || providerLower == mixedProvider {
        for _, a := range auths {
            if a != nil && strings.EqualFold(strings.TrimSpace(a.Provider), codexProvider) {
                codexAuths = append(codexAuths, a)
            }
        }
    }

    if providerLower == codexProvider || (providerLower == mixedProvider && len(codexAuths) > 0) {
        // existing weighted pick logic, but operate on codexAuths instead of auths
        // ... call filterAvailable(codexAuths, ...), preferWebsocketAuths, weighted pick ...
        // If weighted pick fails, fall back to base.Pick(ctx, provider, model, opts, codexAuths)
        // — never to the original `auths` list.
    }

    return s.base.Pick(ctx, provider, model, opts, auths)
}
```

Key invariant the rewrite must preserve: **once we decide the route is Codex, every fallback path must stay within `codexAuths`, never the original `auths`**. The current code falls back to `s.base.Pick(ctx, provider, model, opts, auths)` (line 87, 94, 116, 125, 133 of `selector.go`) which can return a non-codex auth.

### Fix B — Disable session-affinity wrapping order if confirmed problematic

In `main.go`:
```go
var selector coreauth.Selector = weightedselector.New(base, wcfg)
if sessionAffinity {
    selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
        Fallback: selector,
        TTL:      sessionAffinityTTL,
    })
}
```

If session-affinity is enabled and the cache hits with a stale auth ID, `SessionAffinitySelector.Pick` (SDK `selector.go:497-511`) calls `s.fallback.Pick(...)` (which is our `WeightedSelector`). The full `auths` list is passed through, so Fix A still applies.

If `request-log: true` shows session-affinity is causing repeated hits to a misconfigured auth, also drop the SA wrapping when codex weighting is active:
```go
if !sessionAffinity || cfg.Routing.ClaudeCodeSessionAffinity {
    // SA only for claude-code, not for codex
    selector = ...
}
```

### Fix C — If outgoing body lacks codex translation entirely, surface it as a SDK bug

If Step 3 shows the outgoing body is the raw Anthropic Messages payload (model field set, but `messages` not transformed to `input`), that means the translator at `internal/translator/codex/claude/codex_claude_request.go:36` (`ConvertClaudeRequestToCodex`) was bypassed. This would be a bug in `pickNextMixedLegacy` or in `executeMixedOnce` losing the `SourceFormat` somewhere. In that case:

1. Add debug log inside the codex executor before translation:
   ```go
   // internal/runtime/executor/codex_executor.go:~159
   from := opts.SourceFormat
   to := sdktranslator.FromString("codex")
   log.Debugf("codex: translating from=%s to=%s model=%s", from, to, baseModel)
   ```
2. If `from` ends up empty or wrong, file an SDK issue and patch the legacy path locally (vendor the SDK at the plugin level).

Fix C is unlikely if the user's config-only workaround (drop `codex_weights`) restores correct behavior; selector wrapping does not affect translator selection. But document it as a fallback.

## Workaround for the user (immediate, no code change)

Until the plugin fix lands, instruct the user to comment out the `codex_weights:` block in `config.yaml`. With the block absent, `weightedselector.LoadFromYAML` returns `Enabled: false`, the plugin skips `WithCoreAuthManager(...)`, and the binary uses the SDK's default built-in selector → fast path → routing works.

The user loses weighted plan-tier routing but gains correct gpt-5.4 routing.

## Acceptance criteria

The fix is correct when, with `codex_weights:` enabled and the user's full `config.yaml`:

1. Send 50 sequential `POST /v1/messages?beta=true` with `{"model":"gpt-5.4", ...}` — all return 200 with valid streaming responses.
2. Same for `model: gpt-5.4-fast` and `model: gpt-5.4-high-fast` (these go through the legacy path because `routeAwareSelectionRequired` triggers on aliases).
3. Auth selection across requests respects `codex_weights` ratios (pro:10, plus:1, etc.) — verifiable by counting `Use OAuth provider=codex auth_file=...` lines per auth file in `main.log`.
4. No request hits a non-codex auth for gpt-5.4* model names.

## Files to modify

- `internal/weightedselector/selector.go` — primary fix (Fix A)
- `internal/weightedselector/selector_test.go` — add tests for the codex-only filtering invariant
- (optional) `main.go` — Fix B if session-affinity proves problematic

## Files NOT to modify

- Anywhere under `/Users/mac/Documents/cyberk/CLIProxyAPI` — that is read-only SDK source; the plugin imports the published module.
- The user's `config.yaml` — the plan is to fix the plugin so the existing config works.

## Hand-off notes

- Read this entire file before editing.
- Run Step 1 + Step 3 of Verification BEFORE writing Fix A — the diagnostic log output dictates whether Fix A alone is sufficient or Fix C is also needed.
- After implementing, run the existing test suite
- Add a regression test in `internal/weightedselector/selector_test.go` that constructs a mixed auth list (one codex + one non-codex) with `provider: "mixed"`, `model: "gpt-5.4"`, and asserts the selector returns the codex auth, never the non-codex one.
