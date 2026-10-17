import { ApiError } from './api';
import type { InboxOptions, InboxSnapshot, MessageEventPage, WatchCallbacks } from './types';

const maximumFrameBytes = 1 << 20;
const idleMilliseconds = 45_000;
const reconnectDelays = [1_000, 2_000, 4_000, 10_000] as const;
const stableConnectionMilliseconds = 15_000;

type Frame = { event: string; id?: string; data: string; comment: boolean };
// A replaced transport can settle after its watch has renewed. Its own
// signal prevents old authorization failures or queued pages reaching the view.
type OpenStream = { response: Response; close: () => void; signal?: AbortSignal };

export interface MessageWatchSource {
  projectId: string;
  snapshot(worker: string, options: InboxOptions, signal: AbortSignal): Promise<InboxSnapshot>;
  openStream(worker: string, cursor: string, limit: number, signal: AbortSignal): Promise<OpenStream>;
  readFailure(response: Response, signal: AbortSignal): Promise<ApiError>;
  validatePage(value: unknown, worker: string, limit: number): value is MessageEventPage;
  parseStreamError(data: string): ApiError;
}

export class SSEParser {
  private line: number[] = [];
  private frameBytes = 0;
  private previousCR = false;
  private event = '';
  private id: string | undefined;
  private data: string[] = [];
  private comment = false;

  push(chunk: Uint8Array): Frame[] {
    const frames: Frame[] = [];
    for (const byte of chunk) {
      if (this.previousCR) {
        this.previousCR = false;
        if (byte === 0x0a) continue;
      }
      this.frameBytes += 1;
      if (this.frameBytes > maximumFrameBytes) throw new ApiError('stream_frame_too_large', 'The event stream frame exceeded the supported size.');
      if (byte === 0x0d) {
        this.finishLine(frames);
        this.previousCR = true;
      } else if (byte === 0x0a) {
        this.finishLine(frames);
      } else {
        this.line.push(byte);
      }
    }
    return frames;
  }

  private finishLine(frames: Frame[]): void {
    let line: string;
    try { line = new TextDecoder('utf-8', { fatal: true }).decode(new Uint8Array(this.line)); }
    catch { throw new ApiError('invalid_response', 'The event stream returned invalid text.'); }
    this.line = [];
    if (line === '') {
      if (this.comment || this.data.length || this.event || this.id !== undefined) {
        frames.push({ event: this.event || 'message', id: this.id, data: this.data.join('\n'), comment: this.comment && !this.data.length && !this.event });
      }
      this.frameBytes = 0;
      this.event = '';
      this.id = undefined;
      this.data = [];
      this.comment = false;
      return;
    }
    if (line.startsWith(':')) { this.comment = true; return; }
    const colon = line.indexOf(':');
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'event') this.event = value;
    else if (field === 'id' && !value.includes('\u0000')) this.id = value;
    else if (field === 'data') this.data.push(value);
  }
}

function abortError(): ApiError { return new ApiError('cancelled', 'The message watch was cancelled.'); }
function isTerminal(error: ApiError): boolean {
  return error.status === 401 || error.status === 403 || error.status === 400 || error.status === 404 || error.status === 405 || error.status === 409 && error.code !== 'resync_required' || error.code === 'invalid_response' || error.code === 'stream_frame_too_large' || error.code === 'callback_failed' || error.code === 'unauthorized' || error.code === 'forbidden' || error.code === 'validation_failed' || error.code === 'not_found' || error.code === 'instance_mismatch' || error.code === 'invalid_binding';
}
function wait(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(abortError()); return; }
    const finish = () => { signal.removeEventListener('abort', cancel); resolve(); };
    const timer = setTimeout(finish, milliseconds);
    const cancel = () => { clearTimeout(timer); signal.removeEventListener('abort', cancel); reject(abortError()); };
    signal.addEventListener('abort', cancel, { once: true });
  });
}

async function readWithIdle(reader: ReadableStreamDefaultReader<Uint8Array>, signal: AbortSignal, close: () => void): Promise<ReadableStreamReadResult<Uint8Array>> {
  if (signal.aborted) throw abortError();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let cancel: (() => void) | undefined;
  const cancelled = new Promise<never>((_, reject) => {
    cancel = () => reject(abortError());
    signal.addEventListener('abort', cancel, { once: true });
  });
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => { close(); reject(new ApiError('stream_idle', 'The event stream was idle.')); }, idleMilliseconds);
  });
  try { return await Promise.race([reader.read(), timeout, cancelled]); }
  finally { if (timer !== undefined) clearTimeout(timer); if (cancel) signal.removeEventListener('abort', cancel); }
}

export interface EventWatchSource<S, P> {
  snapshot(signal: AbortSignal): Promise<S>;
  snapshotCursor(value: S): string;
  openStream(cursor: string, signal: AbortSignal): Promise<OpenStream>;
  readFailure(response: Response, signal: AbortSignal): Promise<ApiError>;
  decodePage(data: string): P;
  pageCursor(page: P): string;
  substantive(page: P): boolean;
  parseStreamError(data: string): ApiError;
  event: string;
}
export interface EventWatchCallbacks<S, P> {
  snapshot(value: S): void | Promise<void>;
  events(value: P): void | Promise<void>;
  status(value: 'connecting' | 'live' | 'reconnecting' | 'closed'): void;
}
async function deliverSnapshot<S, P>(source: EventWatchSource<S, P>, callbacks: EventWatchCallbacks<S, P>, signal: AbortSignal): Promise<string> {
  const snapshot = await source.snapshot(signal);
  if (signal.aborted) throw abortError();
  try { await callbacks.snapshot(snapshot); }
  catch (error) { if (error instanceof ApiError) throw error; throw new ApiError('callback_failed', 'The event snapshot could not be delivered to the view.'); }
  if (signal.aborted) throw abortError();
  return source.snapshotCursor(snapshot);
}
export async function runMessageWatch(source: MessageWatchSource, worker: string, options: InboxOptions, callbacks: WatchCallbacks, signal: AbortSignal): Promise<void> {
  const limit = options.limit ?? 20;
  if (!Number.isInteger(limit) || limit < 1 || limit > 100 || options.unread !== undefined && typeof options.unread !== 'boolean' || options.unacknowledged !== undefined && typeof options.unacknowledged !== 'boolean') throw new ApiError('invalid_request', 'The message watch options are invalid.');
  return runEventWatch<InboxSnapshot, MessageEventPage>({
    snapshot: selected => source.snapshot(worker, options, selected), snapshotCursor: value => value.event_cursor,
    openStream: (cursor, selected) => source.openStream(worker, cursor, limit, selected), readFailure: source.readFailure,
    decodePage: data => { const value: unknown = JSON.parse(data); if (!source.validatePage(value, worker, limit)) throw new ApiError('invalid_response', 'The event stream returned an invalid event page.'); return value; },
    pageCursor: page => page.cursor, substantive: page => page.items.length > 0, parseStreamError: source.parseStreamError, event: 'message-events',
  }, callbacks, signal);
}
export async function runEventWatch<S, P>(source: EventWatchSource<S, P>, callbacks: EventWatchCallbacks<S, P>, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return;
  let cursor: string;
  callbacks.status('connecting');
  try { cursor = await deliverSnapshot(source, callbacks, signal); }
  catch (error) { if (signal.aborted) return; callbacks.status('closed'); throw error instanceof ApiError ? error : new ApiError('connection_failed', 'The event snapshot could not be loaded.'); }
  let attempts = 0;
  while (!signal.aborted) {
    let connection: OpenStream | undefined;
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
    let attachedAt: number | undefined;
    const retired = () => connection?.signal?.aborted === true;
    const requireCurrent = () => { if (signal.aborted || retired()) throw abortError(); };
    try {
      connection = await source.openStream(cursor, signal);
      requireCurrent();
      const response = connection.response;
      const connectionSignal = connection.signal ? AbortSignal.any([signal, connection.signal]) : signal;
      const failure = async () => { const error = await source.readFailure(response, connectionSignal); requireCurrent(); return error; };
      if (response.status === 409) throw await failure();
      if (response.status === 401 || response.status === 403) throw await failure();
      if (response.status >= 500 || response.status === 0) throw new ApiError('stream_unavailable', 'The event stream is temporarily unavailable.', response.status);
      if (!response.ok) throw await failure();
      if (response.headers.get('content-type')?.split(';', 1)[0].trim().toLowerCase() !== 'text/event-stream' || !response.body) throw new ApiError('invalid_response', 'The service returned an invalid event stream.', response.status);
      if (signal.aborted) break;
      reader = response.body.getReader();
      attachedAt = performance.now();
      callbacks.status('live');
      const parser = new SSEParser();
      while (!signal.aborted) {
        const read = await readWithIdle(reader, connectionSignal, connection.close);
        requireCurrent();
        if (read.done) throw new ApiError('stream_disconnected', 'The event stream disconnected.');
        for (const frame of parser.push(read.value)) {
          requireCurrent();
          if (frame.comment) continue;
          if (frame.event === 'error') {
            throw source.parseStreamError(frame.data);
          }
          if (frame.event !== source.event) throw new ApiError('invalid_response', 'The event stream returned an unsupported event.');
          let page: P;
          try { page = source.decodePage(frame.data); }
          catch { throw new ApiError('invalid_response', 'The event stream returned an invalid event page.'); }
          if (frame.id === undefined || frame.id !== source.pageCursor(page)) throw new ApiError('invalid_response', 'The event stream cursor did not match its page.');
          requireCurrent();
          try { await callbacks.events(page); }
          catch (error) {
            if (error instanceof ApiError) throw error;
            throw new ApiError('callback_failed', 'The event page could not be delivered to the view.');
          }
          requireCurrent();
          cursor = source.pageCursor(page);
          if (source.substantive(page)) attempts = 0;
        }
      }
    } catch (caught) {
      if (signal.aborted) break;
      const error = retired() ? new ApiError('stream_retired', 'The event connection was replaced.') : caught instanceof ApiError ? caught : new ApiError('stream_disconnected', 'The event stream disconnected.');
      if (error.code === 'resync_required') {
        try {
          callbacks.status('connecting');
          cursor = await deliverSnapshot(source, callbacks, signal);
        } catch (snapshotError) {
          if (signal.aborted) break;
          callbacks.status('closed');
          throw snapshotError instanceof ApiError ? snapshotError : new ApiError('connection_failed', 'The inbox snapshot could not be loaded.');
        }
      }
      if (isTerminal(error)) { callbacks.status('closed'); throw error; }
      callbacks.status('reconnecting');
      if (attachedAt !== undefined && performance.now() - attachedAt >= stableConnectionMilliseconds) attempts = 0;
      const delay = reconnectDelays[Math.min(attempts, reconnectDelays.length - 1)];
      attempts += 1;
      try { await wait(delay, signal); }
      catch { break; }
    } finally {
      if (reader) { try { await reader.cancel(); } catch { /* the stream may already be aborted */ } try { reader.releaseLock(); } catch { /* already released */ } }
      connection?.close();
    }
  }
}
