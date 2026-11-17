import type { ReviewPullSummary } from './types';

// Only presentation changes: stored positions, page cursors and the open
// review order remain authoritative. A loaded merged block reads oldest to
// newest so its newest known merge is adjacent to the following queue head.
export function recentMergedDisplayOrder(items: ReviewPullSummary[]): ReviewPullSummary[] {
  const result = [...items];
  for (let start = 0; start < result.length;) {
    if (result[start].state !== 'merged') { start++; continue; }
    let end = start;
    while (end < result.length && result[end].state === 'merged') end++;
    const block = result.slice(start, end).map((pull, index) => ({ pull, index, time: Date.parse(pull.merged_at ?? '') }));
    block.sort((a, b) => {
      const aKnown = Number.isFinite(a.time), bKnown = Number.isFinite(b.time);
      if (aKnown !== bKnown) return aKnown ? 1 : -1;
      return aKnown && a.time !== b.time ? a.time - b.time : a.index - b.index;
    });
    result.splice(start, end - start, ...block.map(item => item.pull));
    start = end;
  }
  return result;
}

// Merged cards move between presentation slots, not between retained queue
// positions. Keep the known negative rail slots ordered toward the head.
export function recentMergedDisplayDistances(items: ReviewPullSummary[], distances: (number | undefined)[]): (number | undefined)[] {
  const result = [...distances];
  for (let start = 0; start < items.length;) {
    if (items[start].state !== 'merged') { start++; continue; }
    let end = start;
    while (end < items.length && items[end].state === 'merged') end++;
    const slots = result.slice(start, end);
    if (slots.every((value): value is number => value !== undefined && value < 0)) {
      result.splice(start, slots.length, ...slots.sort((a, b) => a - b));
    }
    start = end;
  }
  return result;
}
