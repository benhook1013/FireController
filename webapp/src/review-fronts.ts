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

// A contradictory loaded blocker invalidates that whole channel's explanation,
// including later Queued labels. An unloaded predecessor can still be a hold.
export function currentReviewFronts(snapshot: string, pulls: ReviewPullSummary[], fronts?: ReviewFronts, progress?: ReviewProgressItem[]): ReviewFronts | undefined {
  if (!fronts || fronts.snapshot_id !== snapshot) return undefined;
  return {...fronts,channels:fronts.channels.map(channel=>{
    // A dispatch stop or absent provider frame does not erase local queue order.
    // Derive only through known predecessors, never through an unloaded gap.
    if (progress !== undefined && (channel.state === 'held' && channel.reason === 'reviews_paused' || channel.state === 'unknown' && ['source_frame_unavailable', 'configuration_changed', 'visible_source_changed', 'source_prefix_incomplete'].includes(channel.reason ?? ''))) {
      for (const number of fronts.queue) {
        const pull = pulls.find(item => item.number === number);
        if (!pull || pull.state === 'unknown') break;
        if (pull.state !== 'open') continue;
        const round = progress.find(item => item.pull_number === number)?.channels.find(item => item.channel === channel.channel);
        if (!round) break;
        if (round.configured && (round.closed || round.tapered || !round.maximum_unbounded && round.used >= round.maximum)) continue;
        return {...channel,state:'held',reason:'local_queue_position',pull_number:number,head_sha:pull.head_sha,base_sha:pull.base_sha,base_ref:pull.base_ref};
      }
      return {...channel,pull_number:undefined,head_sha:undefined,base_sha:undefined,base_ref:undefined};
    }
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
