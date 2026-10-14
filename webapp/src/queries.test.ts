import { QueryClient, QueryObserver } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { refreshEventQueries } from './queries';

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
