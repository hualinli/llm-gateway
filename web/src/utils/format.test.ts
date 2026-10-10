import { describe, expect, it } from 'vitest';
import { formatTimestamp } from './format';
describe('business timestamp display', () => {
  it('uses Beijing time across a UTC month boundary', () => {
    expect(formatTimestamp('2026-09-30T16:00:00Z')).toBe('2026-10-01 00:00:00（北京时间）');
  });
  it('normalizes timezone offsets to the same business time', () => {
    expect(formatTimestamp('2026-10-07T00:00:00+08:00')).toBe('2026-10-07 00:00:00（北京时间）');
  });
  it('does not replace an invalid timestamp with a fabricated date', () => {
    expect(formatTimestamp('unknown')).toBe('unknown');
    expect(formatTimestamp(null)).toBe('—');
  });
});
