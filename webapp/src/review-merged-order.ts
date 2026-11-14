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
