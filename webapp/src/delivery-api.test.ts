import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import type { DeliverySnapshot } from './types';

const project = { id: 'project_one', name: 'Delivery project', github_repository_id: 42, repository: 'synthetic/delivery' };
const checkpoint = { sequence: 3, job_revision: 1, done: 'API proof passed', next: 'Read the latest brief', blocker: '', done_truncated: false, next_truncated: false, blocker_truncated: false };
function snapshot(): DeliverySnapshot { return { workers: { items: [{ id: 'worker_one', project_id: project.id, name: 'Builder', primary_job_id: 'job_one', paused: false, pause_reason: '', pause_reason_truncated: false }], has_more: false }, jobs: { items: [{ id: 'job_one', title: 'Delivery board', worker_id: 'worker_one', worker_name: 'Builder', status: 'active', revision: 2, checkpoint }], has_more: false } }; }
function json(value: unknown) { return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } }); }
async function client() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = new Api('private-synthetic-test-token', project.id);
  vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ instance_id: 'instance_one' })).mockResolvedValueOnce(json(project)));
  await api.connect(); return api;
}
afterEach(() => vi.unstubAllGlobals());

describe('bounded stored Delivery API', () => {
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
    expect(result.jobs.items[0].checkpoint?.job_revision).toBeLessThan(result.jobs.items[0].revision);
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
      { ...original, jobs: { items: [{ ...job, checkpoint: { ...checkpoint, next: '🔥'.repeat(61) } }], has_more: false } },
      { ...original, jobs: { items: [{ ...job, checkpoint: { ...checkpoint, job_revision: 3 } }], has_more: false } },
      { ...original, jobs: { items: [{ ...job, checkpoint: { ...checkpoint, evidence: ['private-path'] } }], has_more: false } },
      { ...original, jobs: { items: [job], has_more: true } },
    ]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(value)));
      await expect(api.delivery()).rejects.toMatchObject({ code: 'invalid_response' });
    } api.close();
  });
  it('rejects invalid selectors before network and accepts unassigned checkpoint-free jobs', async () => {
    const api = await client(); const fetch = vi.fn(); vi.stubGlobal('fetch', fetch);
    for (const options of [{ job_limit: 0 }, { worker_limit: 101 }, { job_limit: 1.5 }, { worker_cursor: '' }, { job_cursor: 'two words' }]) await expect(api.delivery(options)).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetch).not.toHaveBeenCalled();
    const value = snapshot(); value.jobs.items = [{ ...value.jobs.items[0], worker_id: '', worker_name: '', checkpoint: null }];
    fetch.mockResolvedValue(json(value)); expect(await api.delivery()).toEqual(value); api.close();
  });
});
