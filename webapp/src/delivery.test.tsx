import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Delivery, DeliveryHero, DeliveryLanes, MergeHeadContent, deliveryQuery, deliveryReviewQuery, groupDeliveryLanes, selectMergeHead } from './delivery';
import { WorkspaceContext, type Session } from './workspace';
import type { DeliveryJob, DeliverySnapshot, DeliveryWorker, ReviewPullSummary, ReviewQueue, ReviewRunSummary } from './types';

const home = '/projects/project_one';
const key = ['fc', 'connection_one', 'instance_one', 'project_one', 'actor_one'] as const;
const worker: DeliveryWorker = { id: 'worker_one', project_id: 'project_one', name: 'Builder', primary_job_id: 'job_active', paused: true, pause_reason: 'Waiting <literal>', pause_reason_truncated: true };
const job = (id: string, status: DeliveryJob['status'], worker_id = worker.id, worker_name = worker.name): DeliveryJob => ({ id, title: id, worker_id, worker_name, status, revision: 3, checkpoint: null });
const board: DeliverySnapshot = { workers: { items: [worker, { ...worker, id: 'empty_worker', name: 'Empty worker', primary_job_id: 'outside_job', paused: false }], has_more: true, cursor: 'next_workers' }, jobs: { items: [job('job_parked', 'parked'), job('job_blocked', 'blocked'), job('job_active', 'active'), job('outside_owner_job', 'active', 'outside_owner', 'Elsewhere'), job('unassigned_job', 'active', '', '')], has_more: true, cursor: 'next_jobs' } };
const run = (channel: 'hosted' | 'cli'): ReviewRunSummary => ({ source: 'fixture', id: 'run_' + channel, pull_number: 2, source_run_id: 'source_' + channel, channel, head_sha: 'head', reviewer: 'Reviewer', scope: 'full', coverage_limits: [], outcome: 'completed', attributable: true, complete: true, started_at: '', finished_at: '2026-10-18T00:00:00Z', found: 5, accepted: 2, routed: 1, finalized: true, fixed: 1 });
const pull = (number: number, state: ReviewPullSummary['state']): ReviewPullSummary => ({ number, state, title: `Change ${number}`, head_sha: 'head', base_ref: 'main', base_sha: 'base', ci_state: 'success', mergeability: 'mergeable', complete: true, run_count: 2, attempt_count: 0, recent_runs: [run('hosted'), run('cli')], latest_attempts: [] });
const queue = (items: ReviewPullSummary[], has_more = false): ReviewQueue => ({ items, has_more, ...(has_more ? { cursor: 'next_queue' } : {}), configured: true, project_id: 'project_one', revision: 1, snapshot_id: 'snapshot_one', source: 'retained', observed_at: '2026-10-18T00:00:00Z', recorded_at: '2026-10-18T00:00:00Z' });
function markup(node: React.ReactNode) {
  const session = { project:{id:'project_one',repository:'example/project'}, controller: new AbortController(), api: {} } as Session;
  const client = new QueryClient();
  try { return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: false }}>{node}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>); }
  finally { client.clear(); }
}

describe('bounded Delivery board', () => {
  it('keeps empty workers, real owners outside the worker page, and genuinely unassigned jobs distinct', () => {
    const lanes = groupDeliveryLanes(board);
    expect(lanes.map(lane => lane.id)).toEqual(['worker_one', 'outside_owner', 'empty_worker', '']);
    expect(lanes[0].jobs.map(item => item.status)).toEqual(['active', 'blocked', 'parked']);
    expect(lanes[2].jobs).toEqual([]);
    expect(lanes[1].name).toBe('Elsewhere');
    expect(lanes[1].worker).toBeUndefined();
    expect(lanes[3].jobs.map(item => item.id)).toEqual(['unassigned_job']);
    expect(board.jobs.items[0].status).toBe('parked');
  });
  it('shows explicit pause/focus and safe checkpoint previews without implying an empty lane is idle', () => {
    const current = job('job_active', 'active');
    current.checkpoint = { sequence: 4, job_revision: 1, done: '<script>done</script>', next: 'Next <literal>', blocker: 'Waiting', done_truncated: true, next_truncated: false, blocker_truncated: false };
    const value = { ...board, jobs: { ...board.jobs, items: [current] } };
    const html = markup(<DeliveryLanes value={value} home={home} />);
    expect(html).toContain('Primary focus');
    expect(html).toContain('Paused');
    expect(html).toContain('Waiting &lt;literal&gt;…');
    expect(html).toContain('Checkpoint from job revision 1; current revision 3.');
    expect(html).toContain('&lt;script&gt;done&lt;/script&gt;…');
    expect(html).toContain('>Primary job</a>');
    expect(html).not.toContain('outside this job page');
    expect(html).toContain('No jobs in loaded slice.');
    expect(html).not.toMatch(/<script|Idle|<button/u);
  });
  it('distinguishes a complete empty lane from a later or incomplete job slice', () => {
    const complete = { ...board, jobs: { items: [], has_more: false } };
    expect(markup(<DeliveryLanes value={complete} home={home} />)).toContain('No open jobs.');
    const laterPage = markup(<DeliveryLanes value={complete} home={home} partialJobs />);
    expect(laterPage).toContain('No jobs in loaded slice.');
    expect(laterPage).not.toContain('No open jobs.');
  });
  it('keeps primary focus first without letting a parked primary hide active lane work', () => {
    const value = {...board,workers:{...board.workers,items:[{...worker,primary_job_id:'job_parked'}]}};
    const lanes=groupDeliveryLanes(value);
    expect(lanes[0].jobs.map(item=>item.id)).toEqual(['job_parked','job_active','job_blocked']);
    const html=markup(<DeliveryLanes value={value} home={home} />);
    expect(html).toContain('Worker inbox');
    expect(html).toContain('Job history');
    expect(html).toContain('worker_id=worker_one&amp;status=all');
    expect(html).toContain('badge delivery-primary');
  });
  it('requests one atomic bounded page with independent cursors and cancellation, without actor filtering', async () => {
    const read = vi.fn().mockResolvedValue(board);
    const session = { project:{id:'project_one',repository:'example/project'}, controller: new AbortController(), api: { delivery: read } } as unknown as Session;
    const options = deliveryQuery(session, key, 'workers_two', 'jobs_three');
    const request = new AbortController();
    await options.queryFn({ signal: request.signal });
    expect(read).toHaveBeenCalledTimes(1);
    expect(read.mock.calls[0][0]).toEqual({ worker_limit: 20, job_limit: 50, worker_cursor: 'workers_two', job_cursor: 'jobs_three' });
    expect(options.queryKey).toEqual([...key, 'delivery', 'workers_two', 'jobs_three']);
    session.controller.abort();
    expect(read.mock.calls[0][1].aborted).toBe(true);
  });
});

describe('stored merge head and review summaries', () => {
  it('selects only the first open entry after known merged/closed predecessors', () => {
    expect(selectMergeHead(queue([pull(1, 'merged'), pull(2, 'closed'), pull(3, 'open'), pull(4, 'open')]))).toEqual({ state: 'head', pull: pull(3, 'open') });
    expect(selectMergeHead(queue([pull(1, 'unknown'), pull(2, 'open')]))).toEqual({ state: 'unknown', reason: 'preceding' });
    expect(selectMergeHead(queue([pull(1, 'merged')], true))).toEqual({ state: 'unknown', reason: 'next-page' });
    expect(selectMergeHead(queue([pull(1, 'closed')]))).toEqual({ state: 'none' });
    expect(selectMergeHead({ ...queue([]), configured: false })).toEqual({ state: 'unknown', reason: 'unconfigured' });
  });
  it('does not claim no head or merge readiness from a partial queue', () => {
    const partial = markup(<MergeHeadContent queue={queue([pull(1, 'merged')], true)} home={home} />);
    expect(partial).toContain('Merge head not determined');
    expect(partial).toContain('more queue entries remain');
    expect(partial).not.toContain('No open pull requests');
    const head = markup(<MergeHeadContent queue={queue([pull(2, 'open')])} home={home} />);
    expect(head).toContain('snapshot=snapshot_one');
    expect(head).toContain('CI: Success');
    expect(head).not.toContain('Observed CI');
    expect(head).not.toContain('First open PR');
    expect(head).not.toContain('runs ·');
    expect(partial).toContain(`href="${home}#review-queue"`);
    expect(head).toContain(`href="${home}#review-pr-2"`);
    expect(head).toContain('>Review queue ↗</a>');
    expect(head).not.toContain('delivery-link-arrow');
    expect(head).not.toMatch(/Ready to merge|Taper|Closed review/u);
  });
  it('uses shared separate Hosted and CLI history on the actual merge head', () => {
    const html = markup(<MergeHeadContent queue={queue([pull(2, 'open')])} home={home} />);
    expect(html).toContain('Hosted CodeRabbit');
    expect(html).toContain('CLI CodeRabbit');
    expect(html).toContain('delivery-head-history');
    expect(markup(<MergeHeadContent queue={queue([pull(1, 'merged')])} home={home} />)).toBe('');
    expect(markup(<MergeHeadContent queue={{ ...queue([]), configured: false }} home={home} />)).toBe('');
  });
  it('shares one bounded queue read between hero and overview without run/archive fanout', async () => {
    const read = vi.fn().mockResolvedValue(queue([pull(2, 'open')]));
    const session = { project:{id:'project_one',repository:'example/project'}, controller: new AbortController(), api: { reviewQueue: read } } as unknown as Session;
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    try {
      const hero = deliveryReviewQuery(session, key), overview = deliveryReviewQuery(session, key);
      await Promise.all([client.fetchQuery(hero), client.fetchQuery(overview)]);
      expect(read).toHaveBeenCalledTimes(1);
      expect(read.mock.calls[0][0]).toEqual({ limit: 20 });
      expect(hero.queryKey).toEqual([...key, 'reviews', 'current', 'queue', '']);
    } finally { client.clear(); }
  });
  it('keeps only project identity in the hero when no open or configured queue exists', () => {
    for (const value of [queue([pull(1, 'merged')]), { ...queue([]), configured: false }]) {
      const session = { project:{id:'project_one',repository:'example/project'}, controller: new AbortController(), api: {} } as Session;
      const client = new QueryClient();
      client.setQueryData(deliveryReviewQuery(session, key).queryKey, value);
      try {
        const html = markup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: false }}><DeliveryHero><h1>Project name</h1></DeliveryHero></WorkspaceContext.Provider></QueryClientProvider>);
        expect(html).toContain('delivery-hero-identity-only');
        expect(html).toContain('<h1>Project name</h1>');
        expect(html).not.toContain('Merge train head');
        expect(html).not.toContain('No open pull requests');
      } finally { client.clear(); }
    }
  });
  it('renders both independently paged board and overview from retained data without starting another read', () => {
    const api = { delivery: vi.fn(), reviewQueue: vi.fn() };
    const session = { project:{id:'project_one',repository:'example/project'}, controller: new AbortController(), api } as unknown as Session;
    const client = new QueryClient();
    client.setQueryData(deliveryQuery(session, key).queryKey, board);
    client.setQueryData(deliveryReviewQuery(session, key).queryKey, queue([pull(2, 'open')]));
    try {
      const html = markup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: false }}><DeliveryHero><h1>Project name</h1></DeliveryHero><Delivery /></WorkspaceContext.Provider></QueryClientProvider>);
      expect(html).not.toContain('Next delivery workers');
      expect(html).not.toContain('Next delivery jobs');
      expect(html).toContain('Load more work');
      expect(html).toContain('review-queue-title');
      expect(html).toContain('Owner outside this worker page');
      expect(html).toContain('Manage jobs');
      expect(html).toContain('href="/projects/project_one/jobs"');
      expect(api.delivery).not.toHaveBeenCalled();
      expect(api.reviewQueue).not.toHaveBeenCalled();
    } finally { client.clear(); }
  });
});
