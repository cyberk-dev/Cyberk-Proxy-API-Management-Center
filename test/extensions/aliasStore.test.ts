import { describe, it } from 'node:test';
import { strict as assert } from 'node:assert';
import {
  parseAliasesFromYaml,
  applyAliasToYaml,
  replaceAliasMapInYaml
} from '../../src/extensions/services/aliasStoreCore.ts';

const BASE = `# top-level config comment
port: 8317
api-keys:
  # alice is the admin
  - sk-alice-xxx
  - sk-bob-yyy

# ratelimit plugin
ratelimit:
  window: 5h
  requests: 500
`;

describe('parseAliasesFromYaml', () => {
  it('returns {} when ui-aliases missing', () => {
    assert.deepEqual(parseAliasesFromYaml(BASE), {});
  });

  it('returns {} for empty input', () => {
    assert.deepEqual(parseAliasesFromYaml(''), {});
  });

  it('returns {} for invalid yaml', () => {
    assert.deepEqual(parseAliasesFromYaml('::: :::'), {});
  });

  it('parses an existing ui-aliases map', () => {
    const yaml =
      BASE +
      `ui-aliases:
  sk-alice-xxx: Alice laptop
  sk-bob-yyy: "Bob iOS"
`;
    assert.deepEqual(parseAliasesFromYaml(yaml), {
      'sk-alice-xxx': 'Alice laptop',
      'sk-bob-yyy': 'Bob iOS'
    });
  });

  it('skips empty / whitespace-only alias values', () => {
    const yaml =
      BASE +
      `ui-aliases:
  sk-alice-xxx: ""
  sk-bob-yyy: "   "
  sk-carol-zzz: Carol
`;
    assert.deepEqual(parseAliasesFromYaml(yaml), { 'sk-carol-zzz': 'Carol' });
  });

  it('trims alias values', () => {
    const yaml = `ui-aliases:\n  sk-x: "  padded  "\n`;
    assert.deepEqual(parseAliasesFromYaml(yaml), { 'sk-x': 'padded' });
  });
});

describe('applyAliasToYaml', () => {
  it('adds ui-aliases section when absent and preserves upstream comments', () => {
    const next = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    assert.ok(next.includes('# top-level config comment'));
    assert.ok(next.includes('# alice is the admin'));
    assert.ok(next.includes('# ratelimit plugin'));
    assert.ok(next.includes('ui-aliases:'));
    assert.deepEqual(parseAliasesFromYaml(next), { 'sk-alice-xxx': 'Alice' });
  });

  it('updates an existing alias in place', () => {
    const seeded = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    const updated = applyAliasToYaml(seeded, 'sk-alice-xxx', 'Alice v2');
    assert.deepEqual(parseAliasesFromYaml(updated), { 'sk-alice-xxx': 'Alice v2' });
    assert.ok(updated.includes('# top-level config comment'));
  });

  it('adds a second alias without disturbing the first', () => {
    const step1 = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    const step2 = applyAliasToYaml(step1, 'sk-bob-yyy', 'Bob');
    assert.deepEqual(parseAliasesFromYaml(step2), {
      'sk-alice-xxx': 'Alice',
      'sk-bob-yyy': 'Bob'
    });
  });

  it('empty alias deletes the entry', () => {
    const step1 = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    const step2 = applyAliasToYaml(step1, 'sk-bob-yyy', 'Bob');
    const step3 = applyAliasToYaml(step2, 'sk-alice-xxx', '');
    assert.deepEqual(parseAliasesFromYaml(step3), { 'sk-bob-yyy': 'Bob' });
  });

  it('whitespace-only alias deletes the entry', () => {
    const step1 = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    const step2 = applyAliasToYaml(step1, 'sk-alice-xxx', '   ');
    assert.deepEqual(parseAliasesFromYaml(step2), {});
  });

  it('removes the ui-aliases section entirely when last entry is deleted', () => {
    const step1 = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    const step2 = applyAliasToYaml(step1, 'sk-alice-xxx', '');
    assert.ok(!step2.includes('ui-aliases:'));
  });

  it('trims alias value on write', () => {
    const next = applyAliasToYaml(BASE, 'sk-alice-xxx', '  Alice  ');
    assert.deepEqual(parseAliasesFromYaml(next), { 'sk-alice-xxx': 'Alice' });
  });

  it('preserves unrelated top-level keys and order', () => {
    const next = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    assert.ok(next.indexOf('port:') < next.indexOf('api-keys:'));
    assert.ok(next.indexOf('api-keys:') < next.indexOf('ratelimit:'));
    assert.match(next, /port:\s*8317/);
  });

  it('throws on invalid yaml input', () => {
    assert.throws(() => applyAliasToYaml('::: :::', 'k', 'v'), /parse error/);
  });

  it('handles input with only whitespace as empty doc', () => {
    const next = applyAliasToYaml('   \n\n', 'sk-x', 'X');
    assert.deepEqual(parseAliasesFromYaml(next), { 'sk-x': 'X' });
  });

  it('handles keys containing special characters (dots, colons, dashes)', () => {
    const key = 'sk-proj-abc.123:xyz';
    const next = applyAliasToYaml(BASE, key, 'special');
    assert.deepEqual(parseAliasesFromYaml(next), { [key]: 'special' });
  });
});

describe('replaceAliasMapInYaml', () => {
  it('writes a fresh map, discarding previous entries', () => {
    const seeded = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    const next = replaceAliasMapInYaml(seeded, { 'sk-bob-yyy': 'Bob' });
    assert.deepEqual(parseAliasesFromYaml(next), { 'sk-bob-yyy': 'Bob' });
  });

  it('empty map drops the section', () => {
    const seeded = applyAliasToYaml(BASE, 'sk-alice-xxx', 'Alice');
    const next = replaceAliasMapInYaml(seeded, {});
    assert.ok(!next.includes('ui-aliases:'));
  });

  it('skips empty / whitespace values', () => {
    const next = replaceAliasMapInYaml(BASE, {
      'sk-a': 'A',
      'sk-b': '',
      'sk-c': '   '
    });
    assert.deepEqual(parseAliasesFromYaml(next), { 'sk-a': 'A' });
  });
});
