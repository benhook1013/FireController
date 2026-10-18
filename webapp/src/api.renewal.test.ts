import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import { clearBrowserSession, loadBrowserSession, renewedBrowserSessionBinding, saveBrowserSession, terminalBrowserSession, type BrowserSessionBinding } from './browser-session';
import type { PresentationSnapshot } from './types';

const project = { id: 'project_one', name: 'Project one', github_repository_id: 1, repository: 'owner/project' };
const selector = 'a'.repeat(32), clients: Api[] = [];
function json(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } }); }
function session() { return { session_id: selector, instance_id: 'instance_one', project_id: project.id, expires_at: new Date(Date.now() + 86400000).toISOString(), allow_writes: true, can_edit_presentation: true }; }
async function connected() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = Api.resume(project.id, selector, 'instance_one'); clients.push(api);
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session') ? json(session()) : json(project)));
  await api.connect(); return api;
}
async function flush() { for (let index = 0; index < 50; index++) await Promise.resolve(); }
afterEach(() => { for (const api of clients.splice(0)) api.close(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('finite existing-cookie renewal', () => {
  it('sends only the exact bound cookie request and coalesces overlapping renewals', async () => {
    const api = await connected(); let deliver!: (value: Response) => void;
    const fetchMock = vi.fn((_input: RequestInfo | URL, _init: RequestInit) => new Promise<Response>(resolve => { deliver = resolve; })); vi.stubGlobal('fetch', fetchMock);
    const pending = api.renewBrowserSession(); expect(api.renewBrowserSession()).toBe(pending); expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0], headers = new Headers(init.headers);
    expect(String(url)).toBe(`https://service.example/api/v1/projects/${project.id}/browser-session/renew`);
    expect(init).toMatchObject({ method: 'POST', body: '{}', credentials: 'same-origin', redirect: 'error', cache: 'no-store' });
    expect(headers.get('x-fc-browser-session')).toBe(selector); expect(headers.get('x-fc-instance')).toBe('instance_one'); expect(headers.get('content-type')).toBe('application/json');
    for (const key of ['authorization', 'origin', 'x-fc-worker', 'idempotency-key']) expect(headers.has(key)).toBe(false);
    const reply = { ...session(), can_edit_presentation: false }; deliver(json(reply)); expect(await pending).toEqual(reply);
    expect(api.browserCapabilities).toEqual({ allow_writes: true, can_edit_presentation: false }); expect(api.instanceId).toBe('instance_one');
  });

  it('validates all six response fields, expiry, strict shape and identity before changing capabilities', async () => {
    const api = await connected(); vi.useFakeTimers();
    for (const value of [{ ...session(), session_id: 'b'.repeat(32) }, { ...session(), project_id: 'project_other' }, { ...session(), instance_id: 'instance_other' }, { ...session(), expires_at: new Date(Date.now() - 1).toISOString() }, { ...session(), expires_at: '2026-02-30T12:00:00Z' }, { ...session(), expires_at: new Date(Date.now() + (24 * 60 + 5) * 60 * 1000 + 1).toISOString() }, { ...session(), allow_writes: null }, { ...session(), allow_writes: false, can_edit_presentation: true }, { ...session(), can_edit_presentation: undefined }, { ...session(), credential: 'PRIVATE_SENTINEL' }]) {
      vi.stubGlobal('fetch', vi.fn(async () => json(value))); await expect(api.renewBrowserSession()).rejects.toMatchObject({ code: 'invalid_response' });
      expect(api.browserCapabilities).toEqual({ allow_writes: true, can_edit_presentation: true });
    }
    for (const raw of ['{"session_id":"a","session_id":"b"}', 'x'.repeat(4097)]) {
      vi.stubGlobal('fetch', vi.fn(async () => new Response(raw, { headers: { 'content-type': 'application/json' } })));
      await expect(api.renewBrowserSession()).rejects.toBeDefined(); expect(api.browserCapabilities.can_edit_presentation).toBe(true);
    }
  });

  it('has a five-second bound and no automatic retry, preserving current capabilities on transient failures', async () => {
    const api = await connected(); vi.useFakeTimers();
    const stalled = vi.fn((_input: RequestInfo | URL, init: RequestInit) => new Promise<Response>((_resolve, reject) => { init.signal!.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true }); })); vi.stubGlobal('fetch', stalled);
    const pending = api.renewBrowserSession().catch(error => error); await vi.advanceTimersByTimeAsync(5000); expect(await pending).toMatchObject({ code: 'deadline_exceeded', outcomeUnknown: false }); expect(stalled).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(600000); expect(stalled).toHaveBeenCalledTimes(1); expect(vi.getTimerCount()).toBe(0);
    for (const [status, code] of [[503, 'overloaded'], [401, 'unauthorized'], [403, 'forbidden'], [409, 'instance_mismatch']] as const) {
      const fetchMock = vi.fn(async () => json({ error: { code, message: 'PRIVATE_SENTINEL' }, request_id: 'request_one' }, status)); vi.stubGlobal('fetch', fetchMock);
      const failure = await api.renewBrowserSession().catch(error => error); expect(failure).toMatchObject({ code, status, outcomeUnknown: false }); expect(failure.message).not.toContain('PRIVATE_SENTINEL'); expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(api.browserCapabilities).toEqual({ allow_writes: true, can_edit_presentation: true });
    }
  });

  it('does not dispatch a cancelled request or accept a late response after disconnect', async () => {
    const api = await connected(), controller = new AbortController(); controller.abort();
    const fetchMock = vi.fn(); vi.stubGlobal('fetch', fetchMock); await expect(api.renewBrowserSession(controller.signal)).rejects.toMatchObject({ code: 'cancelled' }); expect(fetchMock).not.toHaveBeenCalled();
    let deliver!: (value: Response) => void;
    vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(resolve => { deliver = resolve; })));
    const pending = api.renewBrowserSession().catch(error => error); api.close(); deliver(json(session()));
    expect(await pending).toMatchObject({ code: 'cancelled' }); expect(api.browserCapabilities).toEqual({ allow_writes: false, can_edit_presentation: false });
    await expect(api.renewBrowserSession()).rejects.toMatchObject({ code: 'cancelled' });
  });

  it('reopens both deliberately retired streams promptly without a false disconnect or snapshot reload', async () => {
    const api = await connected(); vi.useFakeTimers();
    const metadata: PresentationSnapshot = { presentation: { project_id: project.id, revision: 0, name: project.name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' }, event_cursor: 'project_initial' };
    const paths: string[] = [], cursors: string[] = [], streamSignals: AbortSignal[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit) => {
      const url = new URL(String(input)); paths.push(url.pathname);
      if (url.pathname.endsWith('/browser-session/renew')) return json(session());
      if (url.pathname.endsWith('/inbox/worker_one/snapshot')) return json({ inbox: { items: [], has_more: false }, event_cursor: 'message_initial' });
      if (url.pathname.endsWith('/stream')) {
        const message = url.pathname.includes('/message-events/'); cursors.push(url.searchParams.get('cursor')!); streamSignals.push(init.signal!);
        const page = message ? { items: [], cursor: 'message_processed', has_more: false, timed_out: false } : { items: [], event_cursor: 'project_processed', has_more: false };
        const cursor = message ? 'message_processed' : 'project_processed';
        const body = new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode(`event: ${message ? 'message-events' : 'project-events'}\nid: ${cursor}\ndata: ${JSON.stringify(page)}\n\n`)); init.signal!.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')), { once: true }); } });
        return new Response(body, { headers: { 'content-type': 'text/event-stream' } });
      }
      throw new Error(`unexpected API path ${url.pathname}`);
    }));
    const owner = new AbortController(), inboxSnapshots = vi.fn(), projectSnapshots = vi.fn(), inboxEvents = vi.fn(), projectEvents = vi.fn();
    const inboxStatus = vi.fn(), projectStatus = vi.fn();
    const messages = api.watchMessages('worker_one', {}, { snapshot: inboxSnapshots, events: inboxEvents, status: inboxStatus }, owner.signal);
    const projectWatch = api.watchProjectMetadata(metadata, { snapshot: projectSnapshots, events: projectEvents, status: projectStatus }, owner.signal);
    await flush(); expect(cursors).toHaveLength(2); expect(inboxEvents).toHaveBeenCalledTimes(1); expect(projectEvents).toHaveBeenCalledTimes(1);
    await api.renewBrowserSession(); await flush(); expect(streamSignals.slice(0, 2).every(signal => signal.aborted)).toBe(true); expect(owner.signal.aborted).toBe(false);
    expect(cursors).toHaveLength(4); expect(cursors.slice(2).sort()).toEqual(['message_processed', 'project_processed']);
    expect(inboxSnapshots).toHaveBeenCalledTimes(1); expect(projectSnapshots).toHaveBeenCalledTimes(1);
    for (const status of [inboxStatus, projectStatus]) {
      expect(status).not.toHaveBeenCalledWith('reconnecting');
      expect(status).not.toHaveBeenCalledWith('closed');
    }
    expect(paths.filter(path => path.endsWith('/inbox/worker_one/snapshot'))).toHaveLength(1); expect(paths.filter(path => !path.endsWith('/stream') && !path.endsWith('/inbox/worker_one/snapshot'))).toEqual([`/api/v1/projects/${project.id}/browser-session/renew`]);
    owner.abort(); await Promise.all([messages, projectWatch]); expect(vi.getTimerCount()).toBe(0);
  });

  it('reopens an in-flight stream request after renewal but closes cleanly on owner abort', async () => {
    const api = await connected(); vi.useFakeTimers();
    const metadata: PresentationSnapshot = { presentation: { project_id: project.id, revision: 0, name: project.name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' }, event_cursor: 'opening_cursor' };
    let firstSignal: AbortSignal | undefined, opens = 0;
    const cursors: string[] = [], snapshots = vi.fn(), events = vi.fn(), status = vi.fn();
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith('/browser-session/renew')) return json(session());
      if (!url.pathname.endsWith('/stream')) throw new Error(`unexpected API path ${url.pathname}`);
      opens++; cursors.push(url.searchParams.get('cursor')!);
      if (opens === 1) return new Promise<Response>((_resolve, reject) => {
        firstSignal = init.signal!;
        init.signal!.addEventListener('abort', () => reject(init.signal!.reason), { once: true });
      });
      const page = { items: [], event_cursor: 'renewed_cursor', has_more: false };
      const body = new ReadableStream<Uint8Array>({ start(controller) {
        controller.enqueue(new TextEncoder().encode(`event: project-events\nid: renewed_cursor\ndata: ${JSON.stringify(page)}\n\n`));
        init.signal!.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')), { once: true });
      } });
      return new Response(body, { headers: { 'content-type': 'text/event-stream' } });
    }));
    const owner = new AbortController();
    const watch = api.watchProjectMetadata(metadata, { snapshot: snapshots, events, status }, owner.signal);
    await flush(); expect(opens).toBe(1);
    await api.renewBrowserSession(); await flush();
    expect(firstSignal?.aborted).toBe(true); expect(opens).toBe(2); expect(cursors).toEqual(['opening_cursor', 'opening_cursor']);
    expect(snapshots).toHaveBeenCalledTimes(1); expect(events).toHaveBeenCalledTimes(1);
    expect(status).not.toHaveBeenCalledWith('reconnecting'); expect(status).not.toHaveBeenCalledWith('closed');
    owner.abort(); await watch; await flush();
    expect(opens).toBe(2); expect(vi.getTimerCount()).toBe(0);
  });

  it('keeps a real idle close on the ordinary reconnect backoff', async () => {
    const api = await connected(); vi.useFakeTimers();
    const metadata: PresentationSnapshot = { presentation: { project_id: project.id, revision: 0, name: project.name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' }, event_cursor: 'idle_cursor' };
    let opens = 0;
    const cursors: string[] = [], snapshots = vi.fn(), events = vi.fn(), status = vi.fn();
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit) => {
      const url = new URL(String(input));
      if (!url.pathname.endsWith('/stream')) throw new Error(`unexpected API path ${url.pathname}`);
      opens++; cursors.push(url.searchParams.get('cursor')!);
      const body = new ReadableStream<Uint8Array>({ start(controller) {
        init.signal!.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')), { once: true });
      } });
      return new Response(body, { headers: { 'content-type': 'text/event-stream' } });
    }));
    const owner = new AbortController();
    const watch = api.watchProjectMetadata(metadata, { snapshot: snapshots, events, status }, owner.signal);
    await flush(); expect(opens).toBe(1); expect(status).toHaveBeenLastCalledWith('live');
    await vi.advanceTimersByTimeAsync(45_000); await flush();
    expect(status).toHaveBeenLastCalledWith('reconnecting'); expect(opens).toBe(1);
    await vi.advanceTimersByTimeAsync(999); await flush(); expect(opens).toBe(1);
    await vi.advanceTimersByTimeAsync(1); await flush();
    expect(opens).toBe(2); expect(cursors).toEqual(['idle_cursor', 'idle_cursor']); expect(snapshots).toHaveBeenCalledTimes(1);
    owner.abort(); await watch; expect(events).not.toHaveBeenCalled(); expect(vi.getTimerCount()).toBe(0);
  });

  it.each(['headers401', 'headers403', 'failurebody', 'errorframe'])('ignores retired %s but still rejects a current stream revocation', async mode => {
    const api = await connected(); vi.useFakeTimers();
    const metadata: PresentationSnapshot = { presentation: { project_id: project.id, revision: 0, name: project.name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' }, event_cursor: 'retained_cursor' };
    let releaseHeaders: ((value: Response) => void) | undefined;
    let held: ReadableStreamDefaultController<Uint8Array> | undefined;
    let currentStream: ReadableStreamDefaultController<Uint8Array> | undefined;
    let opens = 0;
    const cursors: string[] = [], snapshots = vi.fn(), events = vi.fn(), status = vi.fn();
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith('/browser-session/renew')) return json(session());
      if (!url.pathname.endsWith('/stream')) throw new Error('unexpected snapshot/archive/action');
      opens++; cursors.push(url.searchParams.get('cursor')!);
      if (opens === 3) return mode === 'errorframe' ? new Response('event: error\ndata: {"error":{"code":"unauthorized"}}\n\n', { headers: { 'content-type': 'text/event-stream' } }) : json({ error: { code: 'unauthorized', message: 'private revoked' } }, 401);
      if (opens === 2) return new Response(new ReadableStream<Uint8Array>({ start(controller) { currentStream = controller; } }), { headers: { 'content-type': 'text/event-stream' } });
      if (mode.startsWith('headers') || mode === 'errorframe') return new Promise<Response>(resolve => { releaseHeaders = resolve; });
      const body = new ReadableStream<Uint8Array>({ start(controller) { held = controller; } });
      return new Response(body, { status: 401, headers: { 'content-type': 'application/json' } });
    }));
    const owner = new AbortController();
    const outcome = api.watchProjectMetadata(metadata, { snapshot: snapshots, events, status }, owner.signal).then(() => undefined, error => error);
    await flush(); expect(opens).toBe(1);
    await api.renewBrowserSession();
    if (releaseHeaders) releaseHeaders(mode === 'errorframe'
      ? new Response('event: error\ndata: {"error":{"code":"unauthorized"}}\n\n', { headers: { 'content-type': 'text/event-stream' } })
      : json({ error: { code: mode === 'headers403' ? 'forbidden' : 'unauthorized' } }, mode === 'headers403' ? 403 : 401));
    if (mode === 'failurebody') { held!.enqueue(new TextEncoder().encode(JSON.stringify({ error: { code: 'unauthorized' } }))); held!.close(); }
    await flush(); expect(status).not.toHaveBeenCalledWith('closed'); expect(events).not.toHaveBeenCalled(); expect(opens).toBe(2);
    expect(cursors).toEqual(['retained_cursor', 'retained_cursor']);
    currentStream!.close(); await flush(); await vi.advanceTimersByTimeAsync(1000); await flush();
    expect(await outcome).toMatchObject({ code: 'unauthorized', status: mode === 'errorframe' ? 0 : 401 });
    expect(cursors).toEqual(['retained_cursor', 'retained_cursor', 'retained_cursor']); expect(snapshots).toHaveBeenCalledTimes(1); expect(status).toHaveBeenLastCalledWith('closed'); owner.abort();
    expect(vi.getTimerCount()).toBe(0);
  });


  it.each([false, true])('checks the actual cookie after a lost renewal body and past recorded expiry (denied=%s)', async denied => {
    vi.useFakeTimers(); const initial = Date.now();
    const values = new Map<string, string>();
    const storage = { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); }, removeItem: (key: string) => { values.delete(key); } };
    const binding: BrowserSessionBinding = { version: 1, projectID: project.id, instanceID: 'instance_one', sessionID: selector, selectedworkerID: 'worker_one', expiresAt: new Date(initial + 60000).toISOString() };
    expect(saveBrowserSession(binding, storage)).toBe(true);
    const original = await connected(); let cookieExpiry = '';
    vi.stubGlobal('fetch', vi.fn(async () => {
      // Set-Cookie has been processed, but the finite response body is lost.
      cookieExpiry = new Date(Date.now() + 86400000).toISOString();
      return new Response(new ReadableStream({ start(controller) { controller.error(new TypeError('body lost after cookie')); } }), { headers: { 'content-type': 'application/json' } });
    }));
    await expect(original.renewBrowserSession()).rejects.toMatchObject({ code: 'connection_failed' }); original.close();
    vi.setSystemTime(initial + 120000);
    const past = loadBrowserSession(project.id, undefined, storage)!; expect(past).toEqual(binding);
    const resumed = Api.resume(past.projectID, past.sessionID, past.instanceID); clients.push(resumed); const paths: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit) => {
      paths.push(new URL(String(input)).pathname); expect(init.method).toBe('GET');
      if (String(input).endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (String(input).endsWith('/browser-session')) return denied ? json({ error: { code: 'unauthorized' } }, 401) : json({ ...session(), expires_at: cookieExpiry, can_edit_presentation: false });
      return json(project);
    }));
    if (denied) {
      const failure = await resumed.connect().catch(error => error); expect(failure).toMatchObject({ code: 'unauthorized', status: 401 });
      if (terminalBrowserSession(failure)) clearBrowserSession(storage);
      expect(loadBrowserSession(project.id, undefined, storage)).toBeNull(); expect(resumed.browserSession).toBeUndefined(); expect(resumed.instanceId).toBeUndefined();
    } else {
      await resumed.connect(); const authoritative = resumed.browserSession!;
      const recovered = renewedBrowserSessionBinding(past, authoritative)!; expect(recovered.expiresAt).toBe(cookieExpiry); expect(recovered.selectedworkerID).toBe('worker_one');
      expect(saveBrowserSession(recovered, storage)).toBe(true); expect(loadBrowserSession(project.id, undefined, storage)).toEqual(recovered);
      authoritative.expires_at = 'private mutation'; authoritative.allow_writes = false;
      expect(resumed.browserSession?.expires_at).toBe(cookieExpiry); expect(resumed.browserCapabilities).toEqual({ allow_writes: true, can_edit_presentation: false });
      expect(JSON.stringify([...values.values()])).not.toContain('allow_writes');
    }
    expect(paths).toEqual(['/api/v1/instance', `/api/v1/projects/${project.id}`, `/api/v1/projects/${project.id}/browser-session`]);
  });


  it.each([false, true])('reopens uncertain renewal using the actual cookie (Set-Cookie committed=%s)', async committed => {
    const api = await connected(); vi.useFakeTimers();
    const metadata: PresentationSnapshot = { presentation: { project_id: project.id, revision: 0, name: project.name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' }, event_cursor: 'preserved_cursor' };
    let release!: (value: Response) => void, oldSignal: AbortSignal | undefined, cookieRenewed = false, opens = 0, renewals = 0;
    const cursors: string[] = [], events = vi.fn(), snapshots = vi.fn(), status = vi.fn();
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith('/browser-session/renew')) {
        renewals++;
        if (!committed) throw new TypeError('connection lost before a response');
        cookieRenewed = true;
        return new Response(new ReadableStream({ start(controller) { controller.error(new TypeError('body lost after Set-Cookie')); } }), { headers: { 'content-type': 'application/json' } });
      }
      if (!url.pathname.endsWith('/stream')) throw new Error('unexpected snapshot/archive/action');
      cursors.push(url.searchParams.get('cursor')!); opens++;
      if (opens === 1) { oldSignal = init.signal!; return new Promise<Response>(resolve => { release = resolve; }); }
      if (!cookieRenewed) return json({ error: { code: 'unauthorized' } }, 401);
      const page = { items: [], event_cursor: 'new_cookie_cursor', has_more: false };
      return new Response(new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode(`event: project-events\nid: new_cookie_cursor\ndata: ${JSON.stringify(page)}\n\n`)); init.signal!.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')), { once: true }); } }), { headers: { 'content-type': 'text/event-stream' } });
    }));
    const owner = new AbortController();
    const outcome = api.watchProjectMetadata(metadata, { snapshot: snapshots, events, status }, owner.signal).then(() => undefined, error => error);
    await flush(); expect(opens).toBe(1);
    const cancelled = new AbortController(); cancelled.abort(); await expect(api.renewBrowserSession(cancelled.signal)).rejects.toMatchObject({ code: 'cancelled' });
    expect(renewals).toBe(0); expect(oldSignal?.aborted).toBe(false);
    await expect(api.renewBrowserSession()).rejects.toMatchObject({ code: 'connection_failed' }); expect(oldSignal?.aborted).toBe(true);
    release(json({ error: { code: 'unauthorized' } }, 401)); await flush(); expect(cursors).toEqual(['preserved_cursor', 'preserved_cursor']); expect(snapshots).toHaveBeenCalledTimes(1); expect(renewals).toBe(1);
    if (committed) { expect(status).not.toHaveBeenCalledWith('closed'); expect(status).toHaveBeenLastCalledWith('live'); expect(events).toHaveBeenCalledTimes(1); owner.abort(); expect(await outcome).toBeUndefined(); }
    else { expect(await outcome).toMatchObject({ code: 'unauthorized', status: 401 }); expect(events).not.toHaveBeenCalled(); expect(status).toHaveBeenLastCalledWith('closed'); owner.abort(); }
    expect(vi.getTimerCount()).toBe(0);
  });

});
