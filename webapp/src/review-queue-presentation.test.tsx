import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { WorkspaceContext, type Session } from './workspace';
import { PresentationContext } from './presentation-context';
import { ReviewFindingView, ReviewPullFacts, ReviewQueueRow, ReviewQueueView, ReviewRunsView, formatQueueDistance, groupReviewQueue, missingPullFacts, reviewQueueDistances, reviewQueueHashTarget, reviewQueueQuery } from './review-workspace';
import type { ReviewFronts } from './review-fronts';
import type { ReviewProgressItem } from './review-progress';
import type { Presentation, ReviewAttemptSummary, ReviewFinding, ReviewPullSummary, ReviewQueue, ReviewRunSummary } from './types';

const key = ['fc', 'connection', 'instance', 'project', 'actor'];
const pull = (number: number, state: ReviewPullSummary['state'] = 'open'): ReviewPullSummary => ({ number, title: `Change ${number}`, state, head_sha: 'a'.repeat(40), base_ref: 'main', base_sha: 'b'.repeat(40), ci_state: 'success', mergeability: 'mergeable', complete: true, run_count: 0, run_counts: { hosted: 0, cli: 0, subagent: 0, manual: 0 }, attempt_count: 0, recent_runs: [], latest_attempts: [] });
function markup(node: React.ReactNode, queue?: ReviewQueue, presentation?: Presentation, options: { history?: boolean; fronts?: ReviewFronts; progress?: ReviewProgressItem[] } = {}) {
  const api = { reviewQueue: vi.fn(), reviewRuns: vi.fn() };
  const session = { api, controller: new AbortController(), project: { repository: 'example/project' } } as unknown as Session;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (queue) client.setQueryData(reviewQueueQuery(session, key, options.history ? 'merged' : 'recent').queryKey, queue);
  if (options.fronts) client.setQueryData([...key, 'reviews', 'current', 'fronts'], options.fronts);
  if (options.progress && queue) client.setQueryData([...key, 'reviews', 'current', 'round-progress', queue.items.map(item => item.number).sort((a, b) => a - b)], { items: options.progress });
  try {
    const html = renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><PresentationContext.Provider value={presentation}><WorkspaceContext.Provider value={{ session, key, home: '/projects/project', selectWorker: () => {}, fail: () => {}, ready: true }}>{node}</WorkspaceContext.Provider></PresentationContext.Provider></QueryClientProvider></MemoryRouter>);
    expect(api.reviewQueue).not.toHaveBeenCalled(); expect(api.reviewRuns).not.toHaveBeenCalled();
    return html;
  } finally { client.clear(); }
}
const queue = (items: ReviewPullSummary[]): ReviewQueue => ({ items, has_more: false, configured: true, project_id: 'project', snapshot_id: 'snapshot', revision: 1, source: 'stored', observed_at: '2026-10-19T00:00:00Z', recorded_at: '2026-10-19T00:00:00Z' });
const fronts = (): ReviewFronts => ({ project_id: 'project', configured: true, repository_id: 1, configuration_revision: 1, policy_revision: 1, snapshot_id: 'snapshot', queue: [1, 2, 3], channels: [{ channel: 'hosted', state: 'ready', pull_number: 2, head_sha: pull(2).head_sha, base_sha: pull(2).base_sha, base_ref: 'main', queued_priority: {pull_number:2,head_sha:pull(2).head_sha,base_sha:pull(2).base_sha,base_ref:'main'} }] });
const stackedFronts = (): ReviewFronts => {
  const value = fronts();
  value.queue = [2, 3];
  value.channels = (['hosted', 'cli'] as const).map(channel => ({ channel, state: 'ready' as const, pull_number: 2, head_sha: pull(2).head_sha, base_sha: pull(2).base_sha, base_ref: 'main', queued_priority: { pull_number: 2, head_sha: pull(2).head_sha, base_sha: pull(2).base_sha, base_ref: 'main' } }));
  return value;
};

describe('truthful stored review presentation', () => {
  it('shows actual branch targets for stack heads and preserves explicit parent PR links', () => {
    for (const branch of ['develop', 'main', 'release/next']) {
      const html = markup(<ReviewPullFacts pull={{ ...pull(2), base_ref: branch }} />);
      expect(html).toContain(`Based on ${branch}`);
      expect(html).toContain(`href="https://github.com/example/project/tree/${encodeURIComponent(branch)}"`);
    }
    const stacked = markup(<ReviewPullFacts pull={{ ...pull(2), base_ref: 'feature/parent', parent_pull_number: 1 }} />);
    expect(stacked).toContain('Based on PR #1');
    expect(stacked).toContain('href="https://github.com/example/project/pull/1"');
    expect(stacked).not.toContain('Based on feature/parent');
    const unknownBase = markup(<ReviewPullFacts pull={{ ...pull(2), base_ref: '' }} />);
    expect(unknownBase).toContain('Base unknown');
    expect(unknownBase).not.toContain('github.com/example/project/tree/');
  });

  it('keeps the base inside the always-visible size pill before counts, including history and missing size', () => {
    for (const state of ['open', 'merged', 'closed'] as const) {
      const html = markup(<ReviewPullFacts pull={{ ...pull(2, state), parent_pull_number: 1, diff: { files: 12, additions: 34, deletions: 5 } }} />);
      expect(html).toMatch(/class="badge review-fact-pill review-diff"><a class="review-base-target"[^>]*>Based on PR #1<\/a> · 12 files/);
      expect(html.match(/Based on/g)).toHaveLength(1);
      expect(html).toContain('+34');
    }
    const unknownSize = markup(<ReviewPullFacts pull={pull(2)} />);
    expect(unknownSize).toMatch(/class="badge review-fact-pill review-diff"><a class="review-base-target"[^>]*>Based on main<\/a><\/span>/);
    expect(unknownSize).not.toContain('0 files');
  });

  it('shows newest merged card immediately above the review head, leaving history order unchanged', () => {
    const items = [
      {...pull(88, 'merged'), merged_at: '2026-11-14T05:36:48Z'},
      {...pull(77, 'merged'), merged_at: '2026-11-13T05:00:00Z'},
      {...pull(89, 'merged'), merged_at: '2026-11-12T05:00:00Z'},
      pull(78),
    ];
    const html = markup(<ReviewQueueView />, queue(items));
    expect(html.indexOf('Change 89')).toBeLessThan(html.indexOf('Change 77'));
    expect(html.indexOf('Change 77')).toBeLessThan(html.indexOf('Change 88'));
    expect(html.indexOf('Change 88')).toBeLessThan(html.indexOf('Change 78'));
    const history = markup(<ReviewQueueView history />, queue(items), undefined, { history: true });
    expect(history.indexOf('Change 88')).toBeLessThan(history.indexOf('Change 77'));
  });


  it('keeps chronological merged rails increasing toward the head with stored positions', () => {
    const items = [
      {...pull(88, 'merged'), queue_position: 1, merged_at: '2026-11-14T05:36:48Z'},
      {...pull(77, 'merged'), queue_position: 2, merged_at: '2026-11-13T05:00:00Z'},
      {...pull(89, 'merged'), queue_position: 3, merged_at: '2026-11-12T05:00:00Z'},
      {...pull(78), queue_position: 4},
    ];
    const html = markup(<ReviewQueueView />, {...queue(items), head_position: 4});
    const rows = html.split('class="review-queue-position"').slice(1).map(row => row.match(/aria-label="([^"]+)"/u)?.[1]);
    expect(rows).toEqual(['Queue distance −03', 'Queue distance −02', 'Queue distance −01', 'Queue distance +01: first open PR']);
    expect(html.indexOf('Change 89')).toBeLessThan(html.indexOf('Change 88'));
  });

  it('uses the standard channel badge for process attention and keeps quota reasons noninteractive', () => {
    const attempt = { channel: 'cli', state: 'failed', reason: 'allowance_exhausted', finished_at: '2026-10-22T00:00:20Z' } as ReviewAttemptSummary;
    const html = markup(<ReviewRunsView number={2} snapshot="snapshot" head={pull(2).head_sha} attempts={[attempt]} expected={0} channelTotals={pull(2).run_counts} />);
    expect(html).toContain('class="badge review-channel-position review-attempt-state review-attempt-attention"');
    expect(html).toContain('title="Included CLI quota was exhausted');
    expect(html).not.toContain('>Included CLI quota was exhausted<');
    expect(html).not.toMatch(/<details|<summary|<button|tabindex=/u);
  });

  it('shows current recovery instead of a failed PR verdict while retaining six completed rounds', () => {
    const attempt = { channel: 'cli', head_sha: 'b'.repeat(40), state: 'failed', reason: 'admission_changed', finished_at: '2026-10-22T00:00:20Z', started_at: '2026-10-22T00:00:00Z', source_attempt_id: 'stopped' } as ReviewAttemptSummary;
    const row = { ...pull(2), run_count: 6, run_counts: { ...pull(2).run_counts, cli: 6 }, attempt_count: 7, latest_attempts: [attempt] };
    const rounds: ReviewProgressItem = { pull_number: 2, channels: [{ channel: 'cli', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 7, taper: 2, used: 6, dry: 0, tapered: false, pending: 0, pending_execution: 0, pending_adjudication: 0 }] };
    const stopped = markup(<ReviewQueueRow pull={row} snapshot="snapshot" progress={rounds} />);
    expect(stopped).toContain('>Check attempt</span>'); expect(stopped).toContain('failed (admission_changed)');
    expect(stopped).not.toContain('>Failed</span>'); expect(stopped).toContain('6 runs');
    expect(stopped).toContain('title="Review eligibility changed before start');
    expect(stopped).not.toContain('>Review eligibility changed before start<');
    const retry = markup(<ReviewQueueRow pull={row} snapshot="snapshot" progress={{ ...rounds, channels: [{ ...rounds.channels[0], pending: 1, pending_execution: 1 }] }} />);
    expect(retry).toContain('>Queued</span>'); expect(retry).not.toContain('>Check attempt</span>'); expect(retry).toContain('6 runs');
    const historical = markup(<ReviewQueueRow pull={{ ...row, state: 'merged' }} snapshot="snapshot" progress={rounds} />);
    expect(historical).toContain('>Past attempt</span>'); expect(historical).not.toContain('>Check attempt</span>'); expect(historical).not.toContain('>Queued</span>');
    const held = markup(<ReviewQueueRow pull={{ ...row, latest_attempts: [{ ...attempt, state: 'held', reason: 'predecessor_unfinished' }] }} snapshot="snapshot" progress={rounds} />);
    expect(held).toContain('>Dependency hold</span>'); expect(held).toContain('title="Waiting for earlier PR work'); expect(held).toContain('No retry is confirmed'); expect(held).not.toContain('>Failed</span>');
  });

  it('shows unresolved incoming routes only on their open receiving queue row', () => {
    const rounds: ReviewProgressItem = { pull_number: 2, channels: [
      { channel: 'hosted', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 1, taper: 2, used: 1, dry: 0, tapered: false, pending: 0 },
      { channel: 'cli', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 1, taper: 2, used: 1, dry: 0, tapered: false, pending: 0 },
    ] };
    const html = (count?: number, number = 2, state: 'open'|'merged' = 'open') =>
      markup(<ReviewQueueRow pull={pull(number, state)} snapshot="snapshot" progress={{ ...rounds, incoming_routes_pending: count }} />);
    expect(html(1)).toContain('>1 incoming routed finding</a>');
    expect(html(2)).toContain('>2 incoming routed findings</a>');
    expect(html(2)).toContain('No reviews remaining');
    for (const out of [html(0), html(), html(2, 3), html(2, 2, 'merged')]) expect(out).not.toContain('incoming routed finding');
  });

  it('keeps result obligations out of the lane heading while preserving scheduler holds', () => {
    const v = fronts();
    v.channels = [{ ...v.channels[0], state: 'held', reason: 'review_fixes_pending' }];
    const row = pull(2);
    const fixedStage = markup(<ReviewQueueRow pull={row} snapshot="snapshot" fronts={v} />);
    expect(fixedStage).not.toContain('>Fixes pending</span>');
    expect(fixedStage).not.toContain('>Results pending</span>');
    expect(fixedStage).not.toContain('>Review active</span>');
    const progress: ReviewProgressItem = { pull_number: 2, channels: [{ channel: 'hosted', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 5, taper: 2, used: 1, dry: 0, tapered: false, pending: 1, pending_execution: 0, pending_adjudication: 1 }] };
    const adjudicating = markup(<ReviewQueueRow pull={row} snapshot="snapshot" fronts={v} progress={progress} />);
    expect(adjudicating).not.toContain('>Needs adjudication</span>');
    expect(adjudicating).toContain('1 awaiting adjudication');
    expect(adjudicating).not.toContain('>Fixes pending</span>');
    const unclassified = markup(<ReviewQueueRow pull={row} snapshot="snapshot" fronts={{ ...v, channels: [{ ...v.channels[0], reason: 'review_evidence_pending' }] }} />);
    expect(unclassified).toContain('>Results pending</span>');
    expect(unclassified).not.toContain('>Fixes pending</span>');
  });

  it('shows a later receiving row findings hold without replacing retained channel work', () => {
    const v=fronts();
    v.channels=[{...v.channels[0],state:'held',reason:'review_evidence_pending'}, {...v.channels[0],channel:'cli',state:'held',reason:'review_evidence_pending'}];
    const progress:ReviewProgressItem={pull_number:3,incoming_routes_pending:1,channels:['hosted','cli'].map(channel=>({channel:channel as 'hosted'|'cli',configured:true,revision:1,epoch:1,closed:false,minimum:0,maximum:5,taper:2,used:1,dry:0,tapered:false,pending:1,pending_execution:0,pending_adjudication:1}))};
    const html=markup(<ReviewQueueRow pull={pull(3)} snapshot="snapshot" fronts={v} progress={progress} />);
    expect(html).toContain('>Findings pending</span>');
    expect(html).toContain('>1 incoming routed finding</a>');
    expect(html).toContain('1 awaiting adjudication');
    expect(html).not.toContain('>Needs adjudication</span>');
    expect(html).not.toContain('>Queued</span>');
    for (const incoming of [0,undefined]) {
      expect(markup(<ReviewQueueRow pull={pull(3)} snapshot="snapshot" fronts={v} progress={{...progress,incoming_routes_pending:incoming}} />)).toContain('>Queued</span>');
    }
  });

  it('shows no current badge or explanation for an intentionally cancelled preflight wait', () => {
    const attempt: ReviewAttemptSummary = { source_attempt_id: `review_attempt_${'a'.repeat(32)}`, channel: 'hosted', head_sha: 'c'.repeat(40), state: 'held', reason: 'hosted_allowance_cooldown', started_at: '2026-11-01T00:00:00Z', finished_at: '2026-11-01T00:00:20Z', duration_seconds: 20, source_run_id: '', trigger_id: '', provider_review_id: '' };
    const row = { ...pull(2), attempt_count: 1, latest_attempts: [attempt] };
    const progress: ReviewProgressItem = { pull_number: 2, channels: [{ channel: 'hosted', configured: true, revision: 1, epoch: 1, closed: false, minimum: 2, maximum: 8, taper: 2, used: 0, dry: 0, tapered: false, pending: 0, pending_execution: 0, pending_adjudication: 0, cancelled_wait: { baseline_attempt_id: attempt.source_attempt_id, head_sha: attempt.head_sha, cancelled_at: '2026-11-01T00:01:00Z' } }] };
    const html = markup(<ReviewQueueRow pull={row} snapshot="snapshot" progress={progress} />);
    expect(html).not.toContain('>Request cancelled</span>');
    expect(html).not.toContain('review-attempt-history');
    expect(html).not.toContain('review-attempt-explanation');
    expect(html).not.toContain('held (hosted_allowance_cooldown)');
    expect(html).not.toContain('>Check attempt</span>');
    expect(html).not.toContain('>Review active</span>');
    expect(html).not.toContain('>Queued</span>');
    const historical = markup(<ReviewQueueRow pull={{ ...row, state: 'merged' }} snapshot="snapshot" progress={progress} />);
    expect(historical).toContain('>Past attempt</span>');
    expect(historical).toContain('held (hosted_allowance_cooldown)');
    const unrelated = markup(<ReviewQueueRow pull={{ ...row, latest_attempts: [{ ...attempt, source_attempt_id: `review_attempt_${'b'.repeat(32)}` }] }} snapshot="snapshot" progress={progress} />);
    expect(unrelated).toContain('>Check attempt</span>');
    expect(unrelated).not.toContain('>Request cancelled</span>');
  });

  it('keeps Hosted priority visible alongside a waiting operation and unknown source', () => {
    const v=fronts();v.channels=[{channel:'hosted',state:'unknown',reason:'source_budget_exhausted',queued_priority:{pull_number:2,head_sha:pull(2).head_sha,base_sha:pull(2).base_sha,base_ref:'main'}}];
    const progress:ReviewProgressItem={pull_number:2,channels:[{channel:'hosted',configured:true,revision:1,epoch:1,closed:false,minimum:0,maximum:5,taper:2,used:0,dry:0,tapered:false,pending:1,pending_execution:1,pending_adjudication:0,wait:{baseline_attempt_id:`review_attempt_${'a'.repeat(32)}`,retry_not_before:'2026-10-30T12:30:00Z',paused:false}}]};
    const html=markup(<ReviewQueueView />,queue([pull(1,'merged'),pull(2),pull(3)]),undefined,{fronts:v,progress:[progress]});
    expect(html).toContain('>Review front</span>');
    expect(html).not.toContain('review priority');
    expect(html).toContain('Waiting');
    expect(html).not.toContain('>Up next</span>');
  });

  it('uses one Review front row when channel priorities differ', () => {
    const v=fronts();v.channels=[2,3].map((number,index)=>({channel:index===0?'hosted':'cli',state:'unknown',reason:'source_budget_exhausted',queued_priority:{pull_number:number,head_sha:pull(number).head_sha,base_sha:pull(number).base_sha,base_ref:'main'}}));
    const html=markup(<ReviewQueueView />,queue([pull(1,'merged'),pull(2),pull(3)]),undefined,{fronts:v});
    for(const number of [2,3]) {
      const row=html.match(new RegExp(`<li id="review-pr-${number}"[\\s\\S]*?<\\/li>`,'u'))![0];
      expect(row).toContain(number===2 ? '>Review front</span>' : '>Queued</span>');
      expect(row).toContain('>Source unverified</span>');
      expect(row).not.toMatch(/Hosted review priority|CLI review priority|>Queue priority</u);
    }
    expect(html.match(/>Review front<\/span>/gu)).toHaveLength(1);
    expect(html).not.toContain('>Up next</span>');
  });
  it('shows Draft without the redundant Queued badge', () => {
    const html = markup(<ReviewPullFacts pull={{ ...pull(2), is_draft: true }} status={{kind:'queued',text:'Queued',description:'Waiting in the queue'}} />);
    expect(html).toContain('Draft');
    expect(html).not.toContain('>Queued<');
  });

  it('distinguishes no required CI from an unknown observation without claiming workflow success', () => {
    const html = markup(<ReviewPullFacts pull={{ ...pull(2), ci_state: 'not_required' }} />);
    expect(html).toContain('Required CI: None');
    expect(html).toContain('Optional checks are separate.');
    expect(html).not.toContain('Required CI: Success');
    expect(html).not.toContain('CI status is unknown.');
    expect(markup(<ReviewPullFacts pull={{ ...pull(2), ci_state: 'unknown' }} />)).toContain('>Checking CI</span>');
  });
  it('identifies incomplete Hosted history without calling healthy PR checks a GitHub retry', () => {
    const observation = { state: 'retrying' as const, checked_at: '2026-10-23T13:24:02Z', last_success_at: '2026-10-23T13:24:02Z', error: 'external_hosted_identity_unproven', hosted_error: 'external_hosted_identity_unproven' };
    const html = markup(<ReviewPullFacts pull={{ ...pull(2), observation }} />);
    expect(html).toContain('Hosted history: Incomplete'); expect(html).toContain('See CodeRabbit on GitHub for the full review history.');
    expect(html).toContain('Last successful PR check:'); expect(html).not.toContain('GitHub: Retrying');
    for (const error of ['provider_unavailable', 'provider_rate_limited', 'ci_refresh_unavailable']) {
      const failed = markup(<ReviewPullFacts pull={{ ...pull(2), observation: { ...observation, error } }} />);
      expect(failed).toContain('GitHub: Retrying'); expect(failed).not.toContain('Hosted history: Incomplete');
      if (error === 'ci_refresh_unavailable') { expect(failed).toContain('Couldn’t refresh CI.'); }
    }
    expect(markup(<ReviewPullFacts pull={{ ...pull(2), observation: { ...observation, state: 'current', error: undefined, hosted_error: undefined } }} />)).not.toContain('GitHub:');
  });

  it('keeps the row front concise and places independent front markers beside channel headings', () => {
    const both = fronts(); both.channels.push({ ...both.channels[0], channel: 'cli' });
    const html = markup(<ReviewQueueView />, queue([pull(1, 'merged'), pull(2), pull(3)]), undefined, { fronts: both });
    expect(html).not.toContain('Review front ·');
    const row = html.match(/<li id="review-pr-2"[\s\S]*?<\/li>/u)![0];
    expect(row).toContain('aria-label="hosted current review state"><span data-lane-label-tone="active"><span class="badge review-channel-position">Up next</span></span></span>');
    expect(row).toContain('aria-label="cli current review state"><span data-lane-label-tone="active"><span class="badge review-channel-position">Up next</span></span></span>');
    const independent = markup(<ReviewQueueView />, queue([pull(1, 'merged'), pull(2), pull(3)]), undefined, { fronts: fronts() });
    const selected = independent.match(/<li id="review-pr-2"[\s\S]*?<\/li>/u)![0];
    expect(selected).toContain('aria-label="hosted current review state"><span data-lane-label-tone="active"><span class="badge review-channel-position">Up next</span></span></span>');
    expect(selected).not.toContain('aria-label="cli current review state"><span data-lane-label-tone="active"><span class="badge review-channel-position">Up next</span></span></span>');
  });
  it('keeps an already-started review visible after head movement', () => {
    const attempt = { channel: 'hosted', head_sha: pull(2).head_sha, state: 'active', finished_at: '', started_at: '2026-10-22T00:00:00Z', source_attempt_id: 'active' } as ReviewAttemptSummary;
    const html = markup(<ReviewQueueView />, queue([pull(1, 'merged'), { ...pull(2), latest_attempts: [attempt] }]), undefined, { fronts: fronts() });
    expect(html).toContain('aria-label="hosted execution: Review active"');
    expect(html).not.toContain('>Up next</span>');
    const stale = markup(<ReviewQueueView />, queue([pull(1, 'merged'), { ...pull(2), latest_attempts: [{ ...attempt, head_sha: 'c'.repeat(40) }] }]), undefined, { fronts: fronts() });
    expect(stale).toContain('aria-label="hosted execution: Review active"');
    expect(stale).not.toContain('>Up next</span>');
  });
  it('shows carried admission work without claiming Up next, and keeps independent channel readiness', () => {
    const held=fronts();held.channels[0]={...held.channels[0],state:'held',reason:'source_obligations_pending'};
    held.channels.push({...held.channels[0],channel:'cli',state:'ready',reason:undefined});
    const html=markup(<ReviewQueueView />,queue([pull(2)]),undefined,{fronts:held});
    expect(html).toMatch(/aria-label="hosted current review state"[\s\S]*?title="[^"]*holds new reviews[^"]*">Carried work<\/span>/u);
    expect(html).toContain('aria-label="cli current review state"><span data-lane-label-tone="active"><span class="badge review-channel-position">Up next</span></span></span>');
    expect(html).not.toContain('aria-label="hosted current review state"><span data-lane-label-tone="active"><span class="badge review-channel-position">Up next</span></span></span>');
    const attempt={channel:'hosted',head_sha:pull(2).head_sha,state:'active',finished_at:'',started_at:'2026-10-22T00:00:00Z',source_attempt_id:'active'} as ReviewAttemptSummary;
    const running=markup(<ReviewQueueView />,queue([{...pull(2),latest_attempts:[attempt]}]),undefined,{fronts:held});
    expect(running).toContain('aria-label="hosted execution: Review active"');
    expect(running).not.toContain('>Carried work</span>');
    const stale=markup(<ReviewQueueView />,queue([pull(2)]),undefined,{fronts:{...held,snapshot_id:'old'}});
    expect(stale).not.toContain('>Carried work</span>');
  });
  it('labels a locally derived queue position without claiming verified admission', () => {
    const missing=fronts();missing.channels=[{...missing.channels[0],state:'held',reason:'local_queue_position',pull_number:2,head_sha:pull(2).head_sha,base_sha:pull(2).base_sha,base_ref:pull(2).base_ref,source_observed_at:undefined}];
    const progress:ReviewProgressItem={pull_number:2,channels:[{channel:'hosted',configured:true,revision:1,epoch:1,closed:false,minimum:1,maximum:3,taper:2,used:0,dry:0,tapered:false,pending:0}]};
    missing.queue=[2];
    const html=markup(<ReviewQueueView />,queue([pull(2)]),undefined,{fronts:missing,progress:[progress]});
    expect(html).toContain('>First in line</span>');
    expect(html).toContain('First in line. Source checks are still outstanding.');
    expect(html).not.toContain('>Up next</span>');
  });
	 it('retains the closed badge and striped-rail class while front data catches up',()=>{
	   const page={...queue([pull(2)]),snapshot_id:'new-snapshot'};
	   const progress:ReviewProgressItem={pull_number:2,channels:[{channel:'hosted',configured:true,revision:1,epoch:1,closed:true,minimum:0,maximum:3,taper:1,used:1,dry:0,tapered:false,pending:0}]};
	   const html=markup(<ReviewQueueView/>,page,undefined,{fronts:fronts(),progress:[progress]});
	   expect(html).toContain('review-status-closed');expect(html).toContain('>Review closed</span>');
	   expect(html).not.toContain('>Queued</span>');
	   progress.channels[0]={...progress.channels[0],closed:false};
	   const reopened=markup(<ReviewQueueView/>,page,undefined,{fronts:{...fronts(),snapshot_id:page.snapshot_id},progress:[progress]});
	   expect(reopened).not.toContain('review-status-closed');expect(reopened).toContain('>Review front</span>');
	 });
  it.each([
    ['ready','github_clean','mergeable','Ready'],['blocked','github_blocked','conflicting','Blocked'],['blocked','github_behind','conflicting','Behind'],['blocked','github_dirty','conflicting','Conflicts'],['blocked','github_draft','conflicting','Draft'],['pending','github_unstable','pending','Checks failing'],['pending','github_has_hooks','pending','Hooks'],['unknown','merge_state_unknown','unknown','Unknown'],
  ] as const)('renders native merge state %s/%s separately from required CI', (state,reason,colour,label) => {
    for(const ci_state of ['success','pending','failure','unknown','not_required'] as const){
      const observation = ci_state === 'pending'
        ? { state: 'current' as const, ci_gate_state: 'pending' as const, ci_error: 'ci_required_checks_pending' }
        : undefined;
      const html = markup(<ReviewPullFacts pull={{ ...pull(2), ci_state, observation, merge_status:{state,reason} }} />);
      expect(html).toContain(`review-ci review-fact-${ci_state}`);
      expect(html).toContain(`review-mergeability review-fact-${colour}`);
      expect(html).toContain(`Merge: ${label}`);
      if(reason==='github_unstable') expect(html).toContain('Some may be optional.');
      if(reason==='github_has_hooks') expect(html).toContain('Waiting for GitHub’s merge hooks.');
    }
  });

  it('renders observed diff size and actual merge time without inventing values for old snapshots', () => {
    const merged = { ...pull(1, 'merged'), diff: { files: 73, additions: 6160, deletions: 136 }, merged_at: '2026-10-19T11:00:00Z' };
    const html = markup(<ReviewPullFacts pull={merged} />);
    expect(html).toContain('73 files'); expect(html).toContain('+6,160'); expect(html).toContain('−136');
    expect(html).toContain('dateTime="2026-10-19T11:00:00Z"'); expect(html).toContain('review-merged-age');
    const unknown = markup(<ReviewPullFacts pull={pull(1, 'merged')} />);
    expect(unknown).not.toContain(' files'); expect(unknown).not.toContain('review-merged-age');
    const zero = markup(<ReviewPullFacts pull={{ ...pull(1), diff: { files: 0, additions: 0, deletions: 0 }, is_draft: false }} />);
    expect(zero).toContain('0 files'); expect(zero).toContain('+0'); expect(zero).toContain('−0'); expect(zero).not.toContain('>Draft<');
    expect(markup(<ReviewPullFacts pull={{ ...pull(1), is_draft: true }} />)).toContain('>Draft<');
    expect(markup(<ReviewPullFacts pull={{ ...merged, is_draft: true }} />)).not.toContain('>Draft<');
  });
	 it.each(['unknown','pending','success','failure','not_required'] as const)('hides current %s CI after merge while retaining lifecycle and diff facts',ci_state=>{
	   const value={...pull(2,'merged'),ci_state,diff:{files:3,additions:12,deletions:1}};
	   const html=markup(<ReviewPullFacts pull={value}/>);
	   expect(html).toContain('Merged');expect(html).toContain('3 files');
	   expect(html).not.toContain('Required CI:');expect(html).not.toContain('Merge:');
	   expect(value.ci_state).toBe(ci_state);
	 });

  it('computes signed distance only when the first page establishes the actual head', () => {
    const items = [pull(1, 'merged'), pull(2, 'closed'), pull(3), pull(4)];
    expect(reviewQueueDistances(items, true)).toEqual([-2, -1, 1, 2]);
    expect(reviewQueueDistances([pull(1, 'unknown'), pull(2)], true)).toEqual([undefined, undefined]);
    expect(reviewQueueDistances(items, false)).toEqual([undefined, undefined, undefined, undefined]);
    expect(reviewQueueDistances([pull(1, 'merged')], true)).toEqual([undefined]);
    expect(formatQueueDistance(-1)).toBe('−01'); expect(formatQueueDistance(1)).toBe('+01');
  });
  it('preserves supplied global gaps on filtered and continuation pages without inventing a head', () => {
    const items = [{ ...pull(1, 'merged'), queue_position: 1 }, { ...pull(2), queue_position: 8 }, { ...pull(3), queue_position: 11 }];
    expect(reviewQueueDistances(items, true, 8)).toEqual([-7, 1, 4]);
    expect(reviewQueueDistances(items, false, 8)).toEqual([-7, 1, 4]);
    expect(reviewQueueDistances(items, true)).toEqual([undefined, undefined, undefined]);
    expect(reviewQueueDistances([pull(2)], true, 8)).toEqual([undefined]);
  });
  it('places a single verified status in the facts row and gives terminal lifecycle priority', () => {
    const html = markup(<ReviewQueueView />, queue([pull(1, 'merged'), { ...pull(2), is_draft: true }, pull(3)]), undefined, { fronts: fronts() });
    const rows = [...html.matchAll(/<li id="review-pr-\d+"[\s\S]*?<\/li>/gu)].map(match => match[0]);
    expect(rows).toHaveLength(3);
    expect(rows[0]).toContain('review-state-merged');
    expect(rows[1].match(/>Review front<\/span>/gu)).toHaveLength(1);
    expect(rows[1]).toContain('review-pull-facts"><span class="badge review-position-front"');
    expect(rows[1]).toContain('>Draft</span>');
    expect(rows[2].match(/>Queued<\/span>/gu)).toHaveLength(2); // PR-wide position plus independent channel pill.
    expect(rows[2]).toContain('aria-label="hosted current review state"');
    for (const row of rows) expect(row.slice(0, row.indexOf('review-pull-facts'))).not.toContain('review-position-');
    expect(html).not.toContain('>Open</span>');
    const closed = markup(<ReviewPullFacts pull={pull(1, 'closed')} status={{ kind: 'front', text: 'Review front', description: '' }} />);
    expect(closed).toContain('>Closed</span>'); expect(closed).not.toContain('>Review front</span>');
    for (const state of ['open', 'unknown'] as const) {
      const absent = markup(<ReviewPullFacts pull={pull(1, state)} />);
      expect(absent).not.toMatch(/class="badge review-(state|position)-/u);
    }
    const stale = markup(<ReviewQueueView />, queue([pull(2)]), undefined, { fronts: { ...fronts(), snapshot_id: 'old' } });
    expect(stale).not.toContain('review-position-front');
  });
  it('uses current channel closure as the primary status without a second heading badge', () => {
    const progress: ReviewProgressItem = { pull_number: 2, channels: [{ channel: 'hosted', configured: true, revision: 1, epoch: 1, closed: true, minimum: 0, maximum: 3, taper: 2, used: 1, dry: 0, tapered: false, pending: 0 }] };
    const html = markup(<ReviewQueueView />, queue([pull(2)]), undefined, { fronts: fronts(), progress: [progress] });
    expect(html).toContain('review-pull-facts"><span class="badge review-position-closed"');
    expect(html.match(/class="badge review-position-closed"/gu)).toHaveLength(1);
    expect(html).not.toContain('review-position-front');
  });
  it('holds a moved child without relying on a loaded parent row and keeps aligned queue labels unchanged', () => {
    const child = { ...pull(3), parent_pull_number: 2, parent_base_moved: true as const };
    const held = markup(<ReviewQueueView />, queue([child]), undefined, { fronts: stackedFronts() });
    const row = held.match(/<li id="review-pr-3"[\s\S]*?<\/li>/u)![0];
    expect(row).toContain('>Parent changed</span>');
    expect(row).toContain('title="Parent PR #2 changed; this child still targets its earlier base."');
    expect(row).toMatch(/aria-label="hosted current review state"><span data-lane-label-tone="alert"><span[^>]*>Parent changed<\/span>/u);
    expect(row).toMatch(/aria-label="cli current review state"><span data-lane-label-tone="alert"><span[^>]*>Parent changed<\/span>/u);
    expect(row).not.toContain('>Queued</span>');

    const alignedChild = { ...pull(3), parent_pull_number: 2, base_sha: pull(2).head_sha };
    const aligned = markup(<ReviewQueueView />, queue([alignedChild]), undefined, { fronts: stackedFronts() });
    const alignedRow = aligned.match(/<li id="review-pr-3"[\s\S]*?<\/li>/u)![0];
    expect(alignedRow).toContain('>Queued</span>');
    expect(alignedRow).not.toContain('Parent changed');
  });
  it('keeps old CI and merge-ready facts from reading as current proof after the parent moves', () => {
    const observation = { state: 'current' as const, ci_checked_at: '2026-10-28T12:12:35Z', ci_gate_ref: 'main', ci_gate_state: 'success' as const };
    const moved = { ...pull(3), parent_pull_number: 2, parent_base_moved: true as const, observation, merge_status: { state: 'ready' as const, reason: 'github_clean' } };
    const html = markup(<ReviewPullFacts pull={moved} />);
    expect(html).toContain('>CI for previous base</span>');
    expect(html).toContain('review-ci review-fact-unknown');
    expect(html).toContain('title="Parent PR #2 changed; this CI is for the earlier base. Last checked');
    expect(html).toContain('Last checked');
    expect(html).toContain('>Merge: Parent changed</span>');
    expect(html).not.toContain('>Merge: Ready</span>');
    const conflict = markup(<ReviewPullFacts pull={{ ...moved, merge_status: { state: 'blocked', reason: 'github_dirty' } }} />);
    expect(conflict).toContain('>Merge: Conflicts</span>');
    expect(conflict).not.toContain('>Merge: Parent changed</span>');

    const aligned = markup(<ReviewPullFacts pull={{ ...moved, parent_base_moved: undefined }} />);
    expect(aligned).toContain('>Required CI: Success · main</span>');
    expect(aligned).toContain('>Merge: Ready</span>');
    expect(aligned).not.toContain('CI for previous base');
  });
  it('preserves active work, closed channels, completed rounds and incoming routed-finding data', () => {
    const completed: ReviewRunSummary = { source: 'native-executor', id: 'completed-run', pull_number: 3, source_run_id: 'completed-source-run', channel: 'hosted', head_sha: pull(3).head_sha, reviewer: 'reviewer', scope: 'full', coverage_limits: [], outcome: 'completed', attributable: true, complete: true, started_at: '2026-10-20T00:00:00Z', finished_at: '2026-10-20T00:01:00Z', found: 1, accepted: 1, routed: 0, finalized: true, fixed: 1, current_accepted: 1, current_pending_fixes: 0 };
    const active: ReviewAttemptSummary = { source_attempt_id: 'active-attempt', channel: 'hosted', head_sha: pull(3).head_sha, state: 'active', started_at: '2026-10-21T00:00:00Z', finished_at: '', duration_seconds: null, source_run_id: '', trigger_id: 'trigger', provider_review_id: '' };
    const child = { ...pull(3), parent_pull_number: 2, parent_base_moved: true as const, run_count: 1, run_counts: { ...pull(3).run_counts, hosted: 1 }, recent_runs: [completed], attempt_count: 1, latest_attempts: [active] };
    const progress: ReviewProgressItem = { pull_number: 3, incoming_routes_pending: 1, channels: (['hosted', 'cli'] as const).map(channel => ({ channel, configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 5, taper: 2, used: 1, dry: 0, tapered: false, pending: 0, pending_execution: 0, pending_adjudication: 0 })) };
    const live = markup(<ReviewQueueView />, queue([child]), undefined, { fronts: stackedFronts(), progress: [progress] });
    expect(live).toContain('aria-label="hosted execution: Review active"');
    expect(live).toContain('>1/1/0</strong>');
    expect(live).toContain('1 incoming routed finding');
    expect(live).toMatch(/aria-label="cli current review state"><span data-lane-label-tone="alert"><span[^>]*>Parent changed<\/span>/u);

    const closedProgress: ReviewProgressItem = { pull_number: 3, channels: (['hosted', 'cli'] as const).map(channel => ({ channel, configured: true, revision: 1, epoch: 1, closed: true, minimum: 0, maximum: 5, taper: 2, used: 1, dry: 0, tapered: false, pending: 0, pending_execution: 0, pending_adjudication: 0 })) };
    const closed = markup(<ReviewQueueView />, queue([{ ...pull(3), parent_pull_number: 2, parent_base_moved: true }]), undefined, { fronts: stackedFronts(), progress: [closedProgress] });
    expect(closed).toContain('>Parent changed</span>');
    expect(closed).toContain('>Review closed</span>');
    expect(closed).not.toContain('current review state');
  });
  it('resolves only exact row anchors already present in the loaded queue', () => {
    const items = [pull(1, 'merged'), pull(2)];
    expect(reviewQueueHashTarget(undefined, '#review-pr-2')).toBeUndefined();
    expect(reviewQueueHashTarget([], '#review-pr-2')).toBeUndefined();
    expect(reviewQueueHashTarget(items, '#review-pr-2')).toBe('review-pr-2');
    expect(reviewQueueHashTarget(items, '#review-pr-1')).toBe('review-pr-1');
    for (const hash of ['#review-pr-3', '#review-pr-02', '#review-pr-2%20', '#review-pr-2 div', '#review-queue', '#other']) {
      expect(reviewQueueHashTarget(items, hash)).toBeUndefined();
    }
  });
  it('preserves order and repeats explicit noncontiguous headings without inventing unlabelled groups', () => {
    const items = [pull(4), pull(1), pull(3), pull(2)];
    const groups = [{ title: 'Foundation', pull_numbers: [1, 4, 2] }];
    const sections = groupReviewQueue(items, groups);
    expect(sections.map(section => section.title)).toEqual(['Foundation', undefined, 'Foundation']);
    expect(sections.flatMap(section => section.rows.map(row => row.pull.number))).toEqual([4, 1, 3, 2]);
    expect(groupReviewQueue(items, []).map(section => section.title)).toEqual([undefined]);
  });
  it('keeps existing group identities when another group appears above them', () => {
    const groups = [{ title: 'Foundation', pull_numbers: [1] }, { title: 'Delivery', pull_numbers: [2] }];
    const original = groupReviewQueue([pull(1), pull(2)], groups);
    const inserted = groupReviewQueue([pull(3), pull(1), pull(2)], groups);
    expect(inserted.slice(1).map(section => section.key)).toEqual(original.map(section => section.key));
    expect(new Set(inserted.map(section => section.key)).size).toBe(inserted.length);
  });
  it('names only actually missing metadata and does not fabricate a missing head', () => {
    const incomplete = { ...pull(1), complete: false, base_sha: '', ci_state: 'unknown' as const };
    expect(missingPullFacts(incomplete)).toEqual(['Base commit was not captured.']);
    const html = markup(<ReviewPullFacts pull={incomplete} />);
    expect(html).toContain('<summary>Missing PR details</summary>');
    expect(html).toContain('Base commit was not captured.'); expect(html).not.toContain('Head commit was not captured.');
    expect(html).toContain('>Checking CI</span>'); expect(html).toContain('Merge: Unknown');
    expect(html).toContain('class="badge review-fact-pill review-ci review-fact-unknown"');
    expect(html).toContain('class="badge review-fact-pill review-mergeability review-fact-unknown"');
    expect(markup(<ReviewPullFacts pull={pull(1, 'merged')} />)).toContain('review-state-merged');
    expect(markup(<ReviewPullFacts pull={pull(1, 'merged')} />)).not.toContain('retained history');
  });
  it.each(['unknown', 'mergeable'] as const)('omits %s mergeability for merged PRs while retaining other facts', mergeability => {
    const merged = { ...pull(1, 'merged'), mergeability };
    const html = markup(<ReviewPullFacts pull={merged} />);
    expect(html).toContain('review-state-merged'); expect(html).toContain('Merged');
    expect(html).not.toContain('Required CI:'); expect(html).not.toContain('Merge:');
    expect(html).not.toContain('Mergeability is unknown.'); expect(missingPullFacts(merged)).toEqual([]);
    const incomplete = { ...merged, complete: false, base_sha: '', ci_state: 'unknown' as const };
    expect(missingPullFacts(incomplete)).toEqual(['Base commit was not captured.']);
    const incompleteHTML = markup(<ReviewPullFacts pull={incomplete} />);
    expect(incompleteHTML).toContain('Missing PR details'); expect(incompleteHTML).toContain('Base commit was not captured.');
    expect(incompleteHTML).not.toContain('Required CI:'); expect(incompleteHTML).not.toContain('CI status is unknown.'); expect(incompleteHTML).not.toContain('Mergeability');
  });
  it.each(['open', 'unknown'] as const)('retains unknown mergeability for %s PRs', state => {
    const value = { ...pull(1, state), mergeability: 'unknown' as const };
    expect(missingPullFacts(value)).toEqual(state === 'unknown' ? ['Open or closed state is unknown.'] : []);
    const html = markup(<ReviewPullFacts pull={value} />);
    expect(html).toContain('Merge: Unknown'); expect(html).not.toContain('Mergeability is unknown.');
  });
  it('does not repeat inline unknown facts as a generic incomplete dropdown', () => {
    const value = { ...pull(1), complete: false, ci_state: 'unknown' as const, mergeability: 'unknown' as const };
    expect(missingPullFacts(value)).toEqual([]);
    const html = markup(<ReviewPullFacts pull={value} />);
    expect(html).toContain('>Checking CI</span>'); expect(html).toContain('Merge: Unknown');
    expect(html).not.toContain('Missing PR details');
    for (const state of ['open', 'merged', 'closed'] as const) {
      const retained = { ...pull(1, state), complete: false, ci_state: 'pending' as const, mergeability: 'unknown' as const };
      expect(missingPullFacts(retained)).toEqual([]);
      expect(markup(<ReviewPullFacts pull={retained} />)).not.toContain('Missing PR details');
      expect(retained.complete).toBe(false);
    }
  });
  it('renders retained merged rails, explicit groups and history arrows without automatic requests', () => {
    const presentation: Presentation = { project_id: 'project', revision: 1, name: 'Project', brand_name: 'FireController', tagline: '', headings: {}, theme: {}, updated_at: '', review_groups: [{ title: 'Foundation <literal>', pull_numbers: [1, 3] }] };
    const html = markup(<ReviewQueueView embedded />, queue([pull(1, 'merged'), pull(2), pull(3)]), presentation);
    expect(html).toContain('id="review-pr-1"'); expect(html).toContain('id="review-pr-2"'); expect(html).toContain('id="review-pr-3"');
    expect(html.indexOf('id="review-pr-1"')).toBeLessThan(html.indexOf('id="review-pr-2"'));
    expect(html.indexOf('id="review-pr-2"')).toBeLessThan(html.indexOf('id="review-pr-3"'));
    expect(html).toContain('review-row-merged'); expect(html).toContain('Queue distance −01'); expect(html).toContain('Queue distance +01: first open PR');
    expect(html.match(/Foundation &lt;literal&gt;/gu)).toHaveLength(2);
    expect(html).toContain('href="https://github.com/example/project/pull/2"');
    expect(html).toMatch(/<a\b[^>]*aria-label="Review history for PR #2"[^>]*>Review history ↗<\/a>/u);
    expect(html).not.toContain('review-link-arrow');
    expect(html).toContain('Merged PRs from the last 48 hours and the latest two known merges stay here.');
    expect(html).toContain('Full retained history is in'); expect(html).toContain('href="/projects/project/reviews/history"');
    expect(html).not.toContain('Signed positions'); expect(html).not.toContain('As of ');
    expect(html).not.toContain('Latest attempt:'); expect(html).not.toContain('Incomplete PR observation');
  });
  it('renders merged history in supplied merge-time order with a back link and no current controls', () => {
    const items = [{ ...pull(9, 'merged'), queue_position: 9 }, { ...pull(2, 'merged'), queue_position: 2 }];
    const html = markup(<ReviewQueueView history />, { ...queue(items), head_position: 10, has_more: true, cursor: 'older' }, undefined, { history: true, fronts: fronts() });
    expect(html).toContain('<h2 id="review-queue-title">Queue history</h2>');
    expect(html).toMatch(/href="\/projects\/project\/reviews"[^>]*>Back to review queue/u);
    expect(html.indexOf('id="review-pr-9"')).toBeLessThan(html.indexOf('id="review-pr-2"'));
    expect(html).toContain('Queue distance −01'); expect(html).toContain('Queue distance −08');
    expect(html).toContain('Merged PRs are shown newest first.');
    expect(html).toContain('Next queue history');
    expect(html).not.toContain('Attempt states describe retained history. Limits show current settings; neither grants permission to request another review.'); expect(html).not.toContain('last 48 hours'); expect(html).not.toContain('review-position-front');
  });
  it('isolates bounded recent and merged cursor reads by view and source snapshot', async () => {
    const read = vi.fn().mockResolvedValue(queue([]));
    const session = { controller: new AbortController(), api: { reviewQueue: read } } as unknown as Session;
    const recent = reviewQueueQuery(session, key, 'recent');
    const history = reviewQueueQuery(session, key, 'merged', 'older', 'snapshot');
    expect(recent.queryKey).toEqual([...key, 'reviews', 'current', 'queue', 'recent', '']);
    expect(history.queryKey).toEqual([...key, 'reviews', 'snapshot', 'queue', 'merged', 'older']);
    await recent.queryFn({ signal: new AbortController().signal });
    await history.queryFn({ signal: new AbortController().signal });
    expect(read.mock.calls.map(call => call[0])).toEqual([{ limit: 20, view: 'recent' }, { limit: 20, view: 'merged', cursor: 'older' }]);
    session.controller.abort();
    expect(read.mock.calls.every(call => call[1].aborted)).toBe(true);
  });
  it('omits completed attempt prose and does not imply a current cooldown from an old stopped attempt', () => {
    const attempt: ReviewAttemptSummary = { source_attempt_id: 'attempt', channel: 'cli', head_sha: 'a'.repeat(40), state: 'completed', started_at: '', finished_at: '', duration_seconds: 1, source_run_id: '', trigger_id: '', provider_review_id: '' };
    const props = { number: 1, snapshot: 'snapshot', head: 'a'.repeat(40), attempts: [attempt], expected: 0, channelTotals: { hosted: 0, cli: 0, manual: 0, subagent: 0 } };
    const completed = markup(<ReviewRunsView {...props} />);
    expect(completed).not.toContain('Latest attempt:'); expect(completed).not.toContain('review-attempt-state');
    expect(markup(<ReviewRunsView {...props} attempts={[{ ...attempt, state: 'rate_limited' }]} />)).toContain('Retained attempt: rate_limited.');
    expect(markup(<ReviewRunsView {...props} attempts={[]} />)).not.toContain('review-attempt-state');
    const held = markup(<ReviewRunsView {...props} attempts={[{ ...attempt, channel: 'hosted', state: 'held', reason: 'hosted_allowance_cooldown' }]} />);
    expect(held).toContain('>Check attempt</span>');
    expect(held).toContain('held (hosted_allowance_cooldown)');
    expect(held).not.toContain('>Check in ');
  });
  it('renders full finding Markdown safely while leaving titles and commit IDs literal', () => {
    const finding: ReviewFinding = { source_finding_id: 'id', title: '**Literal title**', detail: 'Useful **detail**\n\n- A step\n\n[unsafe](javascript:alert(1)) ![image](https://outside.invalid/image.png)', severity: 'low', disposition: 'accepted', decision_reason: 'Keep *context*.', actor: '', decided_at: '', fix_sha: '', proof_note: '`tested` with **evidence**', resolved_at: '' };
    const html = markup(<ReviewFindingView finding={finding} />);
    expect(html).toContain('>**Literal title**</h4>'); expect(html).toContain('<strong>detail</strong>'); expect(html).toContain('<li>A step</li>'); expect(html).toContain('<em>context</em>'); expect(html).toContain('<code>tested</code>');
    expect(html).not.toContain('<img'); expect(html).not.toContain('href="javascript:');
  });
});


describe('compact PR sizing display',()=>{
  it('keeps limits and headroom on the file count without repeating review totals or adding a row',()=>{
    const value:ReviewPullSummary={...pull(7),diff:{files:99,additions:20,deletions:4},review_size:{pull_number:7,files:99,max_review_files:100,remaining_files:1,completed_reviews:13,retained_reviews:13,message:'99 files; maximum review size 100; 1 files remaining. 13 completed reviews (historical).'}};
    const html=markup(<ReviewPullFacts pull={value}/>);
    expect(html).toContain('title="Maximum review size: 100 files. 1 file remaining.">99</span> files');
    expect(html).not.toMatch(/review-size-note|review-size-warning|review-size-exceeded|completed reviews|historical/u);
    value.review_size={pull_number:7,files:101,max_review_files:100,excess_files:1,message:'Maximum exceeded.'};value.diff!.files=101;
    const exceeded=markup(<ReviewPullFacts pull={value}/>);
    expect(exceeded).toContain('review-file-count review-size-exceeded');expect(exceeded).toContain('Maximum exceeded by 1 file.');
    value.review_size={pull_number:7,files:96,suggested_max_pr_files:80,above_suggested:true,message:'Suggestion exceeded.'};value.diff!.files=96;
    const suggested=markup(<ReviewPullFacts pull={value}/>);
    expect(suggested).toContain('Suggested maximum PR size: 80 files.');expect(suggested).not.toContain('remaining');
  });
});


describe('retained CI explanation',()=>{
  it('reserves CI pending for an observed required-check verdict, including a comparison-bound final gate', () => {
    const value: ReviewPullSummary = { ...pull(7), ci_state: 'pending' };
    expect(markup(<ReviewPullFacts pull={value} />)).toContain('>Checking CI</span>');
    value.observation = { state: 'current', ci_checked_at: '2026-10-28T12:12:35Z', ci_gate_ref: 'main', ci_gate_state: 'pending', ci_error: 'ci_required_checks_pending' };
    const html = markup(<ReviewPullFacts pull={value} />);
    expect(html).toContain('>CI pending · main</span>');
    expect(html).toContain('Open the PR’s Checks tab on GitHub for workflow progress.');
    expect(html).toContain('review-ci review-fact-pending');
  });

  it.each([
    'ci_provider_unavailable', 'ci_provider_rate_limited', 'ci_authority_unproven',
    'ci_required_checks_unproven', 'ci_inventory_unproven', 'ci_reader_unavailable',
  ])('shows unavailable rather than pending for attempted authoritative lookup failure or ambiguous evidence (%s)', ci_error => {
    for (const ci_state of ['unknown', 'pending'] as const) {
      const value: ReviewPullSummary = { ...pull(7), ci_state, observation: { state: 'current', ci_checked_at: '2026-10-28T12:12:35Z', ci_error } };
      const html = markup(<ReviewPullFacts pull={value} />);
      expect(html).toContain('>CI unverified</span>');
      expect(html).toContain('review-ci review-fact-unknown');
      expect(html).not.toContain('>CI pending');
      expect(html).not.toContain('>Checking CI');
      expect(html).toContain('Last checked');
    }
  });

  it.each([
    ['ci_required_checks_unproven', 'CI unverified', 'Open the PR’s Checks tab on GitHub for workflow progress.'],
    ['ci_required_check_missing', 'Checks not reported', 'A workflow can be running before its required check appears. Open the PR’s Checks tab on GitHub for workflow progress.'],
    ['ci_collection_deadline_head_results', 'CI check timed out', 'Open the PR’s Checks tab on GitHub for workflow progress.'],
    ['ci_provider_timeout', 'CI check timed out', 'Open the PR’s Checks tab on GitHub for workflow progress.'],
    ['ci_provider_rate_limited_head_results', 'CI unverified', 'GitHub’s request limit was reached.'],
  ])('explains unverified CI without repeating the branch or guessing a cause (%s)', (ci_error, label, explanation) => {
    const value: ReviewPullSummary = { ...pull(7), base_ref: 'main', ci_state: 'unknown', observation: { state: 'current', ci_gate_ref: 'main', ci_gate_state: 'unknown', ci_error } };
    const html = markup(<ReviewPullFacts pull={value} />);
    expect(html).toContain(`title="${explanation}"`);
    expect(html).toContain(`>${label} · main</span>`);
  });

  it('distinguishes an unverified active comparison from attempted unknown evidence without guessing from PR success times', () => {
    const value: ReviewPullSummary = { ...pull(7), ci_state: 'unknown', observation: { state: 'current', last_success_at: '2026-10-28T12:12:35Z' } };
    expect(markup(<ReviewPullFacts pull={value} />)).toContain('>Checking CI</span>');
    value.observation!.ci_checked_at = '2026-10-28T12:12:35Z';
    expect(markup(<ReviewPullFacts pull={value} />)).toContain('>CI unverified</span>');
    value.observation = { state: 'current', ci_gate_ref: 'main', ci_gate_state: 'unknown' };
    expect(markup(<ReviewPullFacts pull={value} />)).toContain('>CI unverified · main</span>');
  });

  it.each(['ci_merge_base_mismatch', 'ci_merge_pending', 'ci_merge_identity_unproven', 'ci_candidate_changed', 'ci_target_changed'])('shows Checking CI for a known moving or pending comparison (%s)', ci_error => {
    for (const ci_state of ['unknown', 'pending', 'success'] as const) {
      const value: ReviewPullSummary = { ...pull(7), ci_state, merge_status: { state: 'ready', reason: 'github_clean' }, observation: { state: 'current', ci_checked_at: '2026-10-28T12:12:35Z', ci_error } };
      const html = markup(<ReviewPullFacts pull={value} />);
      expect(html).toContain('>Checking CI</span>');
      expect(html).not.toContain('>CI pending');
      expect(html).not.toContain('>CI unverified');
      expect(html).toContain('Merge: Ready');
      expect(value.ci_state).toBe(ci_state);
    }
  });

  it.each(['head_sha', 'base_sha', 'base_ref'] as const)('does not treat an earlier CI timestamp as verification after exact %s movement', field => {
    const value: ReviewPullSummary = { ...pull(7), [field]: field === 'base_ref' ? 'develop' : 'c'.repeat(40), ci_state: 'success', observation: { state: 'comparison_changed' } };
    const html = markup(<ReviewPullFacts pull={value} />);
    expect(html).toContain('>Checking CI</span>');
    expect(html).not.toContain('>CI pending');
    expect(html).not.toContain('>CI unverified');
    // A CI attempt can match even when the broader observer still reports movement.
    value.observation = { state: 'comparison_changed', ci_error: 'ci_provider_unavailable' };
    expect(markup(<ReviewPullFacts pull={value} />)).toContain('>CI unverified</span>');
    value.observation = { state: 'comparison_changed', ci_error: 'ci_required_checks_pending', ci_gate_ref: 'main', ci_gate_state: 'pending' };
    expect(markup(<ReviewPullFacts pull={value} />)).toContain('>CI pending · main</span>');
  });

  it.each(['success', 'failure', 'not_required'] as const)('preserves retained and comparison-bound %s without requiring observer freshness fields', ci_state => {
    const label = ci_state === 'not_required' ? 'None' : ci_state === 'success' ? 'Success' : 'Failure';
    const value: ReviewPullSummary = { ...pull(7), ci_state };
    expect(markup(<ReviewPullFacts pull={value} />)).toContain(`>Required CI: ${label}</span>`);
    value.observation = { state: 'retrying', ci_gate_ref: 'main', ci_gate_state: ci_state, hosted_error: 'external_hosted_identity_unproven' };
    const html = markup(<ReviewPullFacts pull={value} />);
    expect(html).toContain(`>Required CI: ${label} · main</span>`);
    expect(html).not.toContain('>Checking CI');
    expect(html).not.toContain('>CI unverified');
  });

  it('does not infer a CI lookup failure from errors confined to Hosted history', () => {
    const observation: ReviewPullSummary['observation'] = { state: 'retrying', error: 'external_hosted_identity_unproven', hosted_error: 'external_hosted_identity_unproven', hosted_checked_at: '2026-10-28T12:12:35Z' };
    for (const ci_state of ['success', 'pending', 'unknown'] as const) {
      const html = markup(<ReviewPullFacts pull={{ ...pull(7), ci_state, observation }} />);
      expect(html).toContain('Hosted history: Incomplete');
      expect(html).not.toContain('CI unverified');
      expect(html).toContain(ci_state === 'success' ? '>Required CI: Success</span>' : '>Checking CI</span>');
    }
  });

  it('does not imply active checking for unknown closed history or show current CI after merge', () => {
    for (const ci_error of [undefined, 'ci_merge_pending']) {
      const closed = markup(<ReviewPullFacts pull={{ ...pull(7, 'closed'), ci_state: 'unknown', observation: { state: 'current', ci_error } }} />);
      expect(closed).toContain('>CI unverified</span>');
      expect(closed).not.toContain('Checking CI');
    }
    const merged = markup(<ReviewPullFacts pull={{ ...pull(7, 'merged'), ci_state: 'unknown', observation: { state: 'current', ci_error: 'ci_merge_pending' } }} />);
    expect(merged).not.toContain('Checking CI');
    expect(merged).not.toContain('CI unverified');
    expect(merged).not.toContain('review-ci');
  });

  it('explains stale GitHub test-merge bases without inventing current CI success',()=>{
    const value:ReviewPullSummary={...pull(7),ci_state:'unknown',merge_status:{state:'ready',reason:'github_clean'},observation:{state:'current',ci_checked_at:'2026-10-28T12:12:35Z',ci_error:'ci_merge_base_mismatch'}};
    const html=markup(<ReviewPullFacts pull={value}/>);
    expect(html).toContain('>Checking CI</span>');expect(html).toContain('Merge: Ready');
    expect(html).toContain("GitHub is still testing an older base.");
    expect(html).not.toContain('Ci merge base mismatch');expect(value.ci_state).toBe('unknown');
  });
});


describe('compact queue footer navigation',()=>{
  it('puts review history after the review channels, with draft styling separate from row lifecycle',()=>{
    const value={...pull(7),is_draft:true};
    const html=markup(<ReviewQueueRow pull={value} snapshot="retained" order={{index:0}}/>);
    expect(html).toContain('review-row-draft');expect(html).toContain('review-state-draft');
    expect(html).toContain('aria-label="Review history for PR #7"');expect(html).toContain('>Review history ↗</a>');
    expect(html.indexOf('>Review history ↗</a>')).toBeGreaterThan(html.indexOf('class="review-history"'));
    expect(html).toContain('/reviews/pulls/7?snapshot=retained');
    expect(html).not.toContain('>Review details');
  });
});

it.each(['unknown','pending','success','failure','not_required'] as const)('labels the final gate separately from the actual stacked comparison (%s)', ci_state => {
  const value:ReviewPullSummary={...pull(7),base_ref:'feature/parent',ci_state,observation:{state:'current',last_success_at:'2026-10-28T12:12:35Z',ci_checked_at:'2026-10-28T12:12:35Z',ci_gate_ref:'main',ci_gate_state:ci_state}};
  const html=markup(<ReviewPullFacts pull={value}/>);
  expect(html).toContain('· main');
  expect(html).toContain('Checks for main on the PR against feature/parent.');
  expect(html).toContain('Last checked');
});

it('does not relabel selected-base empty CI as final-gate proof', () => {
  const value:ReviewPullSummary={...pull(7),base_ref:'feature/parent',ci_state:'not_required',observation:{state:'current',last_success_at:'2026-10-28T12:12:35Z',ci_checked_at:'2026-10-28T12:12:35Z',ci_gate_ref:'main',ci_gate_state:'pending',ci_error:'ci_required_checks_pending'}};
  const html=markup(<ReviewPullFacts pull={value}/>);
  expect(html).toContain('>CI pending · main</span>');
  expect(html).not.toContain('Required CI: None');
  expect(html).not.toContain('Optional checks are separate.');
});
