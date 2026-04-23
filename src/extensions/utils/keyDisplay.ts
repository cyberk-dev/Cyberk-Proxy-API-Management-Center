/**
 * Shared helpers for rendering API keys in the extensions UI.
 * Pure functions — unit-testable if needed.
 */

export function maskKey(key: string): string {
  if (!key) return '';
  if (key.length <= 12) return '•'.repeat(key.length);
  return `${key.slice(0, 4)}…${key.slice(-4)}`;
}

export function formatNumber(n: number): string {
  if (!Number.isFinite(n)) return '0';
  return n.toLocaleString(undefined, { maximumFractionDigits: 0 });
}

export function formatCost(cost: number): string {
  if (!Number.isFinite(cost) || cost <= 0) return '$0.00';
  if (cost < 0.01) return `<$0.01`;
  return `$${cost.toFixed(2)}`;
}

export function formatLastActive(ms: number): string {
  if (!ms || !Number.isFinite(ms)) return '—';
  const d = new Date(ms);
  return d.toLocaleString();
}

export function formatLatency(ms: number | undefined): string {
  if (ms == null || !Number.isFinite(ms)) return '—';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}
