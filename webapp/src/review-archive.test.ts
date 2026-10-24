import { describe, expect, it, vi } from 'vitest';
import { ReviewArchive, type ReviewArchivePage } from './review-archive';

const stamp = '2026-10-18T00:00:00Z';
type Item = { source: string; id: string };
function page(start: number, length = 20, total = 137, snapshot = 'snapshot_one'): ReviewArchivePage<Item> {
  const end = Math.min(start + length, total);
  return { project_id: 'project_one', revision: 1, snapshot_id: snapshot, source: 'publisher', observed_at: stamp, recorded_at: stamp, items: Array.from({ length: end - start }, (_, index) => ({ source: index % 2 ? 'producer_b' : 'producer_a', id: `item_${start + index}` })), has_more: end < total, ...(end < total ? { cursor: `after_${end}` } : {}) };
}
const identity = (item: Item) => JSON.stringify([item.source, item.id]);
function deferred<T>() { let resolve!: (value: T) => void; let reject!: (error: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

describe('explicit bounded review archive continuation', () => {
  it('loads only one next page per action through more than100 retained entries, without duplicates or lost tails', async () => {
    const read = vi.fn(async (cursor: string) => page(Number(cursor.slice(6))));
    const archive = new ReviewArchive(read, identity); archive.reset(page(0));
    expect(read).not.toHaveBeenCalled(); expect(archive.snapshot().items).toHaveLength(20);
    for (const shown of [40, 60, 80, 100, 120, 137]) {
      const before = read.mock.calls.length;
      await archive.more(); expect(read).toHaveBeenCalledTimes(before + 1); expect(archive.snapshot().items).toHaveLength(shown);
      expect(new Set(archive.snapshot().items.map(identity)).size).toBe(shown); expect(archive.snapshot().hasMore).toBe(shown !== 137);
    }
    await archive.more(); expect(read).toHaveBeenCalledTimes(6); expect(archive.snapshot().items.at(-1)?.id).toBe('item_136');
  });

  it('keeps producer-qualified identity when two original producers use the same local attempt identifier', async () => {
    const second = page(20, 1, 21); second.items = [{ source: 'producer_b', id: 'shared' }];
    const archive = new ReviewArchive(async () => second, identity);
    const first = page(0, 1, 21); first.items = [{ source: 'producer_a', id: 'shared' }]; archive.reset(first);
    await archive.more(); expect(archive.snapshot().items.map(identity)).toEqual(['["producer_a","shared"]', '["producer_b","shared"]']); expect(archive.snapshot().error).toBeUndefined();
  });

  it('does not overlap continuation and discards a delayed old result after first-page refresh or scope cancellation', async () => {
    for (const cancelOnly of [false, true]) {
      const delayed = deferred<ReviewArchivePage<Item>>(); let signal: AbortSignal | undefined;
      const read = vi.fn((_cursor: string, selected: AbortSignal) => { signal = selected; return delayed.promise; });
      const archive = new ReviewArchive(read, identity); archive.reset(page(0));
      const pending = archive.more(); await archive.more(); expect(read).toHaveBeenCalledTimes(1);
      if (cancelOnly) archive.cancel(); else archive.reset(page(0, 20, 42, 'snapshot_new'));
      expect(signal?.aborted).toBe(true); delayed.resolve(page(20)); await pending;
      expect(archive.snapshot().items).toHaveLength(20); expect(archive.snapshot().hasMore).toBe(true); expect(archive.snapshot().error).toBeUndefined();
    }
  });

  it('clears loaded tails immediately while refetching first page and never auto-drains them again', async () => {
    const read = vi.fn(async () => page(20)); const archive = new ReviewArchive(read, identity); archive.reset(page(0)); await archive.more();
    expect(archive.snapshot().items).toHaveLength(40); archive.reset(undefined, true);
    expect(archive.snapshot()).toMatchObject({ items: [], hasMore: false, busy: true }); await archive.more(); expect(read).toHaveBeenCalledTimes(1);
    archive.reset(page(0)); expect(archive.snapshot().items).toHaveLength(20); expect(read).toHaveBeenCalledTimes(1);
  });

  it.each(['snapshot', 'revision', 'publisher', 'duplicate', 'cursor'])('refuses inconsistent %s without silently dropping or mixing records', async corruption => {
    const second = page(20);
    if (corruption === 'snapshot') second.snapshot_id = 'other_snapshot';
    if (corruption === 'revision') second.revision++;
    if (corruption === 'publisher') second.source = 'other_publisher';
    if (corruption === 'duplicate') second.items[0] = page(0).items[0];
    if (corruption === 'cursor') second.cursor = page(0).cursor;
    const archive = new ReviewArchive(async () => second, identity); archive.reset(page(0)); await archive.more();
    expect(archive.snapshot().items).toHaveLength(20); expect(archive.snapshot().hasMore).toBe(true); expect(archive.snapshot().error).toMatchObject({ code: 'invalid_response' });
  });

  it('keeps failed continuation explicit and ignores late rejection from a retired view', async () => {
    const failure = { code: 'forbidden', status: 403 }; const read = vi.fn().mockRejectedValueOnce(failure).mockResolvedValueOnce(page(20));
    const archive = new ReviewArchive<Item>(read, identity); archive.reset(page(0)); await archive.more();
    expect(archive.snapshot().error).toBe(failure); expect(read).toHaveBeenCalledTimes(1); await archive.more(); expect(read).toHaveBeenCalledTimes(2); expect(archive.snapshot().error).toBeUndefined();
    const held = deferred<ReviewArchivePage<Item>>(); const old = new ReviewArchive(() => held.promise, identity); old.reset(page(0)); const pending = old.more(); old.reset(page(0, 20, 25, 'new_snapshot')); held.reject(failure); await pending; expect(old.snapshot().error).toBeUndefined();
  });
});
