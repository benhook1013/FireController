import { afterEach, describe, expect, it, vi } from 'vitest';
import { reviewQueueHeadHash, reviewQueueHeadTarget, scrollToReviewQueueTarget, useReviewQueueJump } from './review-queue-navigation';
import type { ReviewPullSummary, ReviewQueue } from './types';

const hooks = vi.hoisted(() => ({ ref: { current: '' }, effect: undefined as (() => void | (() => void)) | undefined }));
vi.mock('react', async original => ({ ...await original<typeof import('react')>(), useRef: () => hooks.ref, useEffect: (effect: () => void | (() => void)) => { hooks.effect = effect; } }));

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); hooks.ref.current = ''; hooks.effect = undefined; });

function fixture(desktop: boolean, merged = true, previousTop = 500, headerBottom = 68) {
  const scrollIntoView = vi.fn(), scrollBy = vi.fn(), dispatchEvent = vi.fn();
  const previous = { classList: { contains: (name: string) => merged && name === 'review-row-merged' }, getBoundingClientRect: () => ({ top: previousTop }) };
  const row = { scrollIntoView, closest: () => ({ querySelectorAll: () => [previous, row] }) } as unknown as HTMLElement;
  vi.stubGlobal('window', { matchMedia: () => ({ matches: desktop }), scrollBy, dispatchEvent });
  vi.stubGlobal('document', { querySelector: () => ({ getBoundingClientRect: () => ({ bottom: headerBottom }) }) });
  return { row, scrollIntoView, scrollBy, dispatchEvent };
}

describe('review queue jump context', () => {
  it('aligns the entire preceding merged card below the actual desktop header', () => {
    for (const [top, header] of [[500, 68], [920, 110], [-100, 68]]) {
      const f = fixture(true, true, top, header);
      scrollToReviewQueueTarget(f.row);
      expect(f.scrollBy).toHaveBeenCalledWith({ top: top - header - 12, left: 0, behavior: 'instant' });
      expect(f.scrollIntoView).not.toHaveBeenCalled();
      expect(f.dispatchEvent).toHaveBeenCalledTimes(1);
    }
  });
  it('preserves the existing mobile target and native scroll margin', () => {
    const f = fixture(false);
    scrollToReviewQueueTarget(f.row);
    expect(f.scrollIntoView).toHaveBeenCalledTimes(1);
    expect(f.scrollBy).not.toHaveBeenCalled();
  });
  it('does not jump to a preceding open or closed card', () => {
    const f = fixture(true, false);
    scrollToReviewQueueTarget(f.row);
    expect(f.scrollIntoView).toHaveBeenCalledTimes(1);
    expect(f.scrollBy).not.toHaveBeenCalled();
  });
  it('preserves ordinary navigation when no preceding queue row is loaded', () => {
    const f = fixture(true);
    f.row.closest = () => null;
    scrollToReviewQueueTarget(f.row);
    expect(f.scrollIntoView).toHaveBeenCalledTimes(1);
    expect(f.scrollBy).not.toHaveBeenCalled();
  });
});

const pull = (number: number, state: ReviewPullSummary['state'] = 'open', queue_position?: number) => ({ number, state, ...(queue_position === undefined ? {} : { queue_position }) }) as ReviewPullSummary;
const queue = (items: ReviewPullSummary[], head_position?: number) => ({ configured: true, items, has_more: false, ...(head_position === undefined ? {} : { head_position }) }) as ReviewQueue;

describe('current queue head navigation', () => {
  it('resolves the same stable anchor to the first open PR after merges, closures and reordering', () => {
    expect(reviewQueueHeadHash).toBe('#review-queue-head');
    expect(reviewQueueHeadTarget(queue([pull(1, 'merged'), pull(2), pull(3)]))).toBe('review-pr-2');
    expect(reviewQueueHeadTarget(queue([pull(1, 'merged'), pull(2, 'merged'), pull(3)]))).toBe('review-pr-3');
    expect(reviewQueueHeadTarget(queue([pull(2, 'closed'), pull(3, 'merged'), pull(4)]))).toBe('review-pr-4');
    expect(reviewQueueHeadTarget(queue([pull(9), pull(4)]))).toBe('review-pr-9');
    expect(reviewQueueHeadTarget(queue([pull(1, 'merged', 1), pull(5, 'open', 9), pull(2, 'open', 10)], 9))).toBe('review-pr-5');
  });
  it('waits for loading and falls back safely when the head is absent, uncertain or complete', () => {
    expect(reviewQueueHeadTarget(undefined)).toBeUndefined();
    for (const value of [queue([]), queue([pull(1, 'merged')]), queue([pull(1, 'unknown'), pull(2)]), queue([pull(2, 'open', 9)], 8), queue([pull(2, 'open', 9)]), { ...queue([pull(1)]), configured: false }]) {
      expect(reviewQueueHeadTarget(value)).toBe('review-queue');
    }
  });
  it('waits for rows, ignores live head changes, and resolves a new navigation or reload again', () => {
    const f = fixture(false), frames = new Map<number, FrameRequestCallback>();
    let frame = 0;
    const cleanup: { value: void | (() => void) } = { value: undefined };
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { frames.set(++frame, callback); return frame; });
    vi.stubGlobal('cancelAnimationFrame', (id: number) => frames.delete(id));
    const find = vi.fn(() => f.row);
    vi.stubGlobal('document', { getElementById: find });
    const render = (value: ReviewQueue | undefined, key = 'entry', hash = reviewQueueHeadHash, restoring = false, unavailable = false) => {
      if (typeof cleanup.value === 'function') cleanup.value();
      useReviewQueueJump(value, hash, key, restoring, true, unavailable);
      cleanup.value = hooks.effect!();
    };
    const flush = () => { const callbacks = [...frames.values()]; frames.clear(); callbacks.forEach(callback => callback(0)); };
    const initial = queue([pull(1, 'merged'), pull(2), pull(3)]);
    const refreshed = queue([pull(1, 'merged'), pull(2, 'merged'), pull(3)]);
    render(undefined); flush(); expect(find).not.toHaveBeenCalled();
    render(initial); flush(); expect(find).toHaveBeenLastCalledWith('review-pr-2');
    render(refreshed); flush(); expect(find).toHaveBeenCalledTimes(1);
    render(refreshed, 'clicked-again'); flush(); expect(find).toHaveBeenLastCalledWith('review-pr-3');
    expect(find).toHaveBeenCalledTimes(2);
    hooks.ref.current = ''; // A browser reload creates a new navigation instance.
    render(refreshed, 'clicked-again'); flush(); expect(find).toHaveBeenCalledTimes(3);
    render(refreshed, 'explicit', '#review-pr-2'); flush(); expect(find).toHaveBeenLastCalledWith('review-pr-2');
    render(initial, 'restore', '#review-pr-2', true); flush(); expect(find).toHaveBeenCalledTimes(4);
    render(initial, 'cancelled');
    render(refreshed, 'latest'); flush(); expect(find).toHaveBeenLastCalledWith('review-pr-3');
    expect(find).toHaveBeenCalledTimes(5);
    render(undefined, 'failed-read', reviewQueueHeadHash, false, true); flush();
    expect(find).toHaveBeenLastCalledWith('review-queue');
    expect(find).toHaveBeenCalledTimes(6);
    if (typeof cleanup.value === 'function') cleanup.value();
  });
});
