/**
 * Compact label for a CWD path. Returns the last two path segments to keep
 * the UI scannable in narrow columns (e.g. `Documents/foo` instead of
 * `/Users/mac/Documents/foo`), with the full path preserved on `full` for
 * tooltips. The label is a STABLE function of the input — it does not look
 * at other CWDs for disambiguation, so loading another group later never
 * mutates an existing label.
 *
 * Sentinel handling: `(unknown)` is the reader's marker for entries whose
 * env block was missing (typical for orphan subagents); render verbatim so
 * the UI keeps signaling that state. Empty / missing CWDs map to the same
 * sentinel.
 */
export interface CwdLabel {
  short: string;
  full: string;
}

export function formatCwdLabel(cwd: string | undefined | null): CwdLabel {
  if (!cwd) return { short: '(unknown)', full: '(unknown)' };
  if (cwd === '(unknown)') return { short: cwd, full: cwd };
  const segments = cwd.split('/').filter((s) => s.length > 0);
  if (segments.length <= 2) {
    // 0 segments (e.g. "/") → display the raw input.
    // 1-2 segments → already short enough, no point trimming the leading
    // slash which carries "is absolute" signal.
    return { short: cwd, full: cwd };
  }
  return { short: segments.slice(-2).join('/'), full: cwd };
}
