import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import type { ProjectMapEntry, ProjectMapKind, ProjectMapOptions } from './types';

const project = { id: 'project_one', name: 'Map project', github_repository_id: 42, repository: 'synthetic/map' };
const instant = '2026-10-16T01:00:00.100000000Z';
const entry: ProjectMapEntry = { id: 'entry_one', project_id: project.id, revision: 2, kind: 'workstream', title: 'Build <literal>', description: 'Exact description\n🔥', state: 'RECONSIDER AFTER PROOF', where: 'Current work', next: 'Next milestone', trigger: '', phases: [{ id: 'proof_1', name: 'Proof', state: 'Deferred', archived: false }], links: [{ label: 'Reference', url: 'https://example.com/source#section' }], archived: false, created_at: instant, updated_at: '2026-10-16T00:00:00Z' };
const summary = { id: entry.id, kind: entry.kind, title: entry.title, state: entry.state, archived: entry.archived, revision: entry.revision };
const history = { revision: 2, actor_id: 'principal_one', created_at: instant };
const reference = { job_id: 'job_one', workstream_id: entry.id, workstream_title: 'Recorded older title', revision: 1 };
function json(value: unknown): Response { return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } }); }
async function api(): Promise<Api> {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const client = new Api('synthetic-private-map-token', project.id);
  vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ instance_id: 'instance_one' })).mockResolvedValueOnce(json(project)));
  await client.connect();
  return client;
}
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('scoped read-only project map API', () => {
  it('makes one exact scoped GET per deliberate page/detail/revision/reference read', async () => {
    const client = await api();
    const paths: string[] = [];
    const fetch = vi.fn(async (input: RequestInfo | URL, options?: RequestInit) => {
      const url = new URL(String(input)); paths.push(url.pathname + url.search);
      expect(options?.method).toBe('GET');
      expect(options?.cache).toBe('no-store');
      expect(options?.redirect).toBe('error');
      expect(new Headers(options?.headers).get('x-fc-instance')).toBe('instance_one');
      expect(new Headers(options?.headers).has('x-fc-worker')).toBe(false);
      if (url.pathname.endsWith('/map/entries')) return json({ items: [summary], cursor: 'more_entries', has_more: true });
      if (url.pathname.endsWith('/entry_one/history/2')) return json({ ...history, state: entry });
      if (url.pathname.endsWith('/entry_one/history')) return json({ items: [history], cursor: 'more_history', has_more: true });
      if (url.pathname.endsWith('/entry_one')) return json(entry);
      if (url.pathname.endsWith('/workstream/history')) return json({ items: [{ revision: 1, workstream_id: entry.id, workstream_title: reference.workstream_title, job_revision: 7, actor_id: history.actor_id, created_at: instant }], has_more: false });
      return json(reference);
    });
    vi.stubGlobal('fetch', fetch);
    expect((await client.projectMapEntries({ limit: 1, kind: 'workstream', cursor: 'selected_cursor' })).items).toEqual([summary]);
    expect(await client.projectMapEntry(entry.id)).toEqual(entry);
    expect((await client.projectMapHistory(entry.id, { limit: 1 })).items).toEqual([history]);
    expect((await client.projectMapRevision(entry.id, 2)).state).toEqual(entry);
    expect(await client.jobWorkstream('job_one')).toEqual(reference);
    expect((await client.jobWorkstreamHistory('job_one')).items[0].workstream_title).toBe('Recorded older title');
    expect(fetch).toHaveBeenCalledTimes(6);
    expect(paths).toEqual([
      '/api/v1/projects/project_one/map/entries?limit=1&cursor=selected_cursor&kind=workstream&archived=false',
      '/api/v1/projects/project_one/map/entries/entry_one',
      '/api/v1/projects/project_one/map/entries/entry_one/history?limit=1',
      '/api/v1/projects/project_one/map/entries/entry_one/history/2',
      '/api/v1/projects/project_one/jobs/job_one/workstream',
      '/api/v1/projects/project_one/jobs/job_one/workstream/history?limit=25',
    ]);
    client.close();
  });

  it('rejects malformed full snapshots instead of presenting partial or foreign state', async () => {
    const client = await api();
    const missing = { ...entry } as Partial<ProjectMapEntry>; delete missing.archived;
    for (const value of [
      { ...entry, project_id: 'other_project' }, { ...entry, id: 'entry_two' }, { ...entry, revision: 0 }, { ...entry, revision: Number.MAX_SAFE_INTEGER + 1 },
      { ...entry, kind: 'unknown' }, { ...entry, kind: 'return_point' }, { ...entry, phases: null }, { ...entry, links: null }, missing,
      { ...entry, archived: null }, { ...entry, private_extra: 'not an allowed field' }, { ...entry, state: 'Two\nlines' }, { ...entry, description: 'Invalid\rprose' }, { ...entry, description: '\u0085' },
      { ...entry, title: '\ud800' }, { ...entry, created_at: '2026-02-30T00:00:00Z' }, { ...entry, updated_at: '0001-01-01T00:00:00Z' },
      { ...entry, phases: [entry.phases[0], entry.phases[0]] }, { ...entry, phases: [{ id: 'Upper', name: 'Proof', state: '', archived: false }] },
      { ...entry, phases: [{ id: 'proof', name: 'Proof', state: '' }] }, { ...entry, phases: [{ ...entry.phases[0], archived: null }] },
      { ...entry, links: [{ label: 'Reference', url: 'javascript:alert(1)' }] }, { ...entry, links: [{ label: 'Reference', url: 'https://@example.com/path' }] },
      { ...entry, links: [{ label: 'Reference', url: 'https://example.com/white space' }] },
      { ...entry, phases: Array.from({ length: 65 }, (_, index) => ({ ...entry.phases[0], id: `p${index}` })) },
      { ...entry, description: '<'.repeat(8192), where: '<'.repeat(8192), next: '<'.repeat(8192) },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(client.projectMapEntry(entry.id)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    for (const raw of [JSON.stringify(entry).replace('"revision":2', '"revision":2.0'), JSON.stringify(entry).replace('"archived":false', '"archived":false,"archived":false')]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(raw, { headers: { 'content-type': 'application/json' } })));
      await expect(client.projectMapEntry(entry.id)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    client.close();
  });

  it('requires complete compact pages, exact filter results and captured revision identities', async () => {
    const client = await api();
    for (const value of [
      { items: [], has_more: null }, { items: [] }, { items: null, has_more: false }, { items: [], has_more: false, cursor: null },
      { items: [summary], has_more: true }, { items: [summary], has_more: false, cursor: 'unexpected' }, { items: [summary, summary], has_more: false },
      { items: [{ ...summary, archived: true }], has_more: false }, { items: [{ ...summary, description: 'body leaked into compact page' }], has_more: false },
      { items: Array.from({ length: 26 }, (_, i) => ({ ...summary, id: `entry_${String(i).padStart(2, '0')}` })), has_more: false },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(client.projectMapEntries()).rejects.toMatchObject({ code: 'invalid_response' });
    }
    for (const value of [{ ...history, state: { ...entry, revision: 1 } }, { ...history, state: { ...entry, project_id: 'foreign' } }]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(client.projectMapRevision(entry.id, 2)).rejects.toMatchObject({ code: 'invalid_response' });
    }
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ items: [history, history], has_more: false })));
    await expect(client.projectMapHistory(entry.id)).rejects.toMatchObject({ code: 'invalid_response' });
    for (const value of [{ ...reference, job_id: 'foreign' }, { ...reference, revision: 0 }, { ...reference, workstream_id: '', workstream_title: 'orphan title' }]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(client.jobWorkstream('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    }
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ job_id: 'job_one', workstream_id: '', workstream_title: '', revision: 0 })));
    await expect(client.jobWorkstream('job_one')).resolves.toMatchObject({ revision: 0 });
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ items: [{ revision: 1, workstream_id: entry.id, workstream_title: 'Old title', job_revision: 0, actor_id: history.actor_id, created_at: instant }], has_more: false })));
    await expect(client.jobWorkstreamHistory('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
    client.close();
  });

  it('refuses invalid selectors locally and preserves auth, abort and finite deadline behavior', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    const disconnected = new Api('synthetic-private-map-token', project.id);
    const untouched = vi.fn(); vi.stubGlobal('fetch', untouched);
    for (const read of [
      () => disconnected.projectMapEntry('../entry'), () => disconnected.projectMapRevision(entry.id, 0),
      () => disconnected.projectMapEntries({ limit: 101 }), () => disconnected.projectMapEntries({ kind: 'unknown' as ProjectMapKind }),
      () => disconnected.projectMapEntries({ archived: 'false' } as unknown as ProjectMapOptions),
      () => disconnected.projectMapHistory(entry.id, { cursor: ' padded' }), () => disconnected.jobWorkstream('bad/job'),
    ]) await expect(read()).rejects.toMatchObject({ code: 'invalid_request' });
    expect(untouched).not.toHaveBeenCalled(); disconnected.close();
    const client = await api();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: { code: 'forbidden', message: 'PRIVATE synthetic-private-map-token' } }), { status: 403, headers: { 'content-type': 'application/json' } })));
    await expect(client.projectMapEntry(entry.id)).rejects.toMatchObject({ code: 'forbidden', status: 403, message: 'The service request failed (forbidden).' });
    const pending = vi.fn((_url: unknown, options: RequestInit) => new Promise<Response>((_resolve, reject) => options.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })));
    vi.stubGlobal('fetch', pending);
    const controller = new AbortController();
    const canceled = client.projectMapEntry(entry.id, controller.signal);
    controller.abort();
    await expect(canceled).rejects.toMatchObject({ code: 'cancelled' });
    vi.useFakeTimers();
    const timed = expect(client.projectMapHistory(entry.id)).rejects.toMatchObject({ code: 'deadline_exceeded' });
    await vi.advanceTimersByTimeAsync(5000); await timed;
    expect(pending).toHaveBeenCalledTimes(2);
    client.close();
  });
});
