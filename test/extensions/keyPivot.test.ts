import { describe, it } from 'node:test';
import { strict as assert } from 'node:assert';
import {
  pivotByKey,
  successRate,
  makeCostFn,
  type ModelPrice
} from '../../src/extensions/utils/keyPivot.ts';

const mkDetail = (
  ts: string,
  input: number,
  output: number,
  failed = false,
  extras: Record<string, unknown> = {}
) => ({
  timestamp: ts,
  source: 'openai',
  auth_index: 0,
  tokens: {
    input_tokens: input,
    output_tokens: output,
    total_tokens: input + output,
    reasoning_tokens: 0,
    cached_tokens: 0
  },
  failed,
  ...extras
});

const BASE_USAGE = {
  apis: {
    'sk-alice': {
      models: {
        'gpt-4o': {
          details: [
            mkDetail('2026-04-20T10:00:00Z', 100, 50),
            mkDetail('2026-04-20T11:00:00Z', 200, 80),
            mkDetail('2026-04-20T12:00:00Z', 10, 0, true)
          ]
        },
        'claude-sonnet': {
          details: [mkDetail('2026-04-20T09:00:00Z', 300, 100)]
        }
      }
    },
    'sk-bob': {
      models: {
        'gpt-4o': {
          details: [mkDetail('2026-04-19T10:00:00Z', 50, 20)]
        }
      }
    },
    'sk-orphan': {
      models: {
        'gpt-4o': {
          details: [mkDetail('2026-04-18T10:00:00Z', 10, 10)]
        }
      }
    }
  }
};

const PRICES: Record<string, ModelPrice> = {
  'gpt-4o': { prompt: 5, completion: 15, cache: 0 },
  'claude-sonnet': { prompt: 3, completion: 10, cache: 0 }
};

const costFn = makeCostFn(PRICES);
const noCost = makeCostFn({});

const closeTo = (actual: number, expected: number, epsilon = 1e-6) => {
  assert.ok(Math.abs(actual - expected) < epsilon, `expected ~${expected}, got ${actual}`);
};

describe('pivotByKey', () => {
  it('returns [] for non-object input', () => {
    assert.deepEqual(pivotByKey(null, [], {}, noCost), []);
    assert.deepEqual(pivotByKey('foo', [], {}, noCost), []);
    assert.deepEqual(pivotByKey({}, [], {}, noCost), []);
    assert.deepEqual(pivotByKey({ apis: 'bad' }, [], {}, noCost), []);
  });

  it('aggregates totals per key', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice', 'sk-bob'], {}, costFn);
    assert.equal(result.length, 3);
    const alice = result.find((r) => r.apiKey === 'sk-alice')!;
    assert.equal(alice.totalRequests, 4);
    assert.equal(alice.successCount, 3);
    assert.equal(alice.failureCount, 1);
    assert.equal(alice.inputTokens, 100 + 200 + 10 + 300);
    assert.equal(alice.outputTokens, 50 + 80 + 0 + 100);
  });

  it('splits per-model stats and sorts by request volume desc', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice'], {}, costFn);
    const alice = result.find((r) => r.apiKey === 'sk-alice')!;
    assert.deepEqual(
      alice.perModel.map((m) => m.model),
      ['gpt-4o', 'claude-sonnet']
    );
    assert.equal(alice.perModel[0].requests, 3);
    assert.equal(alice.perModel[1].requests, 1);
  });

  it('attaches alias when provided', () => {
    const result = pivotByKey(
      BASE_USAGE,
      ['sk-alice', 'sk-bob'],
      { 'sk-alice': 'Alice laptop' },
      costFn
    );
    assert.equal(result.find((r) => r.apiKey === 'sk-alice')!.alias, 'Alice laptop');
    assert.equal(result.find((r) => r.apiKey === 'sk-bob')!.alias, undefined);
  });

  it('marks keys not in knownApiKeys as orphan', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice', 'sk-bob'], {}, costFn);
    assert.equal(result.find((r) => r.apiKey === 'sk-alice')!.orphan, false);
    assert.equal(result.find((r) => r.apiKey === 'sk-bob')!.orphan, false);
    assert.equal(result.find((r) => r.apiKey === 'sk-orphan')!.orphan, true);
  });

  it('sorts orphans to the bottom', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice', 'sk-bob'], {}, costFn);
    assert.equal(result[result.length - 1].apiKey, 'sk-orphan');
  });

  it('sorts non-orphans by totalRequests desc', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice', 'sk-bob'], {}, costFn);
    assert.equal(result[0].apiKey, 'sk-alice');
    assert.equal(result[1].apiKey, 'sk-bob');
  });

  it('computes cost using provided prices', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice'], {}, costFn);
    const alice = result.find((r) => r.apiKey === 'sk-alice')!;
    // gpt-4o: (100+200+10)*5/1M + (50+80+0)*15/1M = 0.00155 + 0.00195 = 0.0035
    // claude-sonnet: 300*3/1M + 100*10/1M = 0.0009 + 0.001 = 0.0019
    closeTo(alice.totalCost, 0.0035 + 0.0019);
  });

  it('tracks lastActiveMs as latest detail timestamp', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice'], {}, costFn);
    const alice = result.find((r) => r.apiKey === 'sk-alice')!;
    assert.equal(alice.lastActiveMs, Date.parse('2026-04-20T12:00:00Z'));
  });

  it('handles missing / malformed fields gracefully', () => {
    const usage = {
      apis: {
        'sk-x': {
          models: {
            m1: {
              details: [
                { timestamp: 'not-a-date', tokens: null, failed: false },
                { timestamp: '2026-04-20T00:00:00Z', tokens: { input_tokens: 'bad' } },
                null,
                'junk'
              ]
            }
          }
        },
        'sk-y': { models: 'not-a-map' },
        'sk-z': 'junk'
      }
    };
    const result = pivotByKey(usage, ['sk-x', 'sk-y', 'sk-z'], {}, noCost);
    assert.equal(result.find((r) => r.apiKey === 'sk-z'), undefined);
    const x = result.find((r) => r.apiKey === 'sk-x')!;
    assert.equal(x.totalRequests, 2);
    assert.equal(x.inputTokens, 0);
    assert.equal(result.find((r) => r.apiKey === 'sk-y')!.totalRequests, 0);
  });

  it('skips models with zero records in perModel output', () => {
    const usage = {
      apis: {
        'sk-x': {
          models: {
            good: { details: [mkDetail('2026-04-20T10:00:00Z', 10, 10)] },
            empty: { details: [] }
          }
        }
      }
    };
    const result = pivotByKey(usage, ['sk-x'], {}, noCost);
    assert.equal(result[0].perModel.length, 1);
    assert.equal(result[0].perModel[0].model, 'good');
  });

  it('counts failed detail correctly', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice'], {}, noCost);
    const alice = result.find((r) => r.apiKey === 'sk-alice')!;
    assert.equal(alice.failureCount, 1);
    assert.equal(alice.successCount, 3);
  });
});

describe('makeCostFn', () => {
  it('returns 0 when model not priced', () => {
    const fn = makeCostFn({});
    assert.equal(fn('any', { input_tokens: 1000, output_tokens: 1000 }), 0);
  });

  it('deducts cached from prompt cost', () => {
    const fn = makeCostFn({ m: { prompt: 10, completion: 20, cache: 1 } });
    // 1000 input, 400 cached → prompt=600*10/1M, cache=400*1/1M, completion=200*20/1M
    const c = fn('m', { input_tokens: 1000, output_tokens: 200, cached_tokens: 400 });
    closeTo(c, 0.006 + 0.0004 + 0.004);
  });

  it('takes the max of cached_tokens and cache_tokens', () => {
    const fn = makeCostFn({ m: { prompt: 10, completion: 0, cache: 1 } });
    const c = fn('m', { input_tokens: 1000, output_tokens: 0, cached_tokens: 100, cache_tokens: 300 });
    // cached=300, prompt=700*10/1M=0.007, cache=300*1/1M=0.0003
    closeTo(c, 0.0073);
  });
});

describe('successRate', () => {
  it('returns 0 when no requests', () => {
    const empty = pivotByKey({ apis: { 'sk-x': { models: {} } } }, ['sk-x'], {}, noCost);
    assert.equal(successRate(empty[0]), 0);
  });

  it('returns 100 when all success', () => {
    const usage = {
      apis: {
        'sk-x': {
          models: {
            m: { details: [mkDetail('2026-04-20T10:00:00Z', 10, 10)] }
          }
        }
      }
    };
    const result = pivotByKey(usage, ['sk-x'], {}, noCost);
    assert.equal(successRate(result[0]), 100);
  });

  it('computes fractional rate', () => {
    const result = pivotByKey(BASE_USAGE, ['sk-alice'], {}, noCost);
    const alice = result.find((r) => r.apiKey === 'sk-alice')!;
    closeTo(successRate(alice), 75, 1e-5);
  });
});
