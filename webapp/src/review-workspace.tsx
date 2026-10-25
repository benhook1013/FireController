import { CompactReviewFinding } from './review-finding-display';
import { ReviewObligations } from './review-obligations';
import { useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore, type ReactNode } from 'react';
import { Link, useLocation, useParams, useSearchParams } from 'react-router';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from './api';
import { terminalBrowserSession } from './browser-session';
import { Pager, ReadState, signalFor, usePaging, useWorkspace, type Session } from './workspace';
import { usePresentation, useProjectHeading } from './presentation-context';
import { ReviewArchive, type ReviewArchivePage } from './review-archive';
import { PageSkeleton, ReviewRoundsSkeleton } from './loading';
import { useRetainedRead } from './retained-read';
import { RestoringReadingPosition, scrollToReadingTarget } from './reading-position';
import { RelativeTime } from './relative-time';
import { ReviewLaneDisplay, ReviewRoundPillDisplay, ReviewRowDisplay } from './review-display';
import { EditorialMarkdown } from './editorial-markdown';
import { githubRepositoryURL } from './github-links';
import './review-queue.css';
import './review-history.css';
import { useReviewProgress, ReviewProgressView } from './review-progress-view';
import type { ReviewProgressItem, ReviewRoundProgress } from './review-progress';
import { currentReviewFronts, reviewQueueBadges, reviewChannelPosition, type ReviewChannelPosition, type ReviewFronts, type ReviewQueueBadge } from './review-fronts';
import { useReviewFronts } from './review-fronts-view';
import type { Project, ReviewGroup, ReviewChannel, ReviewRunCounts, ReviewAttemptSummary, ReviewAttemptItem, ReviewFinding, ReviewPullSummary, ReviewRunSummary, ReviewSnapshot } from './types';

const pageSize = 20;
function time(value: string): string { return new Date(value).toLocaleString(); }
function terminal(error: unknown): boolean { return error instanceof ApiError && terminalBrowserSession(error); }
function useReviewSnapshot(): string | undefined {
  const [params] = useSearchParams();
  const values = params.getAll('snapshot');
  return values.length === 0 ? undefined : values.length === 1 ? values[0] : '';
}
function reviewLink(home: string, kind: 'pulls'|'runs', id: string | number, snapshot: string): string {
  return `${home}/reviews/${kind}/${encodeURIComponent(String(id))}?${new URLSearchParams({ snapshot })}`;
}
function githubLink(project: Project, path: string): string | undefined {
  const repository = githubRepositoryURL(project.repository);
  return repository ? `${repository}/${path}` : undefined;
}
export function reviewLabel(value: string): string {
  const words = value.replaceAll('_', ' ');
  return words.charAt(0).toUpperCase() + words.slice(1);
}
function reviewAttemptReason(attempt: ReviewAttemptSummary): string | undefined {
  if (!attempt.reason || !['held', 'uncertain', 'rate_limited', 'failed'].includes(attempt.state)) return undefined;
  const labels: Record<string, string> = {
    hosted_finished_inventory_unproven: 'Finished response could not be verified',
    hosted_allowance_cooldown: 'Waiting for hosted allowance',
    allowance_exhausted: 'CLI allowance exhausted',
  };
  return Object.hasOwn(labels, attempt.reason) ? labels[attempt.reason] : reviewLabel(attempt.reason);
}
function ReviewNotice({ history = false }: { history?: boolean }) {
  const { home } = useWorkspace();
  return <aside className="review-guide" aria-label="Reading the review queue"><h3>Review states</h3><dl><div><dt>Active</dt><dd>Attempt preparing or running</dd></div><div><dt>Cooldown</dt><dd>Provider rate limit</dd></div><div><dt>Held</dt><dd>Attempt stopped by a hold</dd></div><div><dt>Failed / Uncertain</dt><dd>Failure or unconfirmed outcome</dd></div><div><dt>Cancelled</dt><dd>Attempt cancelled</dd></div></dl><p className="review-count-key"><strong>Reading rounds:</strong> found / accepted / routed. Highlighted zero-accepted rounds have no accepted findings. {history ? 'Merged PRs are shown in reverse queue order.' : <>Merged PRs from the last 48 hours and the latest two known merges stay here. Full retained history is in <Link to={`${home}/reviews/history`}>Queue history</Link>.</>}</p></aside>;
}
function ReviewProvenance({ value }: { value: ReviewSnapshot }) {
  return <div className="review-freshness"><details><summary>Source details</summary><dl><div><dt>Source</dt><dd>{value.source}</dd></div><div><dt>Snapshot origin timestamp</dt><dd>{time(value.observed_at)}</dd></div><div><dt>Recorded</dt><dd>{time(value.recorded_at)}</dd></div><div><dt>Snapshot</dt><dd><code>{value.snapshot_id}</code> · revision {value.revision}</dd></div></dl></details></div>;
}
function ReviewQueueTechnicalDetails({ value }: { value: ReviewSnapshot }) {
  const details = useRef<HTMLDetailsElement>(null);
  useEffect(() => {
    const outside = (event: PointerEvent) => {
      if (details.current?.open && event.target instanceof Node && !details.current.contains(event.target)) details.current.open = false;
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || !details.current?.open) return;
      details.current.open = false;
      details.current.querySelector('summary')?.focus();
      event.preventDefault();
    };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('keydown', escape);
    return () => { document.removeEventListener('pointerdown', outside); document.removeEventListener('keydown', escape); };
  }, []);
  return <details ref={details} className="review-technical-details"><summary aria-label="Technical details" title="Technical details"><svg aria-hidden="true" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"><circle cx="12" cy="12" r="9" /><path d="M12 11v6" /><circle cx="12" cy="7.5" r=".9" fill="currentColor" stroke="none" /></svg></summary><dl className="review-technical-popover"><div><dt>Source</dt><dd>{value.source}</dd></div><div><dt title="Timestamp reported by this stored source; individual PR facts can have different ages.">Snapshot origin timestamp</dt><dd><code>{value.observed_at}</code></dd></div><div><dt title="When this source observation was retained by FireController.">Saved locally</dt><dd><code>{value.recorded_at}</code></dd></div><div><dt>Snapshot</dt><dd><code>{value.snapshot_id}</code> · revision {value.revision}</dd></div></dl></details>;
}
type HighlightState = { fingerprint: string; version: number };
export function nextSemanticHighlight(previous: HighlightState | undefined, fingerprint: string): HighlightState {
  if (!previous) return { fingerprint, version: 0 };
  return previous.fingerprint === fingerprint ? previous : { fingerprint, version: previous.version + 1 };
}
function useSemanticHighlight(fingerprint: string, history?: Map<string, string>, identity = ''): string {
  const [state, setState] = useState<HighlightState>(() => ({ fingerprint: history?.get(identity) ?? fingerprint, version: 0 }));
  if (state.fingerprint !== fingerprint) setState(nextSemanticHighlight(state, fingerprint));
  useLayoutEffect(() => {
    if (!history) return;
    history.set(identity, fingerprint);
    if (history.size > 512) history.delete(history.keys().next().value!);
  }, [history, identity, fingerprint]);
  return state.version ? `semantic-change semantic-change-${state.version % 2}` : '';
}
export function runDisplayFingerprint(run: ReviewRunSummary, head: string): string {
  return JSON.stringify([run.channel, run.outcome, run.found, run.accepted, run.routed, run.fixed,
    run.complete, run.finalized, run.attributable, run.finished_at || run.started_at,
    !head || !run.head_sha ? 'unknown' : head === run.head_sha ? 'same' : 'different']);
}
export function pullDisplayFingerprint(pull: ReviewPullSummary): string {
  return JSON.stringify([pull.number, pull.title, pull.state, pull.ci_state, pull.mergeability, pull.merge_status,
    pull.diff, pull.is_draft, pull.merged_at, pull.complete, pull.head_sha, pull.run_count, [pull.run_counts.hosted, pull.run_counts.cli, pull.run_counts.subagent, pull.run_counts.manual], pull.attempt_count,
    pull.latest_attempts.map(attempt => [attempt.channel, attempt.state, attempt.reason, attempt.finished_at || attempt.started_at]),
    pull.recent_runs.map(run => runDisplayFingerprint(run, pull.head_sha))]);
}
export function findingDisplayFingerprint(finding: ReviewFinding): string {
  const route = finding.route;
  return JSON.stringify([finding.title, finding.detail, finding.severity, finding.disposition,
    finding.decision_reason, finding.actor, finding.decided_at,
    route ? [route.target_pull_number, route.disposition, route.state, route.reason, route.actor, route.decided_at] : null,
    route?.fix_sha ?? finding.fix_sha, route?.proof_note ?? finding.proof_note, route?.fixed_at ?? finding.resolved_at]);
}
function RunPill({ run, snapshot, head }: { run: ReviewRunSummary; snapshot: string; head: string }) {
  const { home } = useWorkspace();
  const highlight = useSemanticHighlight(runDisplayFingerprint(run, head));
  return <Link className={`review-round review-outcome-${run.outcome} ${highlight}`} to={reviewLink(home, 'runs', run.id, snapshot)} aria-label={`Open ${run.channel} run ${run.source_run_id}`}><span className="review-channel">{run.channel}</span><strong>{run.found}<small>found</small></strong><span>{reviewLabel(run.outcome)}</span><small>{run.accepted} accepted · {run.routed} routed · {run.fixed} fixed</small>{!run.complete && <span className="review-warning">Incomplete record</span>}{!run.finalized && <span className="review-warning">Counts not finalized</span>}{!run.attributable && <span className="review-warning">Attribution unconfirmed</span>}</Link>;
}
export function missingPullFacts(pull: ReviewPullSummary): string[] {
  const missing: string[] = [];
  if (!pull.head_sha) missing.push('Head commit was not captured.');
  if (!pull.base_ref) missing.push('Base branch was not captured.');
  if (!pull.base_sha) missing.push('Base commit was not captured.');
  if (pull.state === 'unknown') missing.push('Open or closed state is unknown.');
  if (!pull.complete && !missing.length && pull.ci_state !== 'unknown' && (pull.state === 'merged' || pull.mergeability !== 'unknown')) missing.push('The source marked this PR incomplete without identifying another missing field.');
  return missing;
}
function ReviewObservationBadge({ value }: { value: ReviewPullSummary['observation'] }) {
  if (!value || ['current', 'not_enrolled', 'waiting'].includes(value.state)) return null;
  const hostedOnly = value.state === 'retrying' && value.error === 'external_hosted_identity_unproven' && value.hosted_error === value.error;
  const checked = value.last_success_at ? time(value.last_success_at) : 'not yet checked by this service process';
  const title = hostedOnly
    ? `Some Hosted review history could not yet be imported. Last successful PR check: ${checked}.`
    : `GitHub observation: ${reviewLabel(value.state)}${value.error ? ` · ${reviewLabel(value.error)}` : ''}. Last successful check: ${checked}.`;
  return <span className="badge neutral" title={title}>{hostedOnly ? 'Hosted history: Incomplete' : `GitHub: ${reviewLabel(value.state)}`}</span>;
}
export function ReviewMergeStatus({pull}: {pull:ReviewPullSummary}) {
  if (pull.state === 'merged' || pull.state === 'closed') return null;
  const status=pull.merge_status ?? {state:'unknown' as const,reason:'merge_state_unknown'};
  const labels={ready:'Ready',blocked:'Blocked',pending:'Waiting',unknown:'Unknown'};
  const colours={ready:'mergeable',blocked:'conflicting',pending:'pending',unknown:'unknown'};
  const reasons:Record<string,string>={pull_state_unknown:'PR state is unknown.',draft:'Draft PR.',merge_conflicts:'Merge conflicts must be resolved.',required_ci_failed:'Required CI is failing.',github_blocked:'GitHub blocks merging under the current branch requirements.',github_behind:'GitHub reports the branch is behind its required base.',github_draft:'GitHub reports a draft PR.',required_ci_pending:'Required CI is still pending.',required_ci_unknown:'Required CI has not been established for this comparison.',merge_state_unknown:'GitHub merge eligibility has not been established for this comparison.',github_and_required_ci_clear:'GitHub merge requirements and required CI are clear. Review work remains shown separately.'};
  return <span title={reasons[status.reason]} className={`badge review-fact-pill review-mergeability review-fact-${colours[status.state]}`}>Merge: {labels[status.state]}</span>;
}
export function ReviewPullFacts({ pull, status }: { pull: ReviewPullSummary; status?: ReviewQueueBadge }) {
  const missing = missingPullFacts(pull);
  return <div className="review-pull-facts">{pull.state === 'merged' || pull.state === 'closed' ? <span className={`badge review-state-${pull.state}`}>{reviewLabel(pull.state)}</span> : pull.state === 'open' && status ? <span className={`badge review-position-${status.kind}`} title={status.description}>{status.text}</span> : null}{pull.is_draft === true && pull.state === 'open' && <span className="badge review-state-draft">Draft</span>}{pull.state === 'merged' && pull.merged_at && <strong className="review-merged-age"><RelativeTime value={pull.merged_at} /></strong>}{pull.diff && <span className="badge review-fact-pill review-diff">{pull.diff.files.toLocaleString('en-US')} files · <strong className="review-additions">+{pull.diff.additions.toLocaleString('en-US')}</strong> / <strong className="review-deletions">−{pull.diff.deletions.toLocaleString('en-US')}</strong> lines</span>}{pull.state !== 'merged' && <span title={pull.ci_state === "not_required" ? "No CI checks are required for this PR’s selected base branch. Optional workflow results are separate." : pull.observation?.ci_checked_at ? `Required checks for the selected base last checked ${time(pull.observation.ci_checked_at)}${pull.observation.ci_error ? ` · ${reviewLabel(pull.observation.ci_error)}` : ''}` : "Required checks for the selected base branch; optional workflow health is separate."} className={`badge review-fact-pill review-ci review-fact-${pull.ci_state}`}>Required CI: {pull.ci_state === "not_required" ? "None" : reviewLabel(pull.ci_state)}</span>}<ReviewMergeStatus pull={pull} /><ReviewObservationBadge value={pull.observation} />{missing.length > 0 && <details className="review-missing-facts"><summary>Missing PR details</summary><ul>{missing.map(detail => <li key={detail}>{detail}</li>)}</ul></details>}</div>;
}
// Global stored positions preserve gaps in filtered pages; legacy pages require known predecessors.
export function reviewQueueDistances(items: ReviewPullSummary[], firstPage: boolean, headPosition?: number): (number | undefined)[] {
  if (headPosition !== undefined || items.some(item => item.queue_position !== undefined)) {
    return items.map(item => headPosition === undefined || item.queue_position === undefined ? undefined : item.queue_position < headPosition ? item.queue_position - headPosition : item.queue_position - headPosition + 1);
  }
  let head: number | undefined;
  if (firstPage) for (let index = 0; index < items.length; index++) {
    if (items[index].state === 'open') { head = index; break; }
    if (items[index].state !== 'merged' && items[index].state !== 'closed') break;
  }
  return items.map((_, index) => head === undefined ? undefined : index < head ? index - head : index - head + 1);
}
export function formatQueueDistance(value: number): string {
  return `${value < 0 ? '−' : '+'}${String(Math.abs(value)).padStart(2, '0')}`;
}
export function groupReviewQueue(items: ReviewPullSummary[], groups: ReviewGroup[]) {
  const titles = new Map<number, string>();
  for (const group of groups) for (const number of group.pull_numbers) if (!titles.has(number)) titles.set(number, group.title);
  const sections: { title?: string; rows: { pull: ReviewPullSummary; index: number }[] }[] = [];
  items.forEach((pull, index) => {
    const title = titles.get(pull.number), previous = sections.at(-1);
    if (previous && previous.title === title) previous.rows.push({ pull, index });
    else sections.push({ title, rows: [{ pull, index }] });
  });
  const occurrences = new Map<string | undefined, number>();
  return sections.map(section => {
    const occurrence = occurrences.get(section.title) ?? 0;
    occurrences.set(section.title, occurrence + 1);
    return { ...section, key: JSON.stringify([section.title ?? null, occurrence]) };
  });
}
export function ReviewDetailsLink({ number, snapshot }: { number: number; snapshot: string }) {
  const { home } = useWorkspace();
  return <Link className="review-details-link" to={reviewLink(home, 'pulls', number, snapshot)} aria-label={`Review details for PR #${number}`}>Review details ↗</Link>;
}
export function ReviewQueueRow({ pull, snapshot, order, progress, fronts, channels, footer, anchor = true }: { pull: ReviewPullSummary; snapshot: string; order?: { distance?: number; index: number }; progress?: ReviewProgressItem; fronts?: ReviewFronts; channels?: ('hosted'|'cli')[]; footer?: ReactNode; anchor?: boolean }) {
  const { session } = useWorkspace();
  const highlight = useSemanticHighlight(pullDisplayFingerprint(pull));
  const external = githubLink(session.project, `pull/${pull.number}`);
  const position = order && (order.distance === undefined ? String(order.index + 1).padStart(2, '0') : formatQueueDistance(order.distance));
  const front = order?.distance === 1;
  const badges = reviewQueueBadges(pull,snapshot,fronts,progress,channels);
  return <ReviewRowDisplay id={anchor ? `review-pr-${pull.number}` : undefined} lifecycle={pull.state} rail={!!order} position={position} positionLabel={order && (order.distance === undefined ? `Stored order ${position} on this page` : `Queue distance ${position}${front ? ': first open PR' : ''}`)} number={pull.number} title={pull.title} url={external} className={`${front ? 'review-row-front ' : ''}${badges[0] ? `review-status-${badges[0].kind} ` : ''}${highlight}`} facts={<ReviewPullFacts pull={pull} status={badges[0]} />} footer={footer === undefined ? <ReviewDetailsLink number={pull.number} snapshot={snapshot} /> : footer}><ReviewRunsView readingPrefix={anchor ? `review-pr-${pull.number}` : undefined} number={pull.number} snapshot={snapshot} head={pull.head_sha} attempts={pull.latest_attempts} expected={pull.run_count} channelTotals={pull.run_counts} progress={progress} positions={{hosted:reviewChannelPosition(pull,snapshot,'hosted',fronts,progress),cli:reviewChannelPosition(pull,snapshot,'cli',fronts,progress)}} /></ReviewRowDisplay>;
}
export function reviewQueueHashTarget(items: ReviewPullSummary[] | undefined, hash: string): string | undefined {
  const pull = items?.find(item => hash === `#review-pr-${item.number}`);
  return pull ? `review-pr-${pull.number}` : undefined;
}
export function reviewQueueQuery(session: Session, key: readonly string[], view: 'recent' | 'merged', cursor = '', snapshot = 'current') {
  return { queryKey: [...key, 'reviews', snapshot, 'queue', view, cursor], queryFn: ({ signal }: { signal: AbortSignal }) => session.api.reviewQueue({ limit: pageSize, view, ...(cursor ? { cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity, refetchInterval: 60_000 };
}
export function ReviewQueueView({ embedded = false, history = false }: { embedded?: boolean; history?: boolean }) {
  // Each view has its own opaque cursor/snapshot lifetime, including navigation
  // between routes that reuse this component type.
  return <ReviewQueueWindow key={history ? 'merged' : 'recent'} embedded={embedded} history={history} />;
}
function ReviewQueueWindow({ embedded, history }: { embedded: boolean; history: boolean }) {
  const label = useProjectHeading('reviews');
  const location = useLocation();
  const lastJump = useRef('');
  const restoringPosition = useContext(RestoringReadingPosition);
  const groups = usePresentation()?.review_groups ?? [];
  const { session, key, home } = useWorkspace();
  const paging = usePaging();
  const [continuationSnapshot, setContinuationSnapshot] = useState('current');
  const query = useQuery(reviewQueueQuery(session, key, history ? 'merged' : 'recent', paging.cursor, paging.number === 1 ? 'current' : continuationSnapshot));
  const page = query.data;
  const progress = useReviewProgress(history ? [] : page?.items.map(pull => pull.number) ?? []);
  const fronts = useReviewFronts(!history && !!page?.configured);
  const currentFronts = !history && page ? currentReviewFronts(page.snapshot_id, page.items, fronts.error ? undefined : fronts.data) : undefined;
  const channels = !history && fronts.data?.configured ? fronts.data.channels.map(channel=>channel.channel) : undefined;
  const distances = page ? reviewQueueDistances(page.items, paging.number === 1, page.head_position) : [];
  const sections = page ? groupReviewQueue(page.items, groups) : [];
  useEffect(() => {
    const target = reviewQueueHashTarget(page?.configured ? page.items : undefined, location.hash);
    if (!target || restoringPosition) return;
    const jump = `${location.key}:${target}`;
    if (lastJump.current === jump) return;
    const frame = requestAnimationFrame(() => {
      const row = document.getElementById(target);
      if (row) { scrollToReadingTarget(row); lastJump.current = jump; }
    });
    return () => cancelAnimationFrame(frame);
  }, [page, location.hash, location.key, restoringPosition]);
  const navigation = { ...paging, next: (cursor: string) => { if (page) setContinuationSnapshot(page.snapshot_id); paging.next(cursor); } };
  return <section id="review-queue" data-reading-region="reviews" data-reading-ready={!query.isPending} className={embedded ? 'delivery-reviews' : ''} aria-labelledby="review-queue-title"><div className="delivery-section-heading"><h2 id="review-queue-title">{history ? 'Queue history' : label}</h2><div className="review-queue-tools">{history ? <Link className="review-details-link review-queue-history-link" to={`${home}/reviews`}>Back to review queue ↗</Link> : <Link className="review-details-link review-queue-history-link" to={`${home}/reviews/history`}>Queue history ↗</Link>}{page?.configured && <ReviewQueueTechnicalDetails value={page} />}</div></div><div className={embedded ? undefined : 'panel review-page-body'}><ReadState error={query.error} loading={query.isPending} empty={false} retained={!!page}>{page && (page.configured ? <><ReviewNotice history={history} />{!history && fronts.error && <p className="notice">Review position could not be loaded.</p>}{!history && progress.error && <p className="notice">Current round limits could not be loaded. Retained review history remains available.</p>}<section className={embedded ? 'panel review-queue-panel' : 'review-queue-panel'}>{sections.map((section, sectionIndex) => <section key={section.key} className={`review-queue-group${section.title ? '' : ' review-queue-ungrouped'}`} {...(section.title ? { 'aria-labelledby': `review-group-${sectionIndex}` } : {})}>{section.title && <h3 id={`review-group-${sectionIndex}`} className="review-queue-group-title">{section.title}</h3>}<ul className="review-queue">{section.rows.map(({ pull, index }) => <ReviewQueueRow key={pull.number} pull={pull} snapshot={page.snapshot_id} order={{distance: distances[index], index}} progress={history ? undefined : progress.data?.items.find(item => item.pull_number === pull.number)} fronts={currentFronts} channels={channels} />)}</ul></section>)}{page.items.length === 0 && <p className="empty">{history ? 'No merged pull requests in queue history.' : 'No pull requests in the review queue.'}</p>}{(page.has_more || paging.number > 1) && <Pager paging={navigation} page={page} label={history ? 'queue history' : 'review queue'} busy={query.isFetching} />}</section></> : <section className="panel empty"><h3>No review queue yet</h3></section>)}</ReadState></div></section>;
}
function useReviewArchive<T extends ReviewRunSummary | ReviewAttemptItem>(feed: 'runs' | 'attempts', number: number, snapshot: string, expected: number, channel?: ReviewChannel, limit = pageSize, view?: 'timeline') {
  const { session, key } = useWorkspace();
  const read = useCallback((cursor: string | undefined, signal: AbortSignal) => {
    const options = { snapshot, limit, ...(view ? { view } : {}), ...(channel ? { channel } : {}), ...(cursor ? { cursor } : {}) };
    return (feed === 'runs' ? session.api.reviewRuns(number, options, signalFor(session, signal)) : session.api.reviewAttempts(number, options, signalFor(session, signal))) as Promise<ReviewArchivePage<T>>;
  }, [session.api, session.controller, feed, number, snapshot, channel, limit, view]);
  const scope = JSON.stringify([key, number, feed, channel, limit, view]);
  const readCurrent = useRef(read);
  useLayoutEffect(() => { readCurrent.current = read; }, [read]);
  const archive = useMemo(() => new ReviewArchive<T>((cursor, signal) => readCurrent.current(cursor, signal), item => 'id' in item ? item.id : JSON.stringify([item.source, item.source_attempt_id])), [scope]);
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot, 'pull', number, feed, ...(channel ? [channel] : []), ...(view ? [view] : []), 'archive-first', limit], queryFn: ({ signal }) => read(undefined, signal), enabled: expected > 0, staleTime: Infinity });
  const first = useRetainedRead(scope, query.data, expected > 0);
  const missing = useMemo(() => expected > 0 && first?.items.length === 0 ? new ApiError('invalid_response', 'The retained review history is missing its expected rounds.') : undefined, [expected, first]);
  const error = query.error ?? missing;
  const state = useSyncExternalStore(archive.subscribe, archive.snapshot, archive.snapshot);
  const loadingFirst = expected > 0 && !first && query.isPending;
  useLayoutEffect(() => { archive.reset(first, loadingFirst); }, [archive, first, loadingFirst]);
  useLayoutEffect(() => { const cancel = () => archive.cancel(); session.controller.signal.addEventListener('abort', cancel, { once: true }); return () => { session.controller.signal.removeEventListener('abort', cancel); cancel(); }; }, [archive, session.controller]);
  const current = !first || first.snapshot_id === snapshot;
  const displayedExpected = useRetainedRead(`${scope}:count`, current ? expected : undefined, expected > 0) ?? expected;
  return { ...state, expected: displayedExpected, snapshot: state.snapshot ?? first?.snapshot_id ?? snapshot, busy: state.busy || query.isFetching || !current, error: error ?? state.error, loading: expected > 0 && !first && query.isPending && state.items.length === 0, more: () => { if (!query.isFetching && current) void archive.more(); } };
}
function ArchiveProgress({ shown, expected, snapshot, label, hasMore, busy, more }: { shown: number; expected: number; snapshot: string; label: string; hasMore: boolean; busy: boolean; more(): void }) {
  if (!hasMore && shown === expected) return null;
  return <div className="pager"><span>{shown} of {expected} {label}{!hasMore && <small>History count changed; refresh to reload.</small>}</span>{hasMore && <button type="button" onClick={more} disabled={busy}>Load more {label}</button>}</div>;
}
export function ReviewAttempts({ number, snapshot, expected: latestExpected }: { number: number; snapshot: string; expected: number }) {
  const archive = useReviewArchive<ReviewAttemptItem>('attempts', number, snapshot, latestExpected);
  const expected = archive.expected;
  return <section className="panel review-attempts" aria-label={`PR #${number} attempts`}><details><summary>Attempts ({expected})</summary>{!!archive.error && <ReadState error={archive.error} loading={false} empty={false}>{null}</ReadState>}{archive.loading ? <PageSkeleton rows={2} label="Loading retained attempts" /> : archive.items.length === 0 ? <p className="review-empty">{expected === 0 ? 'No attempts recorded.' : 'No attempts loaded.'}</p> : <ul>{archive.items.map(attempt => <li key={JSON.stringify([attempt.source, attempt.source_attempt_id])}><div><span className="review-channel">{attempt.channel}</span><strong>{reviewLabel(attempt.state)}</strong>{reviewAttemptReason(attempt) && <><small>Reason</small><span title={attempt.reason}>{reviewAttemptReason(attempt)}</span></>}</div><div><small>Original source</small><span>{attempt.source}</span></div><div><small>Started</small>{attempt.started_at ? <RelativeTime value={attempt.started_at} /> : 'Unknown'}</div><div><small>Duration</small>{attempt.duration_seconds === null ? 'Unknown' : `${attempt.duration_seconds}s`}</div><div><small>Head</small><code>{attempt.head_sha || 'Unknown'}</code></div><div><small>Source attempt / run</small><span>{attempt.source_attempt_id}</span><span>{attempt.source_run_id || 'No run attached'}</span></div></li>)}</ul>}<ArchiveProgress shown={archive.items.length} expected={expected} snapshot={snapshot} label="attempts" hasMore={archive.hasMore} busy={archive.busy} more={archive.more} /></details></section>;
}
export function ReviewPullView() {
  const { number: rawNumber = '' } = useParams();
  const number = /^[1-9][0-9]*$/u.test(rawNumber) ? Number(rawNumber) : NaN;
  const snapshot = useReviewSnapshot();
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot ?? 'current', 'pull', number], queryFn: ({ signal }) => session.api.reviewPull(number, snapshot, signalFor(session, signal)), staleTime: Infinity });
  const value = query.data;
  return <article className="review-detail-page"><Link className="back-link" to={`${home}/reviews#review-pr-${number}`}>← Review queue</Link><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <>
    <header className="review-detail-heading"><div className="review-detail-title"><a className="review-detail-number" href={githubLink(session.project, `pull/${value.pull.number}`)} target="_blank" rel="noopener noreferrer">#{value.pull.number}</a><h2>{value.pull.title}</h2></div><ReviewPullFacts pull={value.pull} /></header>
    <ReviewObligations number={number} />
    <ReviewRoundHistory key={number} number={number} snapshot={value.snapshot_id} head={value.pull.head_sha} expected={value.pull.run_count} channelTotals={value.pull.run_counts} />
    <footer className="review-detail-technical"><details className="review-comparison-details"><summary>Comparison and source details</summary><dl className="review-ref-grid"><div><dt>Observed head</dt><dd><code>{value.pull.head_sha || 'Unknown'}</code></dd></div><div><dt>Base</dt><dd>{value.pull.base_ref || 'Unknown'}<code>{value.pull.base_sha}</code></dd></div><div><dt>Source</dt><dd>{value.source}</dd></div><div><dt>Snapshot origin timestamp</dt><dd>{time(value.observed_at)}</dd></div><div><dt>Recorded</dt><dd>{time(value.recorded_at)}</dd></div><div><dt>Snapshot</dt><dd><code>{value.snapshot_id}</code> · revision {value.revision}</dd></div></dl></details><ReviewAttempts key={`attempts:${number}`} number={number} snapshot={value.snapshot_id} expected={value.pull.attempt_count} /></footer>
  </>}</ReadState></article>;
}
function CompactRunPill({ run, snapshot, head, highlights }: { run: ReviewRunSummary; snapshot: string; head: string; highlights: Map<string, string> }) {
  const { home } = useWorkspace();
  const date = run.finished_at || run.started_at;
  const highlight = useSemanticHighlight(runDisplayFingerprint(run, head), highlights, JSON.stringify([run.source, run.source_run_id]));
  const warnings = [
    !run.complete && 'Incomplete record', !run.finalized && 'Counts not finalized',
    !run.attributable && 'Attribution unconfirmed',
    (!head || !run.head_sha) && 'Head association unknown',
  ].filter(Boolean).join('. ');
  const description = `${run.found}/${run.accepted}/${run.routed} (found / accepted / routed). ${reviewLabel(run.outcome)}. ${date ? `${run.finished_at ? 'Finished' : 'Started'} ${time(date)} (${date})` : 'Date unknown'}.${warnings ? ` ${warnings}.` : ''}`;
  return <ReviewRoundPillDisplay found={run.found} accepted={run.accepted} routed={run.routed} className={`review-pill-${run.outcome} ${highlight}`} zeroAccepted={run.outcome === 'completed' && run.accepted === 0} title={description} label={`Open ${run.channel} run ${run.source_run_id}: ${description}`} wrap={content => <Link {...content} to={reviewLink(home, 'runs', run.id, snapshot)} />}>{run.outcome !== 'completed' && <span className="review-pill-state">{reviewLabel(run.outcome)}</span>}{date ? <RelativeTime value={date} /> : <span className="review-pill-age">Date unknown</span>}</ReviewRoundPillDisplay>;
}
function ReviewLane({ channel, title, runs, attempts, snapshot, head, loading, failed, partial, highlights, compact = false, progress, total, positionHint, readingAnchor, updateFailed }: { updateFailed?: boolean; readingAnchor?: string; channel: ReviewChannel; title: string; runs: ReviewRunSummary[]; attempts: ReviewAttemptSummary[]; snapshot: string; head: string; loading: boolean; failed: boolean; partial: boolean; highlights: Map<string, string>; compact?: boolean; progress?: ReviewRoundProgress; total: number; positionHint?: ReviewChannelPosition }) {
  const laneRuns = runs.filter(run => run.channel === channel);
  const attempt = [...attempts].reverse().find(item => item.channel === channel);
  const attemptLabel = attempt && ({ preparing: 'Active', prepared: 'Active', posting: 'Active', active: 'Active', rate_limited: 'Cooldown', held: 'Held', failed: 'Failed', uncertain: 'Uncertain', cancelled: 'Cancelled' } as Partial<Record<ReviewAttemptSummary['state'], string>>)[attempt.state];
  const reviewing = attempts.some(item => item.channel === channel && !item.finished_at && ['preparing', 'prepared', 'posting', 'active'].includes(item.state));
  const position = reviewing ? {text:'Reviewing'} : (progress?.pending ?? 0) > 0 ? {text:progress?.pending_adjudication && !progress.pending_execution ? 'Needs adjudication' : 'Pending'} : positionHint;
  const reason = attempt && reviewAttemptReason(attempt);
  const attemptDescription = attemptLabel + (reason ? ` · ${reason}` : '');
  return <ReviewLaneDisplay readingAnchor={readingAnchor} title={title} status={<>{updateFailed && <span className="badge neutral" title="The latest round history could not be loaded. The previous snapshot remains displayed; refresh to retry.">Update failed</span>}{position && <span className="badge review-channel-position" title={position.description}>{position.text}</span>}{attemptLabel && !reviewing && <span className={`review-attempt-state review-attempt-${attempt!.state}`} title={`Latest retained attempt: ${attemptDescription}`} aria-label={`Latest retained ${channel} attempt: ${attemptDescription}`}>{attemptLabel}</span>}</>} metadata={`${total} ${total === 1 ? 'run' : 'runs'}${compact && laneRuns.length > 3 ? ' · latest 3' : ''}`} metadataTitle="Completed PR review progress survives later Git changes; original reviewed commits remain in run details"><ReviewProgressView value={progress} />{loading ? <ReviewRoundsSkeleton count={total} /> : failed ? <p className="review-lane-empty">Round history could not be loaded.</p> : laneRuns.length ? <div className={compact ? 'review-round-pills' : 'review-round-pills review-recent-rounds'}>{laneRuns.slice(compact ? -3 : -6).map(run => <CompactRunPill key={JSON.stringify([run.source, run.source_run_id])} run={run} snapshot={snapshot} head={head} highlights={highlights} />)}</div> : <p className="review-lane-empty">{total > 0 ? 'Round history could not be loaded.' : 'No rounds recorded. Coverage is unknown.'}</p>}</ReviewLaneDisplay>;
}
function ChannelReviewLane({ number, ...props }: Omit<Parameters<typeof ReviewLane>[0], 'runs' | 'loading' | 'failed' | 'partial' | 'highlights'> & { number: number }) {
  // Hero and queue share the same six newest channel summaries.
  // Finding bodies stay behind deliberate run expansion/detail reads.
  const archive = useReviewArchive<ReviewRunSummary>('runs', number, props.snapshot, props.total, props.channel, 6);
  const highlights = useRef(new Map<string, string>()).current;
  const runs = [...archive.items].reverse();
  if (terminal(archive.error)) return <ReadState error={archive.error} loading={false} empty={false}>{null}</ReadState>;
  return <ReviewLane {...props} total={archive.expected} snapshot={archive.snapshot} updateFailed={!!archive.error && runs.length > 0} runs={runs} loading={archive.loading} failed={!!archive.error && runs.length === 0} partial={archive.hasMore || runs.length !== props.total} highlights={highlights} />;
}
export function ReviewRunsView({ number, snapshot, head, attempts, detail = false, compact = false, progress, channelTotals, positions = {}, readingPrefix }: { readingPrefix?: string; number: number; snapshot: string; head: string; attempts: ReviewAttemptSummary[]; expected: number; detail?: boolean; compact?: boolean; progress?: ReviewProgressItem; channelTotals: ReviewRunCounts; positions?: Partial<Record<ReviewChannel, ReviewChannelPosition>> }) {
  const lane = (channel: ReviewChannel, title: string) => <ChannelReviewLane key={channel} readingAnchor={readingPrefix ? `${readingPrefix}-${channel}` : undefined} number={number} channel={channel} title={title} attempts={attempts} snapshot={snapshot} head={head} compact={compact} total={channelTotals[channel]} progress={progress?.channels.find(item => item.channel === channel)} positionHint={positions[channel]} />;
  const otherChannels = ([['manual', 'Manual reviews'], ['subagent', 'Subagent Reviews']] as const).filter(([channel]) => channelTotals[channel] > 0 || attempts.some(attempt => attempt.channel === channel));
  return <section className={compact ? 'review-history delivery-head-history' : detail ? 'panel review-history' : 'review-history'} aria-label={`PR #${number} review rounds`}>{detail && <div className="section-heading"><h3>Review rounds</h3><span className="small-label">Shown rounds · oldest to newest per channel</span></div>}<div className="review-lanes">{lane('hosted', 'Hosted CodeRabbit')}{lane('cli', 'CLI CodeRabbit')}</div>{!compact && otherChannels.length > 0 && <section className="review-other-history"><h4>Other retained reviews</h4><div className="review-lanes">{otherChannels.map(([channel, title]) => lane(channel, title))}</div></section>}</section>;
}

export function InlineReviewFindings({ number, run, snapshot }: { number: number; run: ReviewRunSummary; snapshot: string }) {
  const { session, key } = useWorkspace();
  const query = useQuery({
    queryKey: [...key, 'reviews', snapshot, 'inline-run', run.id],
    queryFn: async ({ signal }) => {
      const value = await session.api.reviewRun(run.id, snapshot, signalFor(session, signal));
      if (value.pull_number !== number) throw new ApiError('invalid_response', 'The retained review run belongs to another pull request.');
      return value;
    },
    staleTime: Infinity, gcTime: 0, refetchOnMount: 'always', retry: false,
  });
  const value = query.data;
  return <div className="review-history-findings"><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <>
    <p className="review-history-finding-count">{value.findings.length} recorded / {value.run.found} reported findings</p>
    <ReviewCoverageLimits limits={value.run.coverage_limits} />
    {value.findings.length ? <ul>{value.findings.map(finding => <ReviewFindingView key={finding.source_finding_id} finding={finding} />)}</ul> : <p className="review-empty">{value.run.found === 0 ? 'No findings reported for this run.' : 'No findings recorded in this source snapshot.'}{!value.run.complete && ' The source record is incomplete.'}</p>}
  </>}</ReadState>{!!query.error && !terminal(query.error) && <button type="button" onClick={() => { void query.refetch(); }} disabled={query.isFetching}>Retry findings</button>}</div>;
}
export function reviewDuration(run: ReviewRunSummary): string | undefined {
  if (!run.started_at || !run.finished_at) return undefined;
  const seconds = Math.floor((Date.parse(run.finished_at) - Date.parse(run.started_at)) / 1000);
  if (!Number.isFinite(seconds) || seconds < 0) return undefined;
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor(seconds % 3600 / 60)}m`;
}
export function ReviewAcceptedCounts({ run }: { run: ReviewRunSummary }) {
  const counts = run.accepted_severity;
  if (!counts?.complete) return <span className="review-count review-count-accepted" title="Accepted severity breakdown unavailable for this retained record">Accepted: <strong>{run.accepted}</strong></span>;
  return <span className="review-count review-count-accepted" title={`${run.accepted} accepted findings · Major / Minor / Trivial`}>
    Accepted: <strong>{counts.major}/{counts.minor}/{counts.trivial}</strong>
    {counts.critical > 0 && <> · {counts.critical} Critical</>}
    {counts.unknown > 0 && <> · {counts.unknown} severity unknown</>}
  </span>;
}
export function ReviewHistoryRow({ number, run, snapshot, head, ordinal }: { number: number; run: ReviewRunSummary; snapshot: string; head: string; ordinal?: number }) {
  const { home } = useWorkspace();
  const [open, setOpen] = useState(false);
  const title = run.channel === 'hosted' ? 'CodeRabbit Hosted' : run.channel === 'cli' ? 'CodeRabbit CLI' : run.channel === 'subagent' ? 'Subagent' : 'Manual';
  const round = `${title}${ordinal ? ` #${ordinal}` : ''}`;
  const duration = reviewDuration(run);
  const findingsID = `review-history-findings-${run.id}`;
  return <li className={`review-history-row review-history-row-${run.channel}`} data-run-id={run.id}>
    <div className="review-history-row-heading"><button className="review-history-toggle" type="button" aria-expanded={open} aria-controls={findingsID} aria-label={`${open ? 'Hide' : 'Show'} findings for ${round}`} onClick={() => setOpen(value => !value)}>
      <span className={`review-history-kind review-history-kind-${run.channel}`}>{round}</span>
      <span className="review-history-age">{run.finished_at ? <RelativeTime value={run.finished_at} /> : run.started_at ? <>Started <RelativeTime value={run.started_at} /></> : 'Date unknown'}{duration && <span className="review-history-duration"> · {duration}</span>}</span>
      <span className="review-history-counts"><span className="review-count review-count-found">Found: <strong>{run.found}</strong></span><ReviewAcceptedCounts run={run} /><span className="review-count review-count-routed">Routed: <strong>{run.routed}</strong></span></span>
      <span className="review-history-disclosure" aria-hidden="true">{open ? '▾' : '▸'}</span>
    </button><Link className="review-history-details-link" to={reviewLink(home, 'runs', run.id, snapshot)} aria-label={`Open ${round} run details`}>Run details ↗</Link></div>
    <div className="review-history-flags">{run.outcome !== 'completed' && <span>{reviewLabel(run.outcome)}</span>}{!run.complete && <span className="review-warning">Incomplete record</span>}{!run.finalized && <span className="review-warning">Counts not finalized</span>}{!run.attributable && <span className="review-warning">Attribution unconfirmed</span>}{!head || !run.head_sha ? <span className="review-warning">Head association unknown</span> : null}</div>
    {open && <div id={findingsID}><InlineReviewFindings number={number} run={run} snapshot={snapshot} /></div>}
  </li>;
}
// The archive supplies newest-first, snapshot-bound chronological pages. Reversing
// the loaded prefix displays oldest-to-newest without re-sorting partial pages.
export function ReviewHistoryGroups({ number, runs, snapshot, head, loading, failed, channelTotals }: { number: number; runs: ReviewRunSummary[]; snapshot: string; head: string; partial: boolean; loading: boolean; failed: boolean; channelTotals: ReviewRunCounts }) {
  const remaining = { ...channelTotals };
  const rows = runs.map(run => ({run, ordinal: remaining[run.channel]--})).reverse();
  const total = Object.values(channelTotals).reduce((a, b) => a + b, 0);
  return <div className="review-timeline">{loading ? <PageSkeleton rows={3} label="Loading retained rounds" /> : rows.length ? <ul className="review-history-run-list">{rows.map(({run, ordinal}) => <ReviewHistoryRow key={run.id} number={number} run={run} snapshot={snapshot} head={head} ordinal={ordinal > 0 ? ordinal : undefined} />)}</ul> : <p className="review-empty">{failed || total > 0 ? 'Round history could not be loaded.' : 'No rounds recorded. Coverage is unknown.'}</p>}</div>;
}
export function ReviewRoundHistory({ number, snapshot, head, channelTotals: latestTotals, expected: latestExpected }: { number: number; snapshot: string; head: string; expected: number; channelTotals: ReviewRunCounts }) {
  const { key } = useWorkspace();
  const archive = useReviewArchive<ReviewRunSummary>('runs', number, snapshot, latestExpected, undefined, 25, 'timeline');
  const channelTotals = useRetainedRead(JSON.stringify([key, number, 'timeline-counts']), archive.snapshot === snapshot ? latestTotals : undefined) ?? latestTotals;
  const expected = archive.expected;
  if (terminal(archive.error)) return <ReadState error={archive.error} loading={false} empty={false}>{null}</ReadState>;
  return <section className="panel review-history review-round-history" aria-label={`PR #${number} review rounds`}>
    <div className="section-heading"><h3>Review rounds</h3><span className="small-label">{expected} {expected === 1 ? 'run' : 'runs'}</span></div>
    {!!archive.error && <ReadState error={archive.error} loading={false} empty={false}>{null}</ReadState>}
    {(archive.hasMore || archive.items.length > 0 && archive.items.length !== expected) && <div className="review-timeline-continuation"><span>{archive.items.length} of {expected} rounds{!archive.hasMore && ' · History count changed; refresh to reload.'}</span>{archive.hasMore && <button type="button" disabled={archive.busy} onClick={archive.more}>Load earlier rounds</button>}</div>}
    <ReviewHistoryGroups number={number} runs={archive.items} snapshot={archive.snapshot} head={head} partial={archive.hasMore} loading={archive.loading} failed={!!archive.error} channelTotals={channelTotals} />
  </section>;
}

export function ReviewFindingView({ finding }: { finding: ReviewFinding }) {
  return <CompactReviewFinding finding={finding} />;
}

function ReviewCoverageLimits({ limits }: { limits: string[] }) {
  return limits.length ? <section className="panel review-coverage"><h3>Source coverage limitations</h3><ul>{limits.map((limit, index) => <li key={index}>{limit}</li>)}</ul></section> : null;
}
function CapturedComparisonAssessment({ evidenceId }: { evidenceId: string }) {
  const [open, setOpen] = useState(false);
  const { key } = useWorkspace();
  const queryClient = useQueryClient();
  const assessmentKey = [...key, 'reviews', 'policy-assessment', evidenceId];
  function close() {
    void queryClient.cancelQueries({ queryKey: assessmentKey, exact: true });
    setOpen(false);
  }
  return <section className="panel review-assessment" aria-label="PR review policy assessment"><div className="section-heading"><div><h3>PR review policy</h3><p>Current project requirements and completed PR review progress.</p></div><button type="button" aria-expanded={open} onClick={() => open ? close() : setOpen(true)}>{open ? 'Close assessment' : 'Assess review policy'}</button></div>{open && <CapturedComparisonResult evidenceId={evidenceId} />}</section>;
}
export function CapturedComparisonRefusal({ code }: { code: 'review_held' | 'review_capacity' }) {
  return <div className="notice" role="alert">{code === 'review_held'
    ? 'The project repository no longer matches this captured comparison. Ask an administrator to verify the repository binding.'
    : 'This assessment exceeds the supported review-history capacity. Ask an operator to address the retained history limit.'}</div>;
}
function CapturedComparisonResult({ evidenceId }: { evidenceId: string }) {
  const { session, key } = useWorkspace();
  // Unmounting consumes the request signal and removes this query immediately.
  // Every explicit reopen reads current policy, rather than showing cached proof.
  const query = useQuery({ queryKey: [...key, 'reviews', 'policy-assessment', evidenceId], queryFn: ({ signal }) => session.api.reviewPolicyAssessment(evidenceId, signalFor(session, signal)), staleTime: Infinity, gcTime: 0, refetchOnMount: 'always' });
  // Durable refusals need operator action. Authorization and cancellation keep
  // their existing ReadState/session handling, independent of the refusal code.
  if (query.error instanceof ApiError && !terminal(query.error) && (query.error.code === 'review_held' || query.error.code === 'review_capacity')) return <CapturedComparisonRefusal code={query.error.code} />;
  const value = query.data;
  return <ReadState error={query.error} loading={query.isPending} empty={false}>{value && <div className="review-assessment-result">{query.isFetching && <p role="status">Refreshing assessment…</p>}<dl className="review-ref-grid"><div><dt>Assessment state</dt><dd><span className="badge neutral">{reviewLabel(value.state)}</span></dd></div><div><dt>Policy revision</dt><dd>{value.policy_revision}</dd></div><div><dt>Captured target</dt><dd>PR #{value.anchor.pull_number}<code>{value.anchor.candidate_sha}</code></dd></div><div><dt>Captured source observed</dt><dd>{time(value.anchor.observed_at)}</dd></div><div><dt>Captured comparison base</dt><dd><code>{value.anchor.base_sha}</code></dd></div><div><dt>Policy evaluated</dt><dd>{time(value.evaluated_at)}</dd></div></dl>{value.incoming_routes_capacity && <p className="notice">Incoming route inspection reached its supported capacity; policy remains held.</p>}{(value.incoming_routes_pending ?? 0) > 0 && <p className="notice">{value.incoming_routes_pending} incoming routed finding obligation(s) remain pending.</p>}{value.state === 'unconfigured' ? <p className="notice">Project review requirements are unconfigured.</p> : <div className="review-assessment-channels">{value.channels.map(channel => <section key={channel.channel} aria-label={`${channel.channel === 'hosted' ? 'Hosted' : 'CLI'} assessment`}><h4>{channel.channel === 'hosted' ? 'Hosted' : 'CLI'}</h4><dl><div><dt>Completed review</dt><dd>{channel.covered ? 'Yes' : 'No'}</dd></div><div><dt>Pending</dt><dd>{channel.pending ? 'Yes' : 'No'}</dd></div>{channel.rounds_configured && <div><dt>Review rounds</dt><dd>{channel.rounds_complete ? 'Finished' : 'More required'}</dd></div>}</dl></section>)}</div>}<p className="item-meta">Completed review progress survives later Git changes. This does not refresh GitHub, assess CI or declare merge readiness.</p></div>}</ReadState>;
}
export function ReviewRunView() {
  const { runId = '' } = useParams();
  const snapshot = useReviewSnapshot();
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot ?? 'current', 'run', runId], queryFn: ({ signal }) => session.api.reviewRun(runId, snapshot, signalFor(session, signal)), staleTime: Infinity });
  const value = query.data;
  const pull = useQuery({ queryKey: [...key, 'reviews', value?.snapshot_id ?? snapshot ?? 'current', 'pull', value?.pull_number ?? 'unknown'], queryFn: ({ signal }) => session.api.reviewPull(value!.pull_number, value!.snapshot_id, signalFor(session, signal)), enabled: !!value, staleTime: Infinity });
  return <><Link className="back-link" to={value ? reviewLink(home, 'pulls', value.pull_number, value.snapshot_id) : `${home}/reviews`}>← {value ? `PR #${value.pull_number} history` : 'Review queue'}</Link><ReviewNotice /><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <><div className="page-heading"><p className="eyebrow">PR #{value.pull_number} · {value.run.channel} run</p><h2>Review findings</h2><p>{value.run.reviewer || 'Reviewer unknown'} · {value.run.scope}</p></div><ReviewProvenance value={value} /><section className="panel review-run-summary"><RunPill run={value.run} snapshot={value.snapshot_id} head={pull.data?.pull.head_sha ?? ''} /><dl className="review-ref-grid"><div><dt>Original run source</dt><dd>{value.run.source}</dd></div><div><dt>Run head</dt><dd><code>{value.run.head_sha || 'Unknown'}</code></dd></div><div><dt>Observed PR head</dt><dd><code>{pull.data?.pull.head_sha || 'Unknown'}</code></dd></div><div><dt>Started / finished</dt><dd>{value.run.started_at ? <RelativeTime value={value.run.started_at} /> : 'Unknown'}<br />{value.run.finished_at ? <RelativeTime value={value.run.finished_at} /> : 'Unknown'}</dd></div></dl>{(!value.run.complete || !value.run.attributable || !value.run.finalized) && <p className="notice">This run has incomplete, unattributed or unfinalized source evidence. Counts are historical observations.</p>}<ReadState error={pull.error} loading={pull.isPending} empty={false}>{!value.run.head_sha || !pull.data?.pull.head_sha ? <p className="review-warning">Head association is unknown.</p> : null}</ReadState></section>{value.run.source === 'native-executor' && /^review_evidence_[a-f0-9]{32}$/u.test(value.run.source_run_id) && <CapturedComparisonAssessment key={value.run.source_run_id} evidenceId={value.run.source_run_id} />}<ReviewCoverageLimits limits={value.run.coverage_limits} /><section className="panel review-findings"><div className="section-heading"><h3>Findings ({value.findings.length} recorded / {value.run.found} reported)</h3></div>{value.findings.length ? <ul>{value.findings.map(finding => <ReviewFindingView key={finding.source_finding_id} finding={finding} />)}</ul> : <p className="empty">No findings recorded for this run.{!value.run.complete ? ' The source record is incomplete.' : ''}</p>}</section></>}</ReadState></>;
}
