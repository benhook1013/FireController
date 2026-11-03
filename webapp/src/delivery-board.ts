import { ApiError } from './api';
import { maximumWindowDepth } from './reload-window';
import type { DeliverySnapshot, Page } from './types';

type BoardState = { value?: DeliverySnapshot; busy: boolean; error: unknown; depth: number; restoring: boolean };
function append<T extends { id: string }>(before: Page<T>, next: Page<T>): Page<T> {
  const items = new Map(before.items.map(item => [item.id, item]));
  for (const item of next.items) items.set(item.id, item);
  return { ...next, items: [...items.values()] };
}

// One deliberate bounded continuation appends to the board. A new first read
// discards its old tail; late completions cannot repopulate refreshed work.
export class DeliveryBoardArchive {
  private state: BoardState = { busy: false, error: undefined, depth: 0, restoring: false };
  private restoreDepth?: number;
  private generation = 0;
  private controller?: AbortController;
  private seen = new Set<string>();
  private listeners = new Set<() => void>();
  constructor(private read: (workers: string, jobs: string, signal: AbortSignal) => Promise<DeliverySnapshot>, depth = 0) {
    this.restoreDepth = Number.isInteger(depth) && depth >= 0 && depth <= maximumWindowDepth ? depth : 0;
    this.state.restoring = this.restoreDepth > 0;
  }
  snapshot = (): BoardState => this.state;
  subscribe = (listener: () => void): (() => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener); }; };
  private publish(state: BoardState): void { this.state = state; for (const listener of this.listeners) listener(); }
  cancel(): void { this.generation++; this.controller?.abort(); this.controller = undefined; }
  reset(value?: DeliverySnapshot): void {
    this.cancel(); this.seen.clear();
    const target = value ? this.restoreDepth ?? 0 : 0;
    this.publish({ value, busy: false, error: undefined, depth: 0, restoring: target > 0 || !value && Boolean(this.restoreDepth) });
    if (value) { this.restoreDepth = undefined; if (target) void this.restore(target); }
  }
  private async restore(target: number): Promise<void> {
    const generation = this.generation;
    while (this.state.depth < target && this.state.value && (this.state.value.workers.has_more || this.state.value.jobs.has_more)) {
      const before = this.state.depth;
      await this.more();
      if (generation !== this.generation) return;
      if (this.state.error || this.state.depth === before) break;
    }
    if (generation === this.generation) this.publish({ ...this.state, restoring: false });
  }
  async more(): Promise<void> {
    const before = this.state.value;
    if (!before || this.state.busy || !before.workers.has_more && !before.jobs.has_more) return;
    const workers = before.workers.has_more ? before.workers.cursor! : '';
    const jobs = before.jobs.has_more ? before.jobs.cursor! : '';
    const identity = JSON.stringify([workers, jobs]);
    if (this.seen.has(identity)) return;
    const generation = this.generation, controller = new AbortController();
    this.controller = controller; this.publish({ ...this.state, busy: true, error: undefined });
    const active = () => this.generation === generation && this.controller === controller && !controller.signal.aborted;
    try {
      const next = await this.read(workers, jobs, controller.signal);
      if (!active()) return;
      if (before.workers.has_more && next.workers.has_more && next.workers.cursor === workers || before.jobs.has_more && next.jobs.has_more && next.jobs.cursor === jobs) throw new ApiError('invalid_response', 'The work board returned a repeated continuation.');
      const nextIdentity = JSON.stringify([before.workers.has_more && next.workers.has_more ? next.workers.cursor : '', before.jobs.has_more && next.jobs.has_more ? next.jobs.cursor : '']);
      if (this.seen.has(nextIdentity)) throw new ApiError('invalid_response', 'The work board returned an earlier continuation.');
      this.seen.add(identity);
      this.publish({ depth: this.state.depth + 1, restoring: this.state.restoring, value: { workers: before.workers.has_more ? append(before.workers, next.workers) : before.workers, jobs: before.jobs.has_more ? append(before.jobs, next.jobs) : before.jobs }, busy: false, error: undefined });
    } catch (error) { if (active()) this.publish({ ...this.state, busy: false, error }); }
    finally { if (this.controller === controller) this.controller = undefined; }
  }
}
