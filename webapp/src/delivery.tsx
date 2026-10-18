import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { Pager, ReadState, signalFor, usePaging, useWorkspace, type Session } from './workspace';
import type { DeliverySnapshot, ReviewPullSummary, ReviewQueue, ReviewRunSummary } from './types';
import './delivery.css';

type DeliveryWorker = DeliverySnapshot['workers']['items'][number];
type DeliveryJob = DeliverySnapshot['jobs']['items'][number];
export type DeliveryLane = { id: string; name: string; worker?: DeliveryWorker; jobs: DeliveryJob[] };
const statusOrder = { active: 0, blocked: 1, parked: 2, completed: 3 };

export function groupDeliveryLanes(value: DeliverySnapshot): DeliveryLane[] {
  const lanes = new Map<string, DeliveryLane>(value.workers.items.map(worker => [worker.id, { id: worker.id, name: worker.name, worker, jobs: [] }]));
  for (const job of value.jobs.items) {
    if (!lanes.has(job.worker_id)) lanes.set(job.worker_id, { id: job.worker_id, name: job.worker_id ? job.worker_name : 'Unassigned', jobs: [] });
    lanes.get(job.worker_id)!.jobs.push(job);
  }
  if (!lanes.has('')) lanes.set('', { id: '', name: 'Unassigned', jobs: [] });
  return [...lanes.values()].map(lane => ({ ...lane, jobs: [...lane.jobs].sort((a, b) => statusOrder[a.status] - statusOrder[b.status]) }));
}
export function deliveryQuery(session: Session, key: readonly string[], workerCursor = '', jobCursor = '') {
  return { queryKey: [...key, 'delivery', workerCursor, jobCursor], queryFn: ({ signal }: { signal: AbortSignal }) => session.api.delivery({ worker_limit: 20, job_limit: 50, ...(workerCursor ? { worker_cursor: workerCursor } : {}), ...(jobCursor ? { job_cursor: jobCursor } : {}) }, signalFor(session, signal)), staleTime: Infinity };
}
export function deliveryReviewQuery(session: Session, key: readonly string[]) {
  return { queryKey: [...key, 'reviews', 'current', 'queue', ''], queryFn: ({ signal }: { signal: AbortSignal }) => session.api.reviewQueue({ limit: 20 }, signalFor(session, signal)), staleTime: Infinity };
}
function preview(value: string, truncated: boolean): string { return value + (truncated ? '…' : ''); }
function snapshotLink(home: string, kind: 'pulls' | 'runs', id: number | string, snapshot: string): string {
  return `${home}/reviews/${kind}/${encodeURIComponent(String(id))}?${new URLSearchParams({ snapshot })}`;
}
function CheckpointPreview({ job }: { job: DeliveryJob }) {
  const checkpoint = job.checkpoint;
  if (!checkpoint) return <p className="delivery-empty">No checkpoint recorded.</p>;
  return <div className="delivery-checkpoint">{checkpoint.job_revision !== job.revision && <p className="delivery-checkpoint-stale">Checkpoint from job revision {checkpoint.job_revision}; current revision {job.revision}.</p>}{checkpoint.next && <p><strong>Next</strong>{preview(checkpoint.next, checkpoint.next_truncated)}</p>}{checkpoint.blocker && <p className="delivery-blocker"><strong>Blocker</strong>{preview(checkpoint.blocker, checkpoint.blocker_truncated)}</p>}{checkpoint.done && <p><strong>Done</strong>{preview(checkpoint.done, checkpoint.done_truncated)}</p>}</div>;
}
export function DeliveryLanes({ value, home, partialJobs = value.jobs.has_more }: { value: DeliverySnapshot; home: string; partialJobs?: boolean }) {
  return <div className="delivery-lanes">{groupDeliveryLanes(value).map(lane => <section className="panel delivery-lane" key={lane.id} aria-label={`${lane.name} jobs`}><header className="delivery-lane-heading"><h3>{lane.id ? <Link to={`${home}/workers/${encodeURIComponent(lane.id)}`}>{lane.name}</Link> : lane.name}</h3>{lane.worker?.paused && <span className="badge status-parked">Paused</span>}{lane.worker?.paused && lane.worker.pause_reason && <p className="delivery-pause">{preview(lane.worker.pause_reason, lane.worker.pause_reason_truncated)}</p>}{lane.id && !lane.worker && <p className="delivery-page-note">Owner outside this worker page</p>}{lane.worker?.primary_job_id && !lane.jobs.some(job => job.id === lane.worker!.primary_job_id) && <Link className="delivery-primary-link" to={`${home}/jobs/${encodeURIComponent(lane.worker.primary_job_id)}`}>Primary job</Link>}</header>{lane.jobs.length ? <ul className="delivery-job-list">{lane.jobs.map(job => <li key={job.id}><div className="delivery-job-state"><span className={`badge status-${job.status}`}>{job.status}</span>{lane.worker?.primary_job_id === job.id && <span className="delivery-primary">Primary focus</span>}</div><h4><Link to={`${home}/jobs/${encodeURIComponent(job.id)}`}>{job.title}</Link></h4><CheckpointPreview job={job} /><Link className="delivery-job-link" to={`${home}/jobs/${encodeURIComponent(job.id)}`} aria-label={`Read job ${job.title}`}>Read job →</Link></li>)}</ul> : <p className="delivery-empty">{partialJobs ? 'No jobs in loaded slice.' : 'No open jobs.'}</p>}</section>)}</div>;
}

type MergeHead = { state: 'head'; pull: ReviewPullSummary } | { state: 'unknown'; reason: 'unconfigured' | 'preceding' | 'next-page' } | { state: 'none' };
export function selectMergeHead(queue: ReviewQueue): MergeHead {
  if (!queue.configured) return { state: 'unknown', reason: 'unconfigured' };
  for (const pull of queue.items) {
    if (pull.state === 'merged' || pull.state === 'closed') continue;
    if (pull.state === 'open') return { state: 'head', pull };
    return { state: 'unknown', reason: 'preceding' };
  }
  return queue.has_more ? { state: 'unknown', reason: 'next-page' } : { state: 'none' };
}
function ObservedPullFacts({ pull }: { pull: ReviewPullSummary }) {
  return <div className="delivery-pull-facts"><span className={`badge ${pull.state === 'merged' || pull.state === 'closed' ? 'neutral' : 'status-active'}`}>{pull.state}{pull.state === 'merged' ? ' · retained' : ''}</span><span>Observed CI: {pull.ci_state}</span><span>Observed mergeability: {pull.mergeability}</span><span>{pull.run_count} runs · {pull.attempt_count} attempts</span>{!pull.complete && <span>Incomplete PR observation</span>}</div>;
}
export function MergeHeadContent({ queue, home }: { queue: ReviewQueue; home: string }) {
  const head = selectMergeHead(queue);
  if (head.state === 'none' || head.state === 'unknown' && head.reason === 'unconfigured') return null;
  return <><p className="eyebrow">Merge train</p>{head.state === 'head' ? <><h2><Link to={snapshotLink(home, 'pulls', head.pull.number, queue.snapshot_id)}>#{head.pull.number} · {head.pull.title}</Link></h2><p className="delivery-head-context">First open PR in the stored queue</p><ObservedPullFacts pull={head.pull} /><SummaryChannels pull={head.pull} snapshot={queue.snapshot_id} home={home} compact /></> : <><h2>Merge head not determined</h2><p>{head.reason === 'preceding' ? 'An earlier queue entry has unknown state.' : 'No open PR is loaded yet; more queue entries remain.'}</p></>}<Link className="delivery-head-link" to={`${home}/reviews`}>Open review queue →</Link></>;
}
export function DeliveryHero({ children }: { children: ReactNode }) {
  const { session, key, home } = useWorkspace();
  const query = useQuery(deliveryReviewQuery(session, key));
  const head = query.data && selectMergeHead(query.data);
  const showHead = Boolean(query.isPending || query.error || head?.state === 'head' || head?.state === 'unknown' && head.reason !== 'unconfigured');
  return <div className={`delivery-hero${showHead ? '' : ' delivery-hero-identity-only'}`}>{children}{showHead && <section className="hero delivery-merge-head" aria-label="Merge train head"><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <MergeHeadContent queue={query.data} home={home} />}</ReadState></section>}</div>;
}
function SummaryRun({ run, pull, snapshot, home }: { run: ReviewRunSummary; pull: ReviewPullSummary; snapshot: string; home: string }) {
  const date = run.finished_at || run.started_at;
  const warnings = [!run.complete && 'Incomplete record', !run.finalized && 'Counts not finalized', !run.attributable && 'Attribution unconfirmed', !run.head_sha || !pull.head_sha ? 'Head association unknown' : run.head_sha !== pull.head_sha && 'Different observed head'].filter(Boolean).join('. ');
  return <Link className={`review-round-pill review-pill-${run.outcome}${run.outcome === 'completed' && run.accepted === 0 ? ' review-pill-zero' : ''}`} to={snapshotLink(home, 'runs', run.id, snapshot)} title={`${run.found}/${run.accepted}/${run.routed} found / accepted / routed. ${run.outcome.replaceAll('_', ' ')}. ${warnings}`} aria-label={`Open ${run.channel} run ${run.source_run_id}: ${run.found}/${run.accepted}/${run.routed} found / accepted / routed. ${run.outcome.replaceAll('_', ' ')}. ${warnings}`}><strong>{run.found}/{run.accepted}/{run.routed}</strong>{run.outcome !== 'completed' && <span className="review-pill-state">{run.outcome.replaceAll('_', ' ')}</span>}{date ? <time dateTime={date}>{new Date(date).toLocaleDateString(undefined, { day: 'numeric', month: 'short' })}</time> : <span className="review-pill-age">Date unknown</span>}</Link>;
}
function SummaryChannels({ pull, snapshot, home, compact = false }: { pull: ReviewPullSummary; snapshot: string; home: string; compact?: boolean }) {
  return <div className={`delivery-review-lanes${compact ? ' delivery-head-lanes' : ''}`}>{(['hosted', 'cli'] as const).map(channel => {
    // Recent summaries preserve newest-first retained order. The head needs
    // only its latest shown run; the overview reads left-to-right by age.
    const runs = pull.recent_runs.filter(run => run.channel === channel);
    const shown = compact ? runs.slice(0, 1) : [...runs].reverse();
    const attempt = [...pull.latest_attempts].reverse().find(item => item.channel === channel);
    return <section className="delivery-review-lane" key={channel} aria-label={`PR #${pull.number} ${channel === 'hosted' ? 'Hosted' : 'CLI'} ${compact ? 'merge head' : 'summary'}`}><h4>{channel === 'hosted' ? 'Hosted' : 'CLI'}</h4>{attempt && <p>Latest attempt: <strong>{attempt.state.replaceAll('_', ' ')}</strong></p>}{compact && !attempt && runs[0] && <p>Latest shown round: <strong>{runs[0].outcome.replaceAll('_', ' ')}</strong></p>}{shown.length ? <div className="review-round-pills">{shown.map(run => <SummaryRun key={run.id} run={run} pull={pull} snapshot={snapshot} home={home} />)}</div> : <p>No recent rounds in this summary.</p>}</section>;
  })}</div>;
}
export function DeliveryReviewSummaries({ queue, home }: { queue: ReviewQueue; home: string }) {
  if (!queue.configured) return <p className="delivery-empty">No stored review queue has been recorded.</p>;
  return <>{queue.observed_at && <p className="delivery-observation">Stored observation · <time dateTime={queue.observed_at}>{new Date(queue.observed_at).toLocaleString()}</time> · recent rounds only</p>}<div className="delivery-review-list">{queue.items.map(pull => <article className="panel delivery-review-card" key={pull.number}><h3><Link to={snapshotLink(home, 'pulls', pull.number, queue.snapshot_id)}>#{pull.number} · {pull.title}</Link></h3><ObservedPullFacts pull={pull} /><SummaryChannels pull={pull} snapshot={queue.snapshot_id} home={home} /><Link className="delivery-job-link" to={snapshotLink(home, 'pulls', pull.number, queue.snapshot_id)}>Full PR #{pull.number} history →</Link></article>)}</div>{queue.items.length === 0 && <p className="delivery-empty">No pull requests in this stored queue.</p>}{queue.has_more && <p className="delivery-page-note">More queue entries are available in the review workspace.</p>}</>;
}
function DeliveryOverviewReviews() {
  const { session, key, home } = useWorkspace();
  const query = useQuery(deliveryReviewQuery(session, key));
  return <section className="delivery-reviews" aria-labelledby="delivery-reviews-title"><div className="delivery-section-heading"><h2 id="delivery-reviews-title">Review queue</h2><Link to={`${home}/reviews`}>All review history →</Link></div><ReadState error={query.error} loading={query.isPending} empty={false}>{query.data && <DeliveryReviewSummaries queue={query.data} home={home} />}</ReadState></section>;
}
export function Delivery() {
  const { session, key, home } = useWorkspace();
  const workers = usePaging(), jobs = usePaging();
  const query = useQuery(deliveryQuery(session, key, workers.cursor, jobs.cursor));
  const value = query.data;
  return <><section aria-labelledby="delivery-title"><div className="delivery-section-heading"><h2 id="delivery-title">Delivery</h2><Link to={`${home}/jobs`}>Manage jobs →</Link></div><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <><p className="delivery-coverage">{value.workers.items.length} workers and {value.jobs.items.length} open jobs loaded. Lanes show the selected job page.{(value.workers.has_more || value.jobs.has_more) && ' More pages are available.'}</p><DeliveryLanes value={value} home={home} partialJobs={jobs.number > 1 || value.jobs.has_more} /><div className="delivery-pages"><section aria-label="Worker pages"><h3>Workers</h3><Pager paging={workers} page={value.workers} label="delivery workers" busy={query.isFetching} /></section><section aria-label="Job pages"><h3>Open jobs</h3><Pager paging={jobs} page={value.jobs} label="delivery jobs" busy={query.isFetching} /></section></div></>}</ReadState></section><DeliveryOverviewReviews /></>;
}
