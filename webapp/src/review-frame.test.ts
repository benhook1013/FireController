import { describe, expect, it } from 'vitest';
import { reviewQueueFrame } from './review-frame';
import { reviewQueueBadges, type ReviewFronts } from './review-fronts';
import { retainedRead } from './retained-read';
import type { ReviewQueue } from './types';

const source = { head_sha: 'a'.repeat(40), base_sha: 'b'.repeat(40), base_ref: 'main' };
function queue(snapshot: string): ReviewQueue {
  return { project_id: 'project_one', configured: true, revision: snapshot === 'old' ? 1 : 2, snapshot_id: snapshot, source: 'fixture', observed_at: '2026-10-26T00:00:00Z', recorded_at: '2026-10-26T00:00:00Z', cursor: '', has_more: false,
    items: [1, 2].map(number => ({ number, title: `PR ${number}`, state: 'open', ...source, ci_state: 'success', mergeability: 'mergeable', complete: true, run_count: 0, run_counts: { hosted: 0, cli: 0, subagent: 0, manual: 0 }, attempt_count: 0, recent_runs: [], latest_attempts: [] })) };
}
function fronts(snapshot: string, number = 1): ReviewFronts {
  return { project_id: 'project_one', configured: true, repository_id: 1, configuration_revision: 1, policy_revision: 1, snapshot_id: snapshot, queue: [1, 2], channels: [{ channel: 'hosted', state: 'ready', pull_number: number, ...source }] };
}
describe('coherent review presentation', () => {
  it.each(['queue first', 'fronts first'])('retains a verified front until both reads arrive: %s', order => {
    const before = reviewQueueFrame(queue('old'), fronts('old'))!;
    const partial = order === 'queue first' ? reviewQueueFrame(queue('new'), fronts('old')) : reviewQueueFrame(queue('old'), fronts('new'));
    expect(partial).toBeUndefined();
    const displayed = retainedRead('session/project/recent', partial, { scope: 'session/project/recent', value: before })!;
    expect(displayed).toBe(before);
    expect(reviewQueueBadges(displayed.queue.items[0], displayed.queue.snapshot_id, displayed.fronts)[0]?.kind).toBe('front');
    const after = reviewQueueFrame(queue('new'), fronts('new'))!;
    expect(retainedRead('session/project/recent', after, { scope: 'session/project/recent', value: before })).toBe(after);
    expect(reviewQueueBadges(after.queue.items[0], after.queue.snapshot_id, after.fronts)[0]?.kind).toBe('front');
  });
  it('publishes a genuine front change once its matching queue arrives', () => {
    const changed = reviewQueueFrame(queue('new'), fronts('new', 2))!;
    expect(reviewQueueBadges(changed.queue.items[0], changed.queue.snapshot_id, changed.fronts)).toEqual([]);
    expect(reviewQueueBadges(changed.queue.items[1], changed.queue.snapshot_id, changed.fronts)[0]?.kind).toBe('front');
  });
  it('does not borrow a frame across projects/pages or require fronts for an unconfigured queue', () => {
    const before = reviewQueueFrame(queue('old'), fronts('old'))!;
    expect(retainedRead('other/project/page', undefined, { scope: 'session/project/recent', value: before })).toBeUndefined();
    expect(reviewQueueFrame(undefined, fronts('old'))).toBeUndefined();
    expect(reviewQueueFrame(queue('new'), undefined)).toBeUndefined();
    const empty = { ...queue('new'), configured: false };
    expect(reviewQueueFrame(empty)).toEqual({ queue: empty, fronts: undefined });
  });
});
