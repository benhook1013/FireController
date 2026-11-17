import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api, ApiError, type BrowserPresentationConfig } from './api';

const project = { id: 'project_one', name: 'Project one', github_repository_id: 1, repository: 'owner/project' };
const selector = 'a'.repeat(32);
const config: BrowserPresentationConfig = { name: 'Updated project', brand_name: 'Project brand', tagline: 'Project tagline', headings: {}, theme: {} };
const clients: Api[] = [];
function json(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } }); }
function session(sessionID: string, writes = true, presentation = true) { return { session_id: sessionID, instance_id: 'instance_one', project_id: project.id, expires_at: new Date(Date.now() + 3600000).toISOString(), allow_writes: writes, can_edit_presentation: presentation }; }
function refusal(status: number, code = 'forbidden') { return json({ error: { code, message: 'PRIVATE_REFUSAL_SENTINEL' }, request_id: 'request_test' }, status); }
async function connected(writes = true, presentation = true): Promise<Api> {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input).endsWith('/instance')) return json({ instance_id: 'instance_one' });
    if (String(input).endsWith('/browser-session')) { const body = JSON.parse(String(init?.body)); expect(Object.keys(body)).toEqual(['session_id']); return json(session(body.session_id, writes, presentation)); }
    return json(project);
  }));
  const api = new Api('private-test-token', project.id); clients.push(api); await api.connect(); await api.startBrowserSession(); return api;
}
afterEach(() => { for (const api of clients.splice(0)) api.close(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('verified browser action capabilities', () => {
  it.each([[false, false], [true, false], [true, true]])('uses the service-authorized capabilities without a login mode (%s, %s)', async (writes, presentation) => {
    const api = await connected(writes, presentation);
    expect(api.browserCapabilities).toEqual({ allow_writes: writes, can_edit_presentation: presentation });
  });
  it('resumes with three current cookie reads and no saved permission flags', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    const api = Api.resume(project.id, selector, 'instance_one'); clients.push(api);
    expect(api.browserCapabilities).toEqual({ allow_writes: false, can_edit_presentation: false });
    const paths: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit) => {
      paths.push(new URL(String(input)).pathname); expect(init.method).toBe('GET'); expect(init.credentials).toBe('same-origin'); expect(new Headers(init.headers).has('authorization')).toBe(false);
      return String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session') ? json(session(selector, true, false)) : json(project);
    }));
    await api.connect(); expect(paths).toEqual(['/api/v1/instance', `/api/v1/projects/${project.id}`, `/api/v1/projects/${project.id}/browser-session`]);
    expect(api.browserCapabilities).toEqual({ allow_writes: true, can_edit_presentation: false });
    const copied = api.browserCapabilities; copied.can_edit_presentation = true; expect(api.browserCapabilities.can_edit_presentation).toBe(false);
    api.close(); expect(api.browserCapabilities.allow_writes).toBe(false);
  });
  it('keeps a read-only connection attenuated and rejects malformed capability responses', async () => {
    const api = await connected(false, false); const fetchMock = vi.fn(); vi.stubGlobal('fetch', fetchMock);
    await expect(api.sendMessage('worker_one', 'worker_two', 'Message', '', 'send-key')).rejects.toMatchObject({ code: 'forbidden', outcomeUnknown: false }); expect(fetchMock).not.toHaveBeenCalled();
    expect(api.instanceId).toBe('instance_one');
    for (const value of [{ ...session(selector), allow_writes: null }, { ...session(selector, false, true) }, { ...session(selector), project_id: 'foreign' }, { ...session(selector), can_edit_presentation: undefined }]) {
      const restored = Api.resume(project.id, selector, 'instance_one'); clients.push(restored);
      vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session') ? json(value) : json(project)));
      await expect(restored.connect()).rejects.toMatchObject({ code: 'invalid_response' }); expect(restored.instanceId).toBeUndefined(); expect(restored.browserCapabilities.allow_writes).toBe(false);
    }
  });
});

describe('finite explicit cookie actions', () => {
  it('binds worker, project, session, instance and original key without bearer or spoofed Origin', async () => {
    const api = await connected(); let writes = 0;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init: RequestInit) => {
      writes++; const url = new URL(String(input)), headers = new Headers(init.headers);
      expect(url.origin).toBe('https://service.example'); expect(url.pathname.startsWith(`/api/v1/projects/${project.id}/`)).toBe(true); expect(init.credentials).toBe('same-origin'); expect(init.redirect).toBe('error'); expect(init.cache).toBe('no-store');
      expect(headers.get('x-fc-worker')).toBe('worker_one'); expect(headers.get('x-fc-instance')).toBe('instance_one'); expect(headers.get('x-fc-browser-session')).toMatch(/^[a-f0-9]{32}$/u); expect(headers.has('authorization')).toBe(false); expect(headers.has('origin')).toBe(false); expect(headers.get('idempotency-key')).toBe(`key-${writes}`);
      const body = JSON.parse(String(init.body));
      if (url.pathname.endsWith('/presentation')) { expect(init.method).toBe('PUT'); expect(body).toEqual({ expected_revision: 3, presentation: config }); return json({ project_id: project.id, revision: 4, outcome: 'updated' }); }
      expect(init.method).toBe('POST');
      if (url.pathname.endsWith('/messages')) { expect(body).toEqual({ recipient_worker_id: 'worker_two', body: 'Exact\nmessage', reply_to_message_id: 'msg_parent' }); return json({ id: 'msg_new', conversation_id: 'conv_one', outcome: 'sent' }, 201); }
      expect(body).toEqual({}); return json({ id: 'msg_one', conversation_id: 'conv_one', outcome: url.pathname.endsWith('/ack') ? 'acknowledged' : 'seen' });
    }));
    expect((await api.sendMessage('worker_one', 'worker_two', 'Exact\nmessage', 'msg_parent', 'key-1')).outcome).toBe('sent');
    await api.markMessageSeen('worker_one', 'msg_one', 'key-2'); await api.acknowledgeMessage('worker_one', 'msg_one', 'key-3'); await api.replacePresentation('worker_one', config, 3, 'key-4'); expect(writes).toBe(4);
  });
  it('rejects invalid action arguments before connecting or fetching', async () => {
    const api = new Api('private-test-token', project.id); clients.push(api); const fetchMock = vi.fn(); vi.stubGlobal('fetch', fetchMock);
    for (const invoke of [() => api.sendMessage('../worker', 'worker_two', 'body', '', 'key'), () => api.sendMessage('worker_one', 'worker_two', ' '.repeat(2), '', 'key'), () => api.sendMessage('worker_one', 'worker_two', 'x'.repeat((16 << 10) + 1), '', 'key'), () => api.sendMessage('worker_one', 'worker_two', 'body', '', ' key'), () => api.markMessageSeen('worker_one', '../message', 'key'), () => api.replacePresentation('worker_one', { ...config, theme: { surface_text: '#f7f6f3' } }, 1, 'key'), () => api.replacePresentation('worker_one', config, -1, 'key')]) await expect(invoke()).rejects.toMatchObject({ code: 'invalid_request', outcomeUnknown: false });
    expect(fetchMock).not.toHaveBeenCalled();
  });
  it('preserves readable sessions after permission refusal and revision conflict', async () => {
    const api = await connected(); let count = 0;
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit) => { count++; if (init.method === 'GET') return json({ items: [], has_more: false }); return refusal(count === 1 ? 403 : 409, count === 1 ? 'forbidden' : 'project_presentation_revision_conflict'); }));
    await expect(api.markMessageSeen('worker_one', 'msg_one', 'seen-key')).rejects.toMatchObject({ code: 'forbidden', status: 403, outcomeUnknown: false });
    await expect(api.replacePresentation('worker_one', config, 1, 'presentation-key')).rejects.toMatchObject({ code: 'project_presentation_revision_conflict', status: 409, outcomeUnknown: false });
    expect((await api.workers()).items).toEqual([]); expect(api.instanceId).toBe('instance_one'); expect(api.browserCapabilities.allow_writes).toBe(true);
  });
  it('returns unknown after a lost result and only retries when the caller repeats the same key/body', async () => {
    const api = await connected(); const requests: RequestInit[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init: RequestInit) => { requests.push(init); if (requests.length === 1) throw new TypeError('lost committed response'); return json({ id: 'msg_new', conversation_id: 'conv_one', outcome: 'sent' }, 201); }));
    await expect(api.sendMessage('worker_one', 'worker_two', 'Original body', '', 'original-key')).rejects.toMatchObject({ code: 'connection_failed', outcomeUnknown: true }); expect(requests).toHaveLength(1);
    await api.sendMessage('worker_one', 'worker_two', 'Original body', '', 'original-key'); expect(requests).toHaveLength(2); expect(requests[0].body).toBe(requests[1].body); expect(new Headers(requests[0].headers).get('idempotency-key')).toBe(new Headers(requests[1].headers).get('idempotency-key'));
  });
  it('distinguishes pre-dispatch cancellation from a received action', async () => {
    for (const received of [false, true]) {
      const api = await connected(), controller = new AbortController(); let calls = 0, arrived!: () => void; const started = new Promise<void>(resolve => { arrived = resolve; });
      vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init: RequestInit) => new Promise<Response>((_resolve, reject) => { calls++; arrived(); init.signal!.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true }); })));
      if (!received) controller.abort(); const pending = api.sendMessage('worker_one', 'worker_two', 'Original body', '', 'original-key', controller.signal).catch(error => error);
      if (received) { await started; controller.abort(); }
      const error = await pending; expect(error).toBeInstanceOf(ApiError); expect(error).toMatchObject({ code: 'cancelled', outcomeUnknown: received }); expect(calls).toBe(received ? 1 : 0);
    }
  });
  it('refuses wrong, duplicate or oversized accepted receipts without claiming an unsent action', async () => {
    const api = await connected();
    for (const body of [JSON.stringify({ id: 'msg_wrong', conversation_id: 'conv_one', outcome: 'seen' }), '{"id":"msg_one","id":"msg_one","conversation_id":"conv_one","outcome":"seen"}', 'x'.repeat(4097)]) {
      vi.stubGlobal('fetch', vi.fn(async () => new Response(body, { headers: { 'content-type': 'application/json' } })));
      await expect(api.markMessageSeen('worker_one', 'msg_one', 'same-key')).rejects.toMatchObject({ outcomeUnknown: true });
    }
    vi.stubGlobal('fetch', vi.fn(async () => json({ project_id: 'foreign', revision: 2, outcome: 'updated' })));
    await expect(api.replacePresentation('worker_one', config, 1, 'same-key')).rejects.toMatchObject({ code: 'invalid_response', outcomeUnknown: true });
  });
  it('keeps the five-second action budget and classifies a server failure as uncertain', async () => {
    const api = await connected(); vi.useFakeTimers();
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init: RequestInit) => new Promise<Response>((_resolve, reject) => { init.signal!.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true }); })));
    const pending = api.acknowledgeMessage('worker_one', 'msg_one', 'ack-key').catch(error => error); await vi.advanceTimersByTimeAsync(5000); expect(await pending).toMatchObject({ code: 'deadline_exceeded', outcomeUnknown: true });
    vi.stubGlobal('fetch', vi.fn(async () => refusal(503, 'overloaded'))); await expect(api.acknowledgeMessage('worker_one', 'msg_one', 'ack-key')).rejects.toMatchObject({ status: 503, outcomeUnknown: true });
  });
});
