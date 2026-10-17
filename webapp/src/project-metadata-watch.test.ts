import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import type { PresentationSnapshot, ProjectMetadataPage } from './types';

const project = { id: 'project_one', name: 'One', github_repository_id: 1, repository: 'owner/repo' };
const initial: PresentationSnapshot = { presentation: { project_id: project.id, revision: 0, name: project.name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' }, event_cursor: 'project_start' };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
const frame = (event: string, cursor: string, value: unknown) => new TextEncoder().encode(`event: ${event}\nid: ${cursor}\ndata: ${JSON.stringify(value)}\n\n`);
async function flush() { for (let i = 0; i < 80; i++) await Promise.resolve(); }
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); });

describe('independent project metadata transport', () => {
  it('keeps its worker-independent cursor/socket while an inbox watch changes workers', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    const streams: { path: string; worker: string | null; signal: AbortSignal; output: ReadableStreamDefaultController<Uint8Array> }[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input)); const headers = new Headers(init?.headers);
      if (url.pathname.endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (url.pathname.endsWith('/project_one')) return json(project);
      if (url.pathname.endsWith('/snapshot')) return json({ inbox: { items: [], has_more: false }, event_cursor: `inbox_${headers.get('x-fc-worker')}` });
      const signal = init!.signal as AbortSignal;
      const body = new ReadableStream<Uint8Array>({ start(output) {
        streams.push({ path: url.pathname, worker: headers.get('x-fc-worker'), signal, output });
        signal.addEventListener('abort', () => { try { output.close(); } catch { /* closed */ } }, { once: true });
        const cursor = url.searchParams.get('cursor')!;
        output.enqueue(frame(url.pathname.includes('project-events') ? 'project-events' : 'message-events', cursor, url.pathname.includes('project-events') ? { items: [], event_cursor: cursor, has_more: false } : { items: [], cursor, has_more: false, timed_out: false }));
      } });
      return new Response(body, { headers: { 'content-type': 'text/event-stream' } });
    }));
    const api = new Api('private-test-token', project.id); await api.connect();
    const metadata = new AbortController(), message = new AbortController();
    const received: string[] = [];
    const watching = api.watchProjectMetadata(initial, { snapshot() {}, events: page => { received.push(...page.items.map(item => item.event_cursor)); }, status() {} }, metadata.signal);
    const firstInbox = api.watchMessages('worker_one', {}, { snapshot() {}, events() {}, status() {} }, message.signal);
    await flush();
    const projectStream = streams.find(stream => stream.path.includes('project-events'))!;
    expect(projectStream.worker).toBeNull(); expect(streams).toHaveLength(2);
    const secondInbox = api.watchMessages('worker_two', {}, { snapshot() {}, events() {}, status() {} }, message.signal);
    await flush();
    expect(streams.find(stream => stream.worker === 'worker_one')?.signal.aborted).toBe(true);
    expect(projectStream.signal.aborted).toBe(false);
    expect(streams.filter(stream => stream.path.includes('project-events'))).toHaveLength(1);
    projectStream.output.enqueue(frame('project-events', 'project_changed', { items: [{ event_type: 'project.presentation_changed', resource_id: project.id, revision: 1, event_cursor: 'project_changed' }], event_cursor: 'project_changed', has_more: false }));
    await flush(); expect(received).toEqual(['project_changed']);
    metadata.abort(); message.abort(); await Promise.all([watching, firstInbox, secondInbox]); api.close();
    expect(streams.every(stream => stream.signal.aborted)).toBe(true);
  });

  it('resnapshots expired replay and reconnects only after applying the processed page', async () => {
    vi.useFakeTimers(); vi.stubGlobal('location', { origin: 'https://service.example' });
    const cursors: string[] = []; let snapshots = 0;
    const fresh = { ...initial, event_cursor: 'fresh_snapshot' };
    let release!: () => void;
    const applied = new Promise<void>(resolve => { release = resolve; });
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (url.pathname.endsWith('/project_one')) return json(project);
      if (url.pathname.endsWith('/presentation')) { snapshots++; return json(fresh); }
      cursors.push(url.searchParams.get('cursor')!);
      if (cursors.length === 1) return json({ error: { code: 'resync_required' } }, 409);
      const signal = init!.signal as AbortSignal;
      return new Response(new ReadableStream<Uint8Array>({ start(output) {
        if (cursors.length === 2) { output.enqueue(frame('project-events', 'processed', { items: [{ event_type: 'map.entry_updated', resource_id: 'entry_one', revision: 2, event_cursor: 'processed' }], event_cursor: 'processed', has_more: false })); output.close(); }
        else signal.addEventListener('abort', () => output.close(), { once: true });
      } }), { headers: { 'content-type': 'text/event-stream' } });
    }));
    const api = new Api('private-test-token', project.id); await api.connect();
    const stop = new AbortController(); const snapshotsApplied: string[] = [];
    const watch = api.watchProjectMetadata(initial, { snapshot: value => { snapshotsApplied.push(value.event_cursor); }, events: async (_page: ProjectMetadataPage) => { await applied; }, status() {} }, stop.signal);
    await flush(); expect(snapshots).toBe(1); expect(snapshotsApplied).toEqual(['project_start', 'fresh_snapshot']);
    await vi.advanceTimersByTimeAsync(1000); await flush(); expect(cursors).toEqual(['project_start', 'fresh_snapshot']);
    await vi.advanceTimersByTimeAsync(2000); expect(cursors).toHaveLength(2);
    release(); await flush(); await vi.advanceTimersByTimeAsync(1000); await flush();
    expect(cursors).toEqual(['project_start', 'fresh_snapshot', 'processed']);
    stop.abort(); await watch; api.close();
  });

  it('ignores a delayed snapshot callback after scope cancellation and closes on terminal access', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    let opens = 0;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path.endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (path.endsWith('/project_one')) return json(project);
      opens++; return json({ error: { code: 'forbidden' } }, 403);
    }));
    const api = new Api('private-test-token', project.id); await api.connect();
    const states: string[] = [];
    await expect(api.watchProjectMetadata(initial, { snapshot() {}, events() {}, status: value => states.push(value) }, new AbortController().signal)).rejects.toMatchObject({ code: 'forbidden' });
    expect(opens).toBe(1); expect(states.at(-1)).toBe('closed');
    const stop = new AbortController(); let applied = 0;
    const watch = api.watchProjectMetadata(initial, { snapshot: () => { applied++; }, events() {}, status() {} }, stop.signal);
    stop.abort(); await watch; expect(applied).toBe(0); expect(opens).toBe(1); api.close();
  });
});
