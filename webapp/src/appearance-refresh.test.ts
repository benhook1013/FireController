import { describe, expect, it, vi } from 'vitest';
import { createAppearanceAcceptor, refreshProjectView } from './appearance-refresh';
import type { Presentation, PresentationSnapshot } from './types';

function presentation(revision: number, project_id = 'project_one'): Presentation { return { project_id, revision, name: `Name ${revision}`, brand_name: 'Brand', tagline: '', headings: {}, theme: {}, updated_at: '' }; }
function snapshot(revision: number): PresentationSnapshot { return { presentation: presentation(revision), event_cursor: `cursor_${revision}` }; }
function deferred<T>() { let resolve!: (value: T) => void, reject!: (error: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

describe('shared project appearance acceptance', () => {
  it('keeps a newer manual observation when old metadata, equal revisions or foreign facts arrive later', () => {
    const publish = vi.fn();
    const accept = createAppearanceAcceptor('project_one', () => true, publish);
    expect(accept(presentation(2))).toBe(true);
    expect(accept(presentation(4))).toBe(true);
    expect(accept(presentation(3))).toBe(false);
    expect(accept({ ...presentation(4), name: 'Late equal revision' })).toBe(false);
    expect(accept(presentation(99, 'other_project'))).toBe(false);
    expect(accept(presentation(5))).toBe(true);
    expect(publish.mock.calls.map(([value]) => value.revision)).toEqual([2, 4, 5]);
  });
  it('retires the old connection and lets a new connection establish its own revision', () => {
    let current = true;
    const oldPublish = vi.fn(), newPublish = vi.fn();
    const old = createAppearanceAcceptor('project_one', () => current, oldPublish);
    old(presentation(9)); current = false;
    expect(old(presentation(10))).toBe(false);
    const next = createAppearanceAcceptor('project_one', () => true, newPublish);
    expect(next(presentation(0))).toBe(true);
    expect(oldPublish).toHaveBeenCalledTimes(1);
    expect(newPublish).toHaveBeenCalledWith(presentation(0));
  });
});

describe('explicit presentation and stored-data refresh', () => {
  it('starts both reads and stays pending after one rejects until the other settles', async () => {
    const read = deferred<PresentationSnapshot>(), queries = deferred<void>();
    const failure = new Error('unavailable'), accept = vi.fn(), ended = vi.fn();
    const readFn = vi.fn(() => read.promise), queryFn = vi.fn(() => queries.promise);
    const result = refreshProjectView(readFn, queryFn, () => true, accept).then(value => { ended(); return value; });
    await vi.waitFor(() => { expect(readFn).toHaveBeenCalledOnce(); expect(queryFn).toHaveBeenCalledOnce(); });
    read.reject(failure); await Promise.resolve(); await Promise.resolve();
    expect(ended).not.toHaveBeenCalled();
    queries.resolve(); expect(await result).toEqual([failure]);
    expect(accept).not.toHaveBeenCalled();
  });
  it('accepts the presentation when the stored-data refresh fails and still reports both failures', async () => {
    const queryFailure = new Error('query failed'), accept = vi.fn();
    expect(await refreshProjectView(() => Promise.resolve(snapshot(2)), () => Promise.reject(queryFailure), () => true, accept)).toEqual([queryFailure]);
    expect(accept).toHaveBeenCalledWith(presentation(2));
    const authFailure = new Error('terminal failure');
    expect(await refreshProjectView(() => Promise.reject(authFailure), () => Promise.reject(queryFailure), () => true, accept)).toEqual([authFailure, queryFailure]);
  });
  it('ignores a held presentation after its session is replaced while awaiting all reads', async () => {
    const read = deferred<PresentationSnapshot>(), queries = deferred<void>(), accept = vi.fn();
    let current = true;
    const result = refreshProjectView(() => read.promise, () => queries.promise, () => current, accept);
    current = false; read.resolve(snapshot(8)); queries.resolve();
    expect(await result).toEqual([]);
    expect(accept).not.toHaveBeenCalled();
  });
  it('lets the other read start even if a source throws synchronously', async () => {
    const failure = new Error('synchronous failure'), refresh = vi.fn().mockResolvedValue(undefined);
    expect(await refreshProjectView(() => { throw failure; }, refresh, () => true, vi.fn())).toEqual([failure]);
    expect(refresh).toHaveBeenCalledOnce();
  });
});
