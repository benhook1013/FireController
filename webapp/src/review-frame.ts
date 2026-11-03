import type { ReviewQueue } from './types';
import type { ReviewFronts } from './review-fronts';

export interface ReviewQueueFrame { queue: ReviewQueue; fronts?: ReviewFronts }

// A snapshot change alone does not mean the front became Queued. Publish the
// queue and its position assessment together; never apply an older assessment
// to newer PR facts merely to retain a color.
export function reviewQueueFrame(queue?: ReviewQueue, fronts?: ReviewFronts): ReviewQueueFrame | undefined {
  if (!queue) return undefined;
  if (!queue.configured || fronts && !fronts.configured) return { queue, fronts };
  return fronts?.snapshot_id === queue.snapshot_id ? { queue, fronts } : undefined;
}
