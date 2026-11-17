import { afterEach, describe, expect, it, vi } from 'vitest';
import { browserSessionKey, browserRecoveryKey, clearBrowserSession, hasBrowserSession, loadBrowserSession, renewedBrowserSessionBinding, saveBrowserSession, sessionExpiry, terminalBrowserSession, type BrowserSessionBinding } from './browser-session';

function memoryStorage() {
  const values = new Map<string, string>();
  return { values, getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); }, removeItem: (key: string) => { values.delete(key); } };
}
const now = Date.parse('2026-10-15T12:00:00Z');
const binding: BrowserSessionBinding = { version: 1, projectID: 'project_one', instanceID: 'instance_one', sessionID: 'a'.repeat(32), expiresAt: '2026-10-16T12:00:00Z', selectedworkerID: 'worker_one' };
afterEach(() => vi.useRealTimers());

describe('per-tab browser session binding', () => {
  it('recovers a recreated tab by exact project or handle using public metadata only', () => {
    const tab = memoryStorage(), recovery = memoryStorage();
    const friendly = { ...binding, projectHandle: 'firemud' };
    expect(saveBrowserSession(friendly, tab, recovery)).toBe(true);
    tab.values.clear();
    expect(hasBrowserSession('firemud', tab, recovery)).toBe(true);
    expect(tab.values.size).toBe(0); // first paint is read-only
    expect(loadBrowserSession('other-project', undefined, tab, recovery)).toBeNull();
    expect(loadBrowserSession(undefined, undefined, tab, recovery)).toBeNull();
    expect(loadBrowserSession('firemud', undefined, tab, recovery)).toEqual(friendly);
    expect(JSON.parse(tab.getItem(browserSessionKey)!)).toEqual(friendly);
    expect(JSON.parse(recovery.getItem(browserRecoveryKey)!)).toEqual([friendly]);
    expect(recovery.getItem(browserRecoveryKey)).not.toMatch(/credential|token|allow_writes|can_edit_presentation/u);
    clearBrowserSession(tab, recovery);
    expect(hasBrowserSession('firemud', tab, recovery)).toBe(false);
    expect(loadBrowserSession('firemud', undefined, tab, recovery)).toBeNull();
  });

  it('keeps existing tab identities authoritative and other project recovery independent', () => {
    const one = memoryStorage(), two = memoryStorage(), recovery = memoryStorage();
    const newer = { ...binding, sessionID: 'b'.repeat(32), selectedworkerID: 'worker_two' };
    const other = { ...binding, projectID: 'project_two', projectHandle: 'other', sessionID: 'c'.repeat(32) };
    saveBrowserSession(binding, one, recovery);
    saveBrowserSession(newer, two, recovery);
    expect(loadBrowserSession(binding.projectID, undefined, one, recovery)).toEqual(binding);
    clearBrowserSession(one, recovery); // an older tab cannot erase a newer login's hint
    expect(loadBrowserSession(binding.projectID, undefined, one, recovery)).toEqual(newer);
    saveBrowserSession(other, two, recovery);
    clearBrowserSession(one, recovery);
    one.values.clear(); two.values.clear();
    expect(loadBrowserSession(binding.projectID, undefined, one, recovery)).toBeNull();
    expect(loadBrowserSession('other', undefined, two, recovery)).toEqual(other);
  });

  it('bounds recovery metadata and tolerates unavailable durable storage', () => {
    const tab = memoryStorage(), recovery = memoryStorage();
    for (let index = 0; index < 10; index++) saveBrowserSession({ ...binding, projectID: `project_${index}` }, tab, recovery);
    const retained = JSON.parse(recovery.getItem(browserRecoveryKey)!) as BrowserSessionBinding[];
    expect(retained).toHaveLength(8);
    expect(retained[0].projectID).toBe('project_2');
    tab.values.clear();
    expect(loadBrowserSession('project_0', undefined, tab, recovery)).toBeNull();
    expect(loadBrowserSession('project_9', undefined, tab, recovery)?.projectID).toBe('project_9');
    const denied = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); }, removeItem() { throw new Error('blocked'); } };
    expect(saveBrowserSession(binding, tab, denied)).toBe(true);
    expect(loadBrowserSession(binding.projectID, undefined, tab, denied)).toEqual(binding);
    for (const raw of ['{', 'x'.repeat(16385), JSON.stringify([{ ...binding, credential: 'secret' }]), JSON.stringify([binding, binding])]) {
      tab.values.clear(); recovery.setItem(browserRecoveryKey, raw);
      expect(hasBrowserSession(binding.projectID, tab, recovery)).toBe(false);
      expect(loadBrowserSession(binding.projectID, undefined, tab, recovery)).toBeNull();
    }
  });

  it('restores only bounded public binding metadata across reload without credential or private data', () => {
    const storage = memoryStorage();
    expect(saveBrowserSession(binding, storage)).toBe(true);
    expect([...storage.values.keys()]).toEqual([browserSessionKey]);
    const saved = storage.getItem(browserSessionKey)!;
    expect(JSON.parse(saved)).toEqual(binding);
    expect(saved).not.toContain('credential');
    expect(saved).not.toContain('token');
    expect(loadBrowserSession('project_one', undefined, storage)).toEqual(binding);
    // A reload/unmount alone does not call the explicit clear operation.
    expect(loadBrowserSession('project_one', undefined, storage)).toEqual(binding);
    clearBrowserSession(storage);
    expect(storage.values.size).toBe(0);
  });

  it('restores a friendly alias only with the same per-tab immutable session identity', () => {
    const one = memoryStorage(), two = memoryStorage();
    const friendly = { ...binding, projectHandle: 'firemud' };
    const other = { ...binding, projectID: 'project_two', projectHandle: 'firecontroller', sessionID: 'b'.repeat(32) };
    expect(saveBrowserSession(friendly, one)).toBe(true);
    expect(saveBrowserSession(other, two)).toBe(true);
    expect(loadBrowserSession('firemud', undefined, one)).toEqual(friendly);
    expect(loadBrowserSession(binding.projectID, undefined, one)).toEqual(friendly);
    expect(renewedBrowserSessionBinding(friendly, {session_id:binding.sessionID,instance_id:binding.instanceID,project_id:binding.projectID,expires_at:binding.expiresAt})).toEqual(friendly);
    const discarded = vi.fn();
    expect(loadBrowserSession('firecontroller', discarded, one)).toBeNull();
    expect(discarded).toHaveBeenCalledWith(friendly);
    expect(loadBrowserSession('firecontroller', undefined, two)).toEqual(other);
    expect(loadBrowserSession(undefined, undefined, two)).toBeNull();
  });

  it('clears corrupt, malformed, mismatched, unknown-field and secret-bearing records', () => {
    const invalid: unknown[] = [null, [], { ...binding, version: 2 }, { ...binding, sessionID: 'A'.repeat(32) }, { ...binding, instanceID: '../other' }, { ...binding, selectedworkerID: null }, { ...binding, expiresAt: '2026-02-30T12:00:00Z' }, { ...binding, expiresAt: '2026-10-16T12:00:00+00:00' }, { ...binding, credential: 'raw-private-token' }, { ...binding, projectHandle: null }, { ...binding, projectHandle: 'UPPER' }, { ...binding, projectHandle: '../other' }, { ...binding, projectHandle: 'a'.repeat(64) }];
    for (const value of invalid) {
      const storage = memoryStorage();
      storage.setItem(browserSessionKey, JSON.stringify(value));
      expect(loadBrowserSession('project_one', undefined, storage)).toBeNull();
      expect(storage.values.size).toBe(0);
    }
    for (const raw of ['{', 'x'.repeat(2049)]) {
      const storage = memoryStorage(); storage.setItem(browserSessionKey, raw);
      expect(loadBrowserSession('project_one', undefined, storage)).toBeNull();
      expect(storage.values.size).toBe(0);
    }
    for (const projectID of ['project_two', undefined]) {
      const storage = memoryStorage(); saveBrowserSession(binding, storage);
      const expired: BrowserSessionBinding[] = [];
      expect(loadBrowserSession(projectID, value => expired.push(value), storage)).toBeNull();
      expect(expired).toEqual([binding]);
      expect(storage.values.size).toBe(0);
    }
    expect(sessionExpiry('2026-10-16T12:00:00.123456789Z')).toBe(true);
  });

  it('saves, restores and renews the same server expiry across device clock skew while preserving worker selection', () => {
    vi.useFakeTimers();
    try {
      const serverExpiry = '2026-10-15T13:00:00Z';
      const serverBinding = { ...binding, expiresAt: serverExpiry };
      const current = { ...serverBinding, selectedworkerID: 'worker_two' };
      const renewal = { session_id: binding.sessionID, instance_id: binding.instanceID, project_id: binding.projectID, expires_at: serverExpiry };
      for (const offsetMinutes of [-2880, -10, 10, 2880]) {
        vi.setSystemTime(now + offsetMinutes * 60 * 1000);
        const storage = memoryStorage();
        expect(saveBrowserSession(serverBinding, storage)).toBe(true);
        expect(loadBrowserSession(binding.projectID, undefined, storage)).toEqual(serverBinding);
        const renewed = renewedBrowserSessionBinding(current, renewal);
        expect(renewed).toEqual(current);
        expect(saveBrowserSession(renewed!, storage)).toBe(true);
        expect(loadBrowserSession(binding.projectID, undefined, storage)).toEqual(current);
        expect(JSON.parse(storage.getItem(browserSessionKey)!)).toEqual(current);
      }
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps metadata for transient transport failures and clears only explicit or terminal access failures', () => {
    const storage = memoryStorage(); saveBrowserSession(binding, storage);
    for (const error of [{ code: 'connection_failed', status: 0 }, { code: 'deadline_exceeded', status: 0 }, { code: 'overloaded', status: 503 }]) {
      if (terminalBrowserSession(error)) clearBrowserSession(storage);
      expect(loadBrowserSession('project_one', undefined, storage)).toEqual(binding);
    }
    for (const error of [{ code: 'unauthorized', status: 401 }, { code: 'forbidden', status: 403 }, { code: 'instance_mismatch', status: 409 }]) {
      saveBrowserSession(binding, storage);
      if (terminalBrowserSession(error)) clearBrowserSession(storage);
      expect(storage.values.size).toBe(0);
    }
    const denied = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); }, removeItem() { throw new Error('blocked'); } };
    expect(saveBrowserSession(binding, denied)).toBe(false);
    expect(loadBrowserSession('project_one', undefined, denied)).toBeNull();
    expect(() => clearBrowserSession(denied)).not.toThrow();
  });

  it('retains only validated renewal expiry and the current worker, without saving permissions', () => {
    const storage = memoryStorage(); saveBrowserSession(binding, storage);
    const reply = { session_id: binding.sessionID, instance_id: binding.instanceID, project_id: binding.projectID, expires_at: '2026-10-16T12:04:00Z', allow_writes: true, can_edit_presentation: true };
    const current = { ...binding, selectedworkerID: 'worker_two' };
    const renewed = renewedBrowserSessionBinding(current, reply)!;
    expect(renewed).toEqual({ ...current, expiresAt: reply.expires_at });
    expect(saveBrowserSession(renewed, storage)).toBe(true);
    expect(JSON.parse(storage.getItem(browserSessionKey)!)).toEqual(renewed);
    expect(loadBrowserSession(binding.projectID, undefined, storage)).toEqual(renewed);
    for (const invalid of [{ ...reply, session_id: 'b'.repeat(32) }, { ...reply, instance_id: 'instance_other' }, { ...reply, project_id: 'project_other' }, { ...reply, expires_at: '2026-02-30T12:00:00Z' }]) {
      expect(renewedBrowserSessionBinding(current, invalid)).toBeNull();
      expect(loadBrowserSession(binding.projectID, undefined, storage)).toEqual(renewed);
    }
  });

  it('preserves worker changes when the recorded expiry lags a renewed cookie', () => {
    const storage = memoryStorage();
    const later = Date.parse(binding.expiresAt) + 48 * 60 * 60 * 1000;
    const selected = { ...binding, selectedworkerID: 'worker_two' };
    expect(saveBrowserSession(selected, storage)).toBe(true);
    expect(loadBrowserSession(binding.projectID, undefined, storage)).toEqual(selected);
    expect(Object.keys(JSON.parse(storage.getItem(browserSessionKey)!))).toEqual(Object.keys(binding));
    expect(saveBrowserSession({ ...selected, expiresAt: '9999-12-31T23:59:59Z' }, storage)).toBe(true);
    expect(storage.values.size).toBe(1);
  });

  it('removes the prior selected worker when retaining a new selection fails', () => {
    const storage = memoryStorage();
    saveBrowserSession(binding, storage);
    const failedWrite = { getItem: storage.getItem, removeItem: storage.removeItem, setItem() { throw new Error('quota denied'); } };
    expect(saveBrowserSession({ ...binding, selectedworkerID: 'worker_two' }, failedWrite)).toBe(false);
    expect(loadBrowserSession('project_one', undefined, storage)).toBeNull();
  });
});
