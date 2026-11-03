import { afterEach, describe, expect, it, vi } from 'vitest';
import { BrowserBuildUpdate, browserAssetIdentity, loadedBrowserIdentity, readBrowserIdentity } from './browser-build';
import { browserUpdateSafe, guardBrowserUpdate } from './browser-update';
import { BrowserActionMemory } from './browser-actions';
import { ApiError } from './api';
const oldAssets = ['/assets/index-OldBuild.js', '/assets/index-OldStyle.css'];
const nextAssets = ['/assets/index-NewBuild.js', '/assets/index-NewStyle.css'];
const old = browserAssetIdentity(oldAssets)!;
const next = browserAssetIdentity(nextAssets)!;
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });
function fixture(initial = old) {
  let safe = true;
  const stored = new Map<string, string>();
  const read = vi.fn(async (_signal: AbortSignal) => initial), reload = vi.fn(), available = vi.fn();
  const options = { loaded: old, read, safe: () => safe, reload, available, storage: { getItem: (key: string) => stored.get(key) ?? null, setItem: (key: string, value: string) => { stored.set(key, value); } } };
  const owner = new BrowserBuildUpdate(options);
  return { owner, options, read, reload, available, safe: (value: boolean) => { safe = value; } };
}
describe('browser deployment lifecycle', () => {
  it('compares with loaded DOM, recognizes CSS changes and ignores unchanged frontend builds', async () => {
    const root = { querySelectorAll: () => oldAssets.map(path => ({ tagName: path.endsWith('.js') ? 'SCRIPT' : 'LINK', getAttribute: () => path })) } as unknown as Document;
    expect(loadedBrowserIdentity(root)).toBe(old);
    expect(browserAssetIdentity([...oldAssets].reverse())).toBe(old);
    vi.useFakeTimers();
    const f = fixture(); f.owner.check(); await vi.runAllTimersAsync();
    expect(f.reload).not.toHaveBeenCalled(); // A backend deployment keeps these assets.
    f.read.mockResolvedValue(browserAssetIdentity([oldAssets[0], nextAssets[1]])!);
    f.owner.check(); await vi.runAllTimersAsync();
    expect(f.reload).toHaveBeenCalledOnce();
    f.owner.stop();
  });
  it('detects a changed build on the first check of an already open tab, and never polls', async () => {
    vi.useFakeTimers(); const f = fixture(next);
    f.owner.check(); f.owner.check(); await vi.runAllTimersAsync();
    expect(f.read).toHaveBeenCalledOnce(); expect(f.reload).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(600_000); expect(f.read).toHaveBeenCalledOnce();
    f.owner.stop();
  });
  it('defers drafts, sending and unknown results until clear, with a fresh coalesced check', async () => {
    vi.useFakeTimers(); const f = fixture(next); f.safe(false);
    f.owner.check(); await vi.runAllTimersAsync();
    expect(f.available).toHaveBeenLastCalledWith(true); expect(f.reload).not.toHaveBeenCalled();
    f.owner.update(); expect(f.reload).not.toHaveBeenCalled();
    f.safe(true); f.owner.reconsider(); f.owner.reconsider(); await vi.runAllTimersAsync();
    expect(f.read).toHaveBeenCalledTimes(2); expect(f.reload).toHaveBeenCalledOnce(); f.owner.stop();
  });
  it('does not loop when a rollout serves the previous index again, but offers an explicit update', async () => {
    vi.useFakeTimers(); const f = fixture(next); f.owner.check(); await vi.runAllTimersAsync(); f.owner.stop();
    const second = new BrowserBuildUpdate(f.options); second.check(); await vi.runAllTimersAsync();
    expect(f.reload).toHaveBeenCalledOnce(); expect(f.available).toHaveBeenLastCalledWith(true);
    second.update(); expect(f.reload).toHaveBeenCalledTimes(2); second.stop();
  });
  it('coalesces a reconnect received while an old-server build read is pending', async () => {
    vi.useFakeTimers(); const f = fixture(next);
    let finish!: (value: string) => void;
    f.read.mockImplementationOnce(() => new Promise<string>(resolve => { finish = resolve; }));
    f.owner.check(); await vi.advanceTimersByTimeAsync(0);
    f.owner.check(); f.owner.check(); finish(old); await vi.runAllTimersAsync();
    expect(f.read).toHaveBeenCalledTimes(2); expect(f.reload).toHaveBeenCalledOnce(); f.owner.stop();
  });
  it('blocks pending and uncertain actions retained across route and worker views', async () => {
    vi.useFakeTimers(); const f = fixture(next);
    const actions = new BrowserActionMemory(() => 'original_key');
    const root = { visibilityState: 'visible', querySelector: () => null, activeElement: null } as unknown as Document;
    const release = guardBrowserUpdate(() => actions.unresolved > 0);
    f.options.safe = () => browserUpdateSafe(root);
    let fail!: (error: unknown) => void;
    const pending = actions.run('worker/send', { body: 'Unsent private body' }, () => new Promise((_, reject) => { fail = reject; }), new AbortController().signal);
    await Promise.resolve(); f.owner.check(); await vi.runAllTimersAsync();
    expect(f.reload).not.toHaveBeenCalled();
    fail(new ApiError('connection_failed', 'Lost response', 0, true)); await pending.catch(() => {});
    f.owner.update(); expect(f.reload).not.toHaveBeenCalled();
    await actions.run('worker/send', { body: 'Must not replace' }, async () => ({}), new AbortController().signal);
    f.owner.reconsider(); await vi.runAllTimersAsync(); expect(f.reload).toHaveBeenCalledOnce();
    f.owner.stop(); release();
  });
  it('leaves an explicit control when tab storage is unavailable', async () => {
    vi.useFakeTimers(); const f = fixture(next);
    f.options.storage.setItem = () => { throw new Error('Storage denied'); };
    f.owner.check(); await vi.runAllTimersAsync(); expect(f.reload).not.toHaveBeenCalled();
    f.owner.update(); expect(f.reload).toHaveBeenCalledOnce(); f.owner.stop();
  });
  it('leaves sessions usable through transient failure and suppresses stopped/late reads', async () => {
    vi.useFakeTimers(); const f = fixture(next); f.read.mockRejectedValueOnce(new Error('503'));
    f.owner.check(); await vi.runAllTimersAsync(); expect(f.reload).not.toHaveBeenCalled();
    f.owner.check(); await vi.runAllTimersAsync(); expect(f.reload).toHaveBeenCalledOnce(); f.owner.stop();
    const late = fixture(next); late.owner.check(); late.owner.stop(); await vi.runAllTimersAsync(); expect(late.read).not.toHaveBeenCalled();
  });
  it('bounds a stalled request and recovers only on another lifecycle event', async () => {
    vi.useFakeTimers(); const f = fixture(next);
    f.read.mockImplementationOnce((signal: AbortSignal) => new Promise<string>((_, reject) => signal.addEventListener('abort', () => reject(new Error('timeout')))));
    f.owner.check(); await vi.advanceTimersByTimeAsync(5001);
    expect(f.reload).not.toHaveBeenCalled(); expect(f.read).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(60_000); expect(f.read).toHaveBeenCalledOnce();
    f.owner.check(); await vi.runAllTimersAsync(); expect(f.reload).toHaveBeenCalledOnce(); f.owner.stop();
  });
});
describe('public build response boundary', () => {
  it('makes one credential-free no-store read with an exact bounded public response', async () => {
    const fetcher = vi.fn(async () => new Response(JSON.stringify({ version: 1, assets: nextAssets }), { headers: { 'Content-Type': 'application/json' } })); vi.stubGlobal('fetch', fetcher);
    expect(await readBrowserIdentity(new AbortController().signal)).toBe(next);
    expect(fetcher).toHaveBeenCalledWith('/browser-build.json', expect.objectContaining({ credentials: 'omit', cache: 'no-store', redirect: 'error' }));
  });
  it.each([
    { version: 1, assets: ['/assets/nohash.js', oldAssets[1]] },
    { version: 1, assets: [...oldAssets, oldAssets[0]] },
    { version: 1, assets: ['https://other.test/index-OldBuild.js', oldAssets[1]] },
    { version: 2, assets: oldAssets }, { version: 1, assets: oldAssets, private: 'not allowed' },
  ])('refuses invalid manifests without navigating', async value => {
    vi.stubGlobal('fetch', async () => new Response(JSON.stringify(value), { headers: { 'Content-Type': 'application/json' } }));
    await expect(readBrowserIdentity(new AbortController().signal)).rejects.toThrow();
  });
  it('refuses HTML errors and oversized decoded payloads', async () => {
    for (const response of [new Response('<html>maintenance</html>'), new Response(' '.repeat(2049), { headers: { 'Content-Type': 'application/json' } })]) {
      vi.stubGlobal('fetch', async () => response); await expect(readBrowserIdentity(new AbortController().signal)).rejects.toThrow();
    }
  });
});
it('guards actual draft editors, hidden tabs and unresolved writes without treating navigation selectors as drafts', () => {
  let dirty = false, editing = false;
  const root = { visibilityState: 'visible', querySelector: () => dirty ? {} : null, activeElement: { matches: () => editing } } as unknown as Document;
  expect(browserUpdateSafe(root)).toBe(true); // A loaded controlled settings value is not itself dirty.
  dirty = true; expect(browserUpdateSafe(root)).toBe(false); dirty = false;
  editing = true; expect(browserUpdateSafe(root)).toBe(false); editing = false;
  const release = guardBrowserUpdate(() => true); expect(browserUpdateSafe(root)).toBe(false); release();
  expect(browserUpdateSafe(root)).toBe(true);
  Object.assign(root, { visibilityState: 'hidden' }); expect(browserUpdateSafe(root)).toBe(false);
});
