import { parseDocument, isMap, isScalar } from 'yaml';

export interface RateLimitRule {
  window: string; // e.g. "5h", "10m"
  windowMs: number;
  requests: number;
}

export interface RateLimitConfig {
  default: RateLimitRule | null;
  // model → rule
  models: Record<string, RateLimitRule | null>;
  // model → { apiKey → per-key override requests (window inherits from model-or-default) }
  keyOverrides: Record<string, Record<string, number>>;
}

/** Parse Go-style duration (1h30m / 500ms / 5h / 10m / 45s). Returns 0 on error. */
export function parseDuration(s: unknown): number {
  if (typeof s !== 'string' || !s) return 0;
  const str = s.trim();
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  let total = 0;
  let matched = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(str)) !== null) {
    matched += m[0].length;
    const n = parseFloat(m[1]);
    const unit = m[2];
    switch (unit) {
      case 'ns': total += n / 1e6; break;
      case 'us':
      case 'µs': total += n / 1000; break;
      case 'ms': total += n; break;
      case 's': total += n * 1000; break;
      case 'm': total += n * 60_000; break;
      case 'h': total += n * 3_600_000; break;
    }
  }
  if (matched !== str.length) return 0;
  return total;
}

function readRule(node: unknown): RateLimitRule | null {
  if (!node || typeof node !== 'object') return null;
  const obj = node as Record<string, unknown>;
  const windowStr = typeof obj.window === 'string' ? obj.window : '';
  const requests =
    typeof obj.requests === 'number'
      ? obj.requests
      : parseInt(String(obj.requests ?? ''), 10);
  if (!Number.isFinite(requests) || requests <= 0) return null;
  const windowMs = parseDuration(windowStr);
  return {
    window: windowStr,
    windowMs,
    requests
  };
}

/**
 * Parse the ratelimit: section out of a full config.yaml string.
 * Exported for unit tests. Returns empty config on missing / malformed.
 *
 * Model keys are lowercased to match the Go plugin which normalizes model
 * names at build-time (`internal/ratelimit/config.go`).
 */
export function parseRateLimitFromYaml(yamlText: string): RateLimitConfig {
  const empty: RateLimitConfig = { default: null, models: {}, keyOverrides: {} };
  if (!yamlText) return empty;
  const doc = parseDocument(yamlText);
  if (doc.errors.length > 0) return empty;
  const rlNode = doc.get('ratelimit');
  if (!isMap(rlNode)) return empty;
  const js = (rlNode as { toJSON: () => unknown }).toJSON() as Record<string, unknown>;
  const result: RateLimitConfig = {
    default: readRule(js),
    models: {},
    keyOverrides: {}
  };
  const modelsRaw = js.models;
  if (modelsRaw && typeof modelsRaw === 'object' && !Array.isArray(modelsRaw)) {
    for (const [model, cfg] of Object.entries(modelsRaw as Record<string, unknown>)) {
      if (!cfg || typeof cfg !== 'object') continue;
      const cfgObj = cfg as Record<string, unknown>;
      const modelKey = model.toLowerCase();
      result.models[modelKey] = readRule(cfgObj);
      const keys = cfgObj.keys;
      if (keys && typeof keys === 'object' && !Array.isArray(keys)) {
        const overrides: Record<string, number> = {};
        for (const [k, v] of Object.entries(keys as Record<string, unknown>)) {
          const n = typeof v === 'number' ? v : parseInt(String(v), 10);
          if (Number.isFinite(n) && n > 0) overrides[k] = n;
        }
        if (Object.keys(overrides).length > 0) result.keyOverrides[modelKey] = overrides;
      }
    }
  }
  // Silence unused import warning for isScalar in some linters.
  void isScalar;
  return result;
}

/** Convert a glob pattern from the plugin (`*`, `?`, `[abc]`) to a RegExp. */
function globToRegex(pattern: string): RegExp {
  let re = '^';
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i];
    if (c === '*') re += '[^/]*';
    else if (c === '?') re += '[^/]';
    else if (c === '[') {
      const end = pattern.indexOf(']', i);
      if (end === -1) {
        re += '\\[';
      } else {
        re += pattern.slice(i, end + 1);
        i = end;
      }
    } else if ('.+^$()|{}\\'.includes(c)) {
      re += '\\' + c;
    } else {
      re += c;
    }
  }
  re += '$';
  return new RegExp(re);
}

function literalCount(pattern: string): number {
  let n = 0;
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i];
    if (c === '*' || c === '?') continue;
    if (c === '[') {
      const end = pattern.indexOf(']', i);
      if (end !== -1) {
        i = end;
        continue;
      }
    }
    n++;
  }
  return n;
}

/**
 * Resolve the effective rate-limit rule for a (apiKey, model) pair.
 * Mirrors the plugin's precedence: exact-model+key → wildcard+key → exact-model →
 * wildcard → default. Most-specific wildcard wins (literal char count).
 *
 * Returns null if no limit applies.
 */
export function resolveRateLimit(
  cfg: RateLimitConfig,
  apiKey: string,
  model: string
): RateLimitRule | null {
  // Normalize model input to match how the plugin's extract.go lowercases
  // incoming requests (`internal/ratelimit/extract.go`).
  const normModel = model.trim().toLowerCase();
  const fillWindow = (rule: RateLimitRule | null): RateLimitRule | null => {
    if (!rule) return null;
    if (rule.windowMs > 0) return rule;
    const win = cfg.default?.windowMs ?? 0;
    return win > 0
      ? { ...rule, window: rule.window || cfg.default?.window || '', windowMs: win }
      : rule;
  };

  const exactOverride = cfg.keyOverrides[normModel]?.[apiKey];
  if (exactOverride && exactOverride > 0) {
    const base = cfg.models[normModel] ?? cfg.default;
    return fillWindow({
      window: base?.window ?? '',
      windowMs: base?.windowMs ?? 0,
      requests: exactOverride
    });
  }

  // Collect wildcard model patterns that match.
  const matches: Array<{ pattern: string; rule: RateLimitRule | null; override?: number }> = [];
  for (const pattern of Object.keys(cfg.models)) {
    if (pattern === normModel) continue; // exact handled above
    if (!/[*?[]/.test(pattern)) continue;
    const re = globToRegex(pattern);
    if (re.test(normModel)) {
      matches.push({
        pattern,
        rule: cfg.models[pattern],
        override: cfg.keyOverrides[pattern]?.[apiKey]
      });
    }
  }
  matches.sort((a, b) => literalCount(b.pattern) - literalCount(a.pattern));

  for (const m of matches) {
    if (m.override && m.override > 0) {
      const base = m.rule ?? cfg.default;
      return fillWindow({
        window: base?.window ?? '',
        windowMs: base?.windowMs ?? 0,
        requests: m.override
      });
    }
  }

  const exact = cfg.models[normModel];
  if (exact) return fillWindow(exact);

  for (const m of matches) {
    if (m.rule) return fillWindow(m.rule);
  }

  return cfg.default;
}
