import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import { parseNoteFilters } from './coordination';
import type { Note, NoteSummary, NoteRevision, WorkerPrimaryHistoryEntry, WorkerPauseHistoryEntry } from './types';

const project = { id: 'project_one', name: 'One', github_repository_id: 1, repository: 'owner/repo' };
const worker = 'worker_one';
const at = '2099-01-01T00:00:00.000000000Z';
const note: Note = { id: 'note_one', project_id: project.id, revision: 2, worker_id: worker, job_id: '', phase: 'plan & examine', kind: 'source', status: 'dismissed', created_at: at, updated_at: '2020-01-01T00:00:00.000000000Z', body: '<script>literal</script>\nsource', status_reason: 'literal reason\n\t' };
const summary: NoteSummary = { id: note.id, project_id: note.project_id, revision: 1, worker_id: worker, job_id: '', phase: note.phase, kind: 'source', status: 'pending', created_at: at, updated_at: at, body_preview: 'preview', body_truncated: false };
const revision: NoteRevision = { ...note, actor_id: 'actor_one', actor_worker_id: '', recorded_at: note.updated_at };
const page = (items: unknown[], extra: Record<string, unknown> = {}) => ({ items, has_more: false, ...extra });
const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } });
function fixture() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  let response: unknown = page([]);
  let raw: string | undefined;
  const calls: { url: URL; init: RequestInit | undefined }[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input)); calls.push({ url, init });
    if (url.pathname.endsWith('/instance')) return json({ instance_id: 'instance_one' });
    if (url.pathname.endsWith('/project_one')) return json(project);
    return raw === undefined ? json(response) : new Response(raw, { headers: { 'content-type': 'application/json' } });
  });
  vi.stubGlobal('fetch', fetch);
  return { api: new Api('private-fixture-token', project.id), calls, fetch, set(value: unknown) { response = value; raw = undefined; }, raw(value: string) { raw = value; } };
}
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('explicit coordination API reads', () => {
  it('reads independent worker identity, absent focus and unpaused state with one bound GET each', async () => {
    const f = fixture(); await f.api.connect(); const before = f.calls.length;
    f.set({ id: worker, project_id: project.id, name: 'Builder' }); expect((await f.api.worker(worker)).id).toBe(worker);
    f.set({ worker_id: worker, revision: 0, job_id: '' }); expect((await f.api.workerPrimary(worker)).revision).toBe(0);
    f.set({ worker_id: worker, revision: 0, paused: false, reason: '' }); expect((await f.api.workerPause(worker)).paused).toBe(false);
    expect(f.calls.length - before).toBe(3);
    for (const call of f.calls.slice(before)) { expect(call.init?.method).toBe('GET'); expect(call.url.search).toBe(''); expect(new Headers(call.init?.headers).get('x-fc-instance')).toBe('instance_one'); }
    f.set({ worker_id: worker, revision: Number.MAX_SAFE_INTEGER, job_id: 'job_other' }); expect((await f.api.workerPrimary(worker)).job_id).toBe('job_other');
    f.set({ worker_id: worker, revision: 7, paused: true, reason: '<'.repeat(1024) }); expect((await f.api.workerPause(worker)).reason.length).toBe(1024);
    f.api.close();
  });

  it('rejects foreign, missing, null, unsafe and contradictory current worker facts', async () => {
    const f = fixture(); await f.api.connect();
    for (const value of [{ worker_id: 'foreign', revision: 0, job_id: '' }, { worker_id: worker, revision: 0, job_id: 'job_one' }, { worker_id: worker, revision: null, job_id: '' }, { worker_id: worker, revision: Number.MAX_SAFE_INTEGER + 1, job_id: '' }, { worker_id: worker, revision: 1 }, { worker_id: worker, revision: 1, job_id: '', extra: 1 }]) { f.set(value); await expect(f.api.workerPrimary(worker)).rejects.toMatchObject({ code: 'invalid_response' }); }
    for (const value of [{ worker_id: worker, revision: 0, paused: true, reason: '' }, { worker_id: worker, revision: 1, paused: false, reason: 'retained' }, { worker_id: worker, revision: 1, paused: null, reason: '' }, { worker_id: worker, revision: 1, paused: true, reason: '\r' }, { worker_id: worker, revision: 1, paused: true, reason: '\ud800' }]) { f.set(value); await expect(f.api.workerPause(worker)).rejects.toMatchObject({ code: 'invalid_response' }); }
    f.set({ id: worker, project_id: 'foreign', name: 'Builder' }); await expect(f.api.worker(worker)).rejects.toMatchObject({ code: 'invalid_response' });
    f.raw(`{"worker_id":"${worker}","revision":0,"revision":0,"job_id":""}`); await expect(f.api.workerPrimary(worker)).rejects.toMatchObject({ code: 'invalid_response' });
    f.api.close();
  });

  it('encodes explicit filters and reads exactly one compact note page without expanding bodies or assignments', async () => {
    const f = fixture(); await f.api.connect(); f.set(page([summary])); const before = f.calls.length;
    expect((await f.api.notes()).items).toEqual([summary]); expect(f.calls.at(-1)?.url.search).toBe('');
    f.set(page([summary], { has_more: true, cursor: 'cursor_next' }));
    await f.api.notes({ worker_id: worker, phase: note.phase, kind: 'source', status: 'all', include_assigned_jobs: false, limit: 1, cursor: 'cursor_previous' });
    const q = f.calls.at(-1)!.url.searchParams; expect(q.get('phase')).toBe(note.phase); expect(q.get('worker_id')).toBe(worker); expect(q.get('include_assigned_jobs')).toBe('false'); expect(q.get('cursor')).toBe('cursor_previous');
    expect(f.calls.length - before).toBe(2); expect(f.calls.slice(before).every(call => call.url.pathname.endsWith('/notes'))).toBe(true);
    // With assignment expansion the server owns the local assignment join;
    // another worker's note naming an assigned job is a legitimate result.
    f.set(page([{ ...summary, worker_id: 'another_worker', job_id: 'job_one' }]));
    await expect(f.api.notes({ worker_id: worker, include_assigned_jobs: true })).resolves.toMatchObject({ items: [{ job_id: 'job_one' }] });
    f.api.close();
  });

  it('validates preview, scope, filters, dropped cursors and nanosecond list ordering', async () => {
    const f = fixture(); await f.api.connect();
    const malformed = [page([{ ...summary, project_id: 'foreign' }]), page([{ ...summary, status: 'consumed' }]), page([{ ...summary, body: 'full body leaked' }]), page([{ ...summary, body_preview: 'x', body_truncated: true }]), page([{ ...summary, body_preview: '雪'.repeat(81) }]), page([{ ...summary, worker_id: '', phase: '' }]), page([summary, summary]), page([summary], { has_more: true }), page([], { cursor: 'stranded' }), page([summary], { cursor: null })];
    for (const value of malformed) { f.set(value); await expect(f.api.notes()).rejects.toMatchObject({ code: 'invalid_response' }); }
    f.set(page([{ ...summary, worker_id: 'other' }])); await expect(f.api.notes({ worker_id: worker })).rejects.toMatchObject({ code: 'invalid_response' });
    f.set(page([{ ...summary, phase: 'other' }])); await expect(f.api.notes({ phase: summary.phase })).rejects.toMatchObject({ code: 'invalid_response' });
    f.set(page([{ ...summary, job_id: 'other' }])); await expect(f.api.notes({ job_id: 'job_one' })).rejects.toMatchObject({ code: 'invalid_response' });
    f.set(page([{ ...summary, kind: 'reminder' }])); await expect(f.api.notes({ kind: 'source' })).rejects.toMatchObject({ code: 'invalid_response' });
    const newer = { ...summary, id: 'note_a', created_at: '2099-01-01T00:00:00.000000002Z' }, older = { ...summary, id: 'note_z', created_at: '2099-01-01T00:00:00.000000001Z' };
    f.set(page([newer, older])); await expect(f.api.notes()).resolves.toMatchObject({ items: [newer, older] });
    f.set(page([older, newer])); await expect(f.api.notes()).rejects.toMatchObject({ code: 'invalid_response' });
    f.api.close();
  });

  it('preserves exact full note text and immutable revision attribution despite backwards wall timestamps', async () => {
    const f = fixture(); await f.api.connect(); const before = f.calls.length;
    f.set(note); expect(await f.api.note(note.id)).toEqual(note);
    f.set(revision); expect(await f.api.noteRevision(note.id, 2)).toEqual(revision);
    expect(f.calls.length - before).toBe(2); expect(f.calls.at(-1)?.url.pathname).toBe(`/api/v1/projects/${project.id}/notes/${note.id}/history/2`);
    for (const value of [{ ...note, id: 'foreign' }, { ...note, revision: 1 }, { ...note, status_reason: '' }, { ...note, body: ' '.repeat(5) }, { ...note, body: '\u0085' }, { ...note, updated_at: '2026-02-30T00:00:00Z' }, { ...note, created_at: '0001-01-01T00:00:00Z' }, { ...note, worker_id: '', phase: '' }]) { f.set(value); await expect(f.api.note(note.id)).rejects.toMatchObject({ code: 'invalid_response' }); }
    for (const value of [{ ...revision, recorded_at: at }, { ...revision, revision: 3 }, { ...revision, actor_id: null }, { ...revision, status_reason: null }]) { f.set(value); await expect(f.api.noteRevision(note.id, 2)).rejects.toMatchObject({ code: 'invalid_response' }); }
    f.raw(JSON.stringify(note).replace('"body":', '"Body":')); await expect(f.api.note(note.id)).rejects.toMatchObject({ code: 'invalid_response' });
    f.api.close();
  });

  it('reads one descending revision page per explicit archive request and rejects wrong resources/attribution/order', async () => {
    const f = fixture(); await f.api.connect();
    const primary: WorkerPrimaryHistoryEntry = { worker_id: worker, revision: 2, job_id: '', reason: 'job_reassigned', actor_id: 'actor_one', actor_worker_id: '', created_at: at };
    const pause: WorkerPauseHistoryEntry = { worker_id: worker, revision: 9, paused: true, reason: '\twhy\n', actor_id: 'actor_one', actor_worker_id: '', created_at: at };
    const historical = { ...summary, revision: 2, actor_id: 'actor_one', actor_worker_id: '', recorded_at: at };
    const before = f.calls.length;
    f.set(page([primary], { has_more: true, cursor: 'next' })); await f.api.workerPrimaryHistory(worker, { limit: 1 });
    f.set(page([pause])); await f.api.workerPauseHistory(worker);
    f.set(page([historical])); await f.api.noteHistory(note.id);
    expect(f.calls.length - before).toBe(3);
    for (const value of [page([primary, primary]), page([{ ...primary, reason: 'selected' }]), page([{ ...primary, worker_id: 'other' }]), page([{ ...primary, actor_id: null }]), page([{ ...primary, created_at: 'not-a-time' }])]) { f.set(value); await expect(f.api.workerPrimaryHistory(worker)).rejects.toMatchObject({ code: 'invalid_response' }); }
    f.set(page([{ ...pause, revision: 1 }, pause])); await expect(f.api.workerPauseHistory(worker)).rejects.toMatchObject({ code: 'invalid_response' });
    f.set(page([{ ...historical, id: 'other' }])); await expect(f.api.noteHistory(note.id)).rejects.toMatchObject({ code: 'invalid_response' });
    f.set(page([{ ...historical, recorded_at: '2020-01-01T00:00:00Z' }])); await expect(f.api.noteHistory(note.id)).rejects.toMatchObject({ code: 'invalid_response' });
    f.api.close();
  });

  it('rejects invalid arguments before any fetch and honors per-call cancellation', async () => {
    const f = fixture(); await f.api.connect(); const before = f.calls.length;
    const bad = [() => f.api.worker('../worker'), () => f.api.workerPrimary(''), () => f.api.workerPause('x/y'), () => f.api.workerPauseHistory(worker, { limit: 0 }), () => f.api.workerPrimaryHistory(worker, { cursor: '\ud800' }), () => f.api.notes({ worker_id: '' }), () => f.api.notes({ phase: '' }), () => f.api.notes({ include_assigned_jobs: true }), () => f.api.notes({ status: null } as never), () => f.api.noteRevision(note.id, Number.MAX_SAFE_INTEGER + 1), () => f.api.noteRevision(note.id, 0), () => f.api.note('../note'), () => f.api.noteHistory(note.id, { cursor: 'white space' })];
    for (const invoke of bad) await expect(invoke()).rejects.toMatchObject({ code: 'invalid_request' });
    expect(f.calls.length).toBe(before);
    let observed: AbortSignal | undefined;
    f.fetch.mockImplementation(async (_input, init) => { observed = init?.signal as AbortSignal; return await new Promise<Response>((_resolve, reject) => observed?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })); });
    const controller = new AbortController(); const pending = f.api.note(note.id, controller.signal); controller.abort();
    await expect(pending).rejects.toMatchObject({ code: 'cancelled' }); expect(observed?.aborted).toBe(true);
    f.api.close();
  });

  it('enforces current and compact byte budgets without rejecting legal escaped pause reasons', async () => {
    const f = fixture(); await f.api.connect();
    f.raw(' '.repeat((128 << 10) + 1)); await expect(f.api.note(note.id)).rejects.toMatchObject({ code: 'response_too_large' });
    f.raw(' '.repeat((512 << 10) + 1)); await expect(f.api.notes()).rejects.toMatchObject({ code: 'response_too_large' });
    f.raw(JSON.stringify({ worker_id: worker, revision: 1, paused: true, reason: '<'.repeat(1024) }).replaceAll('<', '\\u003c')); await expect(f.api.workerPause(worker)).resolves.toMatchObject({ paused: true });
    f.api.close();
  });
});


it('keeps locally admitted URL filters within the real notes client contract', async () => {
  const f = fixture(); await f.api.connect(); f.set(page([]));
  const cases: Record<string, string>[] = [{}, {worker_id:'worker_one'}, {job_id:'job_two'}, {phase:'😀'.repeat(50)}, {phase:'a\u2028b'}, {kind:'source',status:'all'}, {include_assigned_jobs:'false'}, {worker_id:'worker_one',include_assigned_jobs:'true'}];
  for (const values of cases) {
    const options = parseNoteFilters(new URLSearchParams(values)); expect(options).toBeDefined(); await expect(f.api.notes(options)).resolves.toMatchObject({items:[]});
  }
  f.api.close();
});
