import { QueryClient, QueryObserver } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from './api';
import { runMessageWatch, type MessageWatchSource } from './events';
import type { MessageEventPage } from './types';
import { refreshEventQueries } from './queries';

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

const prefix = ['fc', 'session', 'instance', 'project', 'worker', 'thread', 'conversation'];

describe('event-driven query refresh', () => {
  it('replaces a pending first read and retains the committed reply after the old response arrives', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
    const scope = new AbortController();
    const key = [...prefix, ''];
    let release!: (value: string[]) => void;
    const firstResponse = new Promise<string[]>(resolve => { release = resolve; });
    let calls = 0;
    let firstSignal: AbortSignal | undefined;
    const observer = new QueryObserver(client, {
      queryKey: key,
      queryFn: ({ signal }) => {
        calls += 1;
        if (calls === 1) { firstSignal = signal; return firstResponse; }
        return Promise.resolve(['original', 'committed reply']);
      },
    });
    const unsubscribe = observer.subscribe(() => {});
    client.setQueryData([...prefix, 'older-page'], ['older message']);
    try {
      await refreshEventQueries(client, prefix, scope.signal);
      expect(firstSignal?.aborted).toBe(true);
      expect(calls).toBe(2);
      expect(client.getQueryData(key)).toEqual(['original', 'committed reply']);
      expect(client.getQueryState([...prefix, 'older-page'])?.isInvalidated).toBe(true);

      // The transport may finish an old response even after cancellation.
      release(['original']);
      await firstResponse;
      await Promise.resolve();
      expect(client.getQueryData(key)).toEqual(['original', 'committed reply']);
    } finally { unsubscribe(); client.clear(); }
  });

  it('cannot start another read when scope cancellation happens while the old read is cancelled', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
    const scope = new AbortController();
    const key = [...prefix, ''];
    let release!: (value: string[]) => void;
    const firstResponse = new Promise<string[]>(resolve => { release = resolve; });
    let calls = 0;
    const observer = new QueryObserver(client, {
      queryKey: key,
      queryFn: ({ signal }) => {
        calls += 1;
        signal.addEventListener('abort', () => scope.abort(), { once: true });
        return firstResponse;
      },
    });
    const unsubscribe = observer.subscribe(() => {});
    try {
      await refreshEventQueries(client, prefix, scope.signal);
      expect(scope.signal.aborted).toBe(true);
      expect(calls).toBe(1);
      await refreshEventQueries(client, prefix, scope.signal);
      expect(calls).toBe(1);
      release(['old context']);
      await firstResponse;
      await Promise.resolve();
      expect(client.getQueryData(key)).toBeUndefined();
    } finally { unsubscribe(); client.clear(); }
  });
});


function activeQuery(client: QueryClient, key: string[], queryFn: () => Promise<string[]>) {
  client.setQueryData(key, ['old']);
  return new QueryObserver(client, { queryKey: key, queryFn }).subscribe(() => {});
}

function testClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
}

describe('view-local refresh failures', () => {
  it('retains a missing view error while refreshing another active view and invalidating inactive pages', async () => {
    const client = testClient();
    const missing = new ApiError('not_found', 'Missing conversation', 404);
    const missingKey = [...prefix, 'missing'];
    const healthyKey = [...prefix, 'healthy'];
    const inactiveKey = [...prefix, 'inactive'];
    const unsubscribeMissing = activeQuery(client, missingKey, async () => { throw missing; });
    const unsubscribeHealthy = activeQuery(client, healthyKey, async () => ['committed message']);
    client.setQueryData(inactiveKey, ['older']);
    try {
      await expect(refreshEventQueries(client, prefix, new AbortController().signal)).resolves.toBeUndefined();
      expect(client.getQueryState(missingKey)?.error).toBe(missing);
      expect(client.getQueryState(missingKey)?.status).toBe('error');
      expect(client.getQueryData(healthyKey)).toEqual(['committed message']);
      expect(client.getQueryState(inactiveKey)?.isInvalidated).toBe(true);
    } finally { unsubscribeMissing(); unsubscribeHealthy(); client.clear(); }
  });

  it.each([
    new ApiError('unauthorized', 'Access ended', 401),
    new ApiError('forbidden', 'Access ended', 403),
    new ApiError('instance_mismatch', 'Wrong instance', 409),
    new ApiError('invalid_binding', 'Wrong binding'),
    new ApiError('invalid_response', 'Wrong response binding', 404),
    new ApiError('storage_unavailable', 'Try again', 500),
    new ApiError('not_found', 'Inconsistent error status', 403),
    new ApiError('not_found', 'Missing HTTP status'),
  ])('propagates $code/$status after a faster missing-view failure', async (failure) => {
    const client = testClient();
    let reject!: (reason: unknown) => void;
    const later = new Promise<string[]>((_resolve, rejectPromise) => { reject = rejectPromise; });
    const unsubscribeMissing = activeQuery(client, [...prefix, 'missing'], async () => { throw new ApiError('not_found', 'Missing conversation', 404); });
    const unsubscribeOther = activeQuery(client, [...prefix, 'other'], () => later);
    try {
      const refresh = refreshEventQueries(client, prefix, new AbortController().signal);
      const assertion = expect(refresh).rejects.toBe(failure);
      await Promise.resolve();
      reject(failure);
      await assertion;
      expect(client.getQueryState([...prefix, 'other'])?.error).toBe(failure);
    } finally { unsubscribeMissing(); unsubscribeOther(); client.clear(); }
  });

  it.each([
    new ApiError('unauthorized', 'Access ended', 401),
    new ApiError('forbidden', 'Access ended', 403),
    new ApiError('instance_mismatch', 'Wrong instance', 409),
    new ApiError('invalid_binding', 'Wrong binding'),
    new ApiError('invalid_response', 'Wrong response binding'),
  ])('prioritizes $code over an earlier transient error', async (failure) => {
    const client = testClient();
    const unsubscribeTransient = activeQuery(client, [...prefix, 'transient'], async () => { throw new ApiError('storage_unavailable', 'Try again', 500); });
    const unsubscribeTerminal = activeQuery(client, [...prefix, 'terminal'], async () => { await Promise.resolve(); throw failure; });
    try {
      await expect(refreshEventQueries(client, prefix, new AbortController().signal)).rejects.toBe(failure);
    } finally { unsubscribeTransient(); unsubscribeTerminal(); client.clear(); }
  });

  it('keeps the snapshot first page untouched when only later pages need refresh', async () => {
    const client = testClient();
    const first = vi.fn(async () => ['unwanted first-page read']);
    const older = vi.fn(async () => ['fresh older page']);
    const unsubscribeFirst = activeQuery(client, [...prefix, ''], first);
    const unsubscribeOlder = activeQuery(client, [...prefix, 'older'], older);
    try {
      await refreshEventQueries(client, prefix, new AbortController().signal, true);
      expect(first).not.toHaveBeenCalled();
      expect(older).toHaveBeenCalledTimes(1);
      expect(client.getQueryState([...prefix, ''])?.isInvalidated).toBe(false);
      expect(client.getQueryData([...prefix, 'older'])).toEqual(['fresh older page']);
    } finally { unsubscribeFirst(); unsubscribeOlder(); client.clear(); }
  });
});

async function flushMicrotasks() {
  for (let i = 0; i < 80; i += 1) await Promise.resolve();
}

describe('query refresh with the actual message watcher', () => {
  it.each([false, true])('keeps a missing conversation local and preserves cursor replay when transient=%s', async (transient) => {
    vi.useFakeTimers();
    const client = testClient();
    const scope = new AbortController();
    const missing = new ApiError('not_found', 'Missing conversation', 404);
    const missingKey = [...prefix, 'missing'];
    const healthyKey = [...prefix, 'healthy'];
    let healthyReads = 0;
    const unsubscribeMissing = activeQuery(client, missingKey, async () => { throw missing; });
    const unsubscribeHealthy = activeQuery(client, healthyKey, async () => {
      healthyReads += 1;
      // The initial snapshot succeeds; only the first event refresh fails.
      if (transient && healthyReads === 2) throw new ApiError('storage_unavailable', 'Try again', 500);
      return [`fresh read ${healthyReads}`];
    });
    const page: MessageEventPage = {
      items: [{ sequence: 1, project_id: 'project', type: 'message.sent', resource_id: 'message', revision: 1, actor_id: 'sender', payload: { conversation_id: 'conversation', recipient_worker_id: 'worker' }, created_at: '2026-01-01T00:00:00Z' }],
      cursor: 'processed_cursor', has_more: false, timed_out: false,
    };
    const cursors: string[] = [];
    const statuses: string[] = [];
    const source: MessageWatchSource = {
      projectId: 'project',
      snapshot: async () => ({ inbox: { items: [], has_more: false }, event_cursor: 'snapshot_cursor' }),
      openStream: async (_worker, cursor, _limit, signal) => {
        cursors.push(cursor);
        let streamController!: ReadableStreamDefaultController<Uint8Array>;
        const stream = new ReadableStream<Uint8Array>({ start(controller) {
          streamController = controller;
          if (cursors.length <= (transient ? 2 : 1)) {
            controller.enqueue(new TextEncoder().encode(`event: message-events\nid: ${page.cursor}\ndata: ${JSON.stringify(page)}\n\n`));
            controller.close();
          }
        } });
        const close = () => { signal.removeEventListener('abort', close); try { streamController.close(); } catch { /* Already closed. */ } };
        signal.addEventListener('abort', close, { once: true });
        return { response: new Response(stream, { headers: { 'content-type': 'text/event-stream' } }), close };
      },
      validatePage: (value): value is MessageEventPage => typeof value === 'object' && value !== null && 'cursor' in value,
      readFailure: async response => new ApiError('service_error', 'Service error', response.status),
      parseStreamError: () => new ApiError('stream_failed', 'Stream failed'),
    };
    let watch: Promise<void> | undefined;
    try {
      watch = runMessageWatch(source, 'worker', {}, {
        snapshot: () => refreshEventQueries(client, prefix, scope.signal),
        events: () => refreshEventQueries(client, prefix, scope.signal),
        status: value => { statuses.push(value); },
      }, scope.signal);
      await flushMicrotasks();
      expect(statuses).toContain('live');
      expect(statuses).not.toContain('closed');
      expect(client.getQueryState(missingKey)?.error).toBe(missing);
      await vi.advanceTimersByTimeAsync(1_000);
      expect(cursors).toEqual(['snapshot_cursor', transient ? 'snapshot_cursor' : 'processed_cursor']);
      if (transient) {
        await vi.advanceTimersByTimeAsync(1_000);
        expect(cursors).toEqual(['snapshot_cursor', 'snapshot_cursor', 'processed_cursor']);
      }
      expect(client.getQueryData(healthyKey)).toEqual([`fresh read ${transient ? 3 : 2}`]);
      expect(statuses.at(-1)).toBe('live');
    } finally {
      scope.abort();
      await watch;
      unsubscribeMissing(); unsubscribeHealthy(); client.clear();
    }
    expect(vi.getTimerCount()).toBe(0);
  });
});
