import { describe, it } from 'node:test';
import { strict as assert } from 'node:assert';
import {
  filterUsageByUsersTimeRange,
  isUsersTimeRange,
  USERS_TIME_RANGE_OPTIONS
} from '../../src/extensions/utils/timeRangeFilter.ts';

const NOW = Date.parse('2026-04-23T12:00:00Z');
const minutesAgo = (n: number) => new Date(NOW - n * 60_000).toISOString();

function makeUsage() {
  return {
    total_requests: 3,
    apis: {
      openai: {
        models: {
          'gpt-4': {
            total_requests: 3,
            success_count: 2,
            failure_count: 1,
            details: [
              { timestamp: minutesAgo(5), tokens: { total_tokens: 100 } }, // recent
              { timestamp: minutesAgo(90), tokens: { total_tokens: 200 }, failed: true }, // 1.5h
              { timestamp: minutesAgo(60 * 25), tokens: { total_tokens: 500 } } // 25h
            ]
          }
        }
      }
    }
  };
}

describe('isUsersTimeRange', () => {
  it('accepts all valid ranges', () => {
    for (const r of USERS_TIME_RANGE_OPTIONS) {
      assert.equal(isUsersTimeRange(r), true);
    }
  });
  it('rejects invalid values', () => {
    assert.equal(isUsersTimeRange(''), false);
    assert.equal(isUsersTimeRange('7h'), false); // not in our union
    assert.equal(isUsersTimeRange(null), false);
    assert.equal(isUsersTimeRange(12), false);
  });
});

describe('filterUsageByUsersTimeRange', () => {
  it('returns input unchanged for "all"', () => {
    const u = makeUsage();
    const out = filterUsageByUsersTimeRange(u, 'all', NOW);
    assert.strictEqual(out, u);
  });

  it('drops details outside the 1h window', () => {
    const out = filterUsageByUsersTimeRange(makeUsage(), '1h', NOW) as ReturnType<
      typeof makeUsage
    >;
    const details = out.apis.openai.models['gpt-4'].details;
    assert.equal(details.length, 1);
    assert.equal(out.apis.openai.models['gpt-4'].total_requests, 1);
    assert.equal(out.total_requests, 1);
  });

  it('keeps all recent details for 24h', () => {
    const out = filterUsageByUsersTimeRange(makeUsage(), '24h', NOW) as ReturnType<
      typeof makeUsage
    >;
    const model = out.apis.openai.models['gpt-4'];
    assert.equal(model.details.length, 2);
    assert.equal(model.total_requests, 2);
    assert.equal(model.success_count, 1);
    assert.equal(model.failure_count, 1);
    assert.equal(model.total_tokens, 300);
  });

  it('prunes model entirely when no details survive', () => {
    const u = {
      apis: {
        svc: {
          models: {
            oldOnly: {
              details: [{ timestamp: minutesAgo(60 * 24 * 10) }] // 10d ago
            }
          }
        }
      }
    };
    const out = filterUsageByUsersTimeRange(u, '24h', NOW) as typeof u;
    assert.deepEqual(out.apis, {});
  });

  it('skips details with invalid timestamp', () => {
    const u = {
      apis: {
        svc: {
          models: {
            m: {
              details: [
                { timestamp: 'not-a-date' },
                { timestamp: minutesAgo(10), tokens: { total_tokens: 50 } }
              ]
            }
          }
        }
      }
    };
    const out = filterUsageByUsersTimeRange(u, '1h', NOW) as typeof u;
    const m = (out.apis.svc.models.m as { details: unknown[]; total_requests: number });
    assert.equal(m.details.length, 1);
    assert.equal(m.total_requests, 1);
  });

  it('returns input unchanged when usage is not a record', () => {
    assert.equal(filterUsageByUsersTimeRange(null, '1h', NOW), null);
    assert.equal(filterUsageByUsersTimeRange(undefined, '1h', NOW), undefined);
  });

  it('tolerates clock skew (timestamp slightly ahead of nowMs)', () => {
    const slightlyFuture = new Date(NOW + 30_000).toISOString(); // 30s ahead
    const u = {
      apis: {
        svc: {
          models: {
            m: {
              details: [{ timestamp: slightlyFuture, tokens: { total_tokens: 7 } }]
            }
          }
        }
      }
    };
    const out = filterUsageByUsersTimeRange(u, '1h', NOW) as typeof u;
    const m = out.apis.svc.models.m as { details: unknown[]; total_requests: number };
    assert.equal(m.details.length, 1);
    assert.equal(m.total_requests, 1);
  });

  it('still drops far-future timestamps beyond clock-skew tolerance', () => {
    const farFuture = new Date(NOW + 10 * 60_000).toISOString(); // 10 min ahead
    const u = {
      apis: {
        svc: { models: { m: { details: [{ timestamp: farFuture }] } } }
      }
    };
    const out = filterUsageByUsersTimeRange(u, '1h', NOW) as typeof u;
    assert.deepEqual(out.apis, {});
  });
});
