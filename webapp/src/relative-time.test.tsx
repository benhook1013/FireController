import { describe, expect, it } from 'vitest';
import { relativeAge } from './relative-time';
const now = Date.parse('2026-10-19T08:00:00Z');
describe('relative review ages', () => {
  it('distinguishes unknown, future, recent and older dates', () => {
    expect(relativeAge('', now)).toBe('Date unknown');
    expect(relativeAge('2026-10-19T08:01:00Z', now)).toBe('Future date');
    expect(relativeAge('2026-10-19T08:00:00.001Z', now)).toBe('Just now');
    expect(relativeAge('2026-10-19T08:00:59Z', now)).toBe('Just now');
    expect(relativeAge('2026-10-19T07:59:40Z', now)).toBe('Just now');
    expect(relativeAge('2026-10-19T07:57:00Z', now)).toBe('3m ago');
    expect(relativeAge('2026-10-19T06:30:00Z', now)).toBe('1h 30m ago');
    expect(relativeAge('2026-10-17T07:00:00Z', now)).toBe('2d 1h ago');
  });
});
