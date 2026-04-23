import { describe, it } from 'node:test';
import { strict as assert } from 'node:assert';
import {
  parseDuration,
  parseRateLimitFromYaml,
  resolveRateLimit
} from '../../src/extensions/services/ratelimitConfig.ts';

describe('parseDuration', () => {
  it('parses h/m/s', () => {
    assert.equal(parseDuration('5h'), 5 * 3_600_000);
    assert.equal(parseDuration('10m'), 10 * 60_000);
    assert.equal(parseDuration('45s'), 45_000);
    assert.equal(parseDuration('1h30m'), 90 * 60_000);
    assert.equal(parseDuration('500ms'), 500);
  });
  it('returns 0 on invalid', () => {
    assert.equal(parseDuration(''), 0);
    assert.equal(parseDuration('bad'), 0);
    assert.equal(parseDuration('5x'), 0);
    assert.equal(parseDuration(null), 0);
    assert.equal(parseDuration(undefined), 0);
  });
});

const FULL = `
port: 8317
api-keys:
  - alice-key
  - bob-key

ratelimit:
  window: 5h
  requests: 500
  models:
    gpt-5.4:
      window: 2h
      requests: 100
      keys:
        alice-key: 50
    "gpt-5.4-*":
      requests: 300
    "claude-*-sonnet-*":
      window: 10m
      requests: 60
    gemini-2.5-pro:
      requests: 200
`;

describe('parseRateLimitFromYaml', () => {
  it('returns empty for missing section', () => {
    const cfg = parseRateLimitFromYaml('port: 8317\n');
    assert.equal(cfg.default, null);
    assert.deepEqual(cfg.models, {});
    assert.deepEqual(cfg.keyOverrides, {});
  });

  it('parses default + models + key overrides', () => {
    const cfg = parseRateLimitFromYaml(FULL);
    assert.equal(cfg.default?.requests, 500);
    assert.equal(cfg.default?.windowMs, 5 * 3_600_000);
    assert.equal(cfg.models['gpt-5.4']?.requests, 100);
    assert.equal(cfg.models['gpt-5.4']?.windowMs, 2 * 3_600_000);
    assert.equal(cfg.models['gpt-5.4-*']?.requests, 300);
    assert.equal(cfg.models['gpt-5.4-*']?.windowMs, 0);
    assert.deepEqual(cfg.keyOverrides['gpt-5.4'], { 'alice-key': 50 });
  });

  it('ignores malformed yaml', () => {
    assert.equal(parseRateLimitFromYaml(':::').default, null);
  });

  it('ignores non-positive limits', () => {
    const cfg = parseRateLimitFromYaml(`ratelimit:
  window: 5h
  requests: -10
  models:
    m1:
      requests: 0
    m2:
      requests: 50
      keys:
        k1: -1
`);
    assert.equal(cfg.default, null);
    assert.equal(cfg.models['m1'], null);
    assert.equal(cfg.models['m2']?.requests, 50);
    assert.equal(cfg.keyOverrides['m2'], undefined);
  });
});

describe('resolveRateLimit', () => {
  const cfg = parseRateLimitFromYaml(FULL);

  it('exact model + exact key override wins', () => {
    const rule = resolveRateLimit(cfg, 'alice-key', 'gpt-5.4');
    assert.equal(rule?.requests, 50);
    assert.equal(rule?.windowMs, 2 * 3_600_000);
  });

  it('exact model default applies when no key override', () => {
    const rule = resolveRateLimit(cfg, 'bob-key', 'gpt-5.4');
    assert.equal(rule?.requests, 100);
  });

  it('most-specific wildcard wins over broader', () => {
    const c = parseRateLimitFromYaml(`ratelimit:
  window: 5h
  requests: 500
  models:
    "gpt-*":
      requests: 400
    "gpt-5.4-*":
      requests: 300
`);
    const rule = resolveRateLimit(c, 'anyone', 'gpt-5.4-preview');
    assert.equal(rule?.requests, 300);
  });

  it('wildcard fallback', () => {
    const rule = resolveRateLimit(cfg, 'anyone', 'gpt-5.4-preview');
    assert.equal(rule?.requests, 300);
  });

  it('unrelated model falls back to default', () => {
    const rule = resolveRateLimit(cfg, 'anyone', 'unknown-model');
    assert.equal(rule?.requests, 500);
  });

  it('returns null when nothing matches and no default', () => {
    const c = parseRateLimitFromYaml(`ratelimit:
  models:
    gpt-5:
      requests: 100
`);
    assert.equal(resolveRateLimit(c, 'k', 'other'), null);
  });

  it('double-wildcard pattern works', () => {
    const rule = resolveRateLimit(cfg, 'k', 'claude-3.5-sonnet-20241022');
    assert.equal(rule?.requests, 60);
    assert.equal(rule?.windowMs, 10 * 60_000);
  });

  it('exact key override on a wildcard model inherits that model window', () => {
    const c = parseRateLimitFromYaml(`ratelimit:
  window: 5h
  requests: 500
  models:
    "gpt-*":
      window: 1h
      requests: 400
      keys:
        k1: 10
`);
    const rule = resolveRateLimit(c, 'k1', 'gpt-5.4');
    assert.equal(rule?.requests, 10);
    assert.equal(rule?.windowMs, 60 * 60_000);
  });

  it('matches mixed-case model names (plugin lowercases internally)', () => {
    const c = parseRateLimitFromYaml(`ratelimit:
  window: 5h
  requests: 500
  models:
    GPT-5.4:
      requests: 100
`);
    assert.equal(resolveRateLimit(c, 'k', 'gpt-5.4')?.requests, 100);
    assert.equal(resolveRateLimit(c, 'k', 'GPT-5.4')?.requests, 100);
    assert.equal(resolveRateLimit(c, 'k', 'Gpt-5.4')?.requests, 100);
  });

  it('trims whitespace from model input', () => {
    const rule = resolveRateLimit(cfg, 'anyone', '  gpt-5.4  ');
    assert.equal(rule?.requests, 100);
  });

  it('wildcard rule with no window inherits windowMs from default', () => {
    const rule = resolveRateLimit(cfg, 'anyone', 'gpt-5.4-preview');
    assert.equal(rule?.windowMs, 5 * 3_600_000);
  });

  it('charclass wildcard matches', () => {
    const c = parseRateLimitFromYaml(`ratelimit:
  window: 5h
  requests: 500
  models:
    "gpt-[45]*":
      requests: 200
`);
    assert.equal(resolveRateLimit(c, 'k', 'gpt-4o')?.requests, 200);
    assert.equal(resolveRateLimit(c, 'k', 'gpt-5-turbo')?.requests, 200);
    assert.equal(resolveRateLimit(c, 'k', 'gpt-3-something')?.requests, 500);
  });
});
