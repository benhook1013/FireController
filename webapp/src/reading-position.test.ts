import { afterEach, describe, expect, it, vi } from 'vitest';
import { captureReadingPoint, notifyReadingNavigation, readingCorrection, useReadingPosition } from './reading-position';

const hooks = vi.hoisted(() => ({ effect: undefined as (() => void | (() => void)) | undefined }));
vi.mock('react', async importOriginal => ({
  ...await importOriginal<typeof import('react')>(),
  useMemo: (factory: () => unknown) => factory(),
  useLayoutEffect: (effect: () => void | (() => void)) => { hooks.effect = effect; },
}));

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); hooks.effect = undefined; });

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

  it('lets explicit section navigation supersede a pending resize correction', () => {
    const events = new EventTarget(), frames = new Map<number, FrameRequestCallback>();
    let nextFrame = 0, top = -50, resize = () => {};
    const scrollBy = vi.fn();
    vi.stubGlobal('window', Object.assign(events, { innerHeight: 800, scrollY: 1000, scrollBy }));
    vi.stubGlobal('history', { scrollRestoration: 'auto' });
    vi.stubGlobal('sessionStorage', { getItem: () => null, setItem: vi.fn() });
    vi.stubGlobal('document', { documentElement: { style: { overflowAnchor: '' } }, querySelector: () => ({ getBoundingClientRect: () => ({ bottom: 80 }) }) });
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { frames.set(++nextFrame, callback); return nextFrame; });
    vi.stubGlobal('cancelAnimationFrame', (id: number) => frames.delete(id));
    vi.stubGlobal('MutationObserver', class { observe() {} disconnect() {} });
    vi.stubGlobal('ResizeObserver', class { constructor(callback: () => void) { resize = callback; } observe() {} disconnect() {} });
    vi.spyOn(performance, 'now').mockReturnValue(1000);
    const row = { dataset: { readingAnchor: 'review-reading' }, getBoundingClientRect: () => ({ top, bottom: top + 500, height: 500 }) };
    const root = { querySelector: () => row, querySelectorAll: () => [row] } as unknown as HTMLElement;
    const flush = () => { const pending = [...frames.values()]; frames.clear(); pending.forEach(callback => callback(1000)); };
    useReadingPosition({ current: root }, 'project-section-regression', false);
    const cleanup = hooks.effect!();
    try {
      flush(); // Capture the ordinary review reading point.
      top += 300;
      resize();
      flush();
      expect(scrollBy).toHaveBeenCalledWith({ top: 300, left: 0, behavior: 'instant' });
      scrollBy.mockClear();
      top += 200;
      resize();
      expect(frames.size).toBe(1);
      notifyReadingNavigation(); // The Project section click precedes its native fragment jump.
      top = 1200; // Native navigation has moved away from the old review landmark.
      flush();
      expect(scrollBy).not.toHaveBeenCalled();
    } finally { if (typeof cleanup === 'function') cleanup(); }
  });
});
