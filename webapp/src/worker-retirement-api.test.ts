import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import { BrowserActionMemory } from './browser-actions';

const project = { id: 'project_one', name: 'One', github_repository_id: 1, repository: 'owner/repo' };
const worker = { id: 'worker_one', project_id: project.id, name: 'Worker' };
const preview = { worker_id: worker.id, retired: false, can_retire: true, blockers: { jobs: { count: 0, ids: [] }, notes: { count: 0, ids: [] }, messages: { count: 0, ids: [] } } };
const clients: Api[] = [];
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
async function connected(writes = true) {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session') ? json({ session_id: JSON.parse(String(init?.body)).session_id, instance_id: 'instance_one', project_id: project.id, expires_at: '2099-01-01T00:00:00Z', allow_writes: writes, can_edit_presentation: false }) : json(project)));
  const api = new Api('PRIVATE_CREDENTIAL', project.id); clients.push(api); await api.connect(); await api.startBrowserSession(); return api;
}
afterEach(() => { clients.splice(0).forEach(api => api.close()); vi.unstubAllGlobals(); });

describe('worker retirement browser API', () => {
  it('keeps active lists separate from explicit archived reads and direct historical worker reads', async () => {
    const api = await connected(); const urls: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => { const url = new URL(String(input)); urls.push(url.pathname + url.search); return url.pathname.endsWith('/worker_one') ? json({ ...worker, retired: true }) : json({ items: url.searchParams.has('include_retired') ? [{ ...worker, retired: true }] : [worker], has_more: false }); }));
    expect((await api.workers()).items).toEqual([worker]);
    expect((await api.workers({ include_retired: true })).items).toEqual([{ ...worker, retired: true }]);
    expect(await api.worker(worker.id)).toEqual({ ...worker, retired: true });
    expect(urls).toEqual([`/api/v1/projects/${project.id}/workers`, `/api/v1/projects/${project.id}/workers?include_retired=true`, `/api/v1/projects/${project.id}/workers/${worker.id}`]);
    for (const retired of [true, false, null, 'yes']) {
      vi.stubGlobal('fetch', vi.fn(async () => json({ items: [{ ...worker, retired }], has_more: false })));
      await expect(api.workers()).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });
  it('validates exact preview binding, blocker inventories and contradictory permission facts', async () => {
    const api = await connected();
    const blocked = { ...preview, can_retire: false, blockers: { ...preview.blockers, jobs: { count: 23, ids: Array.from({ length: 20 }, (_, index) => `job_${index}`) } } };
    vi.stubGlobal('fetch', vi.fn(async () => json(blocked))); expect(await api.workerRetirement(worker.id)).toEqual(blocked);
    for (const value of [{ ...preview, worker_id: 'foreign' }, { ...preview, can_retire: false }, { ...preview, retired: true }, { ...preview, private: 'PRIVATE' }, { ...preview, blockers: null }, { ...preview, blockers: { ...preview.blockers, jobs: { count: 1, ids: ['bad/path'] } } }, { ...preview, blockers: { ...preview.blockers, jobs: { count: 2, ids: ['job_one', 'job_one'] } } }, { ...preview, blockers: { ...preview.blockers, jobs: { count: -1, ids: [] } } }]) {
      vi.stubGlobal('fetch', vi.fn(async () => json(value))); await expect(api.workerRetirement(worker.id)).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });
  it('preserves actor, recipient, project and original key for explicit ACK and retirement', async () => {
    const api = await connected(); const calls: { path: string; body: unknown; key: string | null }[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const headers = new Headers(init?.headers), path = new URL(String(input)).pathname;
      expect(headers.get('x-fc-worker')).toBe('worker_actor'); expect(headers.get('x-fc-instance')).toBe('instance_one'); expect(headers.has('authorization')).toBe(false); expect(init?.credentials).toBe('same-origin'); expect(init?.method).toBe('POST');
      calls.push({ path, body: JSON.parse(String(init?.body)), key: headers.get('idempotency-key') });
      return path.endsWith('/retire') ? json({ id: worker.id, revision: 2, outcome: 'retired' }) : json({ id: 'message_one', conversation_id: 'conversation_one', outcome: 'acknowledged' });
    }));
    await api.acknowledgeWorkerMessage('worker_actor', worker.id, 'message_one', 'ack_key');
    await api.retireWorker(worker.id, 'worker_actor', 'retire_key');
    expect(calls).toEqual([{ path: `/api/v1/projects/${project.id}/messages/message_one/ack`, body: { worker_id: worker.id }, key: 'ack_key' }, { path: `/api/v1/projects/${project.id}/workers/${worker.id}/retire`, body: {}, key: 'retire_key' }]);
  });
  it('keeps blocked refusals definitive and malformed success receipts uncertain without reflecting private data', async () => {
    const api = await connected();
    vi.stubGlobal('fetch', vi.fn(async () => json({ error: { code: 'worker_retirement_blocked', message: 'PRIVATE_BODY', details: { jobs: { count: 1, ids: ['job_one'] } } }, request_id: 'request_one' }, 409)));
    const refusal = await api.retireWorker(worker.id, 'worker_actor', 'key').catch(error => error);
    expect(refusal).toMatchObject({ code: 'worker_retirement_blocked', status: 409, outcomeUnknown: false }); expect(refusal.message).not.toContain('PRIVATE');
    for (const value of [{ id: 'foreign', revision: 2, outcome: 'retired' }, { id: worker.id, revision: 1, outcome: 'retired' }, { id: worker.id, revision: 2, outcome: 'deleted' }, { id: worker.id, revision: 2, outcome: 'retired', private: 'PRIVATE' }]) {
      vi.stubGlobal('fetch', vi.fn(async () => json(value)));
      await expect(api.retireWorker(worker.id, '', 'key')).rejects.toMatchObject({ code: 'invalid_response', outcomeUnknown: true });
    }
  });
  it('never dispatches read-only or invalid actions, and retries only retained exact retirement after lost response', async () => {
    const readOnly = await connected(false); const none = vi.fn(); vi.stubGlobal('fetch', none);
    await expect(readOnly.retireWorker(worker.id, 'worker_actor', 'key')).rejects.toMatchObject({ code: 'forbidden', outcomeUnknown: false });
    await expect(readOnly.retireWorker('bad/path', '', 'key')).rejects.toMatchObject({ code: 'invalid_request' }); expect(none).not.toHaveBeenCalled();
    const api = await connected(); const requests: string[] = []; let first = true;
    vi.stubGlobal('fetch', vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => { requests.push(JSON.stringify({ key: new Headers(init?.headers).get('idempotency-key'), actor: new Headers(init?.headers).get('x-fc-worker'), body: init?.body })); if (first) { first = false; throw new TypeError('lost'); } return json({ id: worker.id, revision: 2, outcome: 'retired' }); }));
    const memory = new BrowserActionMemory(() => 'original_key'); const execute = (value: { worker: string; actor: string }, key: string, signal: AbortSignal) => api.retireWorker(value.worker, value.actor, key, signal);
    await expect(memory.run('retire', { worker: worker.id, actor: 'worker_actor' }, execute, new AbortController().signal)).rejects.toMatchObject({ outcomeUnknown: true });
    await memory.run('retire', { worker: 'worker_changed', actor: 'actor_changed' }, execute, new AbortController().signal);
    expect(requests).toHaveLength(2); expect(requests[0]).toBe(requests[1]); expect(memory.unresolved).toBe(0);
  });
});
