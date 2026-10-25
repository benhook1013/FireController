import { createContext, useLayoutEffect, useMemo, type RefObject } from 'react';

export type ReadingLandmark = { id: string; top: number; bottom: number };
export type ReadingPoint = { anchor: string; offset: number; y: number; after: string[]; before: string[] };
const storageKey = 'firecontroller.reading-position.v1';
const navigationEvent = 'firecontroller:reading-navigation';
const selector = '[data-reading-anchor]';

export const RestoringReadingPosition = createContext(false);

export function captureReadingPoint(items: ReadingLandmark[], edge: number, height: number, y: number): ReadingPoint | undefined {
  if (y < 2) return undefined;
  const visible = items.filter(item => item.bottom > edge && item.top < height);
  const covering = visible.filter(item => item.top <= edge);
  const anchor = covering.sort((a, b) => b.top - a.top)[0] ?? visible.sort((a, b) => a.top - b.top)[0];
  if (!anchor) return undefined;
  const index = items.indexOf(anchor);
  return { anchor: anchor.id, offset: anchor.top, y, after: items.slice(index + 1, index + 13).map(item => item.id), before: items.slice(Math.max(0, index - 12), index).reverse().map(item => item.id) };
}

export function readingCorrection(point: ReadingPoint, items: ReadingLandmark[], edge: number): number | undefined {
  const anchor = items.find(item => item.id === point.anchor);
  if (anchor) return anchor.top - point.offset;
  // A deleted item cannot retain a pixel position. Keep the nearest surviving
  // following item readable, then fall back to the preceding item.
  for (const id of [...point.after, ...point.before]) {
    const survivor = items.find(item => item.id === id);
    if (survivor) return survivor.top - edge;
  }
  return undefined;
}

function validPoint(value: unknown): value is ReadingPoint {
  if (!value || typeof value !== 'object') return false;
  const p = value as ReadingPoint;
  const id = (value: unknown): value is string => typeof value === 'string' && value.length > 0 && value.length <= 256 && !/[\u0000-\u001f]/u.test(value);
  return id(p.anchor) && Number.isFinite(p.offset) && Math.abs(p.offset) < 1e7 && Number.isFinite(p.y) && p.y >= 0 && p.y < 1e8 &&
    Array.isArray(p.after) && p.after.length <= 12 && p.after.every(id) && Array.isArray(p.before) && p.before.length <= 12 && p.before.every(id);
}

function savedPoints(): { scope: string; point: ReadingPoint }[] {
  try {
    const raw = sessionStorage.getItem(storageKey);
    if (!raw || raw.length > 65536) return [];
    const values: unknown = JSON.parse(raw);
    return Array.isArray(values) && values.length <= 12 ? values.filter(value => value && typeof value.scope === 'string' && value.scope.length <= 4096 && validPoint(value.point)) : [];
  } catch { return []; }
}

function savePoint(scope: string, point: ReadingPoint | undefined): void {
  try {
    const values = savedPoints().filter(value => value.scope !== scope);
    if (point) values.push({ scope, point });
    sessionStorage.setItem(storageKey, JSON.stringify(values.slice(-12)));
  } catch { /* Reading remains usable when tab storage is unavailable. */ }
}

export function scrollToReadingTarget(element: HTMLElement): void {
  window.dispatchEvent(new Event(navigationEvent));
  element.scrollIntoView();
}

// Follow stable document landmarks, not total page height. A tab-local position
// contains only opaque UI identities and offsets, never copied project content.
export function useReadingPosition(root: RefObject<HTMLElement | null>, scope: string, restore: boolean): boolean {
  const initial = useMemo(() => restore ? savedPoints().find(value => value.scope === scope)?.point : undefined, [scope, restore]);
  useLayoutEffect(() => {
    const element = root.current;
    if (!element) return;
    let point = initial, pendingRestore = !!initial, frame = 0, ownScroll = false, readerUntil = 0;
    const documentRoot = document.documentElement;
    const previousAnchoring = documentRoot.style.overflowAnchor;
    if (initial || element.querySelector(selector)) { documentRoot.style.overflowAnchor = 'none'; history.scrollRestoration = 'manual'; }
    const header = document.querySelector('.mast');
    const edge = () => Math.max(0, header?.getBoundingClientRect().bottom ?? 0) + 12;
    const landmarks = (): ReadingLandmark[] => [...element.querySelectorAll<HTMLElement>(selector)].flatMap(node => {
      const box = node.getBoundingClientRect();
      return box.height > 0 ? [{ id: node.dataset.readingAnchor!, top: box.top, bottom: box.bottom }] : [];
    });
    const capture = () => captureReadingPoint(landmarks(), edge(), window.innerHeight, window.scrollY);
    const reconcile = () => {
      frame = 0;
      const items = landmarks();
      documentRoot.style.overflowAnchor = items.length || pendingRestore ? 'none' : previousAnchoring;
      history.scrollRestoration = items.length || pendingRestore ? 'manual' : 'auto';
      if (performance.now() < readerUntil) { point = capture(); return; }
      if (point) {
        const region = point.anchor.startsWith('review-') ? 'reviews' : 'delivery';
        if (pendingRestore && element.querySelector(`[data-reading-region="${region}"]`)?.getAttribute('data-reading-ready') !== 'true') return;
        const delta = readingCorrection(point, items, edge());
        if (delta === undefined && pendingRestore) return; // The selected data is still loading.
        if (delta !== undefined) {
          if (Math.abs(delta) > .5) {
            ownScroll = true;
            window.scrollBy({ top: delta, left: 0, behavior: 'instant' });
          }
          pendingRestore = false;
        }
      }
      const retainedIndex = point ? items.findIndex(item => item.id === point!.anchor) : -1;
      if (point && retainedIndex >= 0) {
        // Compensation must not select a neighbour or accumulate fractional
        // scroll rounding. Only reader input chooses a new surviving anchor.
        point = { ...point, y: window.scrollY, after: items.slice(retainedIndex + 1, retainedIndex + 13).map(item => item.id), before: items.slice(Math.max(0, retainedIndex - 12), retainedIndex).reverse().map(item => item.id) };
      } else point = capture();
    };
    const schedule = () => { if (!frame) frame = requestAnimationFrame(reconcile); };
    const scroll = () => {
      if (ownScroll) { ownScroll = false; return; }
      // Browser reload restoration can emit scroll while only the loading
      // shell exists. Explicit wheel/touch/key input already cancels restore.
      if (pendingRestore) return;
      if (frame) return;
      pendingRestore = false;
      point = capture();
      readerUntil = performance.now() + 100;
    };
    const reader = () => {
      ownScroll = false;
      pendingRestore = false;
      readerUntil = performance.now() + 150;
      point = capture();
    };
    const key = (event: KeyboardEvent) => { if (['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' ', 'Tab'].includes(event.key)) reader(); };
    const scrollbar = (event: PointerEvent) => { if (event.target === documentRoot) reader(); };
    const navigate = () => {
      reader();
      point = undefined;
      schedule();
    };
    const save = () => savePoint(scope, point);
    const changes = new MutationObserver(schedule);
    changes.observe(element, { childList: true, subtree: true, characterData: true, attributes: true });
    const sizes = new ResizeObserver(schedule);
    sizes.observe(element);
    if (header) sizes.observe(header);
    window.addEventListener('scroll', scroll, { passive: true });
    window.addEventListener('wheel', reader, { passive: true });
    window.addEventListener('touchmove', reader, { passive: true });
    window.addEventListener('pointerdown', scrollbar, { passive: true });
    window.addEventListener('keydown', key);
    window.addEventListener('pagehide', save);
    window.addEventListener(navigationEvent, navigate);
    schedule();
    return () => {
      save();
      cancelAnimationFrame(frame);
      changes.disconnect(); sizes.disconnect();
      documentRoot.style.overflowAnchor = previousAnchoring;
      history.scrollRestoration = 'auto';
      window.removeEventListener('scroll', scroll); window.removeEventListener('wheel', reader);
      window.removeEventListener('touchmove', reader); window.removeEventListener('pointerdown', scrollbar);
      window.removeEventListener('keydown', key); window.removeEventListener('pagehide', save);
      window.removeEventListener(navigationEvent, navigate);
    };
  }, [root, scope, initial]);
  return !!initial;
}
