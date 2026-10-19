import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react';
import { Link, useLocation, useParams, useSearchParams } from 'react-router';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from './api';
import { terminalBrowserSession } from './browser-session';
import { Pager, ReadState, signalFor, usePaging, useWorkspace } from './workspace';
import { usePresentation, useProjectHeading } from './presentation-context';
import { ReviewArchive, type ReviewArchivePage } from './review-archive';
import { PageSkeleton } from './loading';
import { RelativeTime } from './relative-time';
import { EditorialMarkdown } from './editorial-markdown';
import { githubRepositoryURL } from './github-links';
import './review-queue.css';
import './review-history.css';
import { useReviewProgress, ReviewProgressView } from './review-progress-view';
import type { ReviewProgressItem, ReviewRoundProgress } from './review-progress';
import type { Project, ReviewGroup, ReviewAttemptSummary, ReviewAttemptItem, ReviewFinding, ReviewPullSummary, ReviewRunSummary, ReviewSnapshot } from './types';

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
function ReviewNotice() {
  return <aside className="review-guide" aria-label="Reading the review queue"><h3>Review states</h3><dl><div><dt>Active</dt><dd>Attempt preparing or running</dd></div><div><dt>Cooldown</dt><dd>Provider rate limit</dd></div><div><dt>Held</dt><dd>Attempt stopped by a hold</dd></div><div><dt>Failed / Uncertain</dt><dd>Failure or unconfirmed outcome</dd></div><div><dt>Cancelled</dt><dd>Attempt cancelled</dd></div></dl><p>Attempt states describe retained history. Limits show current settings; neither grants permission to request another review.</p><p className="review-count-key"><strong>Reading rounds:</strong> found / accepted / routed. Highlighted zero-accepted rounds have no accepted findings. Merged PRs remain in the queue. Signed positions count from the first open PR; unsigned positions show order on this page.</p></aside>;
}
function ReviewProvenance({ value }: { value: ReviewSnapshot }) {
  return <div className="review-freshness"><span>As of <RelativeTime value={value.observed_at} /></span><details><summary>Source details</summary><dl><div><dt>Source</dt><dd>{value.source}</dd></div><div><dt>Observed</dt><dd>{time(value.observed_at)}</dd></div><div><dt>Recorded</dt><dd>{time(value.recorded_at)}</dd></div><div><dt>Snapshot</dt><dd><code>{value.snapshot_id}</code> · revision {value.revision}</dd></div></dl></details></div>;
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
  return JSON.stringify([pull.number, pull.title, pull.state, pull.ci_state, pull.mergeability,
    pull.complete, pull.head_sha, pull.run_count, pull.attempt_count,
    pull.latest_attempts.map(attempt => [attempt.channel, attempt.state, attempt.finished_at || attempt.started_at]),
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
  const mismatch = !!head && !!run.head_sha && head !== run.head_sha;
  const highlight = useSemanticHighlight(runDisplayFingerprint(run, head));
  return <Link className={`review-round review-outcome-${run.outcome} ${highlight}`} to={reviewLink(home, 'runs', run.id, snapshot)} aria-label={`Open ${run.channel} run ${run.source_run_id}`}><span className="review-channel">{run.channel}</span><strong>{run.found}<small>found</small></strong><span>{reviewLabel(run.outcome)}</span><small>{run.accepted} accepted · {run.routed} routed · {run.fixed} fixed</small>{!run.complete && <span className="review-warning">Incomplete record</span>}{!run.finalized && <span className="review-warning">Counts not finalized</span>}{!run.attributable && <span className="review-warning">Attribution unconfirmed</span>}{mismatch && <span className="review-warning">Different observed head</span>}</Link>;
}
export function missingPullFacts(pull: ReviewPullSummary): string[] {
  const missing: string[] = [];
  if (!pull.head_sha) missing.push('Head commit was not captured.');
  if (!pull.base_ref) missing.push('Base branch was not captured.');
  if (!pull.base_sha) missing.push('Base commit was not captured.');
  if (pull.ci_state === 'unknown') missing.push('CI status is unknown.');
  if (pull.state !== 'merged' && pull.mergeability === 'unknown') missing.push('Mergeability is unknown.');
  if (pull.state === 'unknown') missing.push('Open or closed state is unknown.');
  if (!pull.complete && !missing.length) missing.push('The source marked this PR incomplete without identifying another missing field.');
  return missing;
}
export function ReviewPullFacts({ pull }: { pull: ReviewPullSummary }) {
  const missing = missingPullFacts(pull);
  return <div className="review-pull-facts"><span className={`badge review-state-${pull.state}`}>{reviewLabel(pull.state)}</span><span>CI: {reviewLabel(pull.ci_state)}</span>{pull.state !== 'merged' && <span>Mergeability: {reviewLabel(pull.mergeability)}</span>}{missing.length > 0 && <details className="review-missing-facts"><summary>Missing PR details</summary><ul>{missing.map(detail => <li key={detail}>{detail}</li>)}</ul></details>}</div>;
}
// Only a first bounded page can establish the head from its known predecessors.
export function reviewQueueDistances(items: ReviewPullSummary[], firstPage: boolean): (number | undefined)[] {
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
  return sections;
}
export function ReviewDetailsLink({ number, snapshot }: { number: number; snapshot: string }) {
  const { home } = useWorkspace();
  return <Link className="review-details-link" to={reviewLink(home, 'pulls', number, snapshot)} aria-label={`Review details for PR #${number}`}><span>Review details</span><span className="review-link-arrow" aria-hidden="true">↗</span></Link>;
}
function ReviewQueueRow({ pull, snapshot, distance, index, progress }: { pull: ReviewPullSummary; snapshot: string; distance?: number; index: number; progress?: ReviewProgressItem }) {
  const { session } = useWorkspace();
  const highlight = useSemanticHighlight(pullDisplayFingerprint(pull));
  const external = githubLink(session.project, `pull/${pull.number}`);
  const position = distance === undefined ? String(index + 1).padStart(2, '0') : formatQueueDistance(distance);
  const front = distance === 1;
  return <li id={`review-pr-${pull.number}`} className={`review-queue-row review-row-${pull.state}${front ? ' review-row-front' : ''} ${highlight}`}><span className="review-queue-position" aria-label={distance === undefined ? `Stored order ${position} on this page` : `Queue distance ${position}${front ? ': first open PR' : ''}`}>{position}</span><div className="review-queue-content"><div className="review-pull-heading"><span className="review-number">#{pull.number}</span><h3>{external ? <a href={external} target="_blank" rel="noopener noreferrer">{pull.title}</a> : pull.title}</h3></div><ReviewPullFacts pull={pull} /><ReviewRunsView number={pull.number} snapshot={snapshot} head={pull.head_sha} attempts={pull.latest_attempts} expected={pull.run_count} progress={progress} /><ReviewDetailsLink number={pull.number} snapshot={snapshot} /></div></li>;
}
export function reviewQueueHashTarget(items: ReviewPullSummary[] | undefined, hash: string): string | undefined {
  const pull = items?.find(item => hash === `#review-pr-${item.number}`);
  return pull ? `review-pr-${pull.number}` : undefined;
}
export function ReviewQueueView({ embedded = false }: { embedded?: boolean }) {
  const label = useProjectHeading('reviews');
  const location = useLocation();
  const lastJump = useRef('');
  const groups = usePresentation()?.review_groups ?? [];
  const { session, key } = useWorkspace();
  const paging = usePaging();
  const [continuationSnapshot, setContinuationSnapshot] = useState('current');
  const query = useQuery({ queryKey: [...key, 'reviews', paging.number === 1 ? 'current' : continuationSnapshot, 'queue', paging.cursor], queryFn: ({ signal }) => session.api.reviewQueue({ limit: pageSize, ...(paging.cursor ? { cursor: paging.cursor } : {}) }, signalFor(session, signal)), staleTime: Infinity });
  const page = query.data;
  const progress = useReviewProgress(page?.items.map(pull => pull.number) ?? []);
  const distances = page ? reviewQueueDistances(page.items, paging.number === 1) : [];
  const sections = page ? groupReviewQueue(page.items, groups) : [];
  useEffect(() => {
    const target = reviewQueueHashTarget(page?.configured ? page.items : undefined, location.hash);
    if (!target) { lastJump.current = ''; return; }
    const jump = `${location.key}:${target}`;
    if (lastJump.current === jump) return;
    const frame = requestAnimationFrame(() => {
      const row = document.getElementById(target);
      if (row) { row.scrollIntoView(); lastJump.current = jump; }
    });
    return () => cancelAnimationFrame(frame);
  }, [page, location.hash, location.key]);
  const navigation = { ...paging, next: (cursor: string) => { if (page) setContinuationSnapshot(page.snapshot_id); paging.next(cursor); } };
  return <section id="review-queue" className={embedded ? 'delivery-reviews' : ''} aria-labelledby="review-queue-title"><div className="delivery-section-heading"><h2 id="review-queue-title">{label}</h2></div><ReadState error={query.error} loading={query.isPending} empty={false}>{page && (page.configured ? <><ReviewProvenance value={page} /><ReviewNotice />{progress.error && <p className="notice">Current round limits could not be loaded. Retained review history remains available.</p>}<section className="panel review-queue-panel">{sections.map((section, sectionIndex) => <section key={sectionIndex} className={`review-queue-group${section.title ? '' : ' review-queue-ungrouped'}`} {...(section.title ? { 'aria-labelledby': `review-group-${sectionIndex}` } : {})}>{section.title && <h3 id={`review-group-${sectionIndex}`} className="review-queue-group-title">{section.title}</h3>}<ul className="review-queue">{section.rows.map(({ pull, index }) => <ReviewQueueRow key={pull.number} pull={pull} snapshot={page.snapshot_id} distance={distances[index]} index={index} progress={progress.error ? undefined : progress.data?.items.find(item => item.pull_number === pull.number)} />)}</ul></section>)}{page.items.length === 0 && <p className="empty">No pull requests in the review queue.</p>}{(page.has_more || paging.number > 1) && <Pager paging={navigation} page={page} label="review queue" busy={query.isFetching} />}</section></> : <section className="panel empty"><h3>No review queue yet</h3></section>)}</ReadState></section>;
}
function useReviewArchive<T extends ReviewRunSummary | ReviewAttemptItem>(feed: 'runs' | 'attempts', number: number, snapshot: string, expected: number) {
  const { session, key } = useWorkspace();
  const read = useCallback((cursor: string | undefined, signal: AbortSignal) => {
    const options = { snapshot, limit: pageSize, ...(cursor ? { cursor } : {}) };
    return (feed === 'runs' ? session.api.reviewRuns(number, options, signalFor(session, signal)) : session.api.reviewAttempts(number, options, signalFor(session, signal))) as Promise<ReviewArchivePage<T>>;
  }, [session.api, session.controller, feed, number, snapshot]);
  const archive = useMemo(() => new ReviewArchive<T>((cursor, signal) => read(cursor, signal), item => 'id' in item ? item.id : JSON.stringify([item.source, item.source_attempt_id])), [read]);
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot, 'pull', number, feed, 'archive-first', pageSize], queryFn: ({ signal }) => read(undefined, signal), enabled: expected > 0, staleTime: Infinity });
  const state = useSyncExternalStore(archive.subscribe, archive.snapshot, archive.snapshot);
  useLayoutEffect(() => { archive.reset(expected > 0 ? query.data : undefined, expected > 0 && query.isPending, query.error); }, [archive, expected, query.data, query.dataUpdatedAt, query.error]);
  useLayoutEffect(() => { const cancel = () => archive.cancel(); session.controller.signal.addEventListener('abort', cancel, { once: true }); return () => { session.controller.signal.removeEventListener('abort', cancel); cancel(); }; }, [archive, session.controller]);
  return { ...state, busy: state.busy || query.isFetching, error: query.error ?? state.error, loading: expected > 0 && query.isPending && state.items.length === 0, more: () => { if (!query.isFetching) void archive.more(); } };
}
function ArchiveProgress({ shown, expected, snapshot, label, hasMore, busy, more }: { shown: number; expected: number; snapshot: string; label: string; hasMore: boolean; busy: boolean; more(): void }) {
  if (!hasMore && shown === expected) return null;
  return <div className="pager"><span>{shown} of {expected} {label}{!hasMore && <small>History count changed; refresh to reload.</small>}</span>{hasMore && <button type="button" onClick={more} disabled={busy}>Load more {label}</button>}</div>;
}
function ReviewAttempts({ number, snapshot, expected }: { number: number; snapshot: string; expected: number }) {
  const archive = useReviewArchive<ReviewAttemptItem>('attempts', number, snapshot, expected);
  return <section className="panel review-attempts" aria-label={`PR #${number} attempts`}><details><summary>Attempts ({expected})</summary>{!!archive.error && <ReadState error={archive.error} loading={false} empty={false}>{null}</ReadState>}{archive.loading ? <PageSkeleton rows={2} label="Loading retained attempts" /> : archive.items.length === 0 ? <p className="review-empty">{expected === 0 ? 'No attempts recorded.' : 'No attempts loaded.'}</p> : <ul>{archive.items.map(attempt => <li key={JSON.stringify([attempt.source, attempt.source_attempt_id])}><div><span className="review-channel">{attempt.channel}</span><strong>{reviewLabel(attempt.state)}</strong></div><div><small>Original source</small><span>{attempt.source}</span></div><div><small>Started</small>{attempt.started_at ? time(attempt.started_at) : 'Unknown'}</div><div><small>Duration</small>{attempt.duration_seconds === null ? 'Unknown' : `${attempt.duration_seconds}s`}</div><div><small>Head</small><code>{attempt.head_sha || 'Unknown'}</code></div><div><small>Source attempt / run</small><span>{attempt.source_attempt_id}</span><span>{attempt.source_run_id || 'No run attached'}</span></div></li>)}</ul>}<ArchiveProgress shown={archive.items.length} expected={expected} snapshot={snapshot} label="attempts" hasMore={archive.hasMore} busy={archive.busy} more={archive.more} /></details></section>;
}
export function ReviewPullView() {
  const { number: rawNumber = '' } = useParams();
  const number = /^[1-9][0-9]*$/u.test(rawNumber) ? Number(rawNumber) : NaN;
  const snapshot = useReviewSnapshot();
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot ?? 'current', 'pull', number], queryFn: ({ signal }) => session.api.reviewPull(number, snapshot, signalFor(session, signal)), staleTime: Infinity });
  const value = query.data;
  return <><Link className="back-link" to={`${home}/reviews`}>← Review queue</Link><ReviewNotice /><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <><div className="page-heading"><p className="eyebrow">Pull request #{value.pull.number}</p><h2>{value.pull.title}</h2>{githubLink(session.project, `pull/${value.pull.number}`) && <a href={githubLink(session.project, `pull/${value.pull.number}`)} target="_blank" rel="noopener noreferrer">Open PR #{value.pull.number} on GitHub ↗</a>}</div><ReviewProvenance value={value} /><section className="panel review-pull-summary"><ReviewPullFacts pull={value.pull} /><dl className="review-ref-grid"><div><dt>Observed head</dt><dd><code>{value.pull.head_sha || 'Unknown'}</code></dd></div><div><dt>Base</dt><dd>{value.pull.base_ref || 'Unknown'}<code>{value.pull.base_sha}</code></dd></div></dl></section><ReviewRoundHistory key={`${number}:${value.snapshot_id}`} number={number} snapshot={value.snapshot_id} head={value.pull.head_sha} expected={value.pull.run_count} /><ReviewAttempts key={`attempts:${number}`} number={number} snapshot={value.snapshot_id} expected={value.pull.attempt_count} /></>}</ReadState></>;
}
function CompactRunPill({ run, snapshot, head, highlights }: { run: ReviewRunSummary; snapshot: string; head: string; highlights: Map<string, string> }) {
  const { home } = useWorkspace();
  const date = run.finished_at || run.started_at;
  const highlight = useSemanticHighlight(runDisplayFingerprint(run, head), highlights, JSON.stringify([run.source, run.source_run_id]));
  const warnings = [
    !run.complete && 'Incomplete record', !run.finalized && 'Counts not finalized',
    !run.attributable && 'Attribution unconfirmed',
    (!head || !run.head_sha) ? 'Head association unknown' : head !== run.head_sha && 'Different observed head',
  ].filter(Boolean).join('. ');
  const description = `${run.found}/${run.accepted}/${run.routed} (found / accepted / routed). ${reviewLabel(run.outcome)}. ${date ? `${run.finished_at ? 'Finished' : 'Started'} ${time(date)} (${date})` : 'Date unknown'}.${warnings ? ` ${warnings}.` : ''}`;
  return <Link className={`review-round-pill review-pill-${run.outcome}${run.outcome === 'completed' && run.accepted === 0 ? ' review-pill-zero' : ''} ${highlight}`} to={reviewLink(home, 'runs', run.id, snapshot)} title={description} aria-label={`Open ${run.channel} run ${run.source_run_id}: ${description}`}><strong>{run.found}/{run.accepted}/{run.routed}</strong>{run.outcome !== 'completed' && <span className="review-pill-state">{reviewLabel(run.outcome)}</span>}{date ? <RelativeTime value={date} /> : <span className="review-pill-age">Date unknown</span>}</Link>;
}
function ReviewLane({ channel, title, runs, attempts, snapshot, head, loading, failed, partial, highlights, compact = false, progress }: { channel: string; title: string; runs: ReviewRunSummary[]; attempts: ReviewAttemptSummary[]; snapshot: string; head: string; loading: boolean; failed: boolean; partial: boolean; highlights: Map<string, string>; compact?: boolean; progress?: ReviewRoundProgress }) {
  const laneRuns = runs.filter(run => run.channel === channel);
  const attempt = [...attempts].reverse().find(item => item.channel === channel);
  const attemptLabel = attempt && ({ preparing: 'Active', prepared: 'Active', posting: 'Active', active: 'Active', rate_limited: 'Cooldown', held: 'Held', failed: 'Failed', uncertain: 'Uncertain', cancelled: 'Cancelled' } as Partial<Record<ReviewAttemptSummary['state'], string>>)[attempt.state];
  return <section className="review-lane" aria-label={title}><div className="review-lane-heading"><h4>{title}</h4><span title="Retained review runs in this channel; not current-head coverage">{loading ? '' : failed ? 'History unavailable' : `${laneRuns.length} ${laneRuns.length === 1 ? 'run' : 'runs'}${partial ? ' shown' : ''}`}</span></div><ReviewProgressView value={progress} />{attemptLabel && <span className={`review-attempt-state review-attempt-${attempt!.state}`} title={`Latest retained attempt: ${attemptLabel}`} aria-label={`Latest retained ${channel} attempt: ${attemptLabel}`}>{attemptLabel}</span>}{loading ? <PageSkeleton rows={1} label="Loading retained rounds" /> : failed ? <p className="review-lane-empty">Round history could not be loaded.</p> : laneRuns.length ? <div className="review-round-pills">{(compact ? laneRuns.slice(-1) : laneRuns).map(run => <CompactRunPill key={JSON.stringify([run.source, run.source_run_id])} run={run} snapshot={snapshot} head={head} highlights={highlights} />)}</div> : <p className="review-lane-empty">{partial ? 'No rounds in loaded pages. Coverage is unknown.' : 'No rounds recorded. Coverage is unknown.'}</p>}</section>;
}
export function ReviewRunsView({ number, snapshot, head, attempts, expected, detail = false, compact = false, progress }: { number: number; snapshot: string; head: string; attempts: ReviewAttemptSummary[]; expected: number; detail?: boolean; compact?: boolean; progress?: ReviewProgressItem }) {
  const archive = useReviewArchive<ReviewRunSummary>('runs', number, snapshot, expected);
  // Only semantic fingerprints survive snapshot transitions, never old rows.
  const highlights = useRef(new Map<string, string>()).current;
  // The selected newest pages retain producer order; reverse only the shown
  // slice for the established oldest-to-newest channel layout.
  const runs = [...archive.items].reverse();
  const lane = (channel: string, title: string) => <ReviewLane key={channel} channel={channel} title={title} runs={runs} attempts={attempts} snapshot={snapshot} head={head} loading={archive.loading} failed={!!archive.error} partial={archive.hasMore || runs.length !== expected} highlights={highlights} compact={compact} progress={progress?.channels.find(item => item.channel === channel)} />;
  const otherChannels = [['manual', 'Manual reviews'], ['subagent', 'Subagent reviews']].filter(([channel]) => runs.some(run => run.channel === channel) || attempts.some(attempt => attempt.channel === channel));
  return <section className={compact ? 'review-history delivery-head-history' : detail ? 'panel review-history' : 'review-history'} aria-label={`PR #${number} review rounds`}>{detail && <div className="section-heading"><h3>Review rounds</h3><span className="small-label">Shown rounds · oldest to newest per channel</span></div>}{!!archive.error && <ReadState error={archive.error} loading={false} empty={false}>{null}</ReadState>}<div className="review-lanes">{lane('hosted', 'Hosted CodeRabbit')}{lane('cli', 'CLI CodeRabbit')}</div>{!compact && otherChannels.length > 0 && <section className="review-other-history"><h4>Other retained reviews</h4><div className="review-lanes">{otherChannels.map(([channel, title]) => lane(channel, title))}</div></section>}{!compact && <ArchiveProgress shown={runs.length} expected={expected} snapshot={snapshot} label="review rounds" hasMore={archive.hasMore} busy={archive.busy} more={archive.more} />}</section>;
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
export function ReviewHistoryRow({ number, run, snapshot, head }: { number: number; run: ReviewRunSummary; snapshot: string; head: string }) {
  const { home } = useWorkspace();
  const [open, setOpen] = useState(false);
  const title = run.channel === 'hosted' ? 'Hosted' : run.channel === 'cli' ? 'CLI' : reviewLabel(run.channel);
  const findingsID = `review-history-findings-${run.id}`;
  return <li className="review-history-row" data-run-id={run.id}>
    <div className="review-history-row-heading"><button className="review-history-toggle" type="button" aria-expanded={open} aria-controls={findingsID} aria-label={`${open ? 'Hide' : 'Show'} findings for ${run.channel} run ${run.source_run_id}`} onClick={() => setOpen(value => !value)}>
      <span className="review-history-kind">{title}</span><span className="review-history-counts"><strong>{run.found}</strong> found · <strong>{run.accepted}</strong> accepted · <strong>{run.routed}</strong> routed</span>
      <span className="review-history-age">{run.finished_at ? <RelativeTime value={run.finished_at} /> : run.started_at ? <>Started <RelativeTime value={run.started_at} /></> : 'Date unknown'}</span>
      <span className="review-history-disclosure">{open ? 'Hide findings' : 'Show findings'} <span aria-hidden="true">{open ? '▾' : '▸'}</span></span>
    </button><Link className="review-history-details-link" to={reviewLink(home, 'runs', run.id, snapshot)} aria-label={`Open ${run.channel} run ${run.source_run_id}`}>Run details ↗</Link></div>
    <div className="review-history-flags">{run.outcome !== 'completed' && <span>{reviewLabel(run.outcome)}</span>}{!run.complete && <span className="review-warning">Incomplete record</span>}{!run.finalized && <span className="review-warning">Counts not finalized</span>}{!run.attributable && <span className="review-warning">Attribution unconfirmed</span>}{!head || !run.head_sha ? <span className="review-warning">Head association unknown</span> : head !== run.head_sha && <span className="review-warning">Different observed head</span>}</div>
    {open && <div id={findingsID}><InlineReviewFindings number={number} run={run} snapshot={snapshot} /></div>}
  </li>;
}
export function ReviewHistoryGroups({ number, runs, snapshot, head, partial, loading, failed }: { number: number; runs: ReviewRunSummary[]; snapshot: string; head: string; partial: boolean; loading: boolean; failed: boolean }) {
  const channels = [['hosted', 'Hosted CodeRabbit'], ['cli', 'CLI CodeRabbit'], ...[['manual', 'Manual reviews'], ['subagent', 'Subagent reviews']].filter(([channel]) => runs.some(run => run.channel === channel))];
  return <div className="review-history-groups">{channels.map(([channel, title]) => {
    const items = runs.filter(run => run.channel === channel);
    return <section className="review-history-channel" aria-label={title} key={channel}><div className="review-lane-heading"><h4>{title}</h4><span>{loading ? '' : failed ? 'History unavailable' : `${items.length} ${items.length === 1 ? 'run' : 'runs'}${partial ? ' shown' : ''}`}</span></div>
      {loading ? <PageSkeleton rows={2} label="Loading retained rounds" /> : failed && !items.length ? <p className="review-empty">Round history could not be loaded.</p> : items.length ? <ul className="review-history-run-list">{items.map(run => <ReviewHistoryRow key={run.id} number={number} run={run} snapshot={snapshot} head={head} />)}</ul> : <p className="review-empty">{partial ? 'No rounds in loaded pages. Coverage is unknown.' : 'No rounds recorded. Coverage is unknown.'}</p>}
    </section>;
  })}</div>;
}
function ReviewRoundHistory({ number, snapshot, head, expected }: { number: number; snapshot: string; head: string; expected: number }) {
  const archive = useReviewArchive<ReviewRunSummary>('runs', number, snapshot, expected);
  const runs = [...archive.items].reverse();
  if (terminal(archive.error)) return <ReadState error={archive.error} loading={false} empty={false}>{null}</ReadState>;
  return <section className="panel review-history review-round-history" aria-label={`PR #${number} review rounds`}><div className="section-heading"><h3>Review rounds</h3><span className="small-label">Shown rounds · oldest to newest per channel</span></div>
    {!!archive.error && <ReadState error={archive.error} loading={false} empty={false}>{null}</ReadState>}
    <ReviewHistoryGroups number={number} runs={runs} snapshot={snapshot} head={head} loading={archive.loading} failed={!!archive.error} partial={archive.hasMore || runs.length !== expected} />
    <ArchiveProgress shown={runs.length} expected={expected} snapshot={snapshot} label="review rounds" hasMore={archive.hasMore} busy={archive.busy} more={archive.more} />
  </section>;
}

export function ReviewFindingView({ finding }: { finding: ReviewFinding }) {
  const { session } = useWorkspace();
  const route = finding.route;
  const commit = route?.fix_sha ?? finding.fix_sha;
  const proof = route?.proof_note ?? finding.proof_note;
  const resolved = route?.fixed_at ?? finding.resolved_at;
  const highlight = useSemanticHighlight(findingDisplayFingerprint(finding));
  return <li className={`review-finding ${highlight}`}>
    <div className="review-finding-heading"><span className={`badge review-severity-${finding.severity}`}>{reviewLabel(finding.severity)}</span><span className={`badge neutral ${highlight}`}>{reviewLabel(finding.disposition)}</span></div>
    <h3>{finding.title}</h3><EditorialMarkdown className="full-text">{finding.detail || 'No additional detail recorded.'}</EditorialMarkdown>
    <div className="review-decision"><h4>Decision</h4><EditorialMarkdown className="full-text">{finding.decision_reason || 'No decision reason recorded.'}</EditorialMarkdown><p className="item-meta">{finding.actor || 'Actor unknown'} · {finding.decided_at ? time(finding.decided_at) : 'Decision time unknown'}</p></div>
    {route && <div className="review-decision"><h4>Receiving PR</h4><p>{githubLink(session.project, `pull/${route.target_pull_number}`) ? <a href={githubLink(session.project, `pull/${route.target_pull_number}`)} target="_blank" rel="noopener noreferrer">PR #{route.target_pull_number}</a> : `PR #${route.target_pull_number}`} · {reviewLabel(route.disposition)} · {reviewLabel(route.state)}</p><EditorialMarkdown className="full-text">{route.reason || 'Awaiting receiving decision.'}</EditorialMarkdown><p className="item-meta">{route.actor || 'No receiving actor recorded'}{route.decided_at ? ` · ${time(route.decided_at)}` : ''}</p>{route.state === 'active' && (route.disposition === 'unadjudicated' || route.disposition === 'accepted' && !route.fix_sha) && <p className="notice">Receiving obligation remains pending.</p>}</div>}
    <div className="review-proof"><h4>{route ? 'Receiving fix proof' : 'Fix proof'}</h4>{commit ? <p>{githubLink(session.project, `commit/${commit}`) ? <a href={githubLink(session.project, `commit/${commit}`)} target="_blank" rel="noopener noreferrer"><code>{commit}</code> ↗</a> : <code>{commit}</code>}</p> : <p className="muted">{route?.disposition === 'rejected' ? 'Receiving finding rejected; no fix claimed.' : 'No fix commit recorded.'}</p>}<EditorialMarkdown className="full-text">{proof || 'No proof note recorded.'}</EditorialMarkdown><p className="item-meta">{resolved ? `Resolved: ${time(resolved)}` : 'Resolution time unknown'}</p></div>
  </li>;
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
  return <section className="panel review-assessment" aria-label="Captured comparison assessment"><div className="section-heading"><div><h3>Captured comparison policy</h3><p>Current project requirements for this captured comparison.</p></div><button type="button" aria-expanded={open} onClick={() => open ? close() : setOpen(true)}>{open ? 'Close assessment' : 'Assess captured comparison'}</button></div>{open && <CapturedComparisonResult evidenceId={evidenceId} />}</section>;
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
  return <ReadState error={query.error} loading={query.isPending} empty={false}>{value && <div className="review-assessment-result">{query.isFetching && <p role="status">Refreshing assessment…</p>}<dl className="review-ref-grid"><div><dt>Assessment state</dt><dd><span className="badge neutral">{reviewLabel(value.state)}</span></dd></div><div><dt>Policy revision</dt><dd>{value.policy_revision}</dd></div><div><dt>Captured target</dt><dd>PR #{value.anchor.pull_number}<code>{value.anchor.candidate_sha}</code></dd></div><div><dt>Captured source observed</dt><dd>{time(value.anchor.observed_at)}</dd></div><div><dt>Captured comparison base</dt><dd><code>{value.anchor.base_sha}</code></dd></div><div><dt>Policy evaluated</dt><dd>{time(value.evaluated_at)}</dd></div></dl>{value.incoming_routes_capacity && <p className="notice">Incoming route inspection reached its supported capacity; policy remains held.</p>}{(value.incoming_routes_pending ?? 0) > 0 && <p className="notice">{value.incoming_routes_pending} incoming routed finding obligation(s) remain pending.</p>}{value.state === 'unconfigured' ? <p className="notice">Project review requirements are unconfigured.</p> : <div className="review-assessment-channels">{value.channels.map(channel => <section key={channel.channel} aria-label={`${channel.channel === 'hosted' ? 'Hosted' : 'CLI'} assessment`}><h4>{channel.channel === 'hosted' ? 'Hosted' : 'CLI'}</h4><dl><div><dt>Covered</dt><dd>{channel.covered ? 'Yes' : 'No'}</dd></div><div><dt>Pending</dt><dd>{channel.pending ? 'Yes' : 'No'}</dd></div>{channel.rounds_configured && <div><dt>Review rounds</dt><dd>{channel.rounds_complete ? 'Finished' : 'More required'}</dd></div>}</dl></section>)}</div>}<p className="item-meta">This evaluates the captured comparison only. It does not refresh GitHub, assess CI or declare merge readiness.</p></div>}</ReadState>;
}
export function ReviewRunView() {
  const { runId = '' } = useParams();
  const snapshot = useReviewSnapshot();
  const { session, key, home } = useWorkspace();
  const query = useQuery({ queryKey: [...key, 'reviews', snapshot ?? 'current', 'run', runId], queryFn: ({ signal }) => session.api.reviewRun(runId, snapshot, signalFor(session, signal)), staleTime: Infinity });
  const value = query.data;
  const pull = useQuery({ queryKey: [...key, 'reviews', value?.snapshot_id ?? snapshot ?? 'current', 'pull', value?.pull_number ?? 'unknown'], queryFn: ({ signal }) => session.api.reviewPull(value!.pull_number, value!.snapshot_id, signalFor(session, signal)), enabled: !!value, staleTime: Infinity });
  const mismatch = !!value?.run.head_sha && !!pull.data?.pull.head_sha && value.run.head_sha !== pull.data.pull.head_sha;
  return <><Link className="back-link" to={value ? reviewLink(home, 'pulls', value.pull_number, value.snapshot_id) : `${home}/reviews`}>← {value ? `PR #${value.pull_number} history` : 'Review queue'}</Link><ReviewNotice /><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <><div className="page-heading"><p className="eyebrow">PR #{value.pull_number} · {value.run.channel} run</p><h2>Review findings</h2><p>{value.run.reviewer || 'Reviewer unknown'} · {value.run.scope}</p></div><ReviewProvenance value={value} /><section className="panel review-run-summary"><RunPill run={value.run} snapshot={value.snapshot_id} head={pull.data?.pull.head_sha ?? ''} /><dl className="review-ref-grid"><div><dt>Original run source</dt><dd>{value.run.source}</dd></div><div><dt>Run head</dt><dd><code>{value.run.head_sha || 'Unknown'}</code></dd></div><div><dt>Observed PR head</dt><dd><code>{pull.data?.pull.head_sha || 'Unknown'}</code></dd></div><div><dt>Started / finished</dt><dd>{value.run.started_at ? time(value.run.started_at) : 'Unknown'}<br />{value.run.finished_at ? time(value.run.finished_at) : 'Unknown'}</dd></div></dl>{mismatch && <p className="notice">This run belongs to a different head than the PR head in this observation. It does not establish coverage for that head.</p>}{(!value.run.complete || !value.run.attributable || !value.run.finalized) && <p className="notice">This run has incomplete, unattributed or unfinalized source evidence. Counts are historical observations.</p>}<ReadState error={pull.error} loading={pull.isPending} empty={false}>{!value.run.head_sha || !pull.data?.pull.head_sha ? <p className="review-warning">Head association is unknown.</p> : null}</ReadState></section>{value.run.source === 'native-executor' && /^review_evidence_[a-f0-9]{32}$/u.test(value.run.source_run_id) && <CapturedComparisonAssessment key={value.run.source_run_id} evidenceId={value.run.source_run_id} />}<ReviewCoverageLimits limits={value.run.coverage_limits} /><section className="panel review-findings"><div className="section-heading"><h3>Findings ({value.findings.length} recorded / {value.run.found} reported)</h3></div>{value.findings.length ? <ul>{value.findings.map(finding => <ReviewFindingView key={finding.source_finding_id} finding={finding} />)}</ul> : <p className="empty">No findings recorded for this run.{!value.run.complete ? ' The source record is incomplete.' : ''}</p>}</section></>}</ReadState></>;
}
