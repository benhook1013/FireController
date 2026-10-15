import { describe, expect, it } from 'vitest';
import { browserSessionKey, clearBrowserSession, loadBrowserSession, saveBrowserSession, sessionExpiry, terminalBrowserSession, type BrowserSessionBinding } from './browser-session';

function memoryStorage() {
  const values = new Map<string, string>();
  return { values, getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => { values.set(key, value); }, removeItem: (key: string) => { values.delete(key); } };
}
const now = Date.parse('2026-10-15T12:00:00Z');
const binding: BrowserSessionBinding = { version: 1, projectID: 'project_one', instanceID: 'instance_one', sessionID: 'a'.repeat(32), expiresAt: '2026-10-16T12:00:00Z', selectedworkerID: 'worker_one' };

describe('per-tab browser session binding', () => {
  it('restores only bounded public binding metadata across reload without credential or private data', () => {
    const storage = memoryStorage();
    expect(saveBrowserSession(binding, storage, now)).toBe(true);
    expect([...storage.values.keys()]).toEqual([browserSessionKey]);
    const saved = storage.getItem(browserSessionKey)!;
    expect(JSON.parse(saved)).toEqual(binding);
    expect(saved).not.toContain('credential');
    expect(saved).not.toContain('token');
    expect(loadBrowserSession('project_one', undefined, storage, now)).toEqual(binding);
    // A reload/unmount alone does not call the explicit clear operation.
    expect(loadBrowserSession('project_one', undefined, storage, now)).toEqual(binding);
    clearBrowserSession(storage);
    expect(storage.values.size).toBe(0);
  });

  it('clears corrupt, expired, mismatched, unknown-field and secret-bearing records', () => {
    const invalid: unknown[] = [null, [], { ...binding, version: 2 }, { ...binding, sessionID: 'A'.repeat(32) }, { ...binding, instanceID: '../other' }, { ...binding, selectedworkerID: null }, { ...binding, expiresAt: '2026-02-30T12:00:00Z' }, { ...binding, expiresAt: '2026-10-16T12:00:00+00:00' }, { ...binding, credential: 'raw-private-token' }];
    for (const value of invalid) {
      const storage = memoryStorage();
      storage.setItem(browserSessionKey, JSON.stringify(value));
      expect(loadBrowserSession('project_one', undefined, storage, now)).toBeNull();
      expect(storage.values.size).toBe(0);
    }
    for (const raw of ['{', 'x'.repeat(2049)]) {
      const storage = memoryStorage(); storage.setItem(browserSessionKey, raw);
      expect(loadBrowserSession('project_one', undefined, storage, now)).toBeNull();
      expect(storage.values.size).toBe(0);
    }
    for (const projectID of ['project_two', undefined]) {
      const storage = memoryStorage(); saveBrowserSession(binding, storage, now);
      const expired: BrowserSessionBinding[] = [];
      expect(loadBrowserSession(projectID, value => expired.push(value), storage, now)).toBeNull();
      expect(expired).toEqual([binding]);
      expect(storage.values.size).toBe(0);
    }
    const storage = memoryStorage(); saveBrowserSession(binding, storage, now);
    const expired: BrowserSessionBinding[] = [];
    expect(loadBrowserSession('project_one', value => expired.push(value), storage, Date.parse(binding.expiresAt))).toBeNull();
    expect(expired).toEqual([binding]);
    expect(storage.values.size).toBe(0);
    expect(sessionExpiry('2026-10-16T12:00:00.123456789Z')).toBe(true);
  });

  it('keeps metadata for transient transport failures and clears only explicit or terminal access failures', () => {
    const storage = memoryStorage(); saveBrowserSession(binding, storage, now);
    for (const error of [{ code: 'connection_failed', status: 0 }, { code: 'deadline_exceeded', status: 0 }, { code: 'overloaded', status: 503 }]) {
      if (terminalBrowserSession(error)) clearBrowserSession(storage);
      expect(loadBrowserSession('project_one', undefined, storage, now)).toEqual(binding);
    }
    for (const error of [{ code: 'unauthorized', status: 401 }, { code: 'forbidden', status: 403 }, { code: 'instance_mismatch', status: 409 }]) {
      saveBrowserSession(binding, storage, now);
      if (terminalBrowserSession(error)) clearBrowserSession(storage);
      expect(storage.values.size).toBe(0);
    }
    const denied = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); }, removeItem() { throw new Error('blocked'); } };
    expect(saveBrowserSession(binding, denied, now)).toBe(false);
    expect(loadBrowserSession('project_one', undefined, denied, now)).toBeNull();
    expect(() => clearBrowserSession(denied)).not.toThrow();
  });

  it('removes the prior selected worker when retaining a new selection fails', () => {
    const storage = memoryStorage();
    saveBrowserSession(binding, storage, now);
    const failedWrite = { getItem: storage.getItem, removeItem: storage.removeItem, setItem() { throw new Error('quota denied'); } };
    expect(saveBrowserSession({ ...binding, selectedworkerID: 'worker_two' }, failedWrite, now)).toBe(false);
    expect(loadBrowserSession('project_one', undefined, storage, now)).toBeNull();
  });
});
