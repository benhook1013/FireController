import { useLayoutEffect, useState, type Dispatch, type SetStateAction } from 'react';

const field = 'firecontrollerWindows';
const maximumBytes = 32 << 10;
export const maximumWindowDepth = 20;
type Entry = { scope: string; value: unknown };
function entries(): Entry[] {
  const value: unknown = history.state?.[field];
  return Array.isArray(value) && value.length <= 6 ? value.filter(entry => entry && typeof entry.scope === 'string' && entry.scope.length <= 1024 && Object.hasOwn(entry, 'value')) : [];
}
export function readReloadWindow<T>(scope: string, fallback: T, valid: (value: unknown) => value is T): T {
  try {
    const candidate = entries().find(entry => entry.scope === scope)?.value;
    return valid(candidate) && JSON.stringify(candidate).length <= maximumBytes ? candidate : fallback;
  } catch { return fallback; }
}
export function saveReloadWindow(scope: string, value: unknown): boolean {
  try {
    if (scope.length > 1024 || JSON.stringify(value).length > maximumBytes) return false;
    const next = [...entries().filter(entry => entry.scope !== scope), { scope, value }].slice(-6);
    if (JSON.stringify(next).length > maximumBytes) return false;
    history.replaceState({ ...history.state, [field]: next }, '');
    return true;
  } catch { return false; }
}
// Retain only bounded UI selectors in this exact history entry. Router keys,
// URL and session cookies remain untouched; no records or credentials are saved.
export function useReloadWindow<T>(scope: string, fallback: T, valid: (value: unknown) => value is T, entryKey = '', pop = false, restore = true): [T, Dispatch<SetStateAction<T>>, boolean] {
  const read = (otherwise: T) => restore ? readReloadWindow(scope, otherwise, valid) : fallback;
  const [stored, setStored] = useState(() => ({ scope, entryKey, value: read(fallback) }));
  const [saved, setSaved] = useState({ scope, okay: true });
  const value = stored.scope !== scope ? read(fallback) : stored.entryKey !== entryKey && pop ? read(stored.value) : stored.value;
  if (stored.scope !== scope || stored.entryKey !== entryKey) setStored({ scope, entryKey, value });
  useLayoutEffect(() => { setSaved({ scope, okay: saveReloadWindow(scope, value) }); }, [scope, value, entryKey]);
  const change: Dispatch<SetStateAction<T>> = update => setStored(before => ({ scope, entryKey, value: typeof update === 'function' ? (update as (value: T) => T)(before.scope === scope ? before.value : value) : update }));
  return [value, change, saved.scope === scope && saved.okay];
}
export type QueueWindow = { trail: string[]; index: number; snapshot: string };
export function validQueueWindow(value: unknown): value is QueueWindow {
  if (!value || typeof value !== 'object') return false;
  const candidate = value as QueueWindow;
  return Object.keys(candidate).sort().join(',') === 'index,snapshot,trail' && Array.isArray(candidate.trail) && candidate.trail.length > 0 && candidate.trail.length <= maximumWindowDepth && candidate.trail[0] === '' && candidate.trail.every(cursor => typeof cursor === 'string' && cursor.length <= 4096 && !/[\u0000-\u001f]/u.test(cursor)) && Number.isInteger(candidate.index) && candidate.index >= 0 && candidate.index < candidate.trail.length && typeof candidate.snapshot === 'string' && candidate.snapshot.length <= 128 && /^[A-Za-z0-9_-]+$/u.test(candidate.snapshot);
}
export const validWindowDepth = (value: unknown): value is number => Number.isInteger(value) && Number(value) >= 0 && Number(value) <= maximumWindowDepth;
