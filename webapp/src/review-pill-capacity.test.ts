import { describe, expect, it } from 'vitest';
import { reviewPillCapacity } from './review-pill-capacity';

describe('review preview capacity', () => {
  it('fills a single desktop row with however many whole pills fit', () => {
    for (const [width, count] of [[75,1], [76,1], [156,1], [157,2], [480,5], [481,6], [561,6], [562,7], [643,8], [1000,12], [2000,24]]) {
      expect(reviewPillCapacity(width, false)).toBe(count);
      if (width >= 76) expect(count * 76 + (count - 1) * 5).toBeLessThanOrEqual(width);
    }
  });
  it('preserves the six-item mobile grid regardless of lane width', () => {
    for (const width of [240, 320, 390, 600]) expect(reviewPillCapacity(width, true)).toBe(6);
  });
});
