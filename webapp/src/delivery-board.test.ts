import { describe, expect, it, vi } from 'vitest';
import { DeliveryBoardArchive } from './delivery-board';
import type { DeliveryJob, DeliverySnapshot, DeliveryWorker } from './types';
const worker = (id: string): DeliveryWorker => ({ id, project_id: 'project_one', name: id, primary_job_id: '', paused: false, pause_reason: '', pause_reason_truncated: false });
const job = (id: string): DeliveryJob => ({ id, title: id, worker_id: 'worker_one', worker_name: 'Builder', revision: 1, status: 'active', checkpoint: null });
const first: DeliverySnapshot = { workers: { items: [worker('worker_one')], has_more: true, cursor: 'workers_next' }, jobs: { items: [job('job_one')], has_more: true, cursor: 'jobs_next' } };
const next: DeliverySnapshot = { workers: { items: [worker('worker_two')], has_more: false }, jobs: { items: [job('job_two')], has_more: false } };

describe('append-only Delivery continuation', () => {
  it('performs one bounded request on demand and retains earlier lanes/jobs', async () => {
    const read = vi.fn().mockResolvedValue(next), archive = new DeliveryBoardArchive(read);
    archive.reset(first); expect(read).not.toHaveBeenCalled();
    await archive.more();
    expect(read).toHaveBeenCalledTimes(1);
    expect(read.mock.calls[0].slice(0, 2)).toEqual(['workers_next', 'jobs_next']);
    expect(archive.snapshot().value?.workers.items.map(x => x.id)).toEqual(['worker_one', 'worker_two']);
    expect(archive.snapshot().value?.jobs.items.map(x => x.id)).toEqual(['job_one', 'job_two']);
    await archive.more(); expect(read).toHaveBeenCalledTimes(1);
  });
  it('ignores exhausted collections and updates duplicate job IDs without duplicating cards', async () => {
    const read = vi.fn().mockResolvedValue({ workers: { items: [worker('ignored')], has_more: true, cursor: 'unrequested' }, jobs: { items: [{ ...job('job_one'), worker_id: '', status: 'blocked' }], has_more: false } });
    const archive = new DeliveryBoardArchive(read);
    archive.reset({ ...first, workers: { ...first.workers, has_more: false, cursor: undefined } });
    await archive.more();
    expect(read.mock.calls[0].slice(0, 2)).toEqual(['', 'jobs_next']);
    expect(archive.snapshot().value?.workers.items.map(x => x.id)).toEqual(['worker_one']);
    expect(archive.snapshot().value?.jobs.items).toHaveLength(1);
    expect(archive.snapshot().value?.jobs.items[0].worker_id).toBe('');
  });
  it('rejects a repeated cursor without replacing the visible board', async () => {
    const archive = new DeliveryBoardArchive(vi.fn().mockResolvedValue(first));
    archive.reset(first); await archive.more();
    expect(archive.snapshot().error).toBeTruthy();
    expect(archive.snapshot().value).toBe(first);
  });
  it('discards delayed continuation after refresh or session cancellation', async () => {
    let finish!: (value: DeliverySnapshot) => void;
    const read = vi.fn().mockImplementation(() => new Promise<DeliverySnapshot>(resolve => { finish = resolve; }));
    const archive = new DeliveryBoardArchive(read); archive.reset(first);
    const pending = archive.more();
    expect(archive.snapshot().busy).toBe(true);
    archive.reset(next); expect(read.mock.calls[0][2].aborted).toBe(true);
    finish(first); await pending;
    expect(archive.snapshot().value).toBe(next);
    expect(archive.snapshot().error).toBeUndefined();
  });
  it('does not duplicate a request on double click and permits retry after failure', async () => {
    let fail!: (error: Error) => void;
    const read = vi.fn().mockImplementationOnce(() => new Promise<DeliverySnapshot>((_, reject) => { fail = reject; })).mockResolvedValue(next);
    const archive = new DeliveryBoardArchive(read); archive.reset(first);
    const pending = archive.more(); await archive.more(); expect(read).toHaveBeenCalledTimes(1);
    fail(new Error('temporary')); await pending; expect(archive.snapshot().value).toBe(first);
    await archive.more(); expect(archive.snapshot().error).toBeUndefined(); expect(read).toHaveBeenCalledTimes(2);
  });
});
