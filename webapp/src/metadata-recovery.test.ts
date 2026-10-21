import { QueryClient, QueryObserver } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import { startMetadataAttempt } from './metadata-recovery';
import { refreshProjectMetadata } from './project-metadata';
import type { EventWatchCallbacks } from './events';
import type { LiveStatus, PresentationSnapshot, ProjectMetadataPage } from './types';

const project = { id: 'project_one', name: 'One', github_repository_id: 1, repository: 'owner/repo' };
const initial: PresentationSnapshot = { presentation: { project_id: project.id, revision: 0, name: project.name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' }, event_cursor: 'initial_cursor' };
const fresh: PresentationSnapshot = { presentation: { ...initial.presentation, revision: 1, brand_name: 'Fresh brand', updated_at: '2026-10-18T00:00:00Z' }, event_cursor: 'fresh_cursor' };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
async function flush() { for (let i = 0; i < 100; i++) await Promise.resolve(); }
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('project metadata recovery owner', () => {
  it('recovers a closed transport only on explicit retry, from a fresh snapshot after refreshing stored queries', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
    const key = ['fc', 'worker_scope', 'instance_one', project.id, 'worker_one'];
    let mapReads = 0, presentationReads = 0, opens = 0;
    const timeline: string[] = [];
    let streamSignal: AbortSignal | undefined, output: ReadableStreamDefaultController<Uint8Array> | undefined;
    let snapshot = fresh;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (url.pathname.endsWith('/project_one')) return json(project);
      if (url.pathname.endsWith('/presentation')) { presentationReads++; timeline.push('snapshot'); return json(snapshot); }
      expect(url.pathname).toMatch(/\/project-events\/stream$/u);
      expect(new Headers(init?.headers).get('x-fc-worker')).toBeNull();
      opens++; timeline.push(`stream:${url.searchParams.get('cursor')}`);
      if (opens === 1) return json({ error: { code: 'invalid_response' } }, 400);
      streamSignal = init!.signal as AbortSignal;
      return new Response(new ReadableStream<Uint8Array>({ start(controller) {
        output = controller;
        streamSignal!.addEventListener('abort', () => { try { controller.close(); } catch { /* closed */ } }, { once: true });
      } }), { headers: { 'content-type': 'text/event-stream' } });
    }));
    client.setQueryData([...key, 'map'], 'old map');
    client.setQueryData([...key, 'map-history', 'inactive'], 'old history');
    client.setQueryData([...key, 'reviews'], 'unchanged reviews');
    const observer = new QueryObserver(client, { queryKey: [...key, 'map'], queryFn: async () => { timeline.push('query'); return `map ${++mapReads}`; } });
    const unsubscribe = observer.subscribe(() => {});
    const api = new Api('synthetic-token', project.id); await api.connect();
    const lifetime = new AbortController();
    const states: LiveStatus[] = [], failures: unknown[] = [], appearances: string[] = [];
    const owner = { lifetime: lifetime.signal, current: () => true, refresh: (page: ProjectMetadataPage | undefined, signal: AbortSignal) => refreshProjectMetadata(client, key, page, signal), status: (value: LiveStatus) => states.push(value), appearance: (value: typeof fresh.presentation) => appearances.push(value.brand_name), failure: (error: unknown) => failures.push(error) };
    const first = startMetadataAttempt(api, initial, { ...owner, retry: false });
    let retry: ReturnType<typeof startMetadataAttempt> | undefined;
    try {
      await first.done;
      expect(states.at(-1)).toBe('closed'); expect(failures).toHaveLength(1);
      expect(presentationReads).toBe(0); expect(mapReads).toBe(0); expect(opens).toBe(1);
      await flush(); expect(opens).toBe(1); // No owner-level automatic retry.
      first.cancel(); timeline.length = 0;
      retry = startMetadataAttempt(api, initial, { ...owner, retry: true });
      await flush();
      expect(timeline).toEqual(['snapshot', 'query', 'stream:fresh_cursor']);
      expect(states.at(-1)).toBe('live'); expect(appearances.at(-1)).toBe('Fresh brand');
      expect(client.getQueryData([...key, 'map'])).toBe('map 1');
      expect(client.getQueryState([...key, 'map-history', 'inactive'])?.isInvalidated).toBe(true);
      expect(client.getQueryState([...key, 'reviews'])?.isInvalidated).toBe(false);
      expect(opens).toBe(2); expect(streamSignal?.aborted).toBe(false);
      snapshot = { presentation: { ...fresh.presentation, revision: 2, brand_name: 'Live brand' }, event_cursor: 'live_cursor' };
      output!.enqueue(new TextEncoder().encode(`event: project-events\nid: live_cursor\ndata: ${JSON.stringify({ items: [{ event_type: 'project.presentation_changed', resource_id: project.id, revision: 2, event_cursor: 'brand_cursor' }, { event_type: 'map.entry_updated', resource_id: 'entry_one', revision: 2, event_cursor: 'live_cursor' }], event_cursor: 'live_cursor', has_more: false })}\n\n`));
      await flush();
      expect(presentationReads).toBe(2); expect(appearances.at(-1)).toBe('Live brand'); expect(mapReads).toBe(2);
    } finally { first.cancel(); retry?.cancel(); lifetime.abort(); await Promise.all([first.done, retry?.done]); unsubscribe(); client.clear(); api.close(); }
    expect(streamSignal?.aborted).toBe(true);
  });

  it('keeps a failed fresh read closed and retryable without opening a stale watermark', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    let reads = 0, opens = 0;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path.endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (path.endsWith('/project_one')) return json(project);
      if (path.endsWith('/presentation')) { reads++; return json({ error: { code: 'storage_unavailable' } }, 503); }
      opens++; return json({}, 400);
    }));
    const api = new Api('synthetic-token', project.id); await api.connect();
    const lifetime = new AbortController(), status: LiveStatus[] = [], failures: unknown[] = [];
    try {
      for (let i = 0; i < 2; i++) {
        const attempt = startMetadataAttempt(api, initial, { retry: true, lifetime: lifetime.signal, current: () => true, refresh: vi.fn(), appearance: vi.fn(), status: value => status.push(value), failure: value => failures.push(value) });
        await attempt.done; attempt.cancel();
        expect(status.at(-1)).toBe('closed'); expect(reads).toBe(i + 1); expect(opens).toBe(0);
      }
      expect(failures).toHaveLength(2); await flush(); expect(reads).toBe(2);
    } finally { lifetime.abort(); api.close(); }
  });

  it('aborts owned finite reads and ignores an old retry snapshot after a project changes', async () => {
    let release!: (value: PresentationSnapshot) => void;
    let oldSignal: AbortSignal | undefined;
    let currentProject = project.id;
    const oldRefresh = vi.fn(), oldAppearance = vi.fn(), oldStatus = vi.fn(), oldFailure = vi.fn(), oldWatch = vi.fn();
    const source = { presentation: (signal?: AbortSignal) => { oldSignal = signal; return new Promise<PresentationSnapshot>(resolve => { release = resolve; }); }, watchProjectMetadata: oldWatch };
    const lifetime = new AbortController();
    const old = startMetadataAttempt(source, initial, { retry: true, lifetime: lifetime.signal, current: () => currentProject === project.id, refresh: oldRefresh, appearance: oldAppearance, status: oldStatus, failure: oldFailure });
    await flush(); expect(oldSignal?.aborted).toBe(false);
    old.cancel(); currentProject = 'project_two';
    const newAppearance = vi.fn(), newStatus = vi.fn();
    const next = { ...fresh, presentation: { ...fresh.presentation, project_id: currentProject, brand_name: 'Other project' } };
    const sourceTwo = { presentation: vi.fn(), watchProjectMetadata: async (value: PresentationSnapshot, callbacks: EventWatchCallbacks<PresentationSnapshot, ProjectMetadataPage>, signal: AbortSignal) => {
      await callbacks.snapshot(value); callbacks.status('live');
      await new Promise<void>(resolve => signal.addEventListener('abort', () => resolve(), { once: true }));
    } };
    const latest = startMetadataAttempt(sourceTwo, next, { retry: false, lifetime: new AbortController().signal, current: () => currentProject === 'project_two', refresh: vi.fn(), appearance: newAppearance, status: newStatus, failure: vi.fn() });
    await flush(); release(fresh); await old.done;
    expect(oldSignal?.aborted).toBe(true); expect(oldWatch).not.toHaveBeenCalled(); expect(oldRefresh).not.toHaveBeenCalled(); expect(oldAppearance).not.toHaveBeenCalled(); expect(oldFailure).not.toHaveBeenCalled(); expect(oldStatus.mock.calls).toEqual([['connecting']]);
    expect(newAppearance).toHaveBeenCalledWith(next.presentation); expect(newStatus).toHaveBeenLastCalledWith('live');
    latest.cancel(); await latest.done;
  });

  it('cancels a pending event presentation read and suppresses late callbacks from a superseded attempt', async () => {
    let callbacks!: EventWatchCallbacks<PresentationSnapshot, ProjectMetadataPage>;
    let release!: (value: PresentationSnapshot) => void;
    let finiteSignal: AbortSignal | undefined, watchSignal: AbortSignal | undefined;
    const source = { presentation: (signal?: AbortSignal) => { finiteSignal = signal; return new Promise<PresentationSnapshot>(resolve => { release = resolve; }); }, watchProjectMetadata: async (_initial: PresentationSnapshot, selected: EventWatchCallbacks<PresentationSnapshot, ProjectMetadataPage>, signal: AbortSignal) => {
      callbacks = selected; watchSignal = signal; await selected.snapshot(initial); selected.status('live');
      await new Promise<void>(resolve => signal.addEventListener('abort', () => resolve(), { once: true }));
    } };
    const refresh = vi.fn(), appearance = vi.fn(), status = vi.fn(), failure = vi.fn();
    const lifetime = new AbortController();
    const attempt = startMetadataAttempt(source, initial, { retry: false, lifetime: lifetime.signal, current: () => true, refresh, appearance, status, failure });
    await flush();
    const event = callbacks.events({ items: [{ event_type: 'project.presentation_changed', resource_id: project.id, revision: 1, event_cursor: 'late_cursor' }], event_cursor: 'late_cursor', has_more: false });
    await flush(); lifetime.abort();
    expect(finiteSignal?.aborted).toBe(true); expect(watchSignal?.aborted).toBe(true);
    release(fresh); await event; await attempt.done;
    await callbacks.snapshot(fresh); callbacks.status('closed');
    expect(refresh).not.toHaveBeenCalled(); expect(appearance.mock.calls).toEqual([[initial.presentation]]); expect(status).toHaveBeenLastCalledWith('live'); expect(failure).not.toHaveBeenCalled();
  });
});
