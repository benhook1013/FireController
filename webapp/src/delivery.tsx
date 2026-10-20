import { useWorkerPresence, WorkerContactView } from './worker-presence-view';
import type { WorkerPresence } from './worker-presence';
import { useEffect, useLayoutEffect, useMemo, useSyncExternalStore, type ReactNode } from 'react';
import { Link, useLocation } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { ReadState, signalFor, useWorkspace, type Session } from './workspace';
import type { DeliverySnapshot, ReviewPullSummary, ReviewQueue } from './types';
import { ReviewQueueView, ReviewRunsView, ReviewPullFacts, reviewQueueQuery } from './review-workspace';
import { DeliveryBoardArchive } from './delivery-board';
import { RelativeTime } from './relative-time';
import { EditorialMarkdown } from './editorial-markdown';
import './delivery.css';
import { MergeHeadSkeleton } from './loading';
import { useReviewProgress } from './review-progress-view';
import type { ReviewProgressItem } from './review-progress';
import { currentReviewFronts, reviewQueueBadges, type ReviewFronts } from './review-fronts';
import { useReviewFronts } from './review-fronts-view';

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
  return [...lanes.values()].map(lane => ({ ...lane, jobs: [...lane.jobs].sort((a, b) => Number(b.id === lane.worker?.primary_job_id) - Number(a.id === lane.worker?.primary_job_id) || statusOrder[a.status] - statusOrder[b.status]) })).sort((a, b) => {
    if (!a.id) return 1; if (!b.id) return -1;
    return Math.min(3, ...a.jobs.map(job => statusOrder[job.status])) - Math.min(3, ...b.jobs.map(job => statusOrder[job.status]));
  });
}
export function deliveryQuery(session: Session, key: readonly string[], workerCursor = '', jobCursor = '') {
  return { queryKey: [...key, 'delivery', workerCursor, jobCursor], queryFn: ({ signal }: { signal: AbortSignal }) => session.api.delivery({ worker_limit: 20, job_limit: 50, ...(workerCursor ? { worker_cursor: workerCursor } : {}), ...(jobCursor ? { job_cursor: jobCursor } : {}) }, signalFor(session, signal)), staleTime: Infinity };
}
export function deliveryReviewQuery(session: Session, key: readonly string[]) {
  return reviewQueueQuery(session, key, 'recent');
}
function preview(value: string, truncated: boolean): string { return value + (truncated ? '…' : ''); }
function DeliveryProsePreview({ value, truncated, className = '' }: { value: string; truncated: boolean; className?: string }) {
  return <div className={`delivery-prose-preview ${className}`.trim()}><EditorialMarkdown compact className="delivery-preview">{value}</EditorialMarkdown>{truncated && <span className="delivery-preview-truncated" aria-label="Preview truncated">…</span>}</div>;
}
function snapshotLink(home: string, kind: 'pulls' | 'runs', id: number | string, snapshot: string): string {
  return `${home}/reviews/${kind}/${encodeURIComponent(String(id))}?${new URLSearchParams({ snapshot })}`;
}
function CheckpointPreview({ job }: { job: DeliveryJob }) {
  const checkpoint = job.checkpoint;
  if (!checkpoint) return job.brief_preview || job.update || job.checklist ? null : <p className="delivery-empty">No checkpoint recorded.</p>;
  return <div className="delivery-checkpoint"><p className="delivery-context-meta">Checkpoint · <RelativeTime value={checkpoint.created_at} /></p>{checkpoint.job_revision !== job.revision && <p className="delivery-checkpoint-stale">Checkpoint from job revision {checkpoint.job_revision}; current revision {job.revision}.</p>}{checkpoint.next && <div className="delivery-checkpoint-field"><strong className="delivery-checkpoint-label">Next</strong><DeliveryProsePreview value={checkpoint.next} truncated={checkpoint.next_truncated} /></div>}{checkpoint.blocker && <div className="delivery-checkpoint-field delivery-blocker"><strong className="delivery-checkpoint-label">Blocker</strong><DeliveryProsePreview value={checkpoint.blocker} truncated={checkpoint.blocker_truncated} /></div>}{checkpoint.done && <div className="delivery-checkpoint-field"><strong className="delivery-checkpoint-label">Done</strong><DeliveryProsePreview value={checkpoint.done} truncated={checkpoint.done_truncated} /></div>}</div>;
}
function JobContextPreview({ job }: { job: DeliveryJob }) {
  return <div className="delivery-job-context">
    {job.brief_preview && <DeliveryProsePreview className="delivery-brief" value={job.brief_preview} truncated={job.brief_preview_truncated} />}
    <p className="delivery-context-meta">Edited <RelativeTime value={job.updated_at} /></p>
    {job.update && <div className="delivery-update"><p className="delivery-context-meta">Progress · <RelativeTime value={job.update.created_at} /></p><DeliveryProsePreview className="delivery-update-body" value={job.update.body} truncated={job.update.body_truncated} />{job.update.job_revision !== job.revision && <p className="delivery-checkpoint-stale">Progress from job revision {job.update.job_revision}; current revision {job.revision}.</p>}</div>}
    {job.checklist && <div className="delivery-checklist"><p><strong>Checklist {job.checklist.done}/{job.checklist.total} done</strong> · <RelativeTime value={job.checklist.created_at} /></p>{job.checklist.job_revision !== job.revision && <p className="delivery-checkpoint-stale">Checklist from job revision {job.checklist.job_revision}; current revision {job.revision}.</p>}</div>}
    <CheckpointPreview job={job} />
  </div>;
}
function LaneInboxLink({ lane, home }: { lane: DeliveryLane; home: string }) {
  const { session, selectWorker } = useWorkspace();
  return <Link to={`${home}/inbox?${new URLSearchParams({worker_id:lane.id})}`} onClick={event => {
    if (event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) selectWorker({ id: lane.id, name: lane.name, project_id: session.project.id });
  }}>Worker inbox</Link>;
}
export function DeliveryLanes({ value, home, partialJobs = value.jobs.has_more, presence }: { value: DeliverySnapshot; home: string; partialJobs?: boolean; presence?: WorkerPresence }) {
  return <div className="delivery-lanes">{groupDeliveryLanes(value).map(lane => <section className="panel delivery-lane" key={lane.id} aria-label={`${lane.name} jobs`}>
    <header className="delivery-lane-heading"><h3>{lane.id ? <Link to={`${home}/workers/${encodeURIComponent(lane.id)}`}>{lane.name}</Link> : lane.name}{lane.id && <WorkerContactView value={presence} worker={lane.id}/>}</h3>
      {lane.id && <nav className="delivery-worker-links" aria-label={`${lane.name} links`}><LaneInboxLink lane={lane} home={home} /><Link to={`${home}/jobs?${new URLSearchParams({worker_id:lane.id,status:'all'})}`}>Job history</Link></nav>}
      {lane.worker?.paused && <span className="badge status-parked">Paused</span>}
      {lane.worker?.paused && lane.worker.pause_reason && <p className="delivery-pause">{preview(lane.worker.pause_reason, lane.worker.pause_reason_truncated)}</p>}
      {lane.id && !lane.worker && <p className="delivery-page-note">Owner outside this worker page</p>}
      {lane.worker?.primary_job_id && !lane.jobs.some(job => job.id === lane.worker!.primary_job_id) && <Link className="delivery-primary-link" to={`${home}/jobs/${encodeURIComponent(lane.worker.primary_job_id)}`}>Primary job</Link>}
    </header>
    {lane.jobs.length ? <ul className="delivery-job-list">{lane.jobs.map(job => <li key={job.id}>
      <div className="delivery-job-heading"><h4><Link to={`${home}/jobs/${encodeURIComponent(job.id)}`}>{job.title}</Link></h4><div className="delivery-job-state"><span className={`badge status-${job.status}`}>{job.status[0].toUpperCase()+job.status.slice(1)}</span>{lane.worker?.primary_job_id === job.id && <span className="badge delivery-primary">Primary focus</span>}</div></div>
      <JobContextPreview job={job} /><Link className="delivery-job-link" to={`${home}/jobs/${encodeURIComponent(job.id)}`} aria-label={`Read job ${job.title}`}>Read job →</Link>
    </li>)}</ul> : <p className="delivery-empty">{partialJobs ? 'No jobs in loaded slice.' : 'No open jobs.'}</p>}
  </section>)}</div>;
}

type MergeHead = { state: 'head'; pull: ReviewPullSummary } | { state: 'unknown'; reason: 'unconfigured' | 'preceding' | 'next-page' } | { state: 'none' };
export function selectMergeHead(queue: ReviewQueue): MergeHead {
  if (!queue.configured) return { state: 'unknown', reason: 'unconfigured' };
  if (queue.head_position !== undefined || queue.items.some(pull => pull.queue_position !== undefined)) {
    if (queue.head_position !== undefined) {
      const pull = queue.items.find(item => item.queue_position === queue.head_position);
      return pull?.state === 'open' ? { state: 'head', pull } : { state: 'unknown', reason: 'next-page' };
    }
    if (queue.items.some(pull => pull.state !== 'merged' && pull.state !== 'closed')) return { state: 'unknown', reason: 'preceding' };
    return queue.has_more ? { state: 'unknown', reason: 'next-page' } : { state: 'none' };
  }
  for (const pull of queue.items) {
    if (pull.state === 'merged' || pull.state === 'closed') continue;
    if (pull.state === 'open') return { state: 'head', pull };
    return { state: 'unknown', reason: 'preceding' };
  }
  return queue.has_more ? { state: 'unknown', reason: 'next-page' } : { state: 'none' };
}
export function MergeHeadContent({ queue, home, progress, fronts }: { queue: ReviewQueue; home: string; progress?: ReviewProgressItem; fronts?: ReviewFronts }) {
  const head = selectMergeHead(queue);
  if (head.state === 'none' || head.state === 'unknown' && head.reason === 'unconfigured') return null;
  const currentFronts = currentReviewFronts(queue.snapshot_id, queue.items, fronts);
  const status = head.state === 'head' ? reviewQueueBadges(head.pull, queue.snapshot_id, currentFronts, progress)[0] : undefined;
  return <><p className="eyebrow">Merge train</p>{head.state === 'head' ? <><h2><Link to={snapshotLink(home, 'pulls', head.pull.number, queue.snapshot_id)}>#{head.pull.number} · {head.pull.title}</Link></h2><ReviewPullFacts pull={head.pull} status={status} /><ReviewRunsView number={head.pull.number} snapshot={queue.snapshot_id} head={head.pull.head_sha} attempts={head.pull.latest_attempts} expected={head.pull.run_count} compact progress={progress} /></> : <><h2>Merge head not determined</h2><p>{head.reason === 'preceding' ? 'An earlier queue entry has unknown state.' : 'No open PR is loaded yet; more queue entries remain.'}</p></>}<Link className="delivery-head-link" to={`${home}#${head.state === 'head' ? `review-pr-${head.pull.number}` : 'review-queue'}`} aria-label="Jump to review queue">Review queue ↗</Link></>;
}
export function DeliveryHero({ children }: { children: ReactNode }) {
  const { session, key, home } = useWorkspace();
  const query = useQuery(deliveryReviewQuery(session, key));
  const progress = useReviewProgress(query.data?.items.map(pull => pull.number) ?? []);
  const fronts = useReviewFronts(!!query.data?.configured);
  const head = query.data && selectMergeHead(query.data);
  const showHead = Boolean(query.isPending || query.error || head?.state === 'head' || head?.state === 'unknown' && head.reason !== 'unconfigured');
  return <div className={`delivery-hero${showHead ? '' : ' delivery-hero-identity-only'}`}>{children}{showHead && <section className="hero delivery-merge-head" aria-label="Merge train head">{query.isPending && !query.error ? <MergeHeadSkeleton /> : <ReadState error={query.error} loading={false} empty={false}>{query.data && <MergeHeadContent queue={query.data} home={home} fronts={fronts.error ? undefined : fronts.data} progress={progress.error || head?.state !== 'head' ? undefined : progress.data?.items.find(item => item.pull_number === head.pull.number)} />}</ReadState>}</section>}</div>;
}
export function Delivery() {
  const { session, key, home } = useWorkspace();
  const location = useLocation();
  const query = useQuery(deliveryQuery(session, key));
  const archive = useMemo(() => new DeliveryBoardArchive((workers, jobs, signal) => deliveryQuery(session, key, workers, jobs).queryFn({ signal })), [session, key]);
  const board = useSyncExternalStore(archive.subscribe, archive.snapshot, archive.snapshot);
  useLayoutEffect(() => { archive.reset(query.data); }, [archive, query.data, query.dataUpdatedAt]);
  useEffect(() => { const cancel = () => archive.cancel(); session.controller.signal.addEventListener('abort', cancel, { once: true }); return () => { session.controller.signal.removeEventListener('abort', cancel); cancel(); }; }, [archive, session.controller]);
  useEffect(() => { if (location.hash === '#review-queue') { const frame = requestAnimationFrame(() => document.getElementById('review-queue')?.scrollIntoView()); return () => cancelAnimationFrame(frame); } }, [location.hash, location.key]);
  const value = board.value ?? query.data;
  const contact = useWorkerPresence(value?.workers.items.map(worker => worker.id) ?? []);
  const more = Boolean(value && (value.workers.has_more || value.jobs.has_more));
  return <><section aria-labelledby="delivery-title"><div className="delivery-section-heading"><h2 id="delivery-title">Delivery</h2><Link to={`${home}/jobs`}>Manage jobs →</Link></div><ReadState error={query.error} loading={query.isPending} empty={false}>{value && <><DeliveryLanes value={value} home={home} presence={contact.error ? undefined : contact.data} />{board.error && <ReadState error={board.error} loading={false} empty={false}>{null}</ReadState>}{more && <div className="delivery-more"><p className="delivery-page-note">More open work is available. Lanes show the work loaded so far.</p><button type="button" onClick={() => { if (!query.isFetching) void archive.more(); }} disabled={query.isFetching || board.busy}>{board.busy ? 'Loading more work…' : 'Load more work'}</button></div>}</>}</ReadState></section><ReviewQueueView embedded /></>;
}
