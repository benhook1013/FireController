import { useEffect, useRef, useState } from 'react';
import { BrowserBuildUpdate, browserBuildCheckEvent, loadedBrowserIdentity, readBrowserIdentity } from './browser-build';
import { saveReadingPosition } from './reading-position';

const safetyEvent = 'firecontroller:browser-update-safety';
export function notifyBrowserUpdateSafety(): void { window.dispatchEvent(new Event(safetyEvent)); }
const writeGuards = new Set<() => boolean>();
export function guardBrowserUpdate(unsafe: () => boolean): () => void {
  writeGuards.add(unsafe);
  return () => { writeGuards.delete(unsafe); };
}
export function browserUpdateSafe(root: Document = document): boolean {
  if (root.visibilityState !== 'visible' || [...writeGuards].some(unsafe => unsafe()) || root.querySelector('[data-browser-dirty="true"], [data-browser-busy="true"], [data-browser-window-unsafe="true"]')) return false;
  const active = root.activeElement;
  return !active?.matches('[data-browser-draft] input, [data-browser-draft] textarea, [data-browser-draft] select, [data-browser-draft] [contenteditable="true"]');
}
export function BrowserUpdate() {
  const [available, setAvailable] = useState(false);
  const [blocked, setBlocked] = useState(false);
  const [windowBlocked, setWindowBlocked] = useState(false);
  const updater = useRef<BrowserBuildUpdate | null>(null);
  useEffect(() => {
    const owner = new BrowserBuildUpdate({ loaded: loadedBrowserIdentity(document), read: readBrowserIdentity,
      safe: () => browserUpdateSafe(), storage: { getItem: key => sessionStorage.getItem(key), setItem: (key, value) => sessionStorage.setItem(key, value) }, available: setAvailable,
      reload: () => { saveReadingPosition(); window.location.reload(); },
    });
    updater.current = owner;
    let previouslySafe = browserUpdateSafe();
    setBlocked(!previouslySafe); setWindowBlocked(Boolean(document.querySelector('[data-browser-window-unsafe="true"]')));
    const changed = () => { const safe = browserUpdateSafe(); setBlocked(!safe); setWindowBlocked(Boolean(document.querySelector('[data-browser-window-unsafe="true"]'))); if (safe && !previouslySafe) owner.reconsider(); previouslySafe = safe; };
    const visible = () => { changed(); if (document.visibilityState === 'visible') owner.check(); };
    const beforeUnload = (event: BeforeUnloadEvent) => { if (document.querySelector('[data-browser-dirty="true"], [data-browser-busy="true"]')) { event.preventDefault(); event.returnValue = ''; } };
    window.addEventListener('beforeunload', beforeUnload);
    window.addEventListener(safetyEvent, changed);
    const observer = new MutationObserver(changed);
    observer.observe(document.body, { childList: true, subtree: true, attributes: true, attributeFilter: ['data-browser-dirty', 'data-browser-busy', 'data-browser-window-unsafe'] });
    window.addEventListener(browserBuildCheckEvent, owner.check);
    window.addEventListener('focus', visible);
    document.addEventListener('visibilitychange', visible);
    document.addEventListener('focusin', changed);
    document.addEventListener('focusout', changed);
    owner.check();
    return () => { window.removeEventListener('beforeunload', beforeUnload); window.removeEventListener(safetyEvent, changed); owner.stop(); observer.disconnect(); updater.current = null; window.removeEventListener(browserBuildCheckEvent, owner.check); window.removeEventListener('focus', visible); document.removeEventListener('visibilitychange', visible); document.removeEventListener('focusin', changed); document.removeEventListener('focusout', changed); };
  }, []);
  if (!available) return null;
  return <aside className="browser-update" role="status"><button type="button" disabled={blocked} onClick={() => updater.current?.update()}>Update available</button>{blocked && <span>{windowBlocked ? 'Return to the first page or refresh loaded work to update safely.' : 'Finish or clear your edit and resolve pending actions to update.'}</span>}</aside>;
}
