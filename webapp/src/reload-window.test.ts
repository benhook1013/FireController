import { afterEach, describe, expect, it, vi } from 'vitest';
import { readReloadWindow, saveReloadWindow, validQueueWindow, validWindowDepth, useReloadWindow } from './reload-window';
const hooks = vi.hoisted(() => ({ index: 0, state: [] as unknown[], dependencies: [] as (unknown[] | undefined)[], effects: [] as (() => void)[] }));
vi.mock('react', async original => ({ ...await original<typeof import('react')>(),
  useState: (initial: unknown) => { const index = hooks.index++; if (!(index in hooks.state)) hooks.state[index] = typeof initial === 'function' ? initial() : initial; return [hooks.state[index], (update: unknown) => { hooks.state[index] = typeof update === 'function' ? update(hooks.state[index]) : update; }]; },
  useLayoutEffect: (effect: () => void, dependencies: unknown[]) => { const index = hooks.index++, before = hooks.dependencies[index]; if (!before || dependencies.some((value, at) => !Object.is(value, before[at]))) { hooks.dependencies[index] = dependencies; hooks.effects.push(effect); } },
}));
afterEach(() => { vi.unstubAllGlobals(); hooks.index = 0; hooks.state = []; hooks.dependencies = []; hooks.effects = []; });
describe('history-entry reload windows', () => {
  it('preserves router state and exact queue trail/snapshot with isolated session/worker scopes', () => {
    const stored = { state: { key: 'router-key', idx: 3, usr: { selected: 'route' } } as Record<string, unknown>, replaceState: vi.fn((next: Record<string, unknown>) => { stored.state = next; }) };
    vi.stubGlobal('history', stored);
    const window = { trail: ['', 'signed-opaque-cursor'], index: 1, snapshot: 'review_snapshot_one' };
    expect(saveReloadWindow('session-one/worker-one/recent', window)).toBe(true);
    expect(readReloadWindow('session-one/worker-one/recent', { trail: [''], index: 0, snapshot: 'current' }, validQueueWindow)).toEqual(window);
    expect(stored.state).toMatchObject({ key: 'router-key', idx: 3, usr: { selected: 'route' } });
    expect(readReloadWindow('session-two/worker-one/recent', 0, validWindowDepth)).toBe(0);
    expect(readReloadWindow('session-one/worker-two/recent', 0, validWindowDepth)).toBe(0);
  });
  it('bounds stored selectors and continuation depth, rejecting malformed and oversized windows', () => {
    vi.stubGlobal('history', { state: {}, replaceState: vi.fn() });
    expect(validWindowDepth(20)).toBe(true); expect(validWindowDepth(21)).toBe(false);
    expect(validQueueWindow({ trail: ['', 'cursor'], index: 2, snapshot: 'current' })).toBe(false);
    expect(validQueueWindow({ trail: Array(21).fill(''), index: 0, snapshot: 'current' })).toBe(false);
    expect(validQueueWindow({ trail: ['', 'x'.repeat(4097)], index: 1, snapshot: 'current' })).toBe(false);
    expect(saveReloadWindow('scope', 'x'.repeat(32769))).toBe(false);
    expect(saveReloadWindow('x'.repeat(1025), 1)).toBe(false);
  });
  it('refuses inaccessible history and treats corrupt retained state as the initial window', () => {
    vi.stubGlobal('history', { state: { firecontrollerWindows: [{ scope: 'scope', value: { index: -1 } }] }, replaceState: () => { throw new Error('denied'); } });
    expect(readReloadWindow('scope', { trail: [''], index: 0, snapshot: 'current' }, validQueueWindow).index).toBe(0);
    expect(saveReloadWindow('scope', 1)).toBe(false);
  });
});

it('copies the live page on hash PUSH and restores each POP entry without overwriting its own page', () => {
  let entry = 'A';
  const historyEntries = new Map<string, Record<string, unknown>>([['A', { key: 'A', idx: 1 }], ['B', { key: 'B', idx: 2 }]]);
  vi.stubGlobal('history', { get state() { return historyEntries.get(entry); }, replaceState: (next: Record<string, unknown>) => historyEntries.set(entry, structuredClone(next)) });
  const first = { trail: [''], index: 0, snapshot: 'current' };
  const render = (key: string, pop: boolean) => { entry = key; hooks.index = 0; const result = useReloadWindow('session/worker/recent', first, validQueueWindow, key, pop); const effects = hooks.effects.splice(0); effects.forEach(effect => effect()); return result; };
  let [, change] = render('A', true);
  change({ trail: ['', 'page-two-cursor'], index: 1, snapshot: 'review_snapshot_one' });
  render('A', true);
  [, change] = render('B', false); // Same-path #review-queue PUSH keeps the visible page.
  expect(readReloadWindow('session/worker/recent', first, validQueueWindow).index).toBe(1);
  change(first); render('B', false);
  expect(render('A', true)[0].index).toBe(1);
  expect(render('B', true)[0].index).toBe(0);
});
