import { describe, expect, it } from 'vitest';
import { recentMergedDisplayOrder, recentMergedDisplayDistances } from './review-merged-order';
import type { ReviewPullSummary } from './types';
const pull = (number: number, state = 'merged', merged_at?: string) => ({ number, state, merged_at, queue_position: number }) as ReviewPullSummary;
describe('recent merged display order', () => {
  it('places the newest merge beside the open head without mutating stored order or positions', () => {
    const items = [pull(88, 'merged', '2026-11-14T05:36:48Z'), pull(77, 'merged', '2026-11-13T05:00:00Z'), pull(89, 'merged', '2026-11-12T05:00:00Z'), pull(78, 'open')];
    const shown = recentMergedDisplayOrder(items);
    expect(shown.map(p => p.number)).toEqual([89, 77, 88, 78]);
    expect(items.map(p => p.number)).toEqual([88, 77, 89, 78]);
    expect(shown[2]).toBe(items[0]);
    expect(shown[2].queue_position).toBe(88);
  });
  it('preserves open, closed and unknown barriers and stable equal/unknown timestamps', () => {
    const items = [pull(1), pull(2, 'merged', 'bad'), pull(3, 'merged', '2026-11-14T18:00:00+13:00'), pull(4, 'merged', '2026-11-14T05:00:00Z'), pull(5, 'open'), pull(6, 'closed'), pull(7, 'unknown'), pull(8, 'open')];
    expect(recentMergedDisplayOrder(items).map(p => p.number)).toEqual([1, 2, 3, 4, 5, 6, 7, 8]);
  });
});

it('orders only known negative merged slots and preserves gaps and active rails', () => {
  const items = [pull(1), pull(2), pull(3), pull(4, 'open'), pull(5, 'closed')];
  const distances = [-1, -8, -3, 1, 5];
  expect(recentMergedDisplayDistances(items, distances)).toEqual([-8, -3, -1, 1, 5]);
  expect(distances).toEqual([-1, -8, -3, 1, 5]);
  expect(recentMergedDisplayDistances(items, [undefined, -3, -1, 1, 5])).toEqual([undefined, -3, -1, 1, 5]);
  expect(recentMergedDisplayDistances(items, [3, 2, 1, 4, 5])).toEqual([3, 2, 1, 4, 5]);
});
