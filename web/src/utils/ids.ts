/**
 * Identifiers the server stores (idempotency keys, operation ids) must come from
 * a cryptographically secure source. Math.random() is neither unique nor
 * unpredictable enough for them.
 *
 * crypto.randomUUID() exists only in secure contexts (HTTPS or localhost). On a
 * plain-HTTP origin the old fallback was Math.random(), so this helper builds the
 * same RFC 4122 version-4 format from crypto.getRandomValues(), which every
 * origin has. The format is unchanged, so the server sees the same shape.
 */
export function newUUID(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | 0x40; // version 4
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80; // RFC 4122 variant
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
