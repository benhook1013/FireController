import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import type { DeliverySnapshot } from './types';

const project = { id: 'project_one', name: 'Delivery project', github_repository_id: 42, repository: 'synthetic/delivery' };
const progress = { summary: 'API proof passed', next: 'Read the latest brief', blocker: '', summary_truncated: false, next_truncated: false, blocker_truncated: false };
function snapshot(): DeliverySnapshot { return { workers: { items: [{ id: 'worker_one', project_id: project.id, name: 'Builder', primary_job_id: 'job_one', paused: false, pause_reason: '', pause_reason_truncated: false }], has_more: false }, jobs: { items: [{ id: 'job_one', title: 'Delivery board', worker_id: 'worker_one', worker_name: 'Builder', status: 'active', revision: 2, brief_preview: 'Current brief', brief_preview_truncated: false, updated_at: '2026-10-19T09:00:00Z', progress, checklist: null }], has_more: false } }; }
function json(value: unknown) { return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } }); }
async function client() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = new Api('private-synthetic-test-token', project.id);
  vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ instance_id: 'instance_one' })).mockResolvedValueOnce(json(project)));
  await api.connect(); return api;
}
afterEach(() => vi.unstubAllGlobals());

describe('bounded stored Delivery API', () => {
  it('accepts nonempty whitespace-only truncated progress previews', async () => {
    const api = await client();
    for (const prefix of [' '.repeat(240), '\t'.repeat(240), '\u2003'.repeat(80)]) {
      const value = snapshot();
      value.jobs.items = [{ ...value.jobs.items[0], progress: { ...progress, summary: prefix, next: prefix, summary_truncated: true, next_truncated: true } }];
      const fetch = vi.fn().mockResolvedValue(json(value)); vi.stubGlobal('fetch', fetch);
      await expect(api.delivery()).resolves.toEqual(value);
      expect(fetch).toHaveBeenCalledTimes(1);
    }
    api.close();
  });
  it('keeps required preview text, flags and Unicode bounds strict', async () => {
    const api = await client();
    for (const field of ['summary', 'next', 'blocker'] as const) {
      const cases: Array<[unknown, unknown]> = [
        ['valid', null], ['valid', 'true'], ['valid', 1], ['valid', undefined],
        ['a'.repeat(241), true], ['\u2003'.repeat(81), true], ['\ud800', true], ['\udfff', true], ['\u0000', true], ['\u007f', true],
      ];
      cases.push(['', true]);
      for (const [text, truncated] of cases) {
        const value = snapshot();
        value.jobs.items = [{ ...value.jobs.items[0], progress: { ...progress, [field]: text, [`${field}_truncated`]: truncated } as typeof progress }];
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
        await expect(api.delivery()).rejects.toMatchObject({ code: 'invalid_response' });
      }
    }
    // A blocker remains optional, including when its supplied preview is blank.
    for (const blocker of ['', ' '.repeat(240)]) {
      const value = snapshot();
      value.jobs.items = [{ ...value.jobs.items[0], progress: { ...progress, blocker, blocker_truncated: blocker !== '' } }];
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(api.delivery()).resolves.toEqual(value);
    }
    api.close();
  });
  it('still refuses empty or all-whitespace full checkpoints', async () => {
    const api = await client();
    const full = { id: 'checkpoint_one', project_id: project.id, job_id: 'job_one', job_revision: 1, sequence: 1, actor_id: 'principal_one', worker_id: 'worker_one', done: 'Done', next: 'Next', blocker: '', evidence: [], created_at: '2026-10-01T00:00:00Z' };
    const latest = { job_id: 'job_one', job_revision: 1, checkpoint: full };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(latest)));
    await expect(api.checkpointLatest('job_one')).resolves.toEqual(latest);
    for (const field of ['done', 'next'] as const) {
      for (const text of ['', ' '.repeat(241), '\u2003'.repeat(81)]) {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ job_id: 'job_one', job_revision: 1, checkpoint: { ...full, [field]: text } })));
        await expect(api.checkpointLatest('job_one')).rejects.toMatchObject({ code: 'invalid_response' });
      }
    }
    api.close();
  });
  it('uses one scoped read with explicit independent continuation and no worker binding', async () => {
    const api = await client();
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      expect(String(input)).toBe('https://service.example/api/v1/projects/project_one/delivery?worker_limit=1&job_limit=1&worker_cursor=worker_page&job_cursor=job_page');
      expect(init?.method).toBe('GET');
      expect(new Headers(init?.headers).get('x-fc-instance')).toBe('instance_one');
      expect(new Headers(init?.headers).has('x-fc-worker')).toBe(false);
      return json(snapshot());
    }); vi.stubGlobal('fetch', fetch);
    const result = await api.delivery({ worker_limit: 1, job_limit: 1, worker_cursor: 'worker_page', job_cursor: 'job_page' });
    expect(result.jobs.items[0].progress).toEqual(progress);
    expect(fetch).toHaveBeenCalledTimes(1); api.close();
  });
  it('rejects foreign, expanded, oversized, duplicated or inconsistent summaries', async () => {
    const api = await client();
    const original = snapshot(); const worker = original.workers.items[0], job = original.jobs.items[0];
    for (const value of [
      { ...original, extra: 'private' },
      { ...original, workers: { items: [{ ...worker, project_id: 'project_foreign' }], has_more: false } },
      { ...original, workers: { items: [worker, worker], has_more: false } },
      { ...original, workers: { items: [{ ...worker, pause_reason: 'Not paused' }], has_more: false } },
      { ...original, jobs: { items: [{ ...job, brief: 'Do not expand' }], has_more: false } },
      { ...original, jobs: { items: [{ ...job, status: 'completed' }], has_more: false } },
      { ...original, jobs: { items: [job, job], has_more: false } },
      { ...original, jobs: { items: [{ ...job, worker_id: '', worker_name: 'Builder' }], has_more: false } },
      { ...original, jobs: { items: [{ ...job, progress: { ...progress, next: '🔥'.repeat(61) } }], has_more: false } },
      { ...original, jobs: { items: [{ ...job, progress: { ...progress, job_revision: 3 } }], has_more: false } },
      { ...original, jobs: { items: [{ ...job, progress: { ...progress, evidence: ['private-path'] } }], has_more: false } },
      { ...original, jobs: { items: [job], has_more: true } },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(api.delivery()).rejects.toMatchObject({ code: 'invalid_response' });
    } api.close();
  });
  it('accepts compact progress, known-zero checklists and independent recorded times in one read', async () => {
    const api = await client(), value = snapshot();
    value.jobs.items[0] = {...value.jobs.items[0], brief_preview:'b'.repeat(239), brief_preview_truncated:true,updated_at:'2024-02-03T04:05:06+13:00',
      progress:{...progress,summary:' '.repeat(240),summary_truncated:true},
      checklist:{revision:2,job_revision:1,total:0,done:0,created_at:'2099-01-01T00:00:00Z'}};
    const fetch = vi.fn().mockResolvedValue(json(value)); vi.stubGlobal('fetch',fetch);
    expect(await api.delivery()).toEqual(value); expect(fetch).toHaveBeenCalledTimes(1); api.close();
  });
  it('rejects malformed preview, attribution, count and timestamp facts without fetching details', async () => {
    const api = await client(), original = snapshot(), job = original.jobs.items[0];
    const checklist = {revision:2,job_revision:1,total:2,done:1,created_at:'2021-01-01T00:00:00Z'};
    const variants = [
      {brief_preview:'🔥'.repeat(61)}, {brief_preview:null}, {brief_preview_truncated:null}, {brief_preview:'',brief_preview_truncated:true}, {updated_at:'not-a-date'},
      {progress:undefined}, ...[{summary:'a'.repeat(241)},{summary:'\ud800'},{summary:'bad\u0000'},{summary_truncated:null},{evidence:['private']}].map(change=>({progress:{...progress,...change}})),
      {checklist:undefined}, ...[{revision:0},{job_revision:3},{total:101},{done:3},{done:-1},{done:0.5},{total:null},{created_at:'invalid'},{items:[]}].map(change=>({checklist:{...checklist,...change}})),
      {progress:null}, {progress:{...progress,summary:'',summary_truncated:true}},
    ];
    for(const change of variants) {
      const fetch=vi.fn().mockResolvedValue(json({...original,jobs:{items:[{...job,...change}],has_more:false}})); vi.stubGlobal('fetch',fetch);
      await expect(api.delivery()).rejects.toMatchObject({code:'invalid_response'}); expect(fetch).toHaveBeenCalledTimes(1);
    }
    api.close();
  });
  it('rejects invalid selectors before network and accepts unassigned progress-empty jobs', async () => {
    const api = await client(); const fetch = vi.fn(); vi.stubGlobal('fetch', fetch);
    for (const options of [{ job_limit: 0 }, { worker_limit: 101 }, { job_limit: 1.5 }, { worker_cursor: '' }, { job_cursor: 'two words' }]) await expect(api.delivery(options)).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetch).not.toHaveBeenCalled();
    const value = snapshot(); value.jobs.items = [{ ...value.jobs.items[0], worker_id: '', worker_name: '', progress: {summary:'',next:'',blocker:'',summary_truncated:false,next_truncated:false,blocker_truncated:false} }];
    fetch.mockResolvedValue(json(value)); expect(await api.delivery()).toEqual(value); api.close();
  });
});
