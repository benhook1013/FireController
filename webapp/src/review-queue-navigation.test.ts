import { afterEach, describe, expect, it, vi } from 'vitest';
import { scrollToReviewQueueTarget } from './review-queue-navigation';

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

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
