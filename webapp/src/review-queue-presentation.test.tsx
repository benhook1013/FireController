import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { WorkspaceContext, type Session } from './workspace';
import { PresentationContext } from './presentation-context';
import { ReviewFindingView, ReviewPullFacts, ReviewQueueView, ReviewRunsView, formatQueueDistance, groupReviewQueue, missingPullFacts, reviewQueueDistances, reviewQueueHashTarget, reviewQueueQuery } from './review-workspace';
import type { ReviewFronts } from './review-fronts';
import type { ReviewProgressItem } from './review-progress';
import type { Presentation, ReviewAttemptSummary, ReviewFinding, ReviewPullSummary, ReviewQueue } from './types';

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
const fronts = (): ReviewFronts => ({ project_id: 'project', configured: true, repository_id: 1, configuration_revision: 1, policy_revision: 1, snapshot_id: 'snapshot', queue: [1, 2, 3], channels: [{ channel: 'hosted', state: 'ready', pull_number: 2, head_sha: pull(2).head_sha, base_sha: pull(2).base_sha, base_ref: 'main' }] });

describe('truthful stored review presentation', () => {
  it('distinguishes no required CI from an unknown observation without claiming workflow success', () => {
    const html = markup(<ReviewPullFacts pull={{ ...pull(2), ci_state: 'not_required' }} />);
    expect(html).toContain('Required CI: None');
    expect(html).toContain('Optional workflow results are separate.');
    expect(html).not.toContain('Required CI: Success');
    expect(html).not.toContain('CI status is unknown.');
    expect(markup(<ReviewPullFacts pull={{ ...pull(2), ci_state: 'unknown' }} />)).toContain('Required CI: Unknown');
  });
  it('identifies incomplete Hosted history without calling healthy PR checks a GitHub retry', () => {
    const observation = { state: 'retrying' as const, checked_at: '2026-10-23T13:24:02Z', last_success_at: '2026-10-23T13:24:02Z', error: 'external_hosted_identity_unproven', hosted_error: 'external_hosted_identity_unproven' };
    const html = markup(<ReviewPullFacts pull={{ ...pull(2), observation }} />);
    expect(html).toContain('Hosted history: Incomplete'); expect(html).toContain('Some Hosted review history could not yet be imported.');
    expect(html).toContain('Last successful PR check:'); expect(html).not.toContain('GitHub: Retrying');
    for (const error of ['provider_unavailable', 'provider_rate_limited']) {
      const failed = markup(<ReviewPullFacts pull={{ ...pull(2), observation: { ...observation, error } }} />);
      expect(failed).toContain('GitHub: Retrying'); expect(failed).not.toContain('Hosted history: Incomplete');
    }
    expect(markup(<ReviewPullFacts pull={{ ...pull(2), observation: { ...observation, state: 'current', error: undefined, hosted_error: undefined } }} />)).not.toContain('GitHub:');
  });

  it('keeps the row front concise and places independent front markers on channel headings', () => {
    const both = fronts(); both.channels.push({ ...both.channels[0], channel: 'cli' });
    const html = markup(<ReviewQueueView />, queue([pull(1, 'merged'), pull(2), pull(3)]), undefined, { fronts: both });
    expect(html).not.toContain('Review front ·');
    const row = html.match(/<li id="review-pr-2"[\s\S]*?<\/li>/u)![0];
    expect(row).toContain('Hosted CodeRabbit<span class="badge review-channel-position">Up next</span>');
    expect(row).toContain('CLI CodeRabbit<span class="badge review-channel-position">Up next</span>');
    const independent = markup(<ReviewQueueView />, queue([pull(1, 'merged'), pull(2), pull(3)]), undefined, { fronts: fronts() });
    const selected = independent.match(/<li id="review-pr-2"[\s\S]*?<\/li>/u)![0];
    expect(selected).toContain('Hosted CodeRabbit<span class="badge review-channel-position">Up next</span>');
    expect(selected).not.toContain('CLI CodeRabbit<span class="badge review-channel-position">Up next</span>');
  });
  it('keeps an already-started review visible after head movement', () => {
    const attempt = { channel: 'hosted', head_sha: pull(2).head_sha, state: 'active', finished_at: '', started_at: '2026-10-22T00:00:00Z', source_attempt_id: 'active' } as ReviewAttemptSummary;
    const html = markup(<ReviewQueueView />, queue([pull(1, 'merged'), { ...pull(2), latest_attempts: [attempt] }]), undefined, { fronts: fronts() });
    expect(html).toContain('Hosted CodeRabbit<span class="badge review-channel-position">Reviewing</span>');
    expect(html).not.toContain('>Up next</span>');
    const stale = markup(<ReviewQueueView />, queue([pull(1, 'merged'), { ...pull(2), latest_attempts: [{ ...attempt, head_sha: 'c'.repeat(40) }] }]), undefined, { fronts: fronts() });
    expect(stale).toContain('Hosted CodeRabbit<span class="badge review-channel-position">Reviewing</span>');
    expect(stale).not.toContain('>Up next</span>');
  });
  it('shows carried admission work without claiming Up next, and keeps independent channel readiness', () => {
    const held=fronts();held.channels[0]={...held.channels[0],state:'held',reason:'source_obligations_pending'};
    held.channels.push({...held.channels[0],channel:'cli',state:'ready',reason:undefined});
    const html=markup(<ReviewQueueView />,queue([pull(2)]),undefined,{fronts:held});
    expect(html).toMatch(/Hosted CodeRabbit<span class="badge review-channel-position" title="[^"]*holds new reviews[^"]*">Carried work<\/span>/u);
    expect(html).toContain('CLI CodeRabbit<span class="badge review-channel-position">Up next</span>');
    expect(html).not.toContain('Hosted CodeRabbit<span class="badge review-channel-position">Up next</span>');
    const attempt={channel:'hosted',head_sha:pull(2).head_sha,state:'active',finished_at:'',started_at:'2026-10-22T00:00:00Z',source_attempt_id:'active'} as ReviewAttemptSummary;
    const running=markup(<ReviewQueueView />,queue([{...pull(2),latest_attempts:[attempt]}]),undefined,{fronts:held});
    expect(running).toContain('Hosted CodeRabbit<span class="badge review-channel-position">Reviewing</span>');
    expect(running).not.toContain('>Carried work</span>');
    const stale=markup(<ReviewQueueView />,queue([pull(2)]),undefined,{fronts:{...held,snapshot_id:'old'}});
    expect(stale).not.toContain('>Carried work</span>');
  });
  it('labels a locally derived queue position without claiming verified admission', () => {
    const missing=fronts();missing.channels=[{channel:'hosted',state:'unknown',reason:'source_frame_unavailable'}];
    const progress:ReviewProgressItem={pull_number:2,channels:[{channel:'hosted',configured:true,revision:1,epoch:1,closed:false,minimum:1,maximum:3,taper:2,used:0,dry:0,tapered:false,pending:0}]};
    missing.queue=[2];
    const html=markup(<ReviewQueueView />,queue([pull(2)]),undefined,{fronts:missing,progress:[progress]});
    expect(html).toContain('>Queue position</span>');
    expect(html).toContain('Source and admission have not been verified.');
    expect(html).not.toContain('>Up next</span>');
  });
  it.each([
    ['success', 'mergeable'], ['pending', 'conflicting'], ['failure', 'unknown'], ['unknown', 'unknown'],
  ] as const)('colors observed CI %s and mergeability %s without changing their labels', (ci_state, mergeability) => {
    const html = markup(<ReviewPullFacts pull={{ ...pull(2), ci_state, mergeability }} />);
    expect(html).toContain(`review-ci review-fact-${ci_state}`);
    expect(html).toContain(`review-mergeability review-fact-${mergeability}`);
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
    expect(rows[2].match(/>Queued<\/span>/gu)).toHaveLength(1);
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
  it('names only actually missing metadata and does not fabricate a missing head', () => {
    const incomplete = { ...pull(1), complete: false, base_sha: '', ci_state: 'unknown' as const };
    expect(missingPullFacts(incomplete)).toEqual(['Base commit was not captured.']);
    const html = markup(<ReviewPullFacts pull={incomplete} />);
    expect(html).toContain('<summary>Missing PR details</summary>');
    expect(html).toContain('Base commit was not captured.'); expect(html).not.toContain('Head commit was not captured.');
    expect(html).toContain('Required CI: Unknown'); expect(html).toContain('Mergeability: Mergeable');
    expect(html).toContain('class="badge review-fact-pill review-ci review-fact-unknown"');
    expect(html).toContain('class="badge review-fact-pill review-mergeability review-fact-mergeable"');
    expect(markup(<ReviewPullFacts pull={pull(1, 'merged')} />)).toContain('review-state-merged');
    expect(markup(<ReviewPullFacts pull={pull(1, 'merged')} />)).not.toContain('retained history');
  });
  it.each(['unknown', 'mergeable'] as const)('omits %s mergeability for merged PRs while retaining other facts', mergeability => {
    const merged = { ...pull(1, 'merged'), mergeability };
    const html = markup(<ReviewPullFacts pull={merged} />);
    expect(html).toContain('review-state-merged'); expect(html).toContain('Merged');
    expect(html).toContain('Required CI: Success'); expect(html).not.toContain('Mergeability:');
    expect(html).not.toContain('Mergeability is unknown.'); expect(missingPullFacts(merged)).toEqual([]);
    const incomplete = { ...merged, complete: false, base_sha: '', ci_state: 'unknown' as const };
    expect(missingPullFacts(incomplete)).toEqual(['Base commit was not captured.']);
    const incompleteHTML = markup(<ReviewPullFacts pull={incomplete} />);
    expect(incompleteHTML).toContain('Missing PR details'); expect(incompleteHTML).toContain('Base commit was not captured.');
    expect(incompleteHTML).toContain('Required CI: Unknown'); expect(incompleteHTML).not.toContain('CI status is unknown.'); expect(incompleteHTML).not.toContain('Mergeability');
  });
  it.each(['open', 'closed', 'unknown'] as const)('retains unknown mergeability for %s PRs', state => {
    const value = { ...pull(1, state), mergeability: 'unknown' as const };
    expect(missingPullFacts(value)).toEqual(state === 'unknown' ? ['Open or closed state is unknown.'] : []);
    const html = markup(<ReviewPullFacts pull={value} />);
    expect(html).toContain('Mergeability: Unknown'); expect(html).not.toContain('Mergeability is unknown.');
  });
  it('does not repeat inline unknown facts as a generic incomplete dropdown', () => {
    const value = { ...pull(1), complete: false, ci_state: 'unknown' as const, mergeability: 'unknown' as const };
    expect(missingPullFacts(value)).toEqual([]);
    const html = markup(<ReviewPullFacts pull={value} />);
    expect(html).toContain('Required CI: Unknown'); expect(html).toContain('Mergeability: Unknown');
    expect(html).not.toContain('Missing PR details');
    const explicit = { ...pull(1), complete: false };
    expect(missingPullFacts(explicit)).toEqual(['The source marked this PR incomplete without identifying another missing field.']);
    expect(markup(<ReviewPullFacts pull={explicit} />)).toContain('Missing PR details');
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
    expect(html).toMatch(/<a\b[^>]*aria-label="Review details for PR #2"[^>]*>Review details ↗<\/a>/u);
    expect(html).not.toContain('review-link-arrow');
    expect(html).toContain('Merged PRs from the last 48 hours and the latest two known merges stay here.');
    expect(html).toContain('Full retained history is in'); expect(html).toContain('href="/projects/project/reviews/history"');
    expect(html).not.toContain('Signed positions'); expect(html).not.toContain('As of ');
    expect(html).not.toContain('Latest attempt:'); expect(html).not.toContain('Incomplete PR observation');
  });
  it('renders merged history in supplied reverse queue order with a back link and no current controls', () => {
    const items = [{ ...pull(9, 'merged'), queue_position: 9 }, { ...pull(2, 'merged'), queue_position: 2 }];
    const html = markup(<ReviewQueueView history />, { ...queue(items), head_position: 10, has_more: true, cursor: 'older' }, undefined, { history: true, fronts: fronts() });
    expect(html).toContain('<h2 id="review-queue-title">Queue history</h2>');
    expect(html).toMatch(/href="\/projects\/project\/reviews"[^>]*>Back to review queue/u);
    expect(html.indexOf('id="review-pr-9"')).toBeLessThan(html.indexOf('id="review-pr-2"'));
    expect(html).toContain('Queue distance −01'); expect(html).toContain('Queue distance −08');
    expect(html).toContain('Merged PRs are shown in reverse queue order.');
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
  it('omits completed attempt prose and shows a concise retained rate-limit signal only with evidence', () => {
    const attempt: ReviewAttemptSummary = { source_attempt_id: 'attempt', channel: 'cli', head_sha: 'a'.repeat(40), state: 'completed', started_at: '', finished_at: '', duration_seconds: 1, source_run_id: '', trigger_id: '', provider_review_id: '' };
    const props = { number: 1, snapshot: 'snapshot', head: 'a'.repeat(40), attempts: [attempt], expected: 0, channelTotals: { hosted: 0, cli: 0, manual: 0, subagent: 0 } };
    const completed = markup(<ReviewRunsView {...props} />);
    expect(completed).not.toContain('Latest attempt:'); expect(completed).not.toContain('review-attempt-state');
    expect(markup(<ReviewRunsView {...props} attempts={[{ ...attempt, state: 'rate_limited' }]} />)).toContain('Latest retained cli attempt: Cooldown');
    expect(markup(<ReviewRunsView {...props} attempts={[]} />)).not.toContain('review-attempt-state');
  });
  it('renders full finding Markdown safely while leaving titles and commit IDs literal', () => {
    const finding: ReviewFinding = { source_finding_id: 'id', title: '**Literal title**', detail: 'Useful **detail**\n\n- A step\n\n[unsafe](javascript:alert(1)) ![image](https://outside.invalid/image.png)', severity: 'low', disposition: 'accepted', decision_reason: 'Keep *context*.', actor: '', decided_at: '', fix_sha: '', proof_note: '`tested` with **evidence**', resolved_at: '' };
    const html = markup(<ReviewFindingView finding={finding} />);
    expect(html).toContain('>**Literal title**</h4>'); expect(html).toContain('<strong>detail</strong>'); expect(html).toContain('<li>A step</li>'); expect(html).toContain('<em>context</em>'); expect(html).toContain('<code>tested</code>');
    expect(html).not.toContain('<img'); expect(html).not.toContain('href="javascript:');
  });
});
