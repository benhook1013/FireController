import { useEffect, useRef } from 'react';
import { notifyReadingNavigation, retainReadingTarget, scrollToReadingTarget } from './reading-position';
import type { ReviewPullSummary, ReviewQueue } from './types';

export const reviewQueueHeadHash = '#review-queue-head';

export function scrollToReviewQueueTarget(element: HTMLElement): void {
  const queue = element.closest('#review-queue');
  const rows = queue ? [...queue.querySelectorAll<HTMLElement>('.review-queue-row')] : [];
  const previous = rows[rows.indexOf(element) - 1];
  if (!window.matchMedia('(min-width: 651px)').matches || !previous?.classList.contains('review-row-merged')) {
    scrollToReadingTarget(element);
    const anchor = element.dataset?.readingAnchor;
    if (anchor) retainReadingTarget(anchor, parseFloat(getComputedStyle(element).scrollMarginTop) || 0);
    return;
  }
  // Merged cards have variable heights. A fixed margin on the current head
  // can clip their title or history; align the actual preceding card instead.
  const edge = Math.max(0, document.querySelector('.mast')?.getBoundingClientRect().bottom ?? 0) + 12;
  notifyReadingNavigation();
  window.scrollBy({ top: previous.getBoundingClientRect().top - edge, left: 0, behavior: 'instant' });
  const anchor = previous.dataset?.readingAnchor;
  if (anchor) retainReadingTarget(anchor, edge);
}

type MergeHead = { state: 'head'; pull: ReviewPullSummary } | { state: 'unknown'; reason: 'unconfigured' | 'preceding' | 'next-page' } | { state: 'none' };
export function selectMergeHead(queue: ReviewQueue): MergeHead {
  if (!queue.configured) return { state: 'unknown', reason: 'unconfigured' };
  if (queue.head_position !== undefined || queue.items.some(pull => pull.queue_position !== undefined)) {
    if (queue.head_position !== undefined) {
      const pull = queue.items.find(item => item.queue_position === queue.head_position);
      return pull?.state === 'open' ? { state: 'head', pull } : { state: 'unknown', reason: 'next-page' };
    }
    if (queue.items.some(pull => pull.state !== 'merged' && pull.state !== 'closed')) return { state: 'unknown', reason: 'preceding' };
    return queue.has_more ? { state: 'unknown', reason: 'next-page' } : { state: 'none' };
  }
  for (const pull of queue.items) {
    if (pull.state === 'merged' || pull.state === 'closed') continue;
    if (pull.state === 'open') return { state: 'head', pull };
    return { state: 'unknown', reason: 'preceding' };
  }
  return queue.has_more ? { state: 'unknown', reason: 'next-page' } : { state: 'none' };
}

export function reviewQueueHeadTarget(queue: ReviewQueue | undefined): string | undefined {
  if (!queue) return undefined;
  const head = selectMergeHead(queue);
  return head.state === 'head' ? `review-pr-${head.pull.number}` : 'review-queue';
}

export function reviewQueueHashTarget(items: ReviewPullSummary[] | undefined, hash: string): string | undefined {
  const pull = items?.find(item => hash === `#review-pr-${item.number}`);
  return pull ? `review-pr-${pull.number}` : undefined;
}

export function useReviewQueueJump(queue: ReviewQueue | undefined, hash: string, entryKey: string, restoringPosition: boolean, current = true, unavailable = false): void {
  const lastJump = useRef('');
  const jumpToHead = current && hash === reviewQueueHeadHash;
  useEffect(() => {
    const target = jumpToHead ? (unavailable ? 'review-queue' : reviewQueueHeadTarget(queue)) : reviewQueueHashTarget(queue?.configured ? queue.items : undefined, hash);
    if (!target || restoringPosition) return;
    // Resolve once per navigation; background observations must not move a reader.
    const jump = `${entryKey}:${jumpToHead ? reviewQueueHeadHash : target}`;
    if (lastJump.current === jump) return;
    const frame = requestAnimationFrame(() => {
      const row = document.getElementById(target);
      if (row) { scrollToReviewQueueTarget(row); lastJump.current = jump; }
    });
    return () => cancelAnimationFrame(frame);
  }, [queue, hash, entryKey, restoringPosition, jumpToHead, unavailable]);
}
