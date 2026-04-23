/**
 * Network-bound helpers for the ui-aliases section. Thin wrappers over the
 * pure core in `./aliasStoreCore.ts`.
 *
 * Pure YAML logic is tested in isolation under bare Node; the helpers here
 * are covered by manual smoke testing since they only glue `configFileApi`
 * to the core.
 */

import { configFileApi } from '@/services/api';
import {
  type AliasMap,
  applyAliasToYaml,
  parseAliasesFromYaml
} from './aliasStoreCore';

export type { AliasMap };
export {
  ALIAS_YAML_KEY,
  parseAliasesFromYaml,
  applyAliasToYaml,
  replaceAliasMapInYaml
} from './aliasStoreCore';

/** Fetch config.yaml and return the ui-aliases map. */
export async function readAliases(): Promise<AliasMap> {
  const raw = await configFileApi.fetchConfigYaml();
  return parseAliasesFromYaml(raw);
}

/**
 * Set or clear a single alias.
 *
 * Always reads the latest server YAML before writing so we don't clobber
 * unrelated edits. This protects against losing concurrent ConfigPage writes
 * that already landed, but NOT against ConfigPage landing *after* us with a
 * stale merged YAML that omits our alias — that's an upstream flow limitation
 * (`ConfigPage.handleConfirmSave` uses a pre-computed `mergedYaml`). Users
 * with ConfigPage open while editing aliases in another tab may see their
 * alias overwritten when ConfigPage saves; we accept this trade-off rather
 * than patch the upstream file (would break isolation).
 */
export async function writeAlias(apiKey: string, alias: string): Promise<AliasMap> {
  const latest = await configFileApi.fetchConfigYaml();
  const next = applyAliasToYaml(latest, apiKey, alias);
  await configFileApi.saveConfigYaml(next);
  return parseAliasesFromYaml(next);
}
