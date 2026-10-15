import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import type { ChecklistHistoryEntry, JobChecklist, JobUpdate, Page } from './types';

const project = { id: 'project_one', name: 'Progress project', github_repository_id: 42, repository: 'synthetic/progress' };
const instant = '2026-10-15T12:00:00Z';
const update: JobUpdate = { id: 'update_one', project_id: project.id, job_id: 'job_one', job_revision: 2, sequence: 8, actor_id: 'principal_one', worker_id: '', body: '  exact <script>literal</script>\n🔥', evidence: ['second reference', 'first reference'], created_at: instant };
const checklist: JobChecklist = { job_id: 'job_one', job_revision: 2, revision: 3, items: [{ id: '_first', text: '  exact task\n', done: false }, { id: 'Done-2', text: '<img src=x>', done: true }] };
const entry: ChecklistHistoryEntry = { revision: 3, job_revision: 2, actor_id: 'principal_one', worker_id: '', items: checklist.items, created_at: instant };

function json(value: unknown): Response { return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } }); }
async function api(): Promise<Api> {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const client = new Api('synthetic-private-browser-token', project.id);
  vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ instance_id: 'instance_one' })).mockResolvedValueOnce(json(project)));
  await client.connect();
  return client;
}
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('job progress read-only API', () => {
  it('retains exact ordered content and fetches only the explicit bound pages', async () => {
    const client = await api();
    const urls: string[] = [];
    const mock = vi.fn(async (input: RequestInfo | URL, options?: RequestInit) => {
      const url = new URL(String(input)); urls.push(url.pathname + url.search);
      expect(options?.method).toBe('GET');
      expect(options?.cache).toBe('no-store');
      expect(new Headers(options?.headers).get('x-fc-instance')).toBe('instance_one');
      if (url.pathname.endsWith('/updates')) return json({ items: [update], cursor: 'next_updates', has_more: true });
      if (url.pathname.endsWith('/history')) return json({ items: [entry], cursor: 'next_plan', has_more: true });
      return json(checklist);
    });
    vi.stubGlobal('fetch', mock);
    expect((await client.jobUpdates('job_one', { limit: 1, cursor: 'selected_cursor' })).items[0]).toEqual(update);
    expect(await client.jobChecklist('job_one')).toEqual(checklist);
    expect((await client.jobChecklistHistory('job_one', { limit: 1 })).items[0]).toEqual(entry);
    expect(mock).toHaveBeenCalledTimes(3);
    expect(urls).toEqual(['/api/v1/projects/project_one/jobs/job_one/updates?limit=1&cursor=selected_cursor', '/api/v1/projects/project_one/jobs/job_one/checklist', '/api/v1/projects/project_one/jobs/job_one/checklist/history?limit=1']);
    client.close();
  });

  it('accepts absent and cleared plans, C1 prose, and maximum safe read revisions', async () => {
    const client = await api();
    for (const value of [{ ...checklist, revision: 0, items: [] }, { ...checklist, revision: Number.MAX_SAFE_INTEGER, items: [] }]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      expect(await client.jobChecklist('job_one')).toEqual(value);
    }
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ items: [{ ...update, body: 'Allowed C1 \u0081 exact' }], has_more: false })));
    expect((await client.jobUpdates('job_one')).items[0].body).toBe('Allowed C1 \u0081 exact');
    client.close();
  });

  it('rejects malformed scope, unsafe integers, sequence order, content and pagination', async () => {
    const client = await api();
    const page: Page<JobUpdate> = { items: [update], has_more: false };
    for (const value of [
      { ...page, items: [{ ...update, project_id: 'project_two' }] },
      { ...page, items: [{ ...update, job_id: 'job_two' }] },
      { ...page, items: [{ ...update, sequence: Number.MAX_SAFE_INTEGER + 1 }] },
      { ...page, items: [{ ...update, job_revision: 1.5 }] },
      { ...page, items: [{ ...update, sequence: 0 }] },
      { ...page, items: [{ ...update, evidence: null }] },
      { ...page, items: [{ ...update, body: '  ' }] },
      { ...page, items: [{ ...update, body: 'bad\u007f' }] },
      { ...page, items: [{ ...update, body: '\ud800' }] },
      { ...page, items: [{ ...update, body: 'x'.repeat(8193) }] },
      { ...page, items: [{ ...update, evidence: ['x'.repeat(1025)] }] },
      { ...page, items: [{ ...update, evidence: new Array(11).fill('reference') }] },
      { ...page, items: [{ ...update, body: 'x'.repeat(8192), evidence: new Array(9).fill('x'.repeat(1024)) }] },
      { ...page, items: [{ ...update, created_at: '2026-02-30T00:00:00Z' }] },
      { ...page, items: [{ ...update, created_at: '0001-01-01T00:00:00Z' }] },
      { ...page, items: [{ ...update, extra: 1 }] },
      { ...page, items: [update, { ...update, id: 'update_two', sequence: 9 }] },
      { ...page, items: [update, { ...update, sequence: 7 }] },
      { ...page, cursor: 'unexpected_cursor' },
      { ...page, has_more: true },
      { ...page, items: null },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(client.jobUpdates('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    }
    const missing = { ...update } as Partial<JobUpdate>; delete missing.worker_id;
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ items: [missing], has_more: false })));
    await expect(client.jobUpdates('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    client.close();
  });

  it('validates every explicit checklist item field, ID, aggregate, and history revision', async () => {
    const client = await api();
    const duplicateItems = [checklist.items[0], checklist.items[0]];
    const largeItems = Array.from({ length: 17 }, (_, i) => ({ id: `item${i}`, text: 'x'.repeat(1024), done: false }));
    for (const value of [
      { ...checklist, job_id: 'job_two' }, { ...checklist, revision: 0 },
      { ...checklist, job_revision: Number.MAX_SAFE_INTEGER + 1 }, { ...checklist, revision: -1 },
      { ...checklist, items: null }, { ...checklist, items: duplicateItems },
      { ...checklist, items: [{ id: 'one', text: 'task' }] },
      { ...checklist, items: [{ id: 'one', text: 'task', done: null }] },
      { ...checklist, items: [{ id: 'one', text: 'task', done: 'false' }] },
      { ...checklist, items: [{ id: 'one', text: 'task', Done: false }] },
      { ...checklist, items: [{ id: 'one', text: 'task', done: false, extra: 1 }] },
      { ...checklist, items: [{ id: 'é', text: 'task', done: false }] },
      { ...checklist, items: [{ id: 'one:two', text: 'task', done: false }] },
      { ...checklist, items: [{ id: 'one', text: 'bad\u0001', done: false }] },
      { ...checklist, items: largeItems },
      { ...checklist, items: Array.from({ length: 101 }, (_, i) => ({ id: `i${i}`, text: 'task', done: false })) },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(client.jobChecklist('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    }
    for (const items of [[entry, entry], [entry, { ...entry, revision: 4 }], [{ ...entry, revision: 0 }], [{ ...entry, job_revision: null }], [{ ...entry, actor_id: '' }], [{ ...entry, created_at: 'invalid' }], [{ ...entry, project_id: 'unrequested' }]]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ items, has_more: false })));
      await expect(client.jobChecklistHistory('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    }
    client.close();
  });

  it('rejects duplicate JSON keys and noninteger syntax before collapsed JSON can grant valid state', async () => {
    const client = await api();
    for (const raw of [
      JSON.stringify(checklist).replace('"revision":3', '"revision":3,"revision":3'),
      JSON.stringify(checklist).replace('"revision":3', '"revision":3e0'),
      JSON.stringify(checklist).replace('"revision":3', '"revision":3.0'),
      JSON.stringify(checklist).replace('"done":false', '"done":false,"\\u0064one":false'),
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(raw, { headers: { 'content-type': 'application/json' } })));
      await expect(client.jobChecklist('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    }
    client.close();
  });

  it('enforces encoded current/archive budgets without truncating valid escaped text', async () => {
    const client = await api();
    const items = Array.from({ length: 16 }, (_, i) => ({ id: String.fromCharCode(97 + i), text: '&'.repeat(1023), done: false }));
    const raw = JSON.stringify({ ...checklist, items }).replaceAll('&', '\\u0026');
    expect(new TextEncoder().encode(raw).byteLength).toBeGreaterThan(90 << 10);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(raw, { headers: { 'content-type': 'application/json' } })));
    expect((await client.jobChecklist('job_one')).items).toEqual(items);
    for (const [route, size] of [['checklist', 128 << 10], ['updates', 4 << 20], ['history', 4 << 20]] as const) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(' '.repeat(size) + '{}', { headers: { 'content-type': 'application/json' } })));
      const request = route === 'checklist' ? client.jobChecklist('job_one') : route === 'updates' ? client.jobUpdates('job_one') : client.jobChecklistHistory('job_one');
      await expect(request).rejects.toMatchObject({ code: 'response_too_large' });
    }
    client.close();
  });

  it('validates arguments before HTTP and cancels owned reads without later followup', async () => {
    const client = await api();
    const mock = vi.fn(); vi.stubGlobal('fetch', mock);
    await expect(client.jobChecklist('../unsafe')).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(client.jobUpdates('job_one', { limit: 101 })).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(client.jobChecklistHistory('job_one', { cursor: ' bad' })).rejects.toMatchObject({ code: 'invalid_request' });
    expect(mock).not.toHaveBeenCalled();
    let aborted = false;
    vi.stubGlobal('fetch', vi.fn((_input: RequestInfo | URL, init?: RequestInit) => new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => { aborted = true; reject(new DOMException('aborted', 'AbortError')); }, { once: true });
    })));
    const controller = new AbortController();
    const pending = client.jobUpdates('job_one', {}, controller.signal);
    controller.abort();
    await expect(pending).rejects.toMatchObject({ code: 'cancelled' });
    expect(aborted).toBe(true);
    client.close();
  });
});
