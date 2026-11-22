import { afterEach, expect, it, vi } from 'vitest';
import { useReviewQueuePaginationScroll } from './review-queue-navigation';
const hooks = vi.hoisted(() => ({ ref: { current: false }, effect: undefined as (() => void | (() => void)) | undefined }));
vi.mock('react', async original => ({ ...await original<typeof import('react')>(), useRef: () => hooks.ref, useEffect: (effect: () => void | (() => void)) => { hooks.effect = effect; } }));
afterEach(() => { vi.unstubAllGlobals(); hooks.ref.current = false; hooks.effect = undefined; });
function setup() {
  const scrollBy = vi.fn(), dispatchEvent = vi.fn();
  let frame: (() => void) | undefined;
  vi.stubGlobal('window', { scrollBy, dispatchEvent });
  vi.stubGlobal('document', { getElementById: (id: string) => id === 'review-queue-list' ? { getBoundingClientRect: () => ({ top: 700 }) } : null, querySelector: () => ({ getBoundingClientRect: () => ({ bottom: 60 }) }) });
  vi.stubGlobal('requestAnimationFrame', (cb: () => void) => { frame = cb; return 1; });
  vi.stubGlobal('cancelAnimationFrame', () => { frame = undefined; });
  return { scrollBy, dispatchEvent, flush: () => { const cb = frame; frame = undefined; cb?.(); } };
}
it('aligns the loaded list after either page direction', () => {
  for (const key of ['next-cursor', '']) {
    const f = setup();
    const request = useReviewQueuePaginationScroll('old', true, false);
    request();
    expect(f.dispatchEvent).toHaveBeenCalledTimes(1);
    useReviewQueuePaginationScroll(key, false, false); hooks.effect?.(); f.flush();
    expect(f.scrollBy).not.toHaveBeenCalled();
    useReviewQueuePaginationScroll(key, true, false); hooks.effect?.(); f.flush();
    expect(f.scrollBy).toHaveBeenCalledWith({ top: 628, left: 0, behavior: 'instant' });
    expect(hooks.ref.current).toBe(false);
    hooks.effect?.(); f.flush();
    expect(f.scrollBy).toHaveBeenCalledTimes(1);
  }
});
it('does not move initial loads, reload restoration or background refreshes', () => {
  const f = setup(); useReviewQueuePaginationScroll('saved', true, false); hooks.effect?.(); f.flush();
  expect(f.scrollBy).not.toHaveBeenCalled();
});
it('cancels failed and superseded navigation without a delayed stale jump', () => {
  const f = setup(); useReviewQueuePaginationScroll('', true, false)();
  useReviewQueuePaginationScroll('next', true, false); const cleanup = hooks.effect?.();
  if (typeof cleanup === 'function') cleanup();
  useReviewQueuePaginationScroll('next', false, true); hooks.effect?.(); f.flush();
  expect(f.scrollBy).not.toHaveBeenCalled(); expect(hooks.ref.current).toBe(false);
});
