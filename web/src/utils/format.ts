/**
 * Formatting helpers. All money/time rendering goes through here so the app
 * keeps one definition of each unit.
 *
 * Money uses micro-yuan (`1 元 = 1_000_000 µ¥`). Values are decimal strings
 * and are formatted with string arithmetic below, so no amount is parsed into
 * a JavaScript `number` and no precision is lost on the way to the DOM.
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

/* ------------------------------------------------------------- micro-yuan */

/** Decimal places between micro-yuan and yuan. */
const MICRO_DECIMALS = 6;

/** Group an integer digit string (`1234567` -> `1,234,567`) without Number(). */
function groupDigits(value: string): string {
  let out = '';
  for (let index = 0; index < value.length; index += 1) {
    out += value[index];
    const remaining = value.length - index - 1;
    if (remaining > 0 && remaining % 3 === 0) out += ',';
  }
  return out;
}

/**
 * Micro-yuan decimal string -> yuan decimal string, e.g. `"12500000"` ->
 * `"12.5"`, `"-4000"` -> `"-0.004"`, `"1"` -> `"0.000001"`.
 *
 * This is pure string arithmetic on purpose: `Number("9007199254740993")` would
 * silently round a ledger amount, and the T05/T06 contract keeps money on the
 * wire as a decimal string precisely so that cannot happen.
 *
 * A value that is not a plain integer string is returned verbatim: inventing a
 * number the server did not send would be worse than showing the raw field.
 */
export function formatMicroYuan(value: string | null | undefined): string {
  if (value === null || value === undefined) return '—';
  const raw = value.trim();
  if (raw === '') return '—';
  const match = /^([+-]?)([0-9]+)$/.exec(raw);
  if (!match) return raw;
  const sign = match[1] === '-' ? '-' : '';
  const digits = (match[2] ?? '').padStart(MICRO_DECIMALS + 1, '0');
  const integer = digits.slice(0, digits.length - MICRO_DECIMALS);
  const fraction = digits.slice(digits.length - MICRO_DECIMALS).replace(/0+$/, '');
  const grouped = groupDigits(integer);
  return fraction === '' ? `${sign}${grouped}` : `${sign}${grouped}.${fraction}`;
}

/** `12500000` -> `12.5 元`, keeping the exact micro string alongside it. */
export function formatMicroYuanWithUnit(value: string | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  return `${formatMicroYuan(value)} 元`;
}

/** Exact-machine rendering for tooltips and audit views: `12500000 µ¥`. */
export function formatMicroExact(value: string | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  return `${value} µ¥`;
}

/** Convert a non-negative yuan input to exact integer micro-yuan without float math. */
export function yuanToMicro(value: string): string | null {
  const match = /^([0-9]+)(?:\.([0-9]{1,6}))?$/.exec(value.trim());
  if (!match) return null;
  const integer = match[1] ?? '0';
  const fraction = (match[2] ?? '').padEnd(MICRO_DECIMALS, '0');
  const combined = `${integer}${fraction}`.replace(/^0+(?=\d)/, '');
  return combined;
}

/** Convert micro-yuan to a plain editable yuan string without grouping. */
export function microToYuanInput(value: string | null | undefined): string {
  if (value === null || value === undefined || !/^[0-9]+$/.test(value)) return '';
  const digits = value.padStart(MICRO_DECIMALS + 1, '0');
  const integer = digits.slice(0, -MICRO_DECIMALS).replace(/^0+(?=\d)/, '');
  const fraction = digits.slice(-MICRO_DECIMALS).replace(/0+$/, '');
  return fraction ? `${integer}.${fraction}` : integer;
}

/**
 * A ledger entry's signed amount. The sign is the `direction` field; the amount
 * itself is always a strictly positive string, so `formatMicroYuan` is applied
 * to the magnitude and the sign is added as text.
 */
export function formatSignedMicroYuan(
  amountMicro: string | null | undefined,
  direction: 'credit' | 'debit' | null | undefined,
): string {
  if (amountMicro === null || amountMicro === undefined || amountMicro === '') return '—';
  if (direction === 'debit') return `−${formatMicroYuan(amountMicro)}`;
  if (direction === 'credit') return `+${formatMicroYuan(amountMicro)}`;
  return formatMicroYuan(amountMicro);
}

/* --------------------------------------------------------------- months */

const MONTH_PATTERN = /^[0-9]{4}-(0[1-9]|1[0-2])$/;

/** `true` only for the exact `YYYY-MM` shape used by month filters. */
export function isBusinessMonth(value: string): boolean {
  return MONTH_PATTERN.test(value);
}

/** The business consumption month in Asia/Shanghai. */
export function beijingMonthOf(date: Date = new Date()): string {
  const parts = new Intl.DateTimeFormat('en', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
  }).formatToParts(date);
  const year = parts.find((part) => part.type === 'year')?.value;
  const month = parts.find((part) => part.type === 'month')?.value;
  return year && month ? `${year}-${month}` : date.toISOString().slice(0, 7);
}

/**
 * Shift a `YYYY-MM` string by whole months (`delta` may be negative).
 * A malformed input is returned unchanged rather than repaired silently.
 */
export function shiftBusinessMonth(month: string, delta: number): string {
  if (!isBusinessMonth(month) || !Number.isFinite(delta)) return month;
  const year = Number(month.slice(0, 4));
  const monthIndex = Number(month.slice(5, 7)) - 1;
  const shifted = new Date(Date.UTC(year, monthIndex + delta, 1));
  return shifted.toISOString().slice(0, 7);
}

/** `2026-09` -> `2026-09（北京时间）`. */
export function formatBusinessMonthLabel(month: string | null | undefined): string {
  if (!month) return '—';
  return `${month}（北京时间）`;
}

/**
 * A duration in seconds (server sends it as a decimal string) -> a coarse
 * human age. This is a duration, not money, so a Number() round-trip is fine;
 * a value that does not parse is shown verbatim.
 */
export function formatAgeSeconds(value: string | number | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  const seconds = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(seconds) || seconds < 0) return String(value);
  if (seconds < 60) return `${Math.round(seconds)} 秒`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时 ${minutes % 60} 分`;
  return `${Math.floor(hours / 24)} 天 ${hours % 24} 小时`;
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
