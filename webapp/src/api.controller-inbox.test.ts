import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api, inboxOriginReadPath, validateMessageEventPage } from './api';

const project = { id: 'project_one', name: 'Project', github_repository_id: 1, repository: 'synthetic/project' };
const at = '2026-10-15T12:00:00Z';
const item = { origin_kind: 'controller', id: 'message_one', conversation_id: 'conversation_one', sender_worker_id: '', recipient_worker_id: 'worker_one', preview: 'Review ready', preview_truncated: false, seen: false, acknowledged: false, created_at: at };
const message = { origin_kind: 'controller', id: item.id, conversation_id: item.conversation_id, sender_worker_id: '', recipient_worker_id: 'worker_one', body: '**Review ready**\n\n[Read evidence](https://example.com/proof)', created_at: at };
const conversation = { conversation_id: item.conversation_id, root_message_id: item.id, items: [message], has_more: false };
const clients: Api[] = [];
function json(value: unknown): Response { return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } }); }
async function connected(writes = false): Promise<Api> {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : String(input).endsWith('/browser-session') ? json({ session_id: JSON.parse(String(init?.body)).session_id, instance_id: 'instance_one', project_id: project.id, expires_at: new Date(Date.now() + 3600000).toISOString(), allow_writes: writes, can_edit_presentation: false }) : json(project));
  const api = new Api('synthetic-private-token', project.id); clients.push(api); await api.connect(); if (writes) await api.startBrowserSession(); return api;
}
afterEach(() => { clients.splice(0).forEach(api => api.close()); vi.useRealTimers(); vi.unstubAllGlobals(); });

describe('Controller inbox origin', () => {
  it('preserves worker messages and reads Controller inbox/snapshot/root conversations using the narrow opt-in', async () => {
    const api = await connected();
    const worker = { ...item, sender_worker_id: 'worker_two' }; delete (worker as Partial<typeof item>).origin_kind;
    const requests: { path: string; headers: Headers }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(String(input)).pathname; requests.push({ path, headers: new Headers(init?.headers) });
      if (path.endsWith('/workers')) return json({ items: [], has_more: false });
      if (path.includes('/conversations/')) return json(conversation);
      return json(path.endsWith('/snapshot') ? { inbox: { items: [item, worker], has_more: false }, event_cursor: 'snapshot_cursor' } : { items: [item, worker], has_more: false });
    });
    expect((await api.inbox('worker_one')).items).toEqual([item, worker]);
    expect((await api.snapshot('worker_one')).inbox.items).toEqual([item, worker]);
    expect((await api.thread(item.conversation_id)).items).toEqual([message]);
    await api.workers();
    expect(requests.slice(0, 3).every(request => request.headers.get('X-FC-Inbox-Origin') === 'controller-v1')).toBe(true);
    expect(requests[3].headers.has('X-FC-Inbox-Origin')).toBe(false);
  });
  it.each([
    { origin_kind: undefined, sender_worker_id: '' },
    { origin_kind: 'controller', sender_worker_id: 'worker_two' },
    { origin_kind: 'worker', sender_worker_id: 'worker_two' },
    { origin_kind: 'Controller', sender_worker_id: '' },
    { origin_kind: null, sender_worker_id: '' },
  ])('rejects malformed origins/senders consistently: %j', async origin => {
    const api = await connected();
    for (const [body, read] of [
      [{ items: [{ ...item, ...origin }], has_more: false }, () => api.inbox('worker_one')],
      [{ inbox: { items: [{ ...item, ...origin }], has_more: false }, event_cursor: 'snapshot_cursor' }, () => api.snapshot('worker_one')],
      [{ ...conversation, items: [{ ...message, ...origin }] }, () => api.thread(item.conversation_id)],
    ] as [unknown, () => Promise<unknown>][]) { vi.stubGlobal('fetch', async () => json(body)); await expect(read()).rejects.toMatchObject({ code: 'invalid_response' }); }
  });
  it('enforces a one-way Controller root while preserving ordinary worker reply history', async () => {
    const api = await connected();
    for (const body of [
      { ...conversation, items: [{ ...message, reply_to_message_id: 'message_prior' }] },
      { ...conversation, items: [{ ...message, reply_to_message_id: '' }] },
      { ...conversation, root_message_id: 'different_root' },
      { ...conversation, items: [message, { ...message, id: 'message_two' }] },
      { ...conversation, has_more: true, cursor: 'next_cursor' },
    ]) { vi.stubGlobal('fetch', async () => json(body)); await expect(api.thread(item.conversation_id)).rejects.toMatchObject({ code: 'invalid_response' }); }
    const worker = { ...message, sender_worker_id: 'worker_two', reply_to_message_id: 'message_prior' }; delete (worker as Partial<typeof message>).origin_kind;
    vi.stubGlobal('fetch', async () => json({ ...conversation, items: [worker] })); expect((await api.thread(item.conversation_id)).items[0]).toEqual(worker);
  });
  it('keeps normal recipient seen/ack requests and cookie authority for Controller messages', async () => {
    const api = await connected(true); const requests: RequestInit[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => { requests.push(init!); return json({ id: item.id, conversation_id: item.conversation_id, outcome: String(input).endsWith('/ack') ? 'acknowledged' : 'seen' }); });
    expect((await api.markMessageSeen('worker_one', item.id, 'seen_key')).outcome).toBe('seen');
    expect((await api.acknowledgeMessage('worker_one', item.id, 'ack_key')).outcome).toBe('acknowledged');
    for (const init of requests) { const headers = new Headers(init.headers); expect(init.method).toBe('POST'); expect(init.credentials).toBe('same-origin'); expect(headers.get('X-FC-Worker')).toBe('worker_one'); expect(headers.has('Authorization')).toBe(false); expect(headers.has('X-FC-Inbox-Origin')).toBe(false); }
  });
});

it('limits origin opt-in to current-project incoming finite and SSE reads', () => {
  const root = '/api/v1/projects/project_one';
  for (const suffix of ['/inbox/worker_one', '/inbox/worker_one/snapshot?limit=20', '/messages/message_one', '/messages/message_one/delivery', '/conversations/conversation_one', '/message-events?cursor=signed', '/message-events/stream?cursor=signed']) expect(inboxOriginReadPath(root + suffix, root)).toBe(true);
  for (const path of [root, root + '/workers', root + '/project-events/stream', root + '/messages/message_one/ack', root + '/messages', root + '/outbox/worker_one', root + '/inbox/worker_one/unknown', '/api/v1/public/projects/project_one', '/api/v1/projects/project_other/inbox/worker_one']) expect(inboxOriginReadPath(path, root)).toBe(false);
});

it('validates Controller sent/seen/ack event origins without accepting private or spoofed payloads', () => {
  const sent = { sequence: 1, project_id: project.id, type: 'message.sent', resource_id: item.id, revision: 1, actor_id: 'principal_one', payload: { conversation_id: item.conversation_id, recipient_worker_id: 'worker_one', sender_worker_id: '', origin_kind: 'controller' }, created_at: at };
  const valid = (event: unknown) => validateMessageEventPage({ items: [event], cursor: 'event_cursor', has_more: false, timed_out: false }, project.id, 'worker_one', 20);
  expect(valid(sent)).toBe(true);
  for (const payload of [{ ...sent.payload, origin_kind: undefined }, { ...sent.payload, origin_kind: 'worker' }, { ...sent.payload, origin_kind: null }, { ...sent.payload, sender_worker_id: 'worker_two' }, { ...sent.payload, body: 'private content' }]) expect(valid({ ...sent, payload })).toBe(false);
  for (const type of ['message.seen', 'message.acknowledged']) {
    const payload = { conversation_id: item.conversation_id, recipient_worker_id: 'worker_one', origin_kind: 'controller' };
    expect(valid({ ...sent, type, payload })).toBe(true);
    expect(valid({ ...sent, type, payload: { ...payload, origin_kind: null } })).toBe(false);
    expect(valid({ ...sent, type, payload: { ...payload, sender_worker_id: '' } })).toBe(false);
  }
});

it('keeps the opt-in and Controller cursor on every incoming stream reconnect', async () => {
  const api = await connected(); vi.useFakeTimers();
  const abort = new AbortController(), requests: { path: string; headers: Headers; cursor: string | null }[] = []; let streams = 0, delivered = 0;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input)); requests.push({ path: url.pathname, headers: new Headers(init?.headers), cursor: url.searchParams.get('cursor') });
    if (url.pathname.endsWith('/snapshot')) return json({ inbox: { items: [item], has_more: false }, event_cursor: 'snapshot_cursor' });
    streams++;
    const page = { items: [{ sequence: streams, project_id: project.id, type: streams === 1 ? 'message.sent' : 'message.acknowledged', resource_id: item.id, revision: streams, actor_id: 'principal_one', payload: { conversation_id: item.conversation_id, recipient_worker_id: 'worker_one', origin_kind: 'controller', ...(streams === 1 ? { sender_worker_id: '' } : {}) }, created_at: at }], cursor: `processed_${streams}`, has_more: false, timed_out: false };
    return new Response(new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode(`event: message-events\nid: ${page.cursor}\ndata: ${JSON.stringify(page)}\n\n`)); if (streams === 1) controller.close(); } }), { headers: { 'content-type': 'text/event-stream' } });
  });
  const watch = api.watchMessages('worker_one', {}, { snapshot(value) { expect(value.inbox.items[0]).toEqual(item); }, events() { if (++delivered === 2) abort.abort(); }, status() {} }, abort.signal);
  await vi.advanceTimersByTimeAsync(0); expect(streams).toBe(1);
  await vi.advanceTimersByTimeAsync(1000); await watch;
  expect(delivered).toBe(2); expect(requests.map(request => request.cursor)).toEqual([null, 'snapshot_cursor', 'processed_1']);
  for (const request of requests) { expect(request.headers.get('X-FC-Inbox-Origin')).toBe('controller-v1'); expect(request.headers.get('X-FC-Worker')).toBe('worker_one'); }
});
