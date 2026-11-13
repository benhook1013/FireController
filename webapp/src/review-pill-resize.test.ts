import { afterEach, expect, it, vi } from 'vitest';
const state = vi.hoisted(() => ({ element: { clientWidth: 676 }, effect: undefined as undefined | (() => void | (() => void)), set: vi.fn() }));
vi.mock('react', () => ({ useRef: () => ({ current: state.element }), useState: () => [undefined, state.set], useLayoutEffect: (effect: () => void | (() => void)) => { state.effect = effect; } }));
import { useReviewPillCapacity } from './review-pill-capacity';
afterEach(() => { vi.unstubAllGlobals(); state.set.mockClear(); });
it('remeasures on resize and mobile breakpoint transitions and removes observers on unmount', () => {
  let resize = () => {}, change = () => {};
  const disconnect = vi.fn(), observe = vi.fn();
  const media = { matches: false, addEventListener: vi.fn((_event, callback) => { change = callback; }), removeEventListener: vi.fn() };
  vi.stubGlobal('matchMedia', vi.fn(() => media));
  vi.stubGlobal('getComputedStyle', () => ({ paddingLeft: '16px', paddingRight: '16px' }));
  vi.stubGlobal('ResizeObserver', class { constructor(callback: () => void) { resize = callback; } observe = observe; disconnect = disconnect; });
  const initial = useReviewPillCapacity(); expect(initial.capacity).toBe(6); expect(initial.measured).toBe(false); const cleanup = state.effect!();
  expect(observe).toHaveBeenCalledWith(state.element); expect(state.set).toHaveBeenLastCalledWith(8);
  state.element.clientWidth = 270; resize(); expect(state.set).toHaveBeenLastCalledWith(3);
  media.matches = true; change(); expect(state.set).toHaveBeenLastCalledWith(6);
  state.element.clientWidth = 1000; media.matches = false; change(); expect(state.set).toHaveBeenLastCalledWith(12);
  cleanup!(); expect(disconnect).toHaveBeenCalledOnce(); expect(media.removeEventListener).toHaveBeenCalledWith('change', change);
});
