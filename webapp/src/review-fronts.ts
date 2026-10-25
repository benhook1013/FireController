import type { ReviewPullSummary } from './types';
import type { ReviewProgressItem } from './review-progress';

export interface ReviewFrontChannel {
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

// Queue position is useful even without a source frame, but is not admission.
// Keep an established hold visible instead of suggesting another request.
export function reviewChannelPosition(pull: ReviewPullSummary, snapshot: string, channel: 'hosted' | 'cli', fronts?: ReviewFronts, progress?: ReviewProgressItem): ReviewChannelPosition | undefined {
  if (pull.state !== 'open' || !fronts?.configured || fronts.snapshot_id !== snapshot || !fronts.queue.includes(pull.number)) return undefined;
  const front = fronts.channels.find(item => item.channel === channel);
  if (!front || front.pull_number !== pull.number || front.head_sha !== pull.head_sha || front.base_sha !== pull.base_sha || front.base_ref !== pull.base_ref) return undefined;
  if (front.state === 'held' && !['reviews_paused', 'local_queue_position'].includes(front.reason ?? '')) {
    const held = new Map<string, ReviewChannelPosition>([
      ['source_obligations_pending', {text:'Carried work',description:'Unresolved work carried over at cutover holds new reviews. Read the carried work in Review details.'}],
      ['source_obligations_unavailable', {text:'Held',description:'Carried review work could not be checked. New reviews remain held.'}],
      ['routed_findings_pending', {text:'Findings pending',description:'Incoming routed findings hold new reviews.'}],
      ['review_evidence_pending', {text:'Results pending',description:'Retained review results still need to be settled before another review.'}],
      ['predecessor_unfinished', {text:'Dependency pending',description:'This PR depends on unfinished predecessor work.'}],
    ]);
    return held.get(front.reason ?? '') ?? {text:'Held',description:'The stored admission check holds new reviews. Check review details before requesting another review.'};
  }
  const rounds = progress?.pull_number === pull.number ? progress.channels.find(item => item.channel === channel) : undefined;
  if (rounds?.configured && (rounds.closed || rounds.tapered || !rounds.maximum_unbounded && rounds.used >= rounds.maximum)) return undefined;
  if (front.state === 'ready') return {text:'Up next'};
  if (front.state === 'held' && ['reviews_paused', 'local_queue_position'].includes(front.reason ?? '')) return {text:'Queue position',description:front.reason === 'reviews_paused' ? 'First remaining position in this channel; review dispatch is stopped.' : 'First remaining position in this channel. Source and admission have not been verified.'};
  return undefined;
}

// A contradictory loaded blocker invalidates that whole channel's explanation,
// including later Queued labels. An unloaded predecessor can still be a hold.
export function currentReviewFronts(snapshot: string, pulls: ReviewPullSummary[], fronts?: ReviewFronts): ReviewFronts | undefined {
  if (!fronts || fronts.snapshot_id !== snapshot) return undefined;
  return {...fronts,channels:fronts.channels.map(channel=>{
    const pull=pulls.find(item=>item.number===channel.pull_number);
    if (!pull || pull.state==='open' && pull.head_sha===channel.head_sha && pull.base_sha===channel.base_sha && pull.base_ref===channel.base_ref) return channel;
    return {channel:channel.channel,state:'unknown',reason:'visible_source_changed'};
  })};
}

// Current source/control explanations never apply to another loaded snapshot.
// Merge-head distance, old attempts and completed history do not select fronts.
export function reviewQueueBadges(pull: ReviewPullSummary, snapshot: string, fronts?: ReviewFronts, progress?: ReviewProgressItem): ReviewQueueBadge[] {
  if (pull.state !== 'open' || !fronts?.configured || fronts.snapshot_id !== snapshot || !fronts.queue.includes(pull.number)) return [];
  const names = (channels: string[]) => channels.map(channel => channel === 'hosted' ? 'Hosted' : 'CLI').join(' + ');
  const closed = (channel: string) => {
    const value = progress?.pull_number === pull.number ? progress.channels.find(item => item.channel === channel) : undefined;
    return !!value?.configured && (value.closed || value.tapered || !value.maximum_unbounded && value.used >= value.maximum);
  };
  if (fronts.channels.length > 0 && fronts.channels.every(channel => closed(channel.channel))) return [{kind:'closed',text:'Review closed',description:'Current configured channels allow no further rounds; this is not a merge-readiness verdict.'}];
  const matching = fronts.channels.filter(channel => (channel.state === 'ready' || channel.state === 'held' && ['reviews_paused','local_queue_position'].includes(channel.reason ?? '')) && channel.pull_number === pull.number && channel.head_sha === pull.head_sha && channel.base_sha === pull.base_sha && channel.base_ref === pull.base_ref && !closed(channel.channel));
  if (matching.length) return [{kind:'front',text:'Review front',channels:matching.map(channel => channel.channel),description:'Next position under the stored queue and local round controls; requests still recheck source and admission.'}];
  const position = fronts.queue.indexOf(pull.number);
  const ahead = fronts.channels.filter(channel => !closed(channel.channel) && (channel.state === 'ready' || channel.state === 'held') && channel.pull_number !== undefined && fronts.queue.indexOf(channel.pull_number) >= 0 && fronts.queue.indexOf(channel.pull_number) < position);
  return ahead.length ? [{kind:'queued',text:'Queued',description:`Behind the established ${names(ahead.map(channel => channel.channel))} front; provider and admission checks remain separate.`}] : [];
}
