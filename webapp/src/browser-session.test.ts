import { afterEach, describe, expect, it, vi } from 'vitest';
import { browserSessionKey, clearBrowserSession, loadBrowserSession, renewedBrowserSessionBinding, saveBrowserSession, sessionExpiry, terminalBrowserSession, type BrowserSessionBinding } from './browser-session';

function memoryStorage() {
  const values = new Map<string, string>();
  return { values, getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); }, removeItem: (key: string) => { values.delete(key); } };
}
const now = Date.parse('2026-10-15T12:00:00Z');
const binding: BrowserSessionBinding = { version: 1, projectID: 'project_one', instanceID: 'instance_one', sessionID: 'a'.repeat(32), expiresAt: '2026-10-16T12:00:00Z', selectedworkerID: 'worker_one' };
afterEach(() => vi.useRealTimers());

describe('per-tab browser session binding', () => {
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

  it('clears corrupt, malformed, mismatched, unknown-field and secret-bearing records', () => {
    const invalid: unknown[] = [null, [], { ...binding, version: 2 }, { ...binding, sessionID: 'A'.repeat(32) }, { ...binding, instanceID: '../other' }, { ...binding, selectedworkerID: null }, { ...binding, expiresAt: '2026-02-30T12:00:00Z' }, { ...binding, expiresAt: '2026-10-16T12:00:00+00:00' }, { ...binding, credential: 'raw-private-token' }];
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
