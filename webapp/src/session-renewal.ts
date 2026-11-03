export const renewalThrottleMilliseconds = 5 * 60 * 1000;

type RenewalOptions<T> = {
  renew(signal: AbortSignal): Promise<T>;
  current(): boolean;
  visible(): boolean;
  success(value: T): void;
  failure(error: unknown): void;
  now?: () => number;
  initialAttemptAt?: number;
};

// Activity admits a finite request; elapsed time alone never schedules one.
// The same owner survives worker/view changes within one project connection.
export class BrowserSessionRenewal<T> {
  private readonly controller = new AbortController();
  private lastAttempt: number | undefined;
  private inFlight: Promise<void> | undefined;
  constructor(private readonly options: RenewalOptions<T>) { this.lastAttempt = options.initialAttemptAt; }

  activity(): Promise<void> | undefined {
    if (this.controller.signal.aborted || !this.options.current() || !this.options.visible()) return;
    if (this.inFlight) return this.inFlight;
    const now = this.options.now?.() ?? performance.now();
    if (this.lastAttempt !== undefined && now - this.lastAttempt < renewalThrottleMilliseconds) return;
    this.lastAttempt = now;
    const current = () => !this.controller.signal.aborted && this.options.current();
    const pending = Promise.resolve().then(() => {
      if (!current()) return;
      return this.options.renew(this.controller.signal).then(value => { if (current()) this.options.success(value); });
    }).catch(error => { if (current()) this.options.failure(error); }).finally(() => { if (this.inFlight === pending) this.inFlight = undefined; });
    this.inFlight = pending;
    return pending;
  }

  stop(): void { this.controller.abort(); }
}

export function listenForSessionActivity<T>(renewal: BrowserSessionRenewal<T>, target: EventTarget, visibility: EventTarget): () => void {
  const activity = () => { void renewal.activity(); };
  target.addEventListener('pointerdown', activity, { passive: true });
  target.addEventListener('keydown', activity);
  target.addEventListener('pageshow', activity);
  visibility.addEventListener('visibilitychange', activity);
  return () => {
    target.removeEventListener('pointerdown', activity);
    target.removeEventListener('keydown', activity);
    target.removeEventListener('pageshow', activity);
    visibility.removeEventListener('visibilitychange', activity);
    renewal.stop();
  };
}
