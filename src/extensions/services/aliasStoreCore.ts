/**
 * Pure YAML manipulation for the ui-aliases section. No network, no project
 * aliases — safe to import under bare Node (`node --test`) for zero-dep
 * unit testing.
 */

import { parseDocument, YAMLMap, isMap, isScalar } from 'yaml';

export type AliasMap = Record<string, string>;

export const ALIAS_YAML_KEY = 'ui-aliases';

export function parseAliasesFromYaml(yamlText: string): AliasMap {
  if (!yamlText) return {};
  const doc = parseDocument(yamlText);
  if (doc.errors.length > 0) return {};
  const node = doc.get(ALIAS_YAML_KEY);
  if (!isMap(node)) return {};
  const out: AliasMap = {};
  for (const item of (node as YAMLMap).items) {
    const rawKey = isScalar(item.key)
      ? (item.key as { value?: unknown }).value
      : item.key;
    const rawVal = isScalar(item.value)
      ? (item.value as { value?: unknown }).value
      : item.value;
    if (rawKey == null) continue;
    const k = String(rawKey);
    if (!k) continue;
    if (rawVal == null) continue;
    const v = String(rawVal).trim();
    if (!v) continue;
    out[k] = v;
  }
  return out;
}

export function applyAliasToYaml(
  yamlText: string,
  apiKey: string,
  alias: string
): string {
  const doc = parseDocument(yamlText || '');
  if (doc.errors.length > 0) {
    throw new Error(`config.yaml parse error: ${doc.errors[0].message}`);
  }
  let node = doc.get(ALIAS_YAML_KEY);
  if (!isMap(node)) {
    node = new YAMLMap();
    doc.set(ALIAS_YAML_KEY, node);
  }
  const trimmed = alias.trim();
  const mapNode = node as YAMLMap;
  if (trimmed) {
    mapNode.set(apiKey, trimmed);
  } else {
    mapNode.delete(apiKey);
    if (mapNode.items.length === 0) {
      doc.delete(ALIAS_YAML_KEY);
    }
  }
  return doc.toString({ lineWidth: 120 });
}

export function replaceAliasMapInYaml(yamlText: string, aliases: AliasMap): string {
  const doc = parseDocument(yamlText || '');
  if (doc.errors.length > 0) {
    throw new Error(`config.yaml parse error: ${doc.errors[0].message}`);
  }
  const keys = Object.keys(aliases).filter((k) => aliases[k] && aliases[k].trim());
  if (keys.length === 0) {
    doc.delete(ALIAS_YAML_KEY);
    return doc.toString({ lineWidth: 120 });
  }
  const mapNode = new YAMLMap();
  for (const k of keys) {
    mapNode.set(k, aliases[k].trim());
  }
  doc.set(ALIAS_YAML_KEY, mapNode);
  return doc.toString({ lineWidth: 120 });
}
