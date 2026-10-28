import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Delivery, DeliveryHero, DeliveryLanes, DeliveryWork, MergeHeadContent, deliveryQuery, deliveryReviewQuery, groupDeliveryLanes, selectMergeHead } from './delivery';
import { WorkspaceContext, type Session } from './workspace';
import { reviewQueueQuery } from './review-workspace';
import type { ReviewFronts } from './review-fronts';
import type { DeliveryJob, DeliverySnapshot, DeliveryWorker, ReviewPullSummary, ReviewQueue, ReviewRunSummary } from './types';

const home = '/projects/project_one';
const key = ['fc', 'connection_one', 'instance_one', 'project_one', 'actor_one'] as const;
const worker: DeliveryWorker = { id: 'worker_one', project_id: 'project_one', name: 'Builder', primary_job_id: 'job_active', paused: true, pause_reason: 'Waiting <literal>', pause_reason_truncated: true };
const job = (id: string, status: DeliveryJob['status'], worker_id = worker.id, worker_name = worker.name): DeliveryJob => ({ id, title: id, worker_id, worker_name, status, revision: 3, brief_preview: '', brief_preview_truncated: false, updated_at: '2026-10-19T08:00:00Z', progress: {summary:'',next:'',blocker:'',summary_truncated:false,next_truncated:false,blocker_truncated:false}, checklist: null });
const board: DeliverySnapshot = { workers: { items: [worker, { ...worker, id: 'empty_worker', name: 'Empty worker', primary_job_id: 'outside_job', paused: false }], has_more: true, cursor: 'next_workers' }, jobs: { items: [job('job_parked', 'parked'), job('job_blocked', 'blocked'), job('job_active', 'active'), job('outside_owner_job', 'active', 'outside_owner', 'Elsewhere'), job('unassigned_job', 'active', '', '')], has_more: true, cursor: 'next_jobs' } };
const run = (channel: 'hosted' | 'cli'): ReviewRunSummary => ({ source: 'fixture', id: 'run_' + channel, pull_number: 2, source_run_id: 'source_' + channel, channel, head_sha: 'head', reviewer: 'Reviewer', scope: 'full', coverage_limits: [], outcome: 'completed', attributable: true, complete: true, started_at: '', finished_at: '2026-10-18T00:00:00Z', found: 5, accepted: 2, routed: 1, finalized: true, fixed: 1 });
const pull = (number: number, state: ReviewPullSummary['state']): ReviewPullSummary => ({ number, state, title: `Change ${number}`, head_sha: 'head', base_ref: 'main', base_sha: 'base', ci_state: 'success', mergeability: 'mergeable', complete: true, run_count: 2, run_counts: { hosted: 1, cli: 1, subagent: 0, manual: 0 }, attempt_count: 0, recent_runs: [run('hosted'), run('cli')], latest_attempts: [] });
const queue = (items: ReviewPullSummary[], has_more = false): ReviewQueue => ({ items, has_more, ...(has_more ? { cursor: 'next_queue' } : {}), configured: true, project_id: 'project_one', revision: 1, snapshot_id: 'snapshot_one', source: 'retained', observed_at: '2026-10-18T00:00:00Z', recorded_at: '2026-10-18T00:00:00Z' });
function markup(node: React.ReactNode) {
  const session = { project:{id:'project_one',repository:'example/project'}, controller: new AbortController(), api: {} } as Session;
  const client = new QueryClient();
  try { return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: false }}>{node}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>); }
  finally { client.clear(); }
}

describe('bounded Delivery board', () => {
  it('keeps configured workers first, then alphabetical workers, with Unassigned last', () => {
    const lanes = groupDeliveryLanes(board,['empty_worker','worker_one']);
    expect(lanes.map(lane=>lane.id)).toEqual(['empty_worker','worker_one','outside_owner','']);
    expect(groupDeliveryLanes(board,['worker_one']).map(lane=>lane.name)).toEqual(['Builder','Elsewhere','Empty worker','Unassigned']);
  });

  it('provides the same bounded worker board independently of the review queue', () => {
    const api = { delivery: vi.fn(), reviewQueue: vi.fn() };
    const session = { project:{id:'project_one',repository:'example/project'}, controller: new AbortController(), api } as unknown as Session;
    const client = new QueryClient();
    client.setQueryData(deliveryQuery(session, key).queryKey, board);
    try {
      const html = markup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, selectWorker: () => {}, fail: () => {}, ready: false }}><DeliveryWork /></WorkspaceContext.Provider></QueryClientProvider>);
      expect(html).toContain('Manage jobs');
      expect(html).toContain('Load more work');
      expect(html).toContain('Owner outside this worker page');
      expect(html).toContain('Primary focus');
      expect(html).toContain('No jobs in loaded slice.');
      expect(html).not.toContain('review-queue-title');
      expect(client.getQueryCache().getAll().some(query => query.queryKey.includes('reviews'))).toBe(false);
      expect(api.delivery).not.toHaveBeenCalled();
      expect(api.reviewQueue).not.toHaveBeenCalled();
    } finally { client.clear(); }
  });

  it('keeps overview focused on the actual job and current work without a second authored summary', () => {
    const current = {...job('job_active','active'),brief_preview:'PRIVATE_STANDING_INSTRUCTIONS',summary:{job_id:'job_active',job_revision:3,revision:2,title:'Shareable card title',body:'A concise **result**.'}};
    const html=markup(<DeliveryLanes value={{workers:{items:[worker],has_more:false},jobs:{items:[current],has_more:false}}} home={home}/>);
    expect(html).toContain('job_active');expect(html).not.toContain('Shareable card title');expect(html).not.toContain('A concise <strong>result</strong>');
    expect(html).not.toContain('PRIVATE_STANDING_INSTRUCTIONS');expect(html).toContain('/jobs/job_active');
  });

  it('keeps empty jobs compact and renders only supplied current fields without adding fallback brief text', () => {
    const current = {...job('job_empty','parked'),brief_preview:'LONG_STANDING_INSTRUCTIONS',brief_preview_truncated:true};
    const value = {workers:{items:[worker],has_more:false},jobs:{items:[current],has_more:false}};
    const empty = markup(<DeliveryLanes value={value} home={home}/>);
    expect(empty).toContain('job_empty'); expect(empty).toContain('Parked'); expect(empty).toContain('/jobs/job_empty');
    expect(empty).not.toContain('LONG_STANDING_INSTRUCTIONS'); expect(empty).not.toContain('delivery-checkpoint-label');
    current.progress.blocker='Waiting for your instruction to resume.';
    const blocked = markup(<DeliveryLanes value={value} home={home}/>);
    expect(blocked).toContain('Blocker:</strong>'); expect(blocked).toContain(current.progress.blocker);
    expect(blocked).not.toContain('Progress:</strong>'); expect(blocked).not.toContain('Next:</strong>');
  });

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
  it('shows explicit pause/focus and safe progress previews without implying an empty lane is idle', () => {
    const current = job('job_active', 'active');
    current.progress = { summary: '<script>done</script>', next: 'Next <literal>', blocker: 'Waiting', summary_truncated: true, next_truncated: false, blocker_truncated: false };
    const value = { ...board, jobs: { ...board.jobs, items: [current] } };
    const html = markup(<DeliveryLanes value={value} home={home} />);
    expect(html).toContain('Primary focus');
    expect(html).toContain('Paused');
    expect(html).toContain('Waiting &lt;literal&gt;…');
    expect(html).not.toContain('Checkpoint');
    expect(html).toContain('&lt;script&gt;done&lt;/script&gt;');
    expect(html).toContain('</div><span class="delivery-preview-truncated" aria-label="Preview truncated">…</span>');
    expect(html).toContain('>Primary job</a>');
    expect(html).not.toContain('outside this job page');
    expect(html).toContain('No jobs in loaded slice.');
    expect(html).not.toMatch(/<script|Idle|<button/u);
  });
  it('shows current progress and edit age while leaving detailed plan metadata on the job page', () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date('2026-10-19T09:00:00Z'));
    try {
      const current = {...job('job_active','active'),brief_preview:'**literal brief** <script>bad</script>',brief_preview_truncated:true,
        progress:{summary:'**literal update** <script>bad</script>',next:'',blocker:'',summary_truncated:true,next_truncated:false,blocker_truncated:false},
        checklist:{revision:2,job_revision:1,total:0,done:0,created_at:'2099-01-01T00:00:00Z'}};
      const html=markup(<DeliveryLanes value={{...board,jobs:{items:[current],has_more:false}}} home={home}/>);
      expect(html).not.toContain('literal brief');
      expect(html).toContain('<strong>literal update</strong> &lt;script&gt;bad&lt;/script&gt;');
      expect(html).not.toContain('>Job revision 3'); expect(html).toContain('Edited'); expect(html).toContain('1h 0m ago');
      expect(html).not.toContain('Progress from job revision'); expect(html).not.toContain('Checklist from job revision');
      expect(html).not.toContain('Checklist 0/0 done'); expect(html).not.toContain('2099-01-01T00:00:00Z');
      expect(html).not.toContain('No checkpoint recorded.'); expect(html).not.toMatch(/<script>|<em>|Last activity/u);
    } finally {vi.useRealTimers();}
  });
  it('distinguishes a complete empty lane from a later or incomplete job slice', () => {
    const complete = { ...board, jobs: { items: [], has_more: false } };
    expect(markup(<DeliveryLanes value={complete} home={home} />)).toContain('No open jobs.');
    const laterPage = markup(<DeliveryLanes value={complete} home={home} partialJobs />);
    expect(laterPage).toContain('No jobs in loaded slice.');
    expect(laterPage).not.toContain('No open jobs.');
  });
  it('renders only supplied bounded prose as compact Markdown, retaining field labels and safety', () => {
    const current = {
      ...job('job_active', 'active'),
      brief_preview: '## Goal\n\nWrite **clear** release notes.\n\n- Keep the links\n- [Guide](https://example.com/guide)', brief_preview_truncated: true,
      progress: {summary: '### Progress\n\nChecked `proof` and ![image](https://example.com/private.png).\n\n<script>bad</script>\n\n```\npartial **code', summary_truncated: true, next: '**Finish** the [review](javascript:alert%281%29)', next_truncated: true, blocker: '> Waiting on **evidence**', blocker_truncated: false},
    };
    const html = markup(<DeliveryLanes value={{...board,jobs:{items:[current],has_more:false}}} home={home}/>);
    expect(html).not.toContain('<h5>Goal</h5>');
    expect(html).toContain('<h6>Progress</h6>');
    expect(html).not.toContain('<strong>clear</strong>');
    expect(html).toContain('<code>proof</code>');
    expect(html).not.toContain('editorial-list-unordered');
    expect(html).not.toContain('href="https://example.com/guide"');
    expect(html).toContain('Image: image');
    expect(html).toContain('delivery-checkpoint-label">Next:</strong>');
    expect(html).toContain('delivery-checkpoint-label">Blocker:</strong>');
    expect(html).toContain('delivery-checkpoint-label">Progress:</strong>');
    expect(html).toContain('<strong>Finish</strong>');
    expect(html).toContain('review</span>');
    expect(html).toContain('editorial-quote');
    expect(html).toContain('&lt;script&gt;bad&lt;/script&gt;');
    expect(html).toContain('partial **code\n</code></pre></div><span class="delivery-preview-truncated" aria-label="Preview truncated">…</span>');
    expect(html.match(/class="delivery-preview-truncated"/gu)).toHaveLength(2);
    expect(html).not.toMatch(/<img|<script|href="javascript:|<p[^>]*><div/u);
    const context = html.slice(html.indexOf('class="delivery-job-context"'), html.indexOf('class="delivery-job-link"'));
    expect(context).not.toMatch(/<h[1-4]>/u);
  });
  it('keeps primary focus first without letting a parked primary hide active lane work', () => {
    const value = {...board,workers:{...board.workers,items:[{...worker,primary_job_id:'job_parked'}]}};
    const lanes=groupDeliveryLanes(value);
    expect(lanes[0].jobs.map(item=>item.id)).toEqual(['job_parked','job_active','job_blocked']);
    const html=markup(<DeliveryLanes value={value} home={home} />);
    expect(html).toContain('Worker inbox');
    expect(html).toContain(`href="${home}/inbox?worker_id=worker_one"`);
    expect(html).toContain('Job history');
    expect(html).toContain('worker_id=worker_one&amp;status=all');
    expect(html).toContain('badge delivery-primary');
  });
  it.each([
    '![private](https://example.com/private.png',
    '[unsafe](javascript:alert%281%29',
    '```html\n<script>alert(1)</script>\n<img src="https://example.com/private.png">',
    '## Goal\n\n' + '- short line\n'.repeat(16),
  ])('keeps every truncated prose marker outside the inert Markdown: %s', body => {
    const current = {
      ...job('job_active', 'active'), brief_preview: body, brief_preview_truncated: true,
      progress: {summary: body, next: body, blocker: body, summary_truncated: true, next_truncated: true, blocker_truncated: true},
    };
    expect(new TextEncoder().encode(body).length).toBeLessThanOrEqual(240);
    const html = markup(<DeliveryLanes value={{...board,jobs:{items:[current],has_more:false}}} home={home}/>);
    expect(html.match(/class="delivery-preview-truncated"/gu)).toHaveLength(3);
    expect(html.match(/<\/div><span class="delivery-preview-truncated" aria-label="Preview truncated">…<\/span>/gu)).toHaveLength(3);
    const parsedBodies = [...html.matchAll(/class="editorial-markdown delivery-preview">([\s\S]*?)<\/div>/gu)];
    expect(parsedBodies).toHaveLength(3);
    for (const match of parsedBodies) expect(match[1]).not.toContain('…');
    expect(html).not.toMatch(/<img|<script|href="javascript:|src="https:/u);
  });
  it('retains every short list item in supplied untruncated progress previews', () => {
    const body = Array.from({length: 16}, (_, index) => `- Step ${index + 1}`).join('\n');
    expect(new TextEncoder().encode(body).length).toBeLessThan(240);
    const current = {
      ...job('job_active', 'active'), brief_preview: body, brief_preview_truncated: false,
      progress: {summary: body, next: body, blocker: body, summary_truncated: false, next_truncated: false, blocker_truncated: false},
    };
    const html = markup(<DeliveryLanes value={{...board,jobs:{items:[current],has_more:false}}} home={home}/>);
    for (let step = 1; step <= 16; step++) {
      expect(html.match(new RegExp(`<li>Step ${step}</li>`, 'gu'))).toHaveLength(3);
    }
    expect(html).not.toContain('Preview truncated');
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
  it('uses the stored global head and remains conservative when a filtered window omits it', () => {
    const items = [{ ...pull(1, 'merged'), queue_position: 2 }, { ...pull(2, 'open'), queue_position: 9 }, { ...pull(3, 'open'), queue_position: 12 }];
    expect(selectMergeHead({ ...queue(items), head_position: 9 })).toEqual({ state: 'head', pull: items[1] });
    expect(selectMergeHead(queue(items))).toEqual({ state: 'unknown', reason: 'preceding' });
    expect(selectMergeHead({ ...queue([items[2]]), head_position: 9 })).toEqual({ state: 'unknown', reason: 'next-page' });
    expect(selectMergeHead({ ...queue([pull(2, 'open')]), head_position: 9 })).toEqual({ state: 'unknown', reason: 'next-page' });
    expect(selectMergeHead(queue([items[0]]))).toEqual({ state: 'none' });
  });
  it('renders the verified channel status once in the same facts position as the queue', () => {
    const value = queue([pull(2, 'open')]);
    const fronts: ReviewFronts = { project_id: 'project_one', configured: true, repository_id: 1, configuration_revision: 1, policy_revision: 1, snapshot_id: value.snapshot_id, queue: [2], channels: [{ channel: 'hosted', state: 'ready', pull_number: 2, head_sha: 'head', base_sha: 'base', base_ref: 'main' }] };
    const html = markup(<MergeHeadContent queue={value} home={home} fronts={fronts} />);
    expect(html).toContain('review-pull-facts"><span class="badge review-position-front"');
    expect(html.match(/>Review front<\/span>/gu)).toHaveLength(1);
    expect(html).not.toContain('>Open</span>');
    expect(markup(<MergeHeadContent queue={value} home={home} fronts={{ ...fronts, snapshot_id: 'old' }} />)).not.toContain('review-position-front');
    expect(markup(<MergeHeadContent queue={value} home={home} />)).not.toMatch(/class="badge review-(position|state)-/u);
  });
  it('does not claim no head or merge readiness from a partial queue', () => {
    const partial = markup(<MergeHeadContent queue={queue([pull(1, 'merged')], true)} home={home} />);
    expect(partial).toContain('Queue head not determined');
    expect(partial).toContain('more queue entries remain');
    expect(partial).not.toContain('No open pull requests');
    const head = markup(<MergeHeadContent queue={queue([pull(2, 'open')])} home={home} />);
    expect(head).toContain('href="https://github.com/example/project/pull/2"');
    expect(head).toContain('Required CI: Success');
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
    expect(html).toContain('review-queue-row review-row-open review-row-without-order');
    expect(html).not.toContain('delivery-head-history');
    expect(html).toContain('1 run</span>');
    expect(markup(<MergeHeadContent queue={queue([pull(1, 'merged')])} home={home} />)).toBe('');
    expect(markup(<MergeHeadContent queue={{ ...queue([]), configured: false }} home={home} />)).toBe('');
  });
  it('shares one bounded queue read between hero and overview without run/archive fanout', async () => {
    const read = vi.fn().mockResolvedValue(queue([pull(2, 'open')]));
    const session = { project:{id:'project_one',repository:'example/project'}, controller: new AbortController(), api: { reviewQueue: read } } as unknown as Session;
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    try {
      const hero = deliveryReviewQuery(session, key), overview = reviewQueueQuery(session, key, 'recent');
      await Promise.all([client.fetchQuery(hero), client.fetchQuery(overview)]);
      expect(read).toHaveBeenCalledTimes(1);
      expect(read.mock.calls[0][0]).toEqual({ limit: 20, view: 'recent' });
      expect(hero.queryKey).toEqual([...key, 'reviews', 'current', 'queue', 'recent', '']);
    } finally { client.clear(); }
  });
  it('uses dedicated neutral merge-head loading rather than generic page rows', () => {
    const html = markup(<DeliveryHero><section className="hero project-identity"><h1>Current verified project</h1></section></DeliveryHero>);
    expect(html).toContain('Current verified project');
    expect(html).toContain('Loading review queue head');
    expect(html.match(/class="review-lane skeleton-review-lane"/gu)).toHaveLength(2);
    expect(html).not.toContain('skeleton-row');
    expect(html).not.toContain('Review queue ↗');
    expect(html).not.toContain('No open pull requests');
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
        expect(html).not.toContain('Review queue head');
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
