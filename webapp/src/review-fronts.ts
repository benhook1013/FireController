import type { ReviewPullSummary } from './types';
import type { ReviewProgressItem } from './review-progress';

export interface ReviewQueuePriority { pull_number: number; head_sha: string; base_sha: string; base_ref: string }
export interface ReviewFrontChannel {
  queued_priority?: ReviewQueuePriority;
  source_progress?: { verified_pull_count: number; queue_pull_count: number; next_unverified_pull_number?: number };
  review_size?: import('./review-size').ReviewSize;
  channel: 'hosted' | 'cli'; state: 'ready' | 'held' | 'complete' | 'unknown';
  reason?: string; pull_number?: number; head_sha?: string; base_sha?: string; base_ref?: string;
  source_observed_at?: string; checked_at?: string;
}
export interface ReviewFronts {
  project_id: string; configured: boolean; repository_id: number;
  configuration_revision: number; policy_revision: number; snapshot_id?: string;
  queue: number[]; channels: ReviewFrontChannel[];
}
export interface ReviewQueueBadge { kind: 'front' | 'closed' | 'queued'; text: string; description: string; channels?: ('hosted' | 'cli')[] }
export interface ReviewChannelPosition { text: string; description?: string }
export const reviewFixesPending: ReviewChannelPosition = {text:'Fixes pending',description:'See the accepted findings in Review history.'};
export function parentBaseMovedDescription(parentPullNumber?: number): string {
  return parentPullNumber ? `Parent PR #${parentPullNumber} changed; this child still targets its earlier base.` : 'The parent changed; this child still targets its earlier base.';
}

// Queue position is useful even without a source frame, but is not admission.
// Keep an established hold visible instead of suggesting another request.
export function reviewChannelPosition(pull: ReviewPullSummary, snapshot: string, channel: 'hosted' | 'cli', fronts?: ReviewFronts, progress?: ReviewProgressItem): ReviewChannelPosition | undefined {
  if (pull.state !== 'open' || !fronts?.configured || fronts.snapshot_id !== snapshot || !fronts.queue.includes(pull.number)) return undefined;
  const front = fronts.channels.find(item => item.channel === channel);
  if (front?.queued_priority && matchesPriority(pull, front.queued_priority) && (front.state === 'unknown' || front.state === 'held' && ['reviews_paused','local_queue_position','restore_recovery_held'].includes(front.reason ?? ''))) {
    const rounds = progress?.pull_number === pull.number ? progress.channels.find(item => item.channel === channel) : undefined;
    if (rounds?.configured && (rounds.closed || rounds.tapered || !rounds.maximum_unbounded && rounds.used >= rounds.maximum)) return undefined;
    if (front.state === 'unknown') return {text:'Source unverified',description:'First in line. Check the current head and base in Review history.'};
    if (front.reason === 'reviews_paused') return {text:'Paused',description:'First in line. New reviews wait until scheduling resumes.'};
    if (front.reason === 'restore_recovery_held') return {text:'Held',description:'Restored review work still needs operator reconciliation.'};
    return {text:'Queue position',description:'First in line. Source checks are still outstanding.'};
  }
  if (!front) return undefined;
  if (front.pull_number !== pull.number) {
    const priority = front.queued_priority;
    const ahead = priority ? fronts.queue.indexOf(priority.pull_number) : -1;
    const here = fronts.queue.indexOf(pull.number);
    // Stored order establishes only queue position, never source admission.
    if (ahead >= 0 && here > ahead) return {text:'Queued',description:'An earlier PR goes first. Source checks are still outstanding.'};
    return undefined;
  }
  if (front.head_sha !== pull.head_sha || front.base_sha !== pull.base_sha || front.base_ref !== pull.base_ref) return undefined;
  if (front.state === 'held' && !['reviews_paused', 'local_queue_position'].includes(front.reason ?? '')) {
    const held = new Map<string, ReviewChannelPosition>([
      ['source_obligations_pending', {text:'Carried work',description:'Work from an earlier review holds new reviews. See Review history.'}],
      ['source_obligations_unavailable', {text:'Held',description:'FireController could not check earlier review work.'}],
      ['routed_findings_pending', {text:'Findings pending',description:'Open Routed findings in Review history.'}],
      ['external_hosted_pending', {text:'Manual results pending',description:'Finish the captured manual review in Review history.'}],
      ['review_evidence_pending', {text:'Results pending',description:'Check for captured results or decisions in Review history.'}],
      ['review_fixes_pending', reviewFixesPending],
      ['predecessor_unfinished', {text:'Dependency hold',description:'Check the parent PR’s remaining reviews and fixes.'}],
    ]);
    return held.get(front.reason ?? '') ?? {text:'Held',description:'See Review history for details before requesting a review.'};
  }
  const rounds = progress?.pull_number === pull.number ? progress.channels.find(item => item.channel === channel) : undefined;
  if (rounds?.configured && (rounds.closed || rounds.tapered || !rounds.maximum_unbounded && rounds.used >= rounds.maximum)) return undefined;
  if (front.state === 'ready') return {text:'Up next'};
  if (front.state === 'held' && ['reviews_paused', 'local_queue_position'].includes(front.reason ?? '')) return {text:'Queue position',description:front.reason === 'reviews_paused' ? 'First in line. New reviews wait until scheduling resumes.' : 'First in line. Source checks are still outstanding.'};
  return undefined;
}

function matchesPriority(pull: ReviewPullSummary, priority: ReviewQueuePriority): boolean {
  return pull.state === 'open' && pull.number === priority.pull_number && pull.head_sha === priority.head_sha && pull.base_sha === priority.base_sha && pull.base_ref === priority.base_ref;
}

// A contradictory loaded blocker invalidates that whole channel's explanation,
// including later Queued labels. An unloaded predecessor can still be a hold.
export function currentReviewFronts(snapshot: string, pulls: ReviewPullSummary[], fronts?: ReviewFronts): ReviewFronts | undefined {
  if (!fronts || fronts.snapshot_id !== snapshot) return undefined;
  return {...fronts,channels:fronts.channels.map(channel=>{
    const queued = pulls.find(item=>item.number===channel.queued_priority?.pull_number);
    if (queued && channel.queued_priority && !matchesPriority(queued,channel.queued_priority)) channel = {...channel,queued_priority:undefined};
    const pull=pulls.find(item=>item.number===channel.pull_number);
    if (!pull || pull.state==='open' && pull.head_sha===channel.head_sha && pull.base_sha===channel.base_sha && pull.base_ref===channel.base_ref) return channel;
    return {channel:channel.channel,state:'unknown',reason:'visible_source_changed',queued_priority:channel.queued_priority};
  })};
}

// Current source/control explanations never apply to another loaded snapshot.
// Merge-head distance, old attempts and completed history do not select fronts.
export function reviewQueueBadges(pull: ReviewPullSummary, snapshot: string, fronts?: ReviewFronts, progress?: ReviewProgressItem, channels?: ('hosted'|'cli')[]): ReviewQueueBadge[] {
  if (pull.state !== 'open') return [];
  const names = (channels: string[]) => channels.map(channel => channel === 'hosted' ? 'Hosted' : 'CLI').join(' + ');
  const closed = (channel: string) => {
    const value = progress?.pull_number === pull.number ? progress.channels.find(item => item.channel === channel) : undefined;
    return !!value?.configured && (value.closed || value.tapered || !value.maximum_unbounded && value.used >= value.maximum);
  };
  const selected=channels ?? (fronts?.configured ? fronts.channels.map(channel=>channel.channel) : []);
  // Closure is retained PR/channel control state, independent of the source
  // snapshot that happens to be loading. Missing controls never mean closed.
  if (selected.length > 0 && selected.every(closed)) return [{kind:'closed',text:'Review closed',description:'Current configured channels allow no further rounds; this is not a merge-readiness verdict.'}];
  if (!fronts?.configured || fronts.snapshot_id !== snapshot || !fronts.queue.includes(pull.number)) return [];
  const positions = new Map(fronts.queue.map((number, index) => [number, index]));
  const priorities = fronts.channels.flatMap(channel => {
    const priority = channel.queued_priority;
    if (!priority) return [];
    const index = positions.get(priority.pull_number);
    return index === undefined ? [] : [{ channel: channel.channel, priority, index }];
  });
  const first = Math.min(...priorities.map(item => item.index));
  const position = positions.get(pull.number)!;
  if (position === first) {
    const channels = priorities.filter(item => item.index === first && matchesPriority(pull, item.priority) && !closed(item.channel)).map(item => item.channel);
    if (channels.length) return [{kind:'front',text:'Review front',channels,description:`First PR with ${names(channels)} review rounds remaining. Each channel shows its own readiness and execution status.`}];
  }
  if (position <= first) return [];

  return [{kind:'queued',text:'Queued',description:'Behind the first PR with review rounds remaining.'}];
}
