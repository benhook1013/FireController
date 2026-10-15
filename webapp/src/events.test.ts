import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from './api';
import { runMessageWatch, SSEParser, type MessageWatchSource } from './events';
import type { InboxSnapshot, MessageEventPage, WatchCallbacks } from './types';

const projectId = 'project_one';
const workerId = 'worker_one';
const snapshot: InboxSnapshot = { inbox: { items: [], cursor: '', has_more: false }, event_cursor: 'snapshot_cursor' };
const emptyPage: MessageEventPage = { items: [], cursor: 'processed_cursor', has_more: false, timed_out: false };
const eventJSON = JSON.stringify(emptyPage);
const frame = `event: message-events\nid: ${emptyPage.cursor}\ndata: ${eventJSON}\n\n`;

function response(body: ReadableStream<Uint8Array> | string, status = 200, contentType = 'text/event-stream'): Response {
  return new Response(typeof body === 'string' ? body : body, { status, headers: { 'content-type': contentType } });
}
function makeSource(overrides: Partial<MessageWatchSource> = {}): MessageWatchSource {
  return {
    projectId,
    snapshot: async () => snapshot,
    openStream: async () => ({ response: response(new ReadableStream<Uint8Array>({ start(controller) { controller.enqueue(new TextEncoder().encode(frame)); } })), close: () => undefined }),
    readFailure: async (value) => new ApiError('service_error', 'safe failure', value.status),
    validatePage: (value): value is MessageEventPage => typeof value === 'object' && value !== null && 'items' in value && 'cursor' in value,
    parseStreamError: () => new ApiError('stream_failed', 'safe stream failure'),
    ...overrides,
  };
}
function callbacks(overrides: Partial<WatchCallbacks> = {}): WatchCallbacks {
  return { snapshot: () => undefined, events: () => undefined, status: () => undefined, ...overrides };
}
async function flushMicrotasks(): Promise<void> { for (let index = 0; index < 40; index += 1) await Promise.resolve(); }
function controlledStream(data: string, signal: AbortSignal, closeAfterData = true): { response: Response; close: () => void } {
  let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
  const body = new ReadableStream<Uint8Array>({
    start(value) { controller = value; if (data) value.enqueue(new TextEncoder().encode(data)); if (closeAfterData) value.close(); },
  });
  const close = () => { try { controller?.close(); } catch { /* already closed */ } };
  signal.addEventListener('abort', close, { once: true });
  return { response: response(body), close };
}

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

describe('SSE parser and message watch', () => {
  it('parses split UTF-8, CRLF, comments and multiline data without losing frame boundaries', () => {
    const parser = new SSEParser();
    const encoded = new TextEncoder().encode(': keepalive\r\n\r\nevent: message-events\r\nid: cursor\r\ndata: {"label":"café",\r\ndata: "ok":true}\r\n\r\n');
    const split = encoded.indexOf(new TextEncoder().encode('é')[0]) + 1;
    const first = parser.push(encoded.slice(0, split));
    const second = parser.push(encoded.slice(split, split + 5));
    const third = parser.push(encoded.slice(split + 5));
    const frames = [...first, ...second, ...third];
    expect(frames[0]).toMatchObject({ comment: true, data: '' });
    expect(frames[1]).toMatchObject({ event: 'message-events', id: 'cursor', data: '{"label":"café",\n"ok":true}', comment: false });
  });

  it('bounds an SSE frame across chunks and rejects invalid UTF-8', () => {
    const parser = new SSEParser();
    expect(() => parser.push(new Uint8Array((1 << 20) + 1).fill(0x61))).toThrowError(ApiError);
    const invalid = new SSEParser();
    expect(() => invalid.push(new Uint8Array([0x64, 0x61, 0x74, 0x61, 0x3a, 0xff, 0x0a, 0x0a]))).toThrowError(ApiError);
  });

  it('delivers initial snapshot then event page, advances only after callback, and reconnects from processed cursor', async () => {
    vi.useFakeTimers();
    const cursors: string[] = [];
    let opened = 0;
    const source = makeSource({
      openStream: async (_worker, cursor, _limit, signal) => {
        cursors.push(cursor);
        opened += 1;
        return controlledStream(opened === 1 ? frame : '', signal, opened === 1);
      },
    });
    const controller = new AbortController();
    const calls: string[] = [];
    const watch = runMessageWatch(source, workerId, {}, callbacks({
      snapshot: () => { calls.push('snapshot'); },
      events: async (page) => { calls.push('events-start'); await Promise.resolve(); calls.push(`events:${page.cursor}`); },
    }), controller.signal);
    await flushMicrotasks();
    expect(calls).toEqual(['snapshot', 'events-start', 'events:processed_cursor']);
    await vi.advanceTimersByTimeAsync(1000);
    await flushMicrotasks();
    expect(opened).toBe(2);
    expect(cursors).toEqual(['snapshot_cursor', 'processed_cursor']);
    controller.abort();
    await watch;
    expect(calls).toEqual(['snapshot', 'events-start', 'events:processed_cursor']);
  });

  it('backs off repeated empty readiness and comment frames at capped delays and cancels its pending retry', async () => {
    vi.useFakeTimers();
    vi.spyOn(performance, 'now').mockReturnValue(0);
    const controller = new AbortController();
    const close = vi.fn();
    let opened = 0;
    const source = makeSource({
      openStream: async (_worker, _cursor, _limit, signal) => {
        opened += 1;
        const stream = controlledStream(`${frame}: keepalive\n\n`, signal);
        return { ...stream, close: () => { close(); stream.close(); } };
      },
    });
    const watch = runMessageWatch(source, workerId, {}, callbacks(), controller.signal);
    await flushMicrotasks();
    expect(opened).toBe(1);
    for (const delay of [1_000, 2_000, 4_000, 10_000, 10_000]) {
      const before = opened;
      await vi.advanceTimersByTimeAsync(delay - 1);
      expect(opened).toBe(before);
      await vi.advanceTimersByTimeAsync(1);
      expect(opened).toBe(before + 1);
    }
    controller.abort();
    await watch;
    expect(close).toHaveBeenCalledTimes(opened);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('resets backoff after applying substantive events and escalates again after another empty disconnect', async () => {
    vi.useFakeTimers();
    vi.spyOn(performance, 'now').mockReturnValue(0);
    const controller = new AbortController();
    let opened = 0;
    const page: MessageEventPage = { ...emptyPage, items: [{ sequence: 1, project_id: projectId, type: 'message.sent', resource_id: 'message_one', revision: 1, actor_id: 'actor_one', payload: { conversation_id: 'conversation_one', recipient_worker_id: workerId }, created_at: '2026-10-15T00:00:00Z' }] };
    const substantiveFrame = `event: message-events\nid: ${page.cursor}\ndata: ${JSON.stringify(page)}\n\n`;
    const source = makeSource({
      openStream: async (_worker, _cursor, _limit, signal) => {
        opened += 1;
        return controlledStream(opened === 3 ? substantiveFrame : frame, signal);
      },
    });
    const watch = runMessageWatch(source, workerId, {}, callbacks(), controller.signal);
    await flushMicrotasks();
    await vi.advanceTimersByTimeAsync(1_000);
    expect(opened).toBe(2);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(opened).toBe(3);
    await vi.advanceTimersByTimeAsync(999);
    expect(opened).toBe(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(opened).toBe(4);
    await vi.advanceTimersByTimeAsync(1_999);
    expect(opened).toBe(4);
    await vi.advanceTimersByTimeAsync(1);
    expect(opened).toBe(5);
    controller.abort();
    await watch;
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each([{ duration: 14_999, delay: 4_000 }, { duration: 15_000, delay: 1_000 }])('resets idle-stream backoff only after a stable attachment ($duration ms)', async ({ duration, delay }) => {
    vi.useFakeTimers();
    let elapsed = 0;
    vi.spyOn(performance, 'now').mockImplementation(() => elapsed);
    const controller = new AbortController();
    let opened = 0;
    let disconnect: (() => void) | undefined;
    const source = makeSource({
      openStream: async (_worker, _cursor, _limit, signal) => {
        opened += 1;
        const stream = controlledStream(frame, signal, opened !== 3);
        if (opened === 3) disconnect = stream.close;
        return stream;
      },
    });
    const watch = runMessageWatch(source, workerId, {}, callbacks(), controller.signal);
    await flushMicrotasks();
    await vi.advanceTimersByTimeAsync(1_000);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(opened).toBe(3);
    elapsed = duration;
    disconnect?.();
    await flushMicrotasks();
    await vi.advanceTimersByTimeAsync(delay - 1);
    expect(opened).toBe(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(opened).toBe(4);
    controller.abort();
    await watch;
    expect(vi.getTimerCount()).toBe(0);
  });

  it('cancels an attached idle stream without leaving a retry or idle timer', async () => {
    vi.useFakeTimers();
    const controller = new AbortController();
    const close = vi.fn();
    let opened = 0;
    const source = makeSource({
      openStream: async (_worker, _cursor, _limit, signal) => {
        opened += 1;
        const stream = controlledStream(frame, signal, false);
        return { ...stream, close: () => { close(); stream.close(); } };
      },
    });
    const watch = runMessageWatch(source, workerId, {}, callbacks(), controller.signal);
    await flushMicrotasks();
    expect(vi.getTimerCount()).toBe(1);
    controller.abort();
    await watch;
    expect(close).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(opened).toBe(1);
  });

  it('takes a fresh snapshot when the server requires resynchronization', async () => {
    vi.useFakeTimers();
    let snapshots = 0;
    let opened = 0;
    const controller = new AbortController();
    const statuses: string[] = [];
    const source = makeSource({
      snapshot: async () => ({ ...snapshot, event_cursor: `snapshot_${++snapshots}` }),
      openStream: async (_worker, _cursor, _limit, signal) => {
        opened += 1;
        if (opened === 1) return { response: response('', 409, 'application/json'), close: () => undefined };
        return controlledStream('', signal, false);
      },
      readFailure: async () => new ApiError('resync_required', 'safe resync', 409),
    });
    const watch = runMessageWatch(source, workerId, {}, callbacks({ status: (value) => statuses.push(value) }), controller.signal);
    await flushMicrotasks();
    expect(snapshots).toBe(2);
    expect(opened).toBe(1);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(opened).toBe(2);
    expect(statuses).toContain('live');
    controller.abort();
    await watch;
  });

  it.each(['HTTP 409', 'error frame'])('backs off repeated resynchronization from $0 and cancels its pending retry', async (transport) => {
    vi.useFakeTimers();
    vi.spyOn(performance, 'now').mockReturnValue(0);
    const controller = new AbortController();
    const close = vi.fn();
    let snapshots = 0;
    const cursors: string[] = [];
    const resync = new ApiError('resync_required', 'safe resync', 409);
    const source = makeSource({
      snapshot: async () => ({ ...snapshot, event_cursor: `snapshot_${++snapshots}` }),
      openStream: async (_worker, cursor, _limit, signal) => {
        cursors.push(cursor);
        const stream = transport === 'HTTP 409'
          ? { response: response('', 409, 'application/json'), close: () => undefined }
          : controlledStream('event: error\ndata: {"code":"resync_required"}\n\n', signal);
        return { ...stream, close: () => { close(); stream.close(); } };
      },
      readFailure: async () => resync,
      parseStreamError: () => resync,
    });
    const watch = runMessageWatch(source, workerId, {}, callbacks(), controller.signal);
    await flushMicrotasks();
    expect(cursors).toEqual(['snapshot_1']);
    expect(snapshots).toBe(2);
    for (const delay of [1_000, 2_000, 4_000, 10_000, 10_000]) {
      const before = cursors.length;
      await vi.advanceTimersByTimeAsync(delay - 1);
      expect(cursors).toHaveLength(before);
      expect(snapshots).toBe(before + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(cursors).toHaveLength(before + 1);
      expect(cursors.at(-1)).toBe(`snapshot_${before + 1}`);
      expect(snapshots).toBe(before + 2);
    }
    controller.abort();
    await watch;
    expect(close).toHaveBeenCalledTimes(cursors.length);
    expect(vi.getTimerCount()).toBe(0);
    const stopped = snapshots;
    await vi.advanceTimersByTimeAsync(60_000);
    expect(snapshots).toBe(stopped);
  });

  it('terminates without retry when access ends during resynchronization', async () => {
    vi.useFakeTimers();
    let snapshots = 0;
    let opened = 0;
    const source = makeSource({
      snapshot: async () => {
        snapshots += 1;
        if (snapshots > 1) throw new ApiError('forbidden', 'safe forbidden', 403);
        return snapshot;
      },
      openStream: async () => { opened += 1; return { response: response('', 409, 'application/json'), close: () => undefined }; },
      readFailure: async () => new ApiError('resync_required', 'safe resync', 409),
    });
    await expect(runMessageWatch(source, workerId, {}, callbacks(), new AbortController().signal)).rejects.toMatchObject({ code: 'forbidden', status: 403 });
    expect(opened).toBe(1);
    expect(snapshots).toBe(2);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('treats authorization revocation as terminal and does not reconnect', async () => {
    let opened = 0;
    const statuses: string[] = [];
    const source = makeSource({
      openStream: async () => { opened += 1; return { response: response('', 403, 'application/json'), close: () => undefined }; },
      readFailure: async () => new ApiError('forbidden', 'safe forbidden', 403),
    });
    await expect(runMessageWatch(source, workerId, {}, callbacks({ status: (value) => statuses.push(value) }), new AbortController().signal)).rejects.toMatchObject({ code: 'forbidden', status: 403 });
    expect(opened).toBe(1);
    expect(statuses.at(-1)).toBe('closed');
  });

  it('preserves typed API errors raised while applying a snapshot or event page', async () => {
    const forbidden = new ApiError('forbidden', 'safe forbidden', 403);
    await expect(runMessageWatch(makeSource(), workerId, {}, callbacks({ snapshot: () => { throw forbidden; } }), new AbortController().signal)).rejects.toBe(forbidden);
    await expect(runMessageWatch(makeSource(), workerId, {}, callbacks({ events: () => { throw forbidden; } }), new AbortController().signal)).rejects.toBe(forbidden);
  });

  it('rejects a stream page that omits its required resume cursor', async () => {
    const missingID = `event: message-events\ndata: ${eventJSON}\n\n`;
    const source = makeSource({ openStream: async (_worker, _cursor, _limit, signal) => controlledStream(missingID, signal) });
    await expect(runMessageWatch(source, workerId, {}, callbacks(), new AbortController().signal)).rejects.toMatchObject({ code: 'invalid_response' });
  });

  it('does not invoke the snapshot callback after cancellation during snapshot loading', async () => {
    const controller = new AbortController();
    let snapshotCallback = 0;
    const source = makeSource({
      snapshot: async (_worker, _options, signal) => new Promise<InboxSnapshot>((_resolve, reject) => {
        signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
      }),
    });
    const watch = runMessageWatch(source, workerId, {}, callbacks({ snapshot: () => { snapshotCallback += 1; } }), controller.signal);
    controller.abort();
    await watch;
    expect(snapshotCallback).toBe(0);
  });
});
