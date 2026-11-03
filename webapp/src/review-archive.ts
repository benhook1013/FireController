import { ApiError } from './api';
import type { Page, ReviewSnapshot } from './types';

export type ReviewArchivePage<T> = ReviewSnapshot & Page<T>;
export type ReviewArchiveState<T> = { items: T[]; hasMore: boolean; busy: boolean; error: unknown; snapshot?: string };
const metadata = ['project_id', 'revision', 'snapshot_id', 'source', 'observed_at', 'recorded_at'] as const;

// Continuation is explicit and stays outside automatic query refetching. A
// refreshed first page owns a new generation and discards its former tail.
export class ReviewArchive<T> {
  private state: ReviewArchiveState<T> = { items: [], hasMore: false, busy: false, error: undefined };
  private first: ReviewArchivePage<T> | undefined;
  private cursor: string | undefined;
  private seenCursors = new Set<string>();
  private controller: AbortController | undefined;
  private listeners = new Set<() => void>();
  private generation = 0;
  constructor(private readonly read: (cursor: string, signal: AbortSignal) => Promise<ReviewArchivePage<T>>, private readonly identity: (item: T) => string) {}
  snapshot = (): ReviewArchiveState<T> => this.state;
  subscribe = (listener: () => void): (() => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  private publish(state: ReviewArchiveState<T>): void { this.state = state; for (const listener of this.listeners) listener(); }
  cancel(): void { this.generation++; this.controller?.abort(); this.controller = undefined; }
  reset(first?: ReviewArchivePage<T>, busy = false, error?: unknown): void {
    this.cancel(); this.first = first; this.cursor = first?.cursor; this.seenCursors = new Set(first?.cursor ? [first.cursor] : []);
    this.publish({ items: first?.items ?? [], hasMore: first?.has_more ?? false, busy, error, snapshot: first?.snapshot_id });
  }
  async more(): Promise<void> {
    if (this.state.busy || !this.first || !this.state.hasMore || !this.cursor) return;
    const generation = this.generation, first = this.first, cursor = this.cursor, controller = new AbortController();
    this.controller = controller;
    this.publish({ ...this.state, busy: true, error: undefined });
    const active = () => this.generation === generation && this.controller === controller && !controller.signal.aborted;
    try {
      const page = await this.read(cursor, controller.signal);
      if (!active()) return;
      const identifiers = new Set(this.state.items.map(this.identity));
      if (metadata.some(key => page[key] !== first[key]) || page.items.some(item => identifiers.has(this.identity(item))) || page.has_more && (!page.cursor || this.seenCursors.has(page.cursor))) throw new ApiError('invalid_response', 'The retained review archive returned an inconsistent continuation.');
      if (page.cursor) this.seenCursors.add(page.cursor);
      this.cursor = page.cursor;
      this.publish({ items: [...this.state.items, ...page.items], hasMore: page.has_more, busy: false, error: undefined, snapshot: first.snapshot_id });
    } catch (error) { if (active()) this.publish({ ...this.state, busy: false, error }); }
    finally { if (this.controller === controller) this.controller = undefined; }
  }
}
