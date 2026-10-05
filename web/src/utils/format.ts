/**
 * Formatting helpers. All money/time rendering goes through here so the app
 * keeps one definition of each unit.
 *
 * `cost_cents` is a decimal string of *demo consumption* (the legacy demo price
 * is 1 token = 1 cent). It is not a balance and not an invoice amount; the API
 * labels it `cost_kind: demo_consumption` and this module never calls it
 * "available balance".
 */

/** RFC 3339 UTC -> `2026-10-01 12:00:00Z`. Returns the raw string if unparsable. */
export function formatTimestamp(value: string | null | undefined): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return `${date.toISOString().slice(0, 19).replace('T', ' ')}Z`;
}

/** Short local-time rendering for table cells and stale banners. */
export function formatLocalTime(value: string | number | Date | null | undefined): string {
  if (value === null || value === undefined) return '—';
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  return date.toLocaleString();
}

export function formatNumber(value: number | null | undefined): string {
  if (value === null || value === undefined) return '—';
  return value.toLocaleString('en-US');
}

/** Decimal string cents -> `1234` (integer cents, no currency symbol invented). */
export function formatCostCents(value: string | number | null | undefined): string {
  if (value === null || value === undefined) return '—';
  const text = typeof value === 'number' ? String(value) : value;
  if (text === '') return '—';
  return `${text} 分（演示）`;
}

/** `expires_in_seconds` -> the same number of whole hours, rounded up. */
export function secondsToHours(seconds: number): number {
  return Math.max(1, Math.ceil(seconds / 3600));
}

/** Human-readable relative age, used by the stale banner. */
export function formatAge(from: Date, now: Date = new Date()): string {
  const seconds = Math.max(0, Math.round((now.getTime() - from.getTime()) / 1000));
  if (seconds < 60) return `${seconds} 秒前`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.round(minutes / 60);
  return `${hours} 小时前`;
}

/** Trim a long id for a table cell while keeping it copyable/diagnosable. */
export function shortId(value: string | null | undefined, keep = 10): string {
  if (!value) return '—';
  return value.length <= keep ? value : `${value.slice(0, keep)}…`;
}

/** `{from,to}` -> `2026-09-30T12:00:00Z`. Ready for the API query. */
export function toRfc3339(date: Date): string {
  return date.toISOString();
}
