import { usePresentation, useProjectHeading } from './presentation-context';
import { useWorkerPresence, WorkerContactView } from './worker-presence-view';
import type { WorkerPresence } from './worker-presence';
import { useContext, useEffect, useLayoutEffect, useMemo, useSyncExternalStore, type ReactNode } from 'react';
import { Link, useLocation } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { ReadState, signalFor, useWorkspace, type Session } from './workspace';
import type { DeliverySnapshot, ReviewPullSummary, ReviewQueue } from './types';
import { ReviewQueueView, ReviewQueueRow, reviewQueueQuery } from './review-workspace';
import { DeliveryBoardArchive } from './delivery-board';
import { RelativeTime } from './relative-time';
import { EditorialMarkdown } from './editorial-markdown';
import { DeliveryJobCard, WorkerLaneDisplay } from './delivery-display';
import { MergeHeadSkeleton } from './loading';
import { useReviewProgress } from './review-progress-view';
import type { ReviewProgressItem } from './review-progress';
import { currentReviewFronts, type ReviewFronts } from './review-fronts';
import { useReviewFronts } from './review-fronts-view';
import { useReviewQueueFrame } from './review-frame-view';
import { RestoringReadingPosition, scrollToReadingTarget } from './reading-position';

type DeliveryWorker = DeliverySnapshot['workers']['items'][number];
type DeliveryJob = DeliverySnapshot['jobs']['items'][number];
export type DeliveryLane = { id: string; name: string; worker?: DeliveryWorker; jobs: DeliveryJob[] };
const statusOrder = { active: 0, blocked: 1, parked: 2, completed: 3 };

export function groupDeliveryLanes(value: DeliverySnapshot, workerOrder: readonly string[] = []): DeliveryLane[] {
  const lanes = new Map<string, DeliveryLane>(value.workers.items.map(worker => [worker.id, { id: worker.id, name: worker.name, worker, jobs: [] }]));
  for (const job of value.jobs.items) {
    if (!lanes.has(job.worker_id)) lanes.set(job.worker_id, { id: job.worker_id, name: job.worker_id ? job.worker_name : 'Unassigned', jobs: [] });
    lanes.get(job.worker_id)!.jobs.push(job);
  }
  if (!lanes.has('')) lanes.set('', { id: '', name: 'Unassigned', jobs: [] });
  return [...lanes.values()].map(lane => ({ ...lane, jobs: [...lane.jobs].sort((a, b) => Number(b.id === lane.worker?.primary_job_id) - Number(a.id === lane.worker?.primary_job_id) || statusOrder[a.status] - statusOrder[b.status]) })).sort((a, b) => {
    if (!a.id) return 1; if (!b.id) return -1;
    const position = (id: string) => { const index = workerOrder.indexOf(id); return index < 0 ? workerOrder.length : index; };
    const byPin = position(a.id) - position(b.id);
    if (byPin) return byPin;
    const left = a.name.toLowerCase(), right = b.name.toLowerCase();
    return left < right ? -1 : left > right ? 1 : a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
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
function ProgressPreview({ job }: { job: DeliveryJob }) {
  return <div className="delivery-checkpoint">{(['summary', 'next', 'blocker'] as const).map(field => job.progress[field] && <div className={`delivery-checkpoint-field${field === 'blocker' ? ' delivery-blocker' : ''}`} key={field}><strong className="delivery-checkpoint-label">{field === 'summary' ? 'Progress' : field === 'next' ? 'Next' : 'Blocker'}:</strong><DeliveryProsePreview value={job.progress[field]} truncated={job.progress[`${field}_truncated`]} /></div>)}</div>;
}
function JobContextPreview({ job }: { job: DeliveryJob }) {
  return <div className="delivery-job-context">
    <ProgressPreview job={job} />
    <p className="delivery-context-meta">Edited <RelativeTime value={job.updated_at} /></p>
  </div>;
}
function LaneInboxLink({ lane, home }: { lane: DeliveryLane; home: string }) {
  const { session, selectWorker } = useWorkspace();
  return <Link to={`${home}/inbox?${new URLSearchParams({worker_id:lane.id})}`} onClick={event => {
    if (event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) {
      event.preventDefault();
      selectWorker({ id: lane.id, name: lane.name, project_id: session.project.id });
    }
  }}>Worker inbox</Link>;
}
export function DeliveryLanes({ value, home, partialJobs = value.jobs.has_more, presence }: { value: DeliverySnapshot; home: string; partialJobs?: boolean; presence?: WorkerPresence }) {
  const workerOrder = usePresentation()?.worker_order ?? [];
  return <div className="delivery-lanes">{groupDeliveryLanes(value, workerOrder).map(lane => <WorkerLaneDisplay key={lane.id} label={lane.name} readingAnchor={`delivery-worker-${lane.id || 'unassigned'}`}
    name={lane.id ? <Link to={`${home}/workers/${encodeURIComponent(lane.id)}`}>{lane.name}</Link> : lane.name}
    presence={lane.id && <WorkerContactView value={presence} worker={lane.id}/>}
    links={lane.id && <><LaneInboxLink lane={lane} home={home} /><Link to={`${home}/jobs?${new URLSearchParams({worker_id:lane.id,status:'all'})}`}>Job history</Link></>}
    status={lane.worker?.paused && <span className="badge status-parked">Paused</span>}
    details={<>{lane.worker?.paused && lane.worker.pause_reason && <p className="delivery-pause">{preview(lane.worker.pause_reason, lane.worker.pause_reason_truncated)}</p>}
      {lane.id && !lane.worker && <p className="delivery-page-note">Owner outside this worker page</p>}
      {lane.worker?.primary_job_id && !lane.jobs.some(job => job.id === lane.worker!.primary_job_id) && <Link className="delivery-primary-link" to={`${home}/jobs/${encodeURIComponent(lane.worker.primary_job_id)}`}>Primary job</Link>}</>}>
    {lane.jobs.length ? <ul className="delivery-job-list">{lane.jobs.map(job => <DeliveryJobCard key={job.id} readingAnchor={`delivery-job-${job.id}`}
      title={<Link to={`${home}/jobs/${encodeURIComponent(job.id)}`}>{job.title}</Link>}
      status={<><span className={`badge status-${job.status}`}>{job.status[0].toUpperCase()+job.status.slice(1)}</span>{lane.worker?.primary_job_id === job.id && <span className="badge delivery-primary">Primary focus</span>}</>}
      footer={<Link className="delivery-job-link" to={`${home}/jobs/${encodeURIComponent(job.id)}`} aria-label={`Read job ${job.title}`}>Read job →</Link>}>
      <JobContextPreview job={job} />
    </DeliveryJobCard>)}</ul> : <p className="delivery-empty">{partialJobs ? 'No jobs in loaded slice.' : 'No open jobs.'}</p>}
  </WorkerLaneDisplay>)}</div>;
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
export function MergeHeadContent({ queue, home, progress, fronts, channels }:  { queue: ReviewQueue; home: string; progress?: ReviewProgressItem; fronts?: ReviewFronts; channels?: ('hosted'|'cli')[] }) {
  const label = useProjectHeading('reviews');
  const head = selectMergeHead(queue);
  if (head.state === 'none' || head.state === 'unknown' && head.reason === 'unconfigured') return null;
  const currentFronts = currentReviewFronts(queue.snapshot_id, queue.items, fronts);
  const jump = <Link className="review-details-link" to={`${home}#${head.state === 'head' ? `review-pr-${head.pull.number}` : 'review-queue'}`} aria-label="Jump to review queue">Review queue ↗</Link>;
  return <><p className="eyebrow">{label}</p>{head.state === 'head' ? <ul className="review-queue"><ReviewQueueRow pull={head.pull} snapshot={queue.snapshot_id} progress={progress} fronts={currentFronts} channels={channels ?? (fronts?.configured ? fronts.channels.map(channel=>channel.channel) : undefined)} footer={jump} anchor={false} /></ul> : <><h2>Queue head not determined</h2><p>{head.reason === 'preceding' ? 'An earlier queue entry has unknown state.' : 'No open PR is loaded yet; more queue entries remain.'}</p>{jump}</>}</>;
}
export function DeliveryHero({ children }: { children: ReactNode }) {
  const { session, key, home } = useWorkspace();
  const query = useQuery(deliveryReviewQuery(session, key));
  const fronts = useReviewFronts(!!query.data?.configured);
  const frame = useReviewQueueFrame(JSON.stringify([...key, 'delivery-head']), query, fronts);
  const queue = frame?.queue;
  const progress = useReviewProgress(queue?.items.map(pull => pull.number) ?? []);
  const head = queue && selectMergeHead(queue);
  const showHead = Boolean(query.isPending || query.error || head?.state === 'head' || head?.state === 'unknown' && head.reason !== 'unconfigured');
  return <div className={`delivery-hero${showHead ? '' : ' delivery-hero-identity-only'}`}>{children}{showHead && <section className="hero delivery-merge-head" aria-label="Review queue head">{query.isPending && !queue && !query.error ? <MergeHeadSkeleton /> : <ReadState error={query.error} loading={false} empty={false} retained={!!queue}>{queue && <MergeHeadContent queue={queue} home={home} fronts={frame?.fronts} channels={frame?.fronts?.configured ? frame.fronts.channels.map(channel=>channel.channel) : undefined} progress={head?.state !== 'head' ? undefined : progress.data?.items.find(item => item.pull_number === head.pull.number)} />}</ReadState>}</section>}</div>;
}
export function DeliveryWork() {
  const { session, key, home } = useWorkspace();
  const location = useLocation();
  const restoringPosition = useContext(RestoringReadingPosition);
  const query = useQuery(deliveryQuery(session, key));
  const archive = useMemo(() => new DeliveryBoardArchive((workers, jobs, signal) => deliveryQuery(session, key, workers, jobs).queryFn({ signal })), [session, key]);
  const board = useSyncExternalStore(archive.subscribe, archive.snapshot, archive.snapshot);
  useLayoutEffect(() => { archive.reset(query.data); }, [archive, query.data]);
  useEffect(() => { const cancel = () => archive.cancel(); session.controller.signal.addEventListener('abort', cancel, { once: true }); return () => { session.controller.signal.removeEventListener('abort', cancel); cancel(); }; }, [archive, session.controller]);
  useEffect(() => { if (!restoringPosition && !query.isPending && location.hash === '#review-queue') { const frame = requestAnimationFrame(() => { const target = document.getElementById('review-queue'); if (target) scrollToReadingTarget(target); }); return () => cancelAnimationFrame(frame); } }, [location.hash, location.key, query.isPending, restoringPosition]);
  const value = board.value ?? query.data;
  const contact = useWorkerPresence(value?.workers.items.map(worker => worker.id) ?? []);
  const more = Boolean(value && (value.workers.has_more || value.jobs.has_more));
  return <section aria-labelledby="delivery-title" data-reading-region="delivery" data-reading-ready={!!value}><div className="delivery-section-heading tab-heading"><h2 id="delivery-title">Delivery</h2><Link to={`${home}/jobs`}>Manage jobs →</Link></div><ReadState error={query.error} loading={query.isPending} empty={false} retained={!!value}>{value && <><DeliveryLanes value={value} home={home} presence={contact.error ? undefined : contact.data} />{board.error && <ReadState error={board.error} loading={false} empty={false}>{null}</ReadState>}{more && <div className="delivery-more"><p className="delivery-page-note">More open work is available. Lanes show the work loaded so far.</p><button type="button" onClick={() => { if (!query.isFetching) void archive.more(); }} disabled={query.isFetching || board.busy}>{board.busy ? 'Loading more work…' : 'Load more work'}</button></div>}</>}</ReadState></section>;
}
export function Delivery() {
  return <><DeliveryWork /><ReviewQueueView embedded /></>;
}
