import { afterEach, describe, expect, it, vi } from 'vitest';
import { BrowserSessionRenewal, listenForSessionActivity, renewalThrottleMilliseconds } from './session-renewal';

function deferred<T>() { let resolve!: (value: T) => void; let reject!: (error: unknown) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
afterEach(() => { vi.useRealTimers(); });

describe('activity-driven browser renewal', () => {
  it('renews a visible resume once, bounds overlapping activity, and never renews from elapsed time alone', async () => {
    vi.useFakeTimers(); let now = 0, visible = true;
    const reply = deferred<string>(), success = vi.fn(), failure = vi.fn(), renew = vi.fn(() => reply.promise);
    const owner = new BrowserSessionRenewal({ renew, current: () => true, visible: () => visible, success, failure, now: () => now });
    const first = owner.activity(); expect(owner.activity()).toBe(first); await Promise.resolve(); expect(renew).toHaveBeenCalledTimes(1);
    now = renewalThrottleMilliseconds * 2; expect(owner.activity()).toBe(first); expect(renew).toHaveBeenCalledTimes(1);
    reply.resolve('valid'); await first; expect(success).toHaveBeenCalledWith('valid'); expect(failure).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(renewalThrottleMilliseconds * 10); expect(renew).toHaveBeenCalledTimes(1); expect(vi.getTimerCount()).toBe(0);
    visible = false; expect(owner.activity()).toBeUndefined(); expect(renew).toHaveBeenCalledTimes(1);
    visible = true; await owner.activity(); expect(renew).toHaveBeenCalledTimes(2); owner.stop();
  });

  it('counts fresh login as recent renewal and retries transient failures only on later eligible activity', async () => {
    let now = 100;
    const failure = vi.fn(), success = vi.fn(), renew = vi.fn().mockRejectedValueOnce(new Error('temporary')).mockResolvedValue('valid');
    const owner = new BrowserSessionRenewal({ renew, current: () => true, visible: () => true, success, failure, now: () => now, initialAttemptAt: now });
    expect(owner.activity()).toBeUndefined(); now += renewalThrottleMilliseconds - 1; expect(owner.activity()).toBeUndefined(); expect(renew).not.toHaveBeenCalled();
    now++; await owner.activity(); expect(failure).toHaveBeenCalledTimes(1); expect(success).not.toHaveBeenCalled();
    expect(owner.activity()).toBeUndefined(); now += renewalThrottleMilliseconds; expect(renew).toHaveBeenCalledTimes(1);
    await owner.activity(); expect(renew).toHaveBeenCalledTimes(2); expect(success).toHaveBeenCalledWith('valid'); owner.stop();
  });

  it('ignores a disconnected owner before dispatch and late success or failure after replacement', async () => {
    const never = vi.fn(async () => 'never');
    const stopped = new BrowserSessionRenewal({ renew: never, current: () => true, visible: () => true, success: vi.fn(), failure: vi.fn() });
    const pending = stopped.activity(); stopped.stop(); await pending; expect(never).not.toHaveBeenCalled();
    for (const failed of [false, true]) {
      let current = true; const reply = deferred<string>(), success = vi.fn(), failure = vi.fn(); let signal: AbortSignal | undefined;
      const owner = new BrowserSessionRenewal({ renew: selected => { signal = selected; return reply.promise; }, current: () => current, visible: () => true, success, failure });
      const request = owner.activity(); await Promise.resolve(); current = false; owner.stop(); expect(signal?.aborted).toBe(true);
      if (failed) reply.reject(new Error('late')); else reply.resolve('late'); await request;
      expect(success).not.toHaveBeenCalled(); expect(failure).not.toHaveBeenCalled(); expect(owner.activity()).toBeUndefined();
    }
  });

  it('uses pointer, keyboard, visible return and page visits while removing all listeners on disconnect', async () => {
    let now = 0, visible = true; const target = new EventTarget(), visibility = new EventTarget(), renew = vi.fn(async () => 'valid');
    const owner = new BrowserSessionRenewal({ renew, current: () => true, visible: () => visible, success: vi.fn(), failure: vi.fn(), now: () => now });
    const stop = listenForSessionActivity(owner, target, visibility);
    target.dispatchEvent(new Event('pointerdown')); await owner.activity(); expect(renew).toHaveBeenCalledTimes(1);
    target.dispatchEvent(new Event('keydown')); await owner.activity(); expect(renew).toHaveBeenCalledTimes(1);
    now += renewalThrottleMilliseconds; visible = false; visibility.dispatchEvent(new Event('visibilitychange')); target.dispatchEvent(new Event('keydown')); expect(renew).toHaveBeenCalledTimes(1);
    visible = true; visibility.dispatchEvent(new Event('visibilitychange')); await owner.activity(); expect(renew).toHaveBeenCalledTimes(2);
    now += renewalThrottleMilliseconds; target.dispatchEvent(new Event('pageshow')); await owner.activity(); expect(renew).toHaveBeenCalledTimes(3);
    stop(); now += renewalThrottleMilliseconds; for (const kind of ['pointerdown', 'keydown', 'pageshow']) target.dispatchEvent(new Event(kind)); visibility.dispatchEvent(new Event('visibilitychange')); await Promise.resolve(); expect(renew).toHaveBeenCalledTimes(3);
  });
});
