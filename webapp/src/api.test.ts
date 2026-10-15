import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api, ApiError, validateMessageEventPage } from './api';

const project = { id: 'project_one', name: 'Test project', github_repository_id: 123, repository: 'owner/repository' };
const worker = { id: 'worker_one', project_id: 'project_one', name: 'Builder' };
const inboxItem = {
  id: 'msg_one', conversation_id: 'conv_one', sender_worker_id: 'worker_two', recipient_worker_id: 'worker_one',
  preview: 'hello', preview_truncated: false, seen: false, acknowledged: false, created_at: '2026-10-15T12:00:00Z',
};
const jobSummary = { id: 'job_one', title: 'Task', worker_id: 'worker_one', status: 'active', revision: 1 };
const job = {
  ...jobSummary, project_id: 'project_one', brief: 'private brief', created_at: '2026-10-15T12:00:00Z', updated_at: '2026-10-15T12:00:00Z',
};
const message = {
  id: 'msg_one', conversation_id: 'conv_one', sender_worker_id: 'worker_two', recipient_worker_id: 'worker_one',
  body: 'private body', created_at: '2026-10-15T12:00:00Z',
};

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
}
function setupLocation(): void { vi.stubGlobal('location', { origin: 'https://service.example' }); }
function api(): Api { return new Api('private-token-do-not-leak', 'project_one'); }
async function connect(client: Api): Promise<void> {
  vi.stubGlobal('fetch', vi.fn()
    .mockResolvedValueOnce(json({ instance_id: 'instance_one' }))
    .mockResolvedValueOnce(json(project)));
  await client.connect();
}

afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('Api binding and bounded reads', () => {
  it('verifies instance then project and binds every read to same origin, instance and selected worker', async () => {
    setupLocation();
    const client = api();
    const calls: Array<{ url: string; init: RequestInit }> = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init: init ?? {} });
      if (url.endsWith('/api/v1/instance')) return json({ instance_id: 'instance_one' });
      if (url.endsWith('/api/v1/projects/project_one')) return json(project);
      if (url.endsWith('/workers') || url.includes('/workers?')) return json({ items: [worker], has_more: false, cursor: '' });
      if (url.includes('/jobs?')) return json({ items: [jobSummary], has_more: false, cursor: '' });
      if (url.endsWith('/jobs/job_one')) return json(job);
      if (url.includes('/inbox/worker_one/snapshot')) return json({ inbox: { items: [inboxItem], has_more: false, cursor: '' }, event_cursor: 'signed_event_cursor' });
      if (url.includes('/inbox/worker_one?') || url.endsWith('/inbox/worker_one')) return json({ items: [inboxItem], has_more: false, cursor: '' });
      if (url.includes('/conversations/conv_one?') || url.endsWith('/conversations/conv_one')) return json({ conversation_id: 'conv_one', root_message_id: 'msg_one', items: [message], has_more: false, cursor: '' });
      throw new Error('unexpected request');
    }));

    const verified = await client.connect();
    expect(verified.id).toBe('project_one');
    expect(client.instanceId).toBe('instance_one');
    expect((await client.workers()).items).toEqual([worker]);
    expect((await client.jobs({ status: 'active', worker_id: 'worker_one', limit: 10 })).items).toEqual([jobSummary]);
    expect((await client.job('job_one')).brief).toBe('private brief');
    expect((await client.inbox('worker_one', { unread: true, unacknowledged: true })).items).toEqual([inboxItem]);
    expect((await client.snapshot('worker_one')).event_cursor).toBe('signed_event_cursor');
    expect((await client.thread('conv_one')).items).toEqual([message]);

    expect(calls).toHaveLength(8);
    expect(calls[0].url).toBe('https://service.example/api/v1/instance');
    expect(new Headers(calls[0].init.headers).get('authorization')).toBe('Bearer private-token-do-not-leak');
    expect(new Headers(calls[0].init.headers).get('x-fc-instance')).toBeNull();
    for (const call of calls.slice(1)) {
      const url = new URL(call.url);
      expect(url.origin).toBe('https://service.example');
      expect(url.search).not.toContain('private-token');
      expect(new Headers(call.init.headers).get('x-fc-instance')).toBe('instance_one');
      expect(call.init.credentials).toBe('omit');
      expect(call.init.redirect).toBe('error');
      expect(call.init.cache).toBe('no-store');
    }
    expect(new Headers(calls[5].init.headers).get('x-fc-worker')).toBe('worker_one');
    expect(calls[5].url).toContain('unread=true');
    expect(calls[5].url).toContain('unacknowledged=true');
    expect(new Headers(calls[6].init.headers).get('x-fc-worker')).toBe('worker_one');
  });

  it('preserves valid Unicode and control content in job titles, briefs, message bodies, previews and snapshots', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const content = `ANSI \u001b[31mred\u001b[0m <script>literal</script> café 🔥 ${Array.from({ length: 32 }, (_, index) => String.fromCharCode(index)).join('')}\u007f`;
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(json({ ...job, title: content, brief: content }))
      .mockResolvedValueOnce(json({ items: [{ ...jobSummary, title: content }], has_more: false }))
      .mockResolvedValueOnce(json({ conversation_id: 'conv_one', root_message_id: 'msg_one', items: [{ ...message, body: content }], has_more: false }))
      .mockResolvedValueOnce(json({ items: [{ ...inboxItem, preview: content }], has_more: false }))
      .mockResolvedValueOnce(json({ inbox: { items: [{ ...inboxItem, preview: content }], has_more: false }, event_cursor: 'signed_event_cursor' })));
    const detail = await client.job('job_one');
    expect(detail.title).toBe(content);
    expect(detail.brief).toBe(content);
    expect((await client.jobs()).items[0].title).toBe(content);
    expect((await client.thread('conv_one')).items[0].body).toBe(content);
    expect((await client.inbox('worker_one')).items[0].preview).toBe(content);
    expect((await client.snapshot('worker_one')).inbox.items[0].preview).toBe(content);
    client.close();
  });

  it.each([
    { field: 'title', label: 'UTF-8 byte boundary', value: 'é'.repeat(120), valid: true },
    { field: 'title', label: 'UTF-8 byte overflow', value: 'é'.repeat(121), valid: false },
    { field: 'title', label: 'empty content', value: '', valid: false },
    { field: 'title', label: 'wrong type', value: null, valid: false },
    { field: 'title', label: 'unpaired surrogate', value: '\ud800', valid: false },
    { field: 'brief', label: 'UTF-8 byte boundary', value: 'é'.repeat(16384), valid: true },
    { field: 'brief', label: 'UTF-8 byte overflow', value: 'é'.repeat(16385), valid: false },
    { field: 'brief', label: 'empty content', value: '', valid: true },
    { field: 'brief', label: 'wrong type', value: 1, valid: false },
    { field: 'brief', label: 'unpaired surrogate', value: '\ud800', valid: false },
    { field: 'body', label: 'UTF-8 byte boundary', value: 'é'.repeat(8192), valid: true },
    { field: 'body', label: 'UTF-8 byte overflow', value: 'é'.repeat(8193), valid: false },
    { field: 'body', label: 'empty content', value: '', valid: false },
    { field: 'body', label: 'wrong type', value: null, valid: false },
    { field: 'body', label: 'unpaired surrogate', value: '\udfff', valid: false },
    { field: 'preview', label: 'Unicode rune boundary', value: '🔥'.repeat(160), valid: true },
    { field: 'preview', label: 'Unicode rune overflow', value: '🔥'.repeat(161), valid: false },
    { field: 'preview', label: 'empty content', value: '', valid: true },
    { field: 'preview', label: 'wrong type', value: [], valid: false },
    { field: 'preview', label: 'unpaired surrogate', value: '\ud800', valid: false },
  ])('keeps $field validation for $label', async ({ field, value, valid }) => {
    setupLocation();
    const client = api();
    await connect(client);
    const payload = field === 'brief' || field === 'title' ? { ...job, [field]: value }
      : field === 'body' ? { conversation_id: 'conv_one', root_message_id: 'msg_one', items: [{ ...message, body: value }], has_more: false }
        : { items: [{ ...inboxItem, preview: value }], has_more: false };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json(payload)));
    const pending = field === 'brief' || field === 'title' ? client.job('job_one') : field === 'body' ? client.thread('conv_one') : client.inbox('worker_one');
    if (valid) await expect(pending).resolves.toEqual(payload);
    else await expect(pending).rejects.toMatchObject({ code: 'invalid_response' });
    client.close();
  });

  it('continues rejecting control content in credentials, identifiers, cursors and unrelated response fields', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    for (const control of ['\u0000', '\u001b', '\u007f']) {
      expect(() => new Api(`token${control}value`, 'project_one')).toThrowError(ApiError);
      expect(() => new Api('private-token', `project${control}one`)).toThrowError(ApiError);
      await expect(client.job(`job${control}one`)).rejects.toMatchObject({ code: 'invalid_request' });
      await expect(client.inbox('worker_one', { cursor: `cursor${control}one` })).rejects.toMatchObject({ code: 'invalid_request' });
    }
    expect(fetchMock).not.toHaveBeenCalled();
    fetchMock.mockResolvedValueOnce(json({ items: [{ ...worker, id: 'worker\u001bone' }], has_more: false }));
    await expect(client.workers()).rejects.toMatchObject({ code: 'invalid_response' });
    fetchMock.mockResolvedValueOnce(json({ ...job, worker_id: 'worker\u007fone' }));
    await expect(client.job('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    fetchMock.mockResolvedValueOnce(json({ items: [{ ...inboxItem, id: 'msg\u001bone' }], has_more: false }));
    await expect(client.inbox('worker_one')).rejects.toMatchObject({ code: 'invalid_response' });
    client.close();
  });

  it.each(['project', 'worker'])('matches server display-name bounds and Unicode whitespace rules for $0 names', async (kind) => {
    setupLocation();
    for (const [name, valid] of [
      ['Name\u001b[31mred\u001b[0m\u0000\u007f', true],
      ['é'.repeat(100), true],
      ['é'.repeat(101), false],
      ['', false],
      [123, false],
      ['\ud800', false],
      [' name', false],
      ['name\n', false],
      ['\u0085name', false],
      ['name\u2003', false],
      ['\ufeffname', true],
    ] as const) {
      const client = api();
      let pending: Promise<unknown>;
      if (kind === 'project') {
        vi.stubGlobal('fetch', vi.fn()
          .mockResolvedValueOnce(json({ instance_id: 'instance_one' }))
          .mockResolvedValueOnce(json({ ...project, name })));
        pending = client.connect();
      } else {
        await connect(client);
        vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ items: [{ ...worker, name }], has_more: false })));
        pending = client.workers();
      }
      if (valid) await expect(pending).resolves.toBeDefined();
      else await expect(pending).rejects.toMatchObject({ code: 'invalid_response' });
      client.close();
    }
  });

  it('rejects changed identity, incomplete pages and unexpected fields without exposing response text', async () => {
    setupLocation();
    const client = api();
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(json({ instance_id: 'instance_one' }))
      .mockResolvedValueOnce(json({ ...project, id: 'project_two' })));
    await expect(client.connect()).rejects.toMatchObject({ code: 'invalid_response' });
    expect(client.instanceId).toBeUndefined();

    await connect(client);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ items: [worker], has_more: false, cursor: '', secret_field: 'hidden' })));
    await expect(client.workers()).rejects.toMatchObject({ code: 'invalid_response' });

    const token = 'private-token-do-not-leak';
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ error: { code: 'forbidden', message: `token=${token} https://private.invalid/path` } }, 403)));
    const failure = await client.workers().catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(ApiError);
    expect(failure).toMatchObject({ code: 'forbidden', status: 403 });
    expect((failure as Error).message).not.toContain(token);
    expect((failure as Error).message).not.toContain('https://');

    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ error: { code: 'private-token-do-not-leak', message: 'hidden detail' } }, 500)));
    const unknownCode = await client.workers().catch((error: unknown) => error);
    expect(unknownCode).toMatchObject({ code: 'service_error', status: 500 });
    expect((unknownCode as Error).message).not.toContain('private-token-do-not-leak');
  });

  it('rejects oversized response bodies and prevents invalid arguments from making requests', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn().mockResolvedValueOnce(new Response(new ReadableStream<Uint8Array>({
      start(controller) { controller.enqueue(new Uint8Array((1 << 20) + 1)); controller.close(); },
    }), { status: 200, headers: { 'content-type': 'application/json' } }));
    vi.stubGlobal('fetch', fetchMock);
    await expect(client.workers()).rejects.toMatchObject({ code: 'response_too_large' });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    fetchMock.mockClear();
    await expect(client.jobs({ limit: 101 })).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(client.inbox('../other')).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(client.job('bad/id')).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shares the five-second connection budget across instance and project verification', async () => {
    setupLocation();
    vi.useFakeTimers();
    const client = api();
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      if (String(_input).endsWith('/api/v1/instance')) return Promise.resolve(json({ instance_id: 'instance_one' }));
      return new Promise<Response>((_resolve, reject) => init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true }));
    }));
    const assertion = expect(client.connect()).rejects.toMatchObject({ code: 'deadline_exceeded' });
    await vi.advanceTimersByTimeAsync(5000);
    await assertion;
    expect(client.instanceId).toBeUndefined();
  });

  it('cancels finite requests and rejects redirect failures without exposing transport text', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const controller = new AbortController();
    controller.abort();
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      if (init?.signal?.aborted) reject(new DOMException('aborted', 'AbortError'));
      else init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
    })));
    await expect(client.workers({}, controller.signal)).rejects.toMatchObject({ code: 'cancelled' });

    const redirect = vi.fn().mockRejectedValue(new TypeError('redirect to https://elsewhere.invalid with private-token-do-not-leak'));
    vi.stubGlobal('fetch', redirect);
    const failure = await client.workers().catch((error: unknown) => error);
    expect(failure).toMatchObject({ code: 'connection_failed' });
    expect((failure as Error).message).not.toContain('elsewhere.invalid');
    expect((failure as Error).message).not.toContain('private-token-do-not-leak');
    expect(redirect.mock.calls[0][1]).toMatchObject({ redirect: 'error' });
  });

  it('close cancels session-owned requests and prevents later requests', async () => {
    setupLocation();
    const client = api();
    await connect(client);
    const fetchMock = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
    }));
    vi.stubGlobal('fetch', fetchMock);
    const pending = client.workers();
    await Promise.resolve();
    client.close();
    await expect(pending).rejects.toMatchObject({ code: 'cancelled' });
    await expect(client.jobs()).rejects.toMatchObject({ code: 'cancelled' });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(client.instanceId).toBeUndefined();
  });

  it('validates event scope and rejects bodies or unknown payload fields', () => {
    const base = {
      items: [{ sequence: 1, project_id: 'project_one', type: 'message.sent', resource_id: 'msg_one', revision: 1, actor_id: 'principal_one', payload: { conversation_id: 'conv_one', recipient_worker_id: 'worker_one', sender_worker_id: 'worker_two' }, created_at: '2026-10-15T12:00:00Z' }],
      cursor: 'signed_cursor', has_more: false, timed_out: false,
    };
    expect(validateMessageEventPage(base, 'project_one', 'worker_one', 20)).toBe(true);
    expect(validateMessageEventPage(base, 'project_two', 'worker_one', 20)).toBe(false);
    expect(validateMessageEventPage(base, 'project_one', 'worker_two', 20)).toBe(false);
    expect(validateMessageEventPage({ ...base, items: [{ ...base.items[0], payload: { ...base.items[0].payload, body: 'private' } }] }, 'project_one', 'worker_one', 20)).toBe(false);
    expect(validateMessageEventPage({ ...base, private_extra: true }, 'project_one', 'worker_one', 20)).toBe(false);
  });

  it('uses authenticated SSE after a scoped snapshot and aborts without JSON polling', async () => {
    setupLocation();
    const client = api();
    const calls: Array<{ url: string; init: RequestInit }> = [];
    const page = { items: [], cursor: 'stream_cursor', has_more: false, timed_out: false };
    const data = `event: message-events\nid: stream_cursor\ndata: ${JSON.stringify(page)}\n\n`;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init: init ?? {} });
      if (url.endsWith('/api/v1/instance')) return json({ instance_id: 'instance_one' });
      if (url.endsWith('/api/v1/projects/project_one')) return json(project);
      if (url.includes('/inbox/worker_one/snapshot')) return json({ inbox: { items: [], cursor: '', has_more: false }, event_cursor: 'snapshot_cursor' });
      if (url.includes('/message-events/stream')) return new Response(new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode(data)); } }), { status: 200, headers: { 'content-type': 'text/event-stream' } });
      throw new Error('unexpected request');
    }));
    const abort = new AbortController();
    let eventCount = 0;
    const watch = client.watchMessages('worker_one', { unread: true }, {
      snapshot(value) { expect(value.event_cursor).toBe('snapshot_cursor'); },
      events(value) { eventCount += 1; expect(value.cursor).toBe('stream_cursor'); abort.abort(); },
      status() {},
    }, abort.signal);
    await watch;
    expect(eventCount).toBe(1);
    expect(calls).toHaveLength(4);
    const stream = calls[3];
    const url = new URL(stream.url);
    expect(url.origin).toBe('https://service.example');
    expect(url.pathname).toBe('/api/v1/projects/project_one/message-events/stream');
    expect(url.searchParams.get('cursor')).toBe('snapshot_cursor');
    expect(url.searchParams.get('limit')).toBe('20');
    expect(url.search).not.toContain('private-token');
    expect(new Headers(stream.init.headers).get('authorization')).toBe('Bearer private-token-do-not-leak');
    expect(new Headers(stream.init.headers).get('x-fc-instance')).toBe('instance_one');
    expect(new Headers(stream.init.headers).get('x-fc-worker')).toBe('worker_one');
    expect(stream.init.credentials).toBe('omit');
    expect(stream.init.redirect).toBe('error');
    expect(calls.every((call) => !call.url.includes('github.com'))).toBe(true);
  });

  it('does not start a watch when its caller signal is already cancelled', async () => {
    setupLocation();
    const client = api();
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    const controller = new AbortController();
    controller.abort();
    await client.watchMessages('worker_one', {}, { snapshot() {}, events() {}, status() {} }, controller.signal);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
