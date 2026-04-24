// Builds a canonical, index-stable ordered list of API keys used by the Users
// extension pages. Config keys come first (preserving user-defined order),
// followed by orphan keys observed in usage (sorted alphabetically for
// determinism). Both UsersPage and UserDetailPage resolve `/custom/users/:index`
// against this list so deep-links never embed the raw key in the URL.

export function buildKeyList(knownKeys: readonly string[], usage: unknown): string[] {
  const result: string[] = [];
  const seen = new Set<string>();

  for (const k of knownKeys) {
    if (typeof k !== 'string' || !k) continue;
    if (seen.has(k)) continue;
    seen.add(k);
    result.push(k);
  }

  if (usage && typeof usage === 'object' && !Array.isArray(usage)) {
    const apis = (usage as { apis?: unknown }).apis;
    if (apis && typeof apis === 'object' && !Array.isArray(apis)) {
      const orphans = Object.keys(apis as Record<string, unknown>)
        .filter((k) => !seen.has(k))
        .sort();
      for (const k of orphans) {
        seen.add(k);
        result.push(k);
      }
    }
  }

  return result;
}

export function keyIndexOf(
  apiKey: string,
  knownKeys: readonly string[],
  usage: unknown
): number {
  return buildKeyList(knownKeys, usage).indexOf(apiKey);
}

export function resolveKeyByIndex(
  rawIndex: string | undefined,
  knownKeys: readonly string[],
  usage: unknown
): string | null {
  if (!rawIndex) return null;
  const n = Number(rawIndex);
  if (!Number.isInteger(n) || n < 0) return null;
  const list = buildKeyList(knownKeys, usage);
  if (n >= list.length) return null;
  return list[n];
}
