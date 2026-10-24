import { describe, expect, it } from 'vitest';
import { captureReadingPoint, readingCorrection } from './reading-position';

const rows = [{ id: 'previous', top: -300, bottom: -50 }, { id: 'reading', top: -50, bottom: 450 }, { id: 'next', top: 450, bottom: 900 }];
describe('reading position across layout changes', () => {
  it('keeps the same visible item and offset after insertion, removal or resizing above it', () => {
    const point = captureReadingPoint(rows, 80, 800, 1000)!;
    expect(point.anchor).toBe('reading');
    for (const change of [0, 300, -250]) {
      expect(readingCorrection(point, rows.map(row => ({ ...row, top: row.top + change, bottom: row.bottom + change })), 80)).toBe(change);
    }
  });
  it('uses the nearest surviving next item if the item being read is removed', () => {
    const point = captureReadingPoint(rows, 80, 800, 1000)!;
    expect(readingCorrection(point, [rows[0], { ...rows[2], top: -50 }], 80)).toBe(-130);
    expect(readingCorrection(point, [rows[0]], 80)).toBe(-380);
    expect(readingCorrection(point, [], 80)).toBeUndefined();
  });
  it('does not anchor the top of the page or an unpopulated loading shell', () => {
    expect(captureReadingPoint(rows, 80, 800, 0)).toBeUndefined();
    expect(captureReadingPoint([], 80, 800, 1000)).toBeUndefined();
    const nested = [...rows, { id: 'visible-channel', top: 50, bottom: 450 }];
    expect(captureReadingPoint(nested, 80, 800, 1000)?.anchor).toBe('visible-channel');
  });
});
