import type { WorkerPrimary, WorkerPrimaryHistoryEntry, WorkerPause, WorkerPauseHistoryEntry, NoteOptions, NoteSummary, Note, NoteHistorySummary, NoteRevision } from './types';
import type {
  Checkpoint, ConversationPage, InboxItem, InboxOptions, InboxSnapshot, Job, JobOptions, LatestCheckpoint,
  JobSummary, JobUpdate, JobChecklist, ChecklistItem, ChecklistHistoryEntry, Message, MessageEvent, MessageEventPage, Page, PageOptions,
  Project, WatchCallbacks, Worker, ProjectMapEntry, ProjectMapSummary, ProjectMapOptions, ProjectMapHistorySummary, ProjectMapRevision, JobWorkstream, JobWorkstreamHistoryEntry,
  ReviewAttemptSummary, ReviewFinding, ReviewPull, ReviewPullSummary, ReviewQueue, ReviewRunDetail,
  ReviewRunOptions, ReviewRuns, ReviewRunSummary, ReviewPolicyAssessment, ReviewAttemptOptions, ReviewAttempts, ReviewAttemptItem,
} from './types';
import { runMessageWatch, runEventWatch, type MessageWatchSource, type EventWatchCallbacks } from './events';
import { validPresentationSnapshot, validPresentation, validMetadataPage, validPresentationConfig, type PresentationConfig } from './presentation';
import type { PresentationSnapshot, PresentationRevision, ProjectMetadataPage } from './types';
import { newSessionID, boundedSessionExpiry, sessionIdentity, sessionSelector } from './browser-session';

const maxBytes = 1 << 20;
const maxItems = 100;
const idPattern = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
const jobStatuses = new Set(['active', 'parked', 'blocked', 'completed']);
const serverErrorCodes = new Set([
  'bootstrap_failed', 'configuration_invalid', 'credential_already_configured', 'empty_edit', 'forbidden',
  'idempotency_conflict', 'idempotency_key_required', 'instance_mismatch', 'internal_error', 'invalid_brief',
  'invalid_cursor', 'invalid_filter', 'invalid_query', 'invalid_revision', 'invalid_status', 'invalid_title',
  'method_not_allowed', 'not_found', 'note_not_found', 'worker_not_found', 'overloaded', 'provider_unavailable', 'review_held', 'review_capacity', 'resync_required', 'schema_invalid',
  'schema_unsupported', 'storage_unavailable', 'stream_failed', 'stream_frame_too_large', 'stream_unavailable',
  'unauthorized', 'validation_failed', 'worker_already_configured',
  'project_presentation_revision_conflict', 'worker_required',
]);

export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  readonly outcomeUnknown: boolean;

  constructor(code: string, message: string, status = 0, outcomeUnknown = false) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.status = status;
    this.outcomeUnknown = outcomeUnknown;
  }
}

export interface BrowserCapabilities { allow_writes: boolean; can_edit_presentation: boolean }
export interface BrowserSessionView extends BrowserCapabilities { session_id: string; instance_id: string; project_id: string; expires_at: string }
export type BrowserPresentationConfig = PresentationConfig;
export interface BrowserMessageReceipt { id: string; conversation_id: string; outcome: 'sent' | 'seen' | 'acknowledged' | 'unchanged' }
export interface BrowserPresentationReceipt { project_id: string; revision: number; outcome: 'updated' | 'unchanged' }
function browserSessionView(value: unknown, selector: string, instance: string, project: string): value is BrowserSessionView {
  return record(value) && keys(value, ['session_id', 'instance_id', 'project_id', 'expires_at', 'allow_writes', 'can_edit_presentation']) && value.session_id === selector && value.instance_id === instance && value.project_id === project && boundedSessionExpiry(value.expires_at) && typeof value.allow_writes === 'boolean' && typeof value.can_edit_presentation === 'boolean' && (!value.can_edit_presentation || value.allow_writes);
}
function mutationKey(value: unknown): value is string { return typeof value === 'string' && value.length > 0 && value.length <= 128 && value.trim() === value && !/[^\x20-\x7e]/u.test(value); }

type RequestBudget = { signal: AbortSignal; expired: () => boolean; close: () => void };
function requestBudget(parents: Array<AbortSignal | undefined>, milliseconds: number): RequestBudget {
  const controller = new AbortController();
  let expired = false;
  const links = parents.filter((parent): parent is AbortSignal => parent !== undefined).map((parent) => {
    const abort = () => controller.abort();
    if (parent.aborted) controller.abort();
    else parent.addEventListener('abort', abort, { once: true });
    return { parent, abort };
  });
  const timer = setTimeout(() => { expired = true; controller.abort(); }, milliseconds);
  return { signal: controller.signal, expired: () => expired, close: () => { clearTimeout(timer); for (const link of links) link.parent.removeEventListener('abort', link.abort); } };
}

function origin(): string {
  const value = globalThis.location?.origin;
  if (!value || !/^https?:\/\//i.test(value)) throw new ApiError('browser_origin_unavailable', 'Open the browser from the service origin.');
  return value;
}
function urlFor(path: string): string {
  const base = origin();
  const url = new URL(path, base);
  if (url.origin !== base || url.username || url.password) throw new ApiError('invalid_request', 'The service request is invalid.');
  return url.toString();
}
function record(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) && Object.getPrototypeOf(value) === Object.prototype;
}
function keys(value: Record<string, unknown>, required: string[], optional: string[] = []): boolean {
  return required.every((key) => Object.hasOwn(value, key)) && Object.keys(value).every((key) => required.includes(key) || optional.includes(key));
}
function text(value: unknown, max: number, empty = false): value is string {
  return typeof value === 'string' && (empty || value.length > 0) && new TextEncoder().encode(value).byteLength <= max && !/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/u.test(value);
}
function contentText(value: unknown, max: number, empty = false): value is string {
  return typeof value === 'string' && (empty || value.length > 0) && !/[\uD800-\uDFFF]/u.test(value) && new TextEncoder().encode(value).byteLength <= max;
}
function displayName(value: unknown): value is string {
  return contentText(value, 200) && !/^\p{White_Space}|\p{White_Space}$/u.test(value);
}
function id(value: unknown): value is string { return typeof value === 'string' && idPattern.test(value); }
function cursor(value: unknown, optional = false): value is string | undefined {
  return optional && value === undefined || typeof value === 'string' && value.length > 0 && value.length <= 4096 && value.trim() === value && !/[\u0000-\u001f\u007f]/u.test(value);
}
function timestamp(value: unknown): value is string {
  return typeof value === 'string' && value.length <= 64 && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/u.test(value) && Number.isFinite(Date.parse(value));
}
function pageOptions(options: PageOptions = {}): URLSearchParams {
  if (options.limit !== undefined && (!Number.isInteger(options.limit) || options.limit < 1 || options.limit > maxItems)) throw new ApiError('invalid_request', 'Choose a page limit from 1 to 100.');
  if (options.cursor !== undefined && !cursor(options.cursor)) throw new ApiError('invalid_request', 'The page cursor is invalid.');
  const query = new URLSearchParams();
  if (options.limit !== undefined) query.set('limit', String(options.limit));
  if (options.cursor !== undefined) query.set('cursor', options.cursor);
  return query;
}
function withQuery(path: string, query: URLSearchParams): string { const value = query.toString(); return value ? `${path}?${value}` : path; }
function validPage(value: unknown, limit: number): value is Record<string, unknown> & { items: unknown[]; has_more: boolean; cursor?: string } {
  if (!record(value) || !keys(value, ['items', 'has_more'], ['cursor']) || !Array.isArray(value.items) || value.items.length > limit || typeof value.has_more !== 'boolean') return false;
  if (value.cursor !== undefined && (typeof value.cursor !== 'string' || value.cursor.length > 4096 || value.cursor.trim() !== value.cursor || /[\u0000-\u001f\u007f]/u.test(value.cursor))) return false;
  return !value.has_more || (!!value.cursor && value.items.length > 0);
}
function defaultLimit(options: PageOptions): number { return options.limit ?? 20; }

async function readJson(response: Response, signal: AbortSignal, responseMaxBytes = maxBytes, decode: (text: string) => unknown = JSON.parse): Promise<unknown> {
  const declared = response.headers.get('content-length');
  if (declared && Number.isFinite(Number(declared)) && Number(declared) > responseMaxBytes) {
    void response.body?.cancel().catch(() => {});
    throw new ApiError('response_too_large', 'The service response exceeded the supported size.', response.status);
  }
  if (response.headers.get('content-type')?.split(';', 1)[0].trim().toLowerCase() !== 'application/json' || !response.body) throw new ApiError('invalid_response', 'The service returned an invalid response.', response.status);
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    while (true) {
      if (signal.aborted) throw new DOMException('Aborted', 'AbortError');
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > responseMaxBytes) throw new ApiError('response_too_large', 'The service response exceeded the supported size.', response.status);
      chunks.push(value);
    }
  } catch (error) {
    try { await reader.cancel(); } catch { /* the response may already be aborted */ }
    throw error;
  } finally { reader.releaseLock(); }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
  try { return decode(new TextDecoder('utf-8', { fatal: true }).decode(bytes)); }
  catch { throw new ApiError('invalid_response', 'The service returned invalid JSON.', response.status); }
}
function serverError(value: unknown, status: number): ApiError {
  if (record(value) && record(value.error)) {
    const code = text(value.error.code, 64) && serverErrorCodes.has(value.error.code) ? value.error.code : 'service_error';
    return new ApiError(code, `The service request failed (${code}).`, status);
  }
  return new ApiError('service_error', 'The service request failed.', status);
}
async function readFailure(response: Response, signal: AbortSignal): Promise<ApiError> {
  try { return serverError(await readJson(response, signal), response.status); }
  catch (error) {
    if (signal.aborted) throw error;
    return error instanceof ApiError ? error : new ApiError('service_error', 'The service request failed.', response.status);
  }
}

function validProject(value: unknown, expected: string): value is Project {
  return record(value) && keys(value, ['id', 'name', 'github_repository_id', 'repository']) && value.id === expected && displayName(value.name) && Number.isSafeInteger(value.github_repository_id) && Number(value.github_repository_id) > 0 && text(value.repository, 300);
}
function validWorker(value: unknown, projectId: string): value is Worker {
  return record(value) && keys(value, ['id', 'project_id', 'name']) && id(value.id) && value.project_id === projectId && displayName(value.name);
}
function validSummary(value: unknown): value is JobSummary {
  return record(value) && keys(value, ['id', 'title', 'worker_id', 'status', 'revision']) && id(value.id) && contentText(value.title, 240) && text(value.worker_id, 128, true) && typeof value.status === 'string' && jobStatuses.has(value.status) && Number.isSafeInteger(value.revision) && Number(value.revision) > 0;
}
function validJob(value: unknown, projectId: string): value is Job {
  return record(value) && keys(value, ['id', 'project_id', 'title', 'brief', 'worker_id', 'status', 'revision', 'created_at', 'updated_at']) && value.project_id === projectId && id(value.id) && contentText(value.title, 240) && contentText(value.brief, 32768, true) && text(value.worker_id, 128, true) && typeof value.status === 'string' && jobStatuses.has(value.status) && Number.isSafeInteger(value.revision) && Number(value.revision) > 0 && timestamp(value.created_at) && timestamp(value.updated_at);
}
function checkpointText(value: unknown, max: number, required: boolean): value is string {
  if (!text(value, max, !required) || required && /^\p{White_Space}*$/u.test(value)) return false;
  // JSON.parse preserves unpaired escapes, while TextEncoder would repair
  // them. Reject those code units without rejecting literal replacement text.
  for (const character of value) {
    const point = character.codePointAt(0)!;
    if (point >= 0xd800 && point <= 0xdfff) return false;
  }
  return true;
}
function validCheckpoint(value: unknown, projectId: string, jobId: string): value is Checkpoint {
  if (!record(value) || !keys(value, ['id', 'project_id', 'job_id', 'job_revision', 'sequence', 'actor_id', 'worker_id', 'done', 'next', 'blocker', 'evidence', 'created_at']) || !id(value.id) || value.project_id !== projectId || value.job_id !== jobId || !Number.isSafeInteger(value.job_revision) || Number(value.job_revision) < 1 || !Number.isSafeInteger(value.sequence) || Number(value.sequence) < 1 || !id(value.actor_id) || value.worker_id !== '' && !id(value.worker_id) || !timestamp(value.created_at)) return false;
  if (!checkpointText(value.done, 8 << 10, true) || !checkpointText(value.next, 8 << 10, true) || !checkpointText(value.blocker, 4 << 10, false) || !Array.isArray(value.evidence) || value.evidence.length > 10 || !value.evidence.every(item => checkpointText(item, 1024, true))) return false;
  const encoder = new TextEncoder();
  return [value.done, value.next, value.blocker, ...value.evidence].reduce((size, item) => size + encoder.encode(item).byteLength, 0) <= 16 << 10;
}

// Notes and worker coordination are independent stored facts. Individual wall
// timestamps validate structurally; revisions order their mutation histories.
function coordinationText(value: unknown, max: number, required = false, multiline = false): value is string {
  return contentText(value, max, !required) && !(multiline ? /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/u : /\p{Cc}/u).test(value) && (!required || !/^\p{White_Space}*$/u.test(value));
}
function coordinationTime(value: unknown): value is string {
  return assessmentTime(value) && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/u.test(value);
}
function coordinationTimeKey(value: string): string { const [seconds, fraction = ''] = value.slice(0, -1).split('.'); return `${seconds}.${fraction.padEnd(9, '0')}`; }
function coordinationPage(value: unknown, limit: number): value is Record<string, unknown> & {items: unknown[]; has_more: boolean; cursor?: string} {
  return validPage(value, limit) && (!value.cursor || cursor(value.cursor) && coordinationText(value.cursor, 4096) && !/\s/u.test(value.cursor)) && (value.has_more || !value.cursor);
}
function coordinationQuery(options: PageOptions): URLSearchParams {
  const query = pageOptions(options);
  if (options.cursor !== undefined && (!coordinationText(options.cursor, 4096) || /\s/u.test(options.cursor))) throw new ApiError('invalid_request', 'The page cursor is invalid.');
  return query;
}
function validWorkerPrimary(value: unknown, worker: string): value is WorkerPrimary {
  return record(value) && keys(value, ['worker_id', 'revision', 'job_id']) && value.worker_id === worker && nonnegative(value.revision) && (value.job_id === '' || id(value.job_id)) && (value.revision !== 0 || value.job_id === '');
}
function validPauseFields(value: Record<string, unknown>): boolean {
  return typeof value.paused === 'boolean' && coordinationText(value.reason, 1024, false, true) && (value.paused || value.reason === '');
}
function validWorkerPause(value: unknown, worker: string): value is WorkerPause {
  return record(value) && keys(value, ['worker_id', 'revision', 'paused', 'reason']) && value.worker_id === worker && nonnegative(value.revision) && validPauseFields(value) && (value.revision !== 0 || value.paused === false);
}
const noteFields = ['id', 'project_id', 'revision', 'worker_id', 'job_id', 'phase', 'kind', 'status', 'created_at', 'updated_at'];
const noteActors = ['actor_id', 'actor_worker_id', 'recorded_at'];
function validNoteFields(value: Record<string, unknown>, project: string): boolean {
  return id(value.id) && value.project_id === project && positive(value.revision) && (value.worker_id === '' || id(value.worker_id)) && (value.job_id === '' || id(value.job_id)) && coordinationText(value.phase, 200, value.phase !== '') && (value.worker_id !== '' || value.job_id !== '' || value.phase !== '') && (value.kind === 'reminder' || value.kind === 'source') && typeof value.status === 'string' && ['pending', 'consumed', 'dismissed'].includes(value.status) && (value.revision !== 1 || value.status === 'pending') && coordinationTime(value.created_at) && coordinationTime(value.updated_at);
}
function validNoteActor(value: Record<string, unknown>): boolean {
  return id(value.actor_id) && (value.actor_worker_id === '' || id(value.actor_worker_id)) && coordinationTime(value.recorded_at) && value.recorded_at === value.updated_at;
}
function validNoteSummary(value: unknown, project: string, history = false): value is NoteSummary | NoteHistorySummary {
  if (!record(value) || !keys(value, [...noteFields, 'body_preview', 'body_truncated', ...(history ? noteActors : [])]) || !validNoteFields(value, project) || !coordinationText(value.body_preview, 240, false, true) || value.body_preview === '' || typeof value.body_truncated !== 'boolean' || history && !validNoteActor(value)) return false;
  const bytes = new TextEncoder().encode(value.body_preview).length;
  return value.body_truncated ? bytes >= 237 : !/^\p{White_Space}*$/u.test(value.body_preview);
}
function validNote(value: unknown, project: string, expected: string, history = false): value is Note | NoteRevision {
  return record(value) && keys(value, [...noteFields, 'body', 'status_reason', ...(history ? noteActors : [])]) && value.id === expected && validNoteFields(value, project) && coordinationText(value.body, 16 << 10, true, true) && coordinationText(value.status_reason, 1024, value.status === 'dismissed', true) && (value.status === 'dismissed' || value.status_reason === '') && (!history || validNoteActor(value));
}

function validJobUpdate(value: unknown, projectId: string, jobId: string): value is JobUpdate {
  if (!record(value) || !keys(value, ['id', 'project_id', 'job_id', 'job_revision', 'sequence', 'actor_id', 'worker_id', 'body', 'evidence', 'created_at']) || !id(value.id) || value.project_id !== projectId || value.job_id !== jobId || !positive(value.job_revision) || !positive(value.sequence) || !id(value.actor_id) || value.worker_id !== '' && !id(value.worker_id) || !assessmentTime(value.created_at)) return false;
  if (!checkpointText(value.body, 8 << 10, true) || !Array.isArray(value.evidence) || value.evidence.length > 10 || !value.evidence.every(item => checkpointText(item, 1024, true))) return false;
  const encoder = new TextEncoder();
  return [value.body, ...value.evidence].reduce((size, item) => size + encoder.encode(item).byteLength, 0) <= 16 << 10;
}
function validChecklistItems(value: unknown): value is ChecklistItem[] {
  if (!Array.isArray(value) || value.length > 100) return false;
  const seen = new Set<string>();
  const encoder = new TextEncoder();
  let size = 0;
  for (const item of value) {
    if (!record(item) || !keys(item, ['id', 'text', 'done']) || typeof item.id !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/u.test(item.id) || seen.has(item.id) || !checkpointText(item.text, 1024, true) || typeof item.done !== 'boolean') return false;
    seen.add(item.id);
    size += encoder.encode(item.id).byteLength + encoder.encode(item.text).byteLength;
    if (size > 16 << 10) return false;
  }
  return true;
}
function validChecklistHistoryEntry(value: unknown): value is ChecklistHistoryEntry {
  return record(value) && keys(value, ['revision', 'job_revision', 'actor_id', 'worker_id', 'items', 'created_at']) && positive(value.revision) && positive(value.job_revision) && id(value.actor_id) && (value.worker_id === '' || id(value.worker_id)) && validChecklistItems(value.items) && assessmentTime(value.created_at);
}
function mapText(value: unknown, max: number, prose = false): value is string {
  return contentText(value, max, true) && !(prose ? /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/u : /\p{Cc}/u).test(value);
}
function mapName(value: unknown, max: number): value is string {
  return mapText(value, max) && value.length > 0 && !/^\p{White_Space}|\p{White_Space}$/u.test(value);
}
export function projectMapURL(value: unknown): value is string {
  if (!mapText(value, 2048) || !/^https?:\/\//iu.test(value) || /\p{White_Space}/u.test(value) || value.split(/[/?#]/u).slice(2, 3).join('').includes('@')) return false;
  try { const url = new URL(value); return (url.protocol === 'https:' || url.protocol === 'http:') && !!url.hostname && !url.username && !url.password; }
  catch { return false; }
}
function validMapSummary(value: unknown): value is ProjectMapSummary {
  return record(value) && keys(value, ['id', 'kind', 'title', 'state', 'archived', 'revision']) && id(value.id) && (value.kind === 'workstream' || value.kind === 'return_point') && mapName(value.title, 240) && mapText(value.state, 240) && typeof value.archived === 'boolean' && positive(value.revision);
}
function validMapEntry(value: unknown, project: string, expected: string): value is ProjectMapEntry {
  if (!record(value) || !keys(value, ['id', 'kind', 'title', 'state', 'archived', 'revision', 'project_id', 'description', 'where', 'next', 'trigger', 'phases', 'links', 'created_at', 'updated_at']) || value.id !== expected || !id(value.id) || value.project_id !== project || (value.kind !== 'workstream' && value.kind !== 'return_point') || !mapName(value.title, 240) || !mapText(value.state, 240) || typeof value.archived !== 'boolean' || !positive(value.revision) || !assessmentTime(value.created_at) || !assessmentTime(value.updated_at) || !mapText(value.description, 8192, true) || !mapText(value.where, 8192, true) || !mapText(value.next, 8192, true) || !mapText(value.trigger, 8192, true) || !Array.isArray(value.phases) || value.phases.length > 64 || !Array.isArray(value.links) || value.links.length > 12) return false;
  const seen = new Set<string>();
  for (const phase of value.phases) {
    if (!record(phase) || !keys(phase, ['id', 'name', 'state', 'archived']) || typeof phase.id !== 'string' || !/^[a-z][a-z0-9_-]{0,63}$/u.test(phase.id) || seen.has(phase.id) || !mapName(phase.name, 160) || !mapText(phase.state, 240) || typeof phase.archived !== 'boolean') return false;
    seen.add(phase.id);
  }
  if (!value.links.every(link => record(link) && keys(link, ['label', 'url']) && mapName(link.label, 120) && projectMapURL(link.url))) return false;
  if (value.kind === 'workstream' ? value.trigger !== '' : value.where !== '' || value.next !== '' || value.phases.length !== 0) return false;
  const editable = { kind: value.kind, title: value.title, description: value.description, state: value.state, where: value.where, next: value.next, trigger: value.trigger, phases: value.phases, links: value.links, archived: value.archived };
  // Match the service's encoded editable budget, including Go's HTML escapes.
  return new TextEncoder().encode(JSON.stringify(editable).replace(/[<>&\u2028\u2029]/gu, char => `\\u${char.charCodeAt(0).toString(16).padStart(4, '0')}`)).byteLength <= 64 << 10;
}
function mapPageQuery(options: PageOptions): URLSearchParams { return pageOptions({ ...options, limit: options.limit ?? 25 }); }
function validMapHistory(value: unknown): value is ProjectMapHistorySummary {
  return record(value) && keys(value, ['revision', 'actor_id', 'created_at']) && positive(value.revision) && id(value.actor_id) && assessmentTime(value.created_at);
}
function validWorkstreamReference(value: Record<string, unknown>): boolean {
  return value.workstream_id === '' ? value.workstream_title === '' : id(value.workstream_id) && mapName(value.workstream_title, 240);
}
function reviewEvidenceID(value: unknown): value is string { return typeof value === 'string' && /^review_evidence_[a-f0-9]{32}$/u.test(value); }
// JSON.parse alone loses duplicate keys. This bounded response additionally
// checks the original tokens, including escaped aliases, before accepting it.
function assessmentJSON(raw: string): unknown {
  const value: unknown = JSON.parse(raw);
  const tokens = raw.match(/"(?:\\.|[^"\\])*"|[{}\[\]:,]|true|false|null|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/gu) ?? [];
  let position = 0;
  function visit(depth: number): void {
    if (depth > 32) throw new Error('Invalid assessment JSON');
    const token = tokens[position++];
    if (token === '{') {
      const names = new Set<string>();
      if (tokens[position] === '}') { position++; return; }
      while (true) {
        const name = JSON.parse(tokens[position++]) as string;
        if (names.has(name) || tokens[position++] !== ':') throw new Error('Invalid assessment JSON');
        names.add(name);
        visit(depth + 1);
        const next = tokens[position++];
        if (next === '}') return;
        if (next !== ',') throw new Error('Invalid assessment JSON');
      }
    }
    if (token === '[') {
      if (tokens[position] === ']') { position++; return; }
      while (true) {
        visit(depth + 1);
        const next = tokens[position++];
        if (next === ']') return;
        if (next !== ',') throw new Error('Invalid assessment JSON');
      }
    }
    if (token === undefined || /^-?\d/u.test(token) && /[.eE]/u.test(token)) throw new Error('Invalid assessment JSON');
  }
  visit(0);
  if (position !== tokens.length) throw new Error('Invalid assessment JSON');
  return value;
}
function assessmentTime(value: unknown): value is string {
  if (!timestamp(value)) return false;
  const date = value.slice(0, 10).split('-').map(Number);
  const calendar = new Date(0);
  calendar.setUTCFullYear(date[0], date[1] - 1, date[2]);
  return calendar.getUTCFullYear() === date[0] && calendar.getUTCMonth() === date[1] - 1 && calendar.getUTCDate() === date[2] && Number(value.slice(11, 13)) <= 23 && Number(value.slice(14, 16)) <= 59 && Number(value.slice(17, 19)) <= 59 && (Date.parse(value) !== -62135596800000 || /\.\d*[1-9]\d*(?:Z|[+-])/u.test(value));
}
function validAssessmentAnchor(value: unknown): boolean {
  if (!record(value) || !keys(value, ['repository_id', 'repository', 'pull_number', 'parent_pull_number', 'candidate_sha', 'base_sha', 'parent_sha', 'merge_base_sha', 'patch_sha256', 'base_ref', 'observed_at'])) return false;
  const repository = typeof value.repository === 'string' ? value.repository.split('/') : [];
  return positive(value.repository_id) && repository.length === 2 && repository.every(part => /^[A-Za-z0-9._-]{1,100}$/u.test(part) && part !== '.' && part !== '..') && pullNumber(value.pull_number) && nonnegative(value.parent_pull_number) && value.parent_pull_number <= 2147483647 && [value.candidate_sha, value.base_sha, value.parent_sha, value.merge_base_sha].every(item => typeof item === 'string' && /^[a-f0-9]{40}$/u.test(item)) && typeof value.patch_sha256 === 'string' && /^[a-f0-9]{64}$/u.test(value.patch_sha256) && checkpointText(value.base_ref, 256, true) && !/[\u0080-\u009f]/u.test(value.base_ref) && assessmentTime(value.observed_at);
}
function validPolicyAssessment(value: unknown, project: string, evidence: string): value is ReviewPolicyAssessment {
  if (!record(value) || !keys(value, ['project_id', 'evidence_id', 'anchor', 'source', 'policy_revision', 'evaluated_at', 'state', 'channels'], ['incoming_routes_pending', 'incoming_routes_capacity', 'reason']) || value.project_id !== project || value.evidence_id !== evidence || !validAssessmentAnchor(value.anchor) || value.source !== 'captured_comparison' || !nonnegative(value.policy_revision) || !assessmentTime(value.evaluated_at) || !Array.isArray(value.channels) || value.channels.length > 2) return false;
  const channels = value.channels;
  if (!channels.every(item => record(item) && keys(item, ['channel', 'covered', 'pending']) && (item.channel === 'cli' || item.channel === 'hosted') && typeof item.covered === 'boolean' && typeof item.pending === 'boolean') || new Set(channels.map(item => (item as Record<string, unknown>).channel)).size !== channels.length) return false;
  if (value.incoming_routes_pending !== undefined && !nonnegative(value.incoming_routes_pending) || value.incoming_routes_capacity !== undefined && typeof value.incoming_routes_capacity !== 'boolean' || value.reason !== undefined && typeof value.reason !== 'string') return false;
  const incoming = value.incoming_routes_pending ?? 0;
  const capacity = value.incoming_routes_capacity ?? false;
  const reason = value.reason ?? '';
  if (!nonnegative(incoming) || incoming > 100 || typeof capacity !== 'boolean' || (capacity && incoming !== 0) || (capacity ? reason !== 'routed_findings_capacity' : incoming > 0 ? reason !== 'routed_findings_pending' : reason !== '')) return false;
  if (value.state === 'unconfigured') return value.policy_revision === 0 && channels.length === 0 && incoming === 0 && !capacity;
  return value.policy_revision > 0 && channels.length > 0 && (value.state === 'sufficient' || value.state === 'held') && (incoming === 0 && !capacity && channels.every(item => (item as Record<string, unknown>).covered && !(item as Record<string, unknown>).pending)) === (value.state === 'sufficient');
}
const reviewMetadata = ['project_id', 'revision', 'snapshot_id', 'source', 'observed_at', 'recorded_at'];
const reviewOutcomes = new Set(['completed', 'active', 'prepared', 'posting', 'failed', 'cancelled', 'rate_limited', 'uncertain', 'unknown']);
const reviewAttemptStates = new Set([...reviewOutcomes, 'preparing', 'held']);
function positive(value: unknown): value is number { return Number.isSafeInteger(value) && Number(value) > 0; }
function nonnegative(value: unknown): value is number { return Number.isSafeInteger(value) && Number(value) >= 0; }
function pullNumber(value: unknown): value is number { return positive(value) && value <= 2147483647; }
function reviewSnapshotID(value: unknown): value is string { return typeof value === 'string' && /^review_snapshot_[a-f0-9]{32}$/u.test(value); }
function reviewRunID(value: unknown): value is string { return typeof value === 'string' && /^review_run_[a-f0-9]{32}$/u.test(value); }
function sha(value: unknown): value is string { return typeof value === 'string' && (value === '' || /^[a-f0-9]{40}$/u.test(value)); }
function maybeTimestamp(value: unknown): value is string { return value === '' || timestamp(value); }
function orderedTimes(start: string, finish: string): boolean { return !start || !finish || Date.parse(start) <= Date.parse(finish); }
function validReviewSnapshot(value: Record<string, unknown>, project: string, expected?: string): boolean {
  return value.project_id === project && positive(value.revision) && reviewSnapshotID(value.snapshot_id) && (expected === undefined || value.snapshot_id === expected) && checkpointText(value.source, 128, true) && timestamp(value.observed_at) && timestamp(value.recorded_at);
}
function validReviewRun(value: unknown, number?: number): value is ReviewRunSummary {
  return record(value) && keys(value, ['source', 'id', 'pull_number', 'source_run_id', 'channel', 'head_sha', 'reviewer', 'scope', 'coverage_limits', 'outcome', 'attributable', 'complete', 'started_at', 'finished_at', 'found', 'accepted', 'routed', 'finalized', 'fixed']) && checkpointText(value.source, 128, true) && reviewRunID(value.id) && pullNumber(value.pull_number) && (number === undefined || value.pull_number === number) && checkpointText(value.source_run_id, 256, true) && typeof value.channel === 'string' && ['cli', 'hosted', 'manual', 'subagent'].includes(value.channel) && sha(value.head_sha) && checkpointText(value.reviewer, 128, false) && checkpointText(value.scope, 128, true) && Array.isArray(value.coverage_limits) && value.coverage_limits.length <= 16 && value.coverage_limits.every(item => checkpointText(item, 1024, true)) && typeof value.outcome === 'string' && reviewOutcomes.has(value.outcome) && typeof value.attributable === 'boolean' && typeof value.complete === 'boolean' && maybeTimestamp(value.started_at) && maybeTimestamp(value.finished_at) && orderedTimes(value.started_at, value.finished_at) && ((value.outcome !== 'completed' && !value.complete) || (!!value.started_at && !!value.finished_at)) && nonnegative(value.found) && nonnegative(value.accepted) && nonnegative(value.routed) && value.accepted + value.routed <= value.found && nonnegative(value.fixed) && value.fixed <= value.found && typeof value.finalized === 'boolean';
}
function validReviewAttempt(value: unknown): value is ReviewAttemptSummary {
  return record(value) && keys(value, ['source_attempt_id', 'channel', 'head_sha', 'state', 'started_at', 'finished_at', 'duration_seconds', 'source_run_id', 'trigger_id', 'provider_review_id']) && checkpointText(value.source_attempt_id, 256, true) && typeof value.channel === 'string' && ['cli', 'hosted', 'manual', 'subagent'].includes(value.channel) && sha(value.head_sha) && typeof value.state === 'string' && reviewAttemptStates.has(value.state) && maybeTimestamp(value.started_at) && maybeTimestamp(value.finished_at) && orderedTimes(value.started_at, value.finished_at) && (value.state !== 'completed' || (!!value.started_at && !!value.finished_at)) && (value.duration_seconds === null || nonnegative(value.duration_seconds)) && checkpointText(value.source_run_id, 256, false) && checkpointText(value.trigger_id, 128, false) && checkpointText(value.provider_review_id, 128, false);
}
function validReviewAttemptItem(value: unknown): value is ReviewAttemptItem {
  if (!record(value) || !checkpointText(value.source, 128, true)) return false;
  const { source: _source, ...attempt } = value;
  return validReviewAttempt(attempt);
}
function validReviewPull(value: unknown): value is ReviewPullSummary {
  if (!record(value) || !keys(value, ['number', 'title', 'state', 'head_sha', 'base_ref', 'base_sha', 'ci_state', 'mergeability', 'complete', 'run_count', 'attempt_count', 'recent_runs', 'latest_attempts']) || !pullNumber(value.number) || !checkpointText(value.title, 500, true) || (typeof value.state !== 'string' || !['open', 'merged', 'closed', 'unknown'].includes(value.state)) || !sha(value.head_sha) || !checkpointText(value.base_ref, 256, false) || !sha(value.base_sha) || (typeof value.ci_state !== 'string' || !['unknown', 'pending', 'success', 'failure'].includes(value.ci_state)) || (typeof value.mergeability !== 'string' || !['unknown', 'mergeable', 'conflicting'].includes(value.mergeability)) || typeof value.complete !== 'boolean' || !nonnegative(value.run_count) || !nonnegative(value.attempt_count) || !Array.isArray(value.recent_runs) || value.recent_runs.length > Math.min(3, value.run_count) || !value.recent_runs.every(item => validReviewRun(item, Number(value.number))) || value.run_count > 0 && value.recent_runs.length === 0 || !Array.isArray(value.latest_attempts) || value.latest_attempts.length > Math.min(4, value.attempt_count) || !value.latest_attempts.every(validReviewAttempt) || value.attempt_count > 0 && value.latest_attempts.length === 0) return false;
  return new Set(value.recent_runs.map(item => item.id)).size === value.recent_runs.length && new Set(value.latest_attempts.map(item => item.channel)).size === value.latest_attempts.length;
}
function validFindingRoute(value: unknown): boolean {
  if (!record(value) || !keys(value, ['id','target_pull_number','revision','state','disposition','reason','actor','decided_at','fix_sha','proof_note','fixed_at']) || typeof value.id !== 'string' || !/^review_route_[a-f0-9]{32}$/u.test(value.id) || !pullNumber(value.target_pull_number) || !positive(value.revision) || typeof value.state !== 'string' || !['active','superseded'].includes(value.state) || typeof value.disposition !== 'string' || !['unadjudicated','accepted','rejected'].includes(value.disposition) || !checkpointText(value.reason,2048,false) || !checkpointText(value.actor,128,false) || !maybeTimestamp(value.decided_at) || !sha(value.fix_sha) || !checkpointText(value.proof_note,2048,false) || !maybeTimestamp(value.fixed_at)) return false;
  if (value.disposition === 'unadjudicated') return value.reason === '' && value.actor === '' && value.decided_at === '' && value.fix_sha === '' && value.proof_note === '' && value.fixed_at === '';
  if (!checkpointText(value.reason,2048,true) || !id(value.actor) || !timestamp(value.decided_at)) return false;
  if (value.fix_sha === '') return value.proof_note === '' && value.fixed_at === '';
  return value.disposition === 'accepted' && checkpointText(value.proof_note,2048,true) && timestamp(value.fixed_at);
}
function validReviewFinding(value: unknown): value is ReviewFinding {
  return record(value) && keys(value, ['source_finding_id', 'title', 'detail', 'severity', 'disposition', 'decision_reason', 'actor', 'decided_at', 'fix_sha', 'proof_note', 'resolved_at'], ['route']) && (value.route === undefined || value.disposition === 'routed' && validFindingRoute(value.route)) && checkpointText(value.source_finding_id, 256, true) && checkpointText(value.title, 500, true) && checkpointText(value.detail, 16 << 10, false) && typeof value.severity === 'string' && ['info', 'low', 'medium', 'high', 'critical', 'unknown'].includes(value.severity) && typeof value.disposition === 'string' && ['unadjudicated', 'accepted', 'routed', 'rejected'].includes(value.disposition) && checkpointText(value.decision_reason, 2048, false) && checkpointText(value.actor, 128, false) && maybeTimestamp(value.decided_at) && sha(value.fix_sha) && checkpointText(value.proof_note, 2048, false) && maybeTimestamp(value.resolved_at) && (value.disposition === 'accepted' || value.fix_sha === '' && value.proof_note === '' && value.resolved_at === '') && (value.resolved_at === '' || value.fix_sha !== '');
}
function reviewQuery(snapshot?: string): URLSearchParams {
  if (snapshot !== undefined && !reviewSnapshotID(snapshot)) throw new ApiError('invalid_request', 'The review snapshot identifier is invalid.');
  return new URLSearchParams(snapshot === undefined ? {} : { snapshot });
}
function validReviewPage(value: unknown, options: PageOptions): value is Record<string, unknown> & { items: unknown[]; has_more: boolean; cursor?: string } {
  return validPage(value, defaultLimit(options)) && (value.has_more || !value.cursor);
}
function validInboxItem(value: unknown, worker: string): value is InboxItem {
  return record(value) && keys(value, ['id', 'conversation_id', 'sender_worker_id', 'recipient_worker_id', 'preview', 'preview_truncated', 'seen', 'acknowledged', 'created_at']) && id(value.id) && id(value.conversation_id) && id(value.sender_worker_id) && value.recipient_worker_id === worker && contentText(value.preview, 1024, true) && [...value.preview].length <= 160 && typeof value.preview_truncated === 'boolean' && typeof value.seen === 'boolean' && typeof value.acknowledged === 'boolean' && (!value.acknowledged || value.seen) && timestamp(value.created_at);
}
function validMessage(value: unknown, conversation: string): value is Message {
  return record(value) && keys(value, ['id', 'conversation_id', 'sender_worker_id', 'recipient_worker_id', 'body', 'created_at'], ['reply_to_message_id']) && id(value.id) && value.conversation_id === conversation && id(value.sender_worker_id) && id(value.recipient_worker_id) && (value.reply_to_message_id === undefined || id(value.reply_to_message_id)) && contentText(value.body, 16384) && timestamp(value.created_at);
}
function validEvent(value: unknown, project: string, worker: string): value is MessageEvent {
  if (!record(value) || !keys(value, ['sequence', 'project_id', 'type', 'resource_id', 'revision', 'actor_id', 'payload', 'created_at']) || !Number.isSafeInteger(value.sequence) || Number(value.sequence) < 1 || value.project_id !== project || !id(value.resource_id) || !Number.isSafeInteger(value.revision) || Number(value.revision) < 1 || !id(value.actor_id) || !timestamp(value.created_at) || !record(value.payload)) return false;
  if (value.type === 'message.sent') return keys(value.payload, ['conversation_id', 'recipient_worker_id', 'sender_worker_id']) && id(value.payload.conversation_id) && value.payload.recipient_worker_id === worker && id(value.payload.sender_worker_id);
  if (value.type === 'message.seen' || value.type === 'message.acknowledged') return keys(value.payload, ['conversation_id', 'recipient_worker_id']) && id(value.payload.conversation_id) && value.payload.recipient_worker_id === worker;
  return false;
}
export function validateMessageEventPage(value: unknown, project: string, worker: string, limit: number): value is MessageEventPage {
  if (!record(value) || !keys(value, ['items', 'cursor', 'has_more', 'timed_out']) || !Array.isArray(value.items) || value.items.length > limit || !cursor(value.cursor) || typeof value.has_more !== 'boolean' || typeof value.timed_out !== 'boolean' || value.has_more && value.items.length === 0 || value.timed_out && (value.items.length > 0 || value.has_more)) return false;
  let previous = 0;
  for (const event of value.items) { if (!validEvent(event, project, worker) || event.sequence <= previous) return false; previous = event.sequence; }
  return true;
}

export class Api {
  readonly projectId: string;
  private token: string;
  private readonly session = new AbortController();
  private closed = false;
  private verifiedInstanceId?: string;
  private activeWatch?: AbortController;
  private activeMetadataWatch?: AbortController;
  private browserSessionID?: string;
  private pendingBrowserSessionID?: string;
  private expectedInstanceID?: string;
  private capabilities: BrowserCapabilities = { allow_writes: false, can_edit_presentation: false };
  private renewal?: Promise<BrowserSessionView>;
  private currentSession?: BrowserSessionView;
  private readonly streamConnections = new Set<AbortController>();

  constructor(token: string, projectId: string) {
    if (!text(token, 4096) || !id(projectId)) throw new ApiError('invalid_binding', 'Select a valid project and credential.');
    this.token = token;
    this.projectId = projectId;
  }
  get instanceId(): string | undefined { return this.verifiedInstanceId; }
  get browserSession(): BrowserSessionView | undefined { return this.currentSession && { ...this.currentSession }; }
  get browserCapabilities(): BrowserCapabilities { return { ...this.capabilities }; }

  static resume(projectID: string, sessionID: string, expectedInstanceID: string): Api {
    if (!sessionIdentity(projectID) || !sessionSelector(sessionID) || !sessionIdentity(expectedInstanceID)) throw new ApiError('invalid_binding', 'The saved project session is invalid.');
    // The public nonce satisfies constructor admission but is never sent as a
    // bearer credential: the factory switches mode synchronously before return.
    const api = new Api(sessionID, projectID);
    api.token = '';
    api.browserSessionID = sessionID;
    api.expectedInstanceID = expectedInstanceID;
    return api;
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.verifiedInstanceId = undefined;
    this.capabilities = { allow_writes: false, can_edit_presentation: false };
    this.currentSession = undefined;
    this.token = '';
    this.session.abort();
    this.activeWatch?.abort();
    this.activeMetadataWatch?.abort();
  }

  async connect(signal?: AbortSignal): Promise<Project> {
    this.ensureOpen();
    const budget = requestBudget([signal, this.session.signal], 5000);
    this.verifiedInstanceId = undefined;
    this.capabilities = { allow_writes: false, can_edit_presentation: false };
    this.currentSession = undefined;
    try {
      const value = await this.getJson('/api/v1/instance', this.expectedInstanceID, budget.signal);
      if (!record(value) || !keys(value, ['instance_id']) || !id(value.instance_id)) throw new ApiError('invalid_response', 'The service returned an invalid instance identity.');
      if (this.expectedInstanceID && value.instance_id !== this.expectedInstanceID) throw new ApiError('instance_mismatch', 'The service identity differs from this saved project session.', 409);
      const project = await this.getJson(this.projectRoute(''), value.instance_id, budget.signal);
      if (!validProject(project, this.projectId)) throw new ApiError('invalid_response', 'The service returned a project outside this binding.');
      let capabilities: BrowserCapabilities = { allow_writes: false, can_edit_presentation: false };
      let session: BrowserSessionView | undefined;
      if (this.browserSessionID) {
        const current = await this.getJson(this.projectRoute('browser-session'), value.instance_id, budget.signal, undefined, 4096, assessmentJSON);
        if (!browserSessionView(current, this.browserSessionID, value.instance_id, this.projectId)) throw new ApiError('invalid_response', 'The service returned an invalid browser session.');
        capabilities = { allow_writes: current.allow_writes, can_edit_presentation: current.can_edit_presentation };
        session = current;
      }
      if (budget.signal.aborted) throw new ApiError('cancelled', 'The browser connection was cancelled.');
      this.verifiedInstanceId = value.instance_id;
      this.capabilities = capabilities;
      this.currentSession = session;
      return project;
    } catch (error) { throw this.transportError(error, budget.expired(), budget.signal); }
    finally { budget.close(); }
  }

  async startBrowserSession(signal?: AbortSignal): Promise<BrowserSessionView> {
    this.ensureOpen();
    if (!this.verifiedInstanceId || !this.token || this.browserSessionID || this.pendingBrowserSessionID) throw new ApiError('not_connected', 'Verify the project credential before starting a browser session.');
    const selector = newSessionID();
    const instance = this.verifiedInstanceId;
    const budget = requestBudget([signal, this.session.signal], 5000);
    // Keep the selector for best-effort cleanup even if the POST response is lost.
    this.pendingBrowserSessionID = selector;
    this.expectedInstanceID = instance;
    try {
      const headers = new Headers({ Authorization: `Bearer ${this.token}`, Accept: 'application/json', 'Content-Type': 'application/json', 'X-FC-Instance': instance });
      const response = await fetch(urlFor(this.projectRoute('browser-session')), { method: 'POST', headers, body: JSON.stringify({ session_id: selector }), redirect: 'error', credentials: 'same-origin', cache: 'no-store', signal: budget.signal });
      if (!response.ok) throw await readFailure(response, budget.signal);
      const value = await readJson(response, budget.signal, 4096, assessmentJSON);
      if (!browserSessionView(value, selector, instance, this.projectId)) throw new ApiError('invalid_response', 'The service returned an invalid browser session.');
      if (budget.signal.aborted) throw new ApiError('cancelled', 'The browser connection was cancelled.');
      this.browserSessionID = selector;
      this.pendingBrowserSessionID = undefined;
      this.token = '';
      this.capabilities = { allow_writes: value.allow_writes, can_edit_presentation: value.can_edit_presentation };
      this.currentSession = { ...value };
      return value;
    } catch (error) { throw this.transportError(error, budget.expired(), budget.signal); }
    finally { budget.close(); }
  }

  async endBrowserSession(signal?: AbortSignal): Promise<void> {
    const selector = this.browserSessionID ?? this.pendingBrowserSessionID;
    if (!selector) return;
    const budget = requestBudget([signal], 5000);
    try {
      const headers = new Headers({ Accept: 'application/json', 'Content-Type': 'application/json', 'X-FC-Browser-Session': selector });
      if (this.expectedInstanceID) headers.set('X-FC-Instance', this.expectedInstanceID);
      const response = await fetch(urlFor(this.projectRoute('browser-session')), { method: 'DELETE', headers, body: '{}', redirect: 'error', credentials: 'same-origin', cache: 'no-store', signal: budget.signal });
      if (!response.ok) throw await readFailure(response, budget.signal);
      const value = await readJson(response, budget.signal, 4096);
      if (!record(value) || !keys(value, ['session_id', 'logged_out']) || value.session_id !== selector || value.logged_out !== true) throw new ApiError('invalid_response', 'The service returned an invalid logout response.');
      this.capabilities = { allow_writes: false, can_edit_presentation: false };
      this.currentSession = undefined;
    } catch (error) { throw this.transportError(error, budget.expired(), budget.signal); }
    finally { budget.close(); }
  }

  renewBrowserSession(signal?: AbortSignal): Promise<BrowserSessionView> {
    if (this.closed) return Promise.reject(new ApiError('cancelled', 'This service session is closed.'));
    if (!this.browserSessionID || !this.verifiedInstanceId) return Promise.reject(new ApiError('not_connected', 'Verify the browser session before renewing it.'));
    if (this.renewal) return this.renewal;
    const pending = this.renewSession(signal).finally(() => { if (this.renewal === pending) this.renewal = undefined; });
    this.renewal = pending;
    return pending;
  }

  private async renewSession(signal?: AbortSignal): Promise<BrowserSessionView> {
    const selector = this.browserSessionID!, instance = this.verifiedInstanceId!;
    const budget = requestBudget([signal, this.session.signal], 5000);
    let dispatched = false, knownRefusal = false;
    try {
      const headers = this.headers(instance);
      headers.set('Content-Type', 'application/json');
      if (budget.signal.aborted) throw new DOMException('Aborted', 'AbortError');
      dispatched = true;
      const response = await fetch(urlFor(this.projectRoute('browser-session/renew')), { method: 'POST', headers, body: '{}', redirect: 'error', credentials: 'same-origin', cache: 'no-store', signal: budget.signal });
      knownRefusal = !response.ok && response.status < 500;
      if (!response.ok) throw await readFailure(response, budget.signal);
      const value = await readJson(response, budget.signal, 4096, assessmentJSON);
      if (!browserSessionView(value, selector, instance, this.projectId)) throw new ApiError('invalid_response', 'The service returned an invalid browser session.');
      if (budget.signal.aborted || this.closed) throw new DOMException('Aborted', 'AbortError');
      this.capabilities = { allow_writes: value.allow_writes, can_edit_presentation: value.can_edit_presentation };
      this.currentSession = { ...value };
      // Replace transport connections, not watch lifetimes. Their processed
      // cursors survive, so renewal does not resnapshot or expand archives.
      for (const connection of this.streamConnections) connection.abort();
      return value;
    } catch (error) {
      // Set-Cookie can succeed before its response body is lost. Reopen using
      // the actual cookie instead of trusting an old connection's expiry.
      if (dispatched && !knownRefusal) for (const connection of this.streamConnections) connection.abort();
      throw this.transportError(error, budget.expired(), budget.signal);
    } finally { budget.close(); }
  }

  async sendMessage(worker: string, recipient: string, body: string, replyTo: string, key: string, signal?: AbortSignal): Promise<BrowserMessageReceipt & { outcome: 'sent' }> {
    if (!id(worker) || !id(recipient) || !contentText(body, 16 << 10) || body.trim() === '' || replyTo !== '' && !id(replyTo) || !mutationKey(key)) throw new ApiError('invalid_request', 'Use a selected worker, recipient, bounded message and original action key.');
    return this.browserMutation(this.projectRoute('messages'), 'POST', worker, key, { recipient_worker_id: recipient, body, reply_to_message_id: replyTo }, 201,
      (value): value is BrowserMessageReceipt & { outcome: 'sent' } => record(value) && keys(value, ['id', 'conversation_id', 'outcome']) && id(value.id) && id(value.conversation_id) && value.outcome === 'sent', signal);
  }
  async markMessageSeen(worker: string, messageId: string, key: string, signal?: AbortSignal): Promise<BrowserMessageReceipt> {
    return this.messageTransition(worker, messageId, key, false, signal);
  }
  async acknowledgeMessage(worker: string, messageId: string, key: string, signal?: AbortSignal): Promise<BrowserMessageReceipt> {
    return this.messageTransition(worker, messageId, key, true, signal);
  }
  private async messageTransition(worker: string, messageId: string, key: string, acknowledge: boolean, signal?: AbortSignal): Promise<BrowserMessageReceipt> {
    if (!id(worker) || !id(messageId) || !mutationKey(key)) throw new ApiError('invalid_request', 'Use the selected recipient, message and original action key.');
    return this.browserMutation(this.projectRoute(`messages/${encodeURIComponent(messageId)}/${acknowledge ? 'ack' : 'seen'}`), 'POST', worker, key, {}, 200,
      (value): value is BrowserMessageReceipt => record(value) && keys(value, ['id', 'conversation_id', 'outcome']) && value.id === messageId && id(value.conversation_id) && (value.outcome === 'unchanged' || value.outcome === (acknowledge ? 'acknowledged' : 'seen')), signal);
  }
  async replacePresentation(worker: string, config: BrowserPresentationConfig, expectedRevision: number, key: string, signal?: AbortSignal): Promise<BrowserPresentationReceipt> {
    if (!id(worker) || !validPresentationConfig(config) || !Number.isSafeInteger(expectedRevision) || expectedRevision < 0 || expectedRevision >= Number.MAX_SAFE_INTEGER || !mutationKey(key)) throw new ApiError('invalid_request', 'Use a validated presentation, observed revision, selected worker and original action key.');
    return this.browserMutation(this.projectRoute('presentation'), 'PUT', worker, key, { expected_revision: expectedRevision, presentation: config }, 200,
      (value): value is BrowserPresentationReceipt => record(value) && keys(value, ['project_id', 'revision', 'outcome']) && value.project_id === this.projectId && (value.outcome === 'updated' && value.revision === expectedRevision + 1 || value.outcome === 'unchanged' && value.revision === expectedRevision), signal, true);
  }

  private async browserMutation<T>(path: string, method: 'POST' | 'PUT', worker: string, key: string, body: unknown, expectedStatus: number, validate: (value: unknown) => value is T, signal?: AbortSignal, presentation = false): Promise<T> {
    this.ensureOpen();
    if (!this.browserSessionID || !this.verifiedInstanceId) throw new ApiError('not_connected', 'Verify the browser session before changing this project.');
    if (!this.capabilities.allow_writes || presentation && !this.capabilities.can_edit_presentation) throw new ApiError('forbidden', 'This browser session does not permit this action.', 403);
    const budget = requestBudget([signal, this.session.signal], 5000);
    let dispatched = false, knownRefusal = false;
    try {
      const url = urlFor(path), headers = this.headers(this.verifiedInstanceId, worker);
      headers.set('Idempotency-Key', key); headers.set('Content-Type', 'application/json');
      const encoded = JSON.stringify(body);
      if (budget.signal.aborted) throw new DOMException('Aborted', 'AbortError');
      // Once Fetch is invoked, cancellation or transport failure cannot prove
      // that the transaction was unsent. Recovery keeps this exact body/key.
      dispatched = true;
      const response = await fetch(url, { method, headers, body: encoded, redirect: 'error', credentials: 'same-origin', cache: 'no-store', signal: budget.signal });
      if (response.status !== expectedStatus) {
        if (response.status < 400 || response.status >= 600) { void response.body?.cancel().catch(() => {}); throw new ApiError('invalid_response', 'The action returned an unexpected response.', response.status); }
        const value = await readJson(response, budget.signal, 64 << 10, assessmentJSON);
        if (!record(value) || !keys(value, ['error', 'request_id']) || !id(value.request_id) || !record(value.error) || !keys(value.error, ['code', 'message'], ['details']) || !text(value.error.code, 64) || !text(value.error.message, 512)) throw new ApiError('invalid_response', 'The action returned an invalid refusal.', response.status);
        knownRefusal = response.status < 500 && !(record(value.error.details) && value.error.details.outcome_unknown === true);
        throw serverError(value, response.status);
      }
      const value = await readJson(response, budget.signal, 4096, assessmentJSON);
      if (budget.signal.aborted) throw new DOMException('Aborted', 'AbortError');
      if (!validate(value)) throw new ApiError('invalid_response', 'The action result could not be verified.', response.status);
      return value;
    } catch (error) {
      const failure = this.transportError(error, budget.expired(), budget.signal);
      throw new ApiError(failure.code, failure.message, failure.status, dispatched && !knownRefusal);
    } finally { budget.close(); }
  }

  async workers(options: PageOptions = {}, signal?: AbortSignal): Promise<Page<Worker>> {
    const value = await this.finiteGet(withQuery(this.projectRoute('workers'), pageOptions(options)), signal);
    if (!validPage(value, defaultLimit(options)) || !value.items.every((item) => validWorker(item, this.projectId))) throw new ApiError('invalid_response', 'The service returned an invalid worker page.');
    return value as Page<Worker>;
  }
  async worker(workerId: string, signal?: AbortSignal): Promise<Worker> {
    if (!id(workerId)) throw new ApiError('invalid_request', 'The worker identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`workers/${encodeURIComponent(workerId)}`), signal, undefined, 4096, assessmentJSON);
    if (!validWorker(value, this.projectId) || value.id !== workerId) throw new ApiError('invalid_response', 'The service returned an invalid worker.');
    return value;
  }
  async workerPrimary(workerId: string, signal?: AbortSignal): Promise<WorkerPrimary> {
    if (!id(workerId)) throw new ApiError('invalid_request', 'The worker identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`workers/${encodeURIComponent(workerId)}/primary-job`), signal, undefined, 4096, assessmentJSON);
    if (!validWorkerPrimary(value, workerId)) throw new ApiError('invalid_response', 'The service returned invalid worker focus.');
    return value;
  }
  async workerPrimaryHistory(workerId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<Page<WorkerPrimaryHistoryEntry>> {
    if (!id(workerId)) throw new ApiError('invalid_request', 'The worker identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`workers/${encodeURIComponent(workerId)}/primary-job/history`), coordinationQuery(options)), signal, undefined, maxBytes, assessmentJSON);
    if (!coordinationPage(value, defaultLimit(options))) throw new ApiError('invalid_response', 'The service returned invalid focus history.');
    let previous = Infinity;
    for (const item of value.items) {
      if (!record(item) || !keys(item, ['worker_id', 'revision', 'job_id', 'actor_id', 'actor_worker_id', 'reason', 'created_at']) || item.worker_id !== workerId || !positive(item.revision) || item.revision >= previous || !id(item.actor_id) || item.actor_worker_id !== '' && !id(item.actor_worker_id) || !coordinationTime(item.created_at) || (item.reason === 'selected' ? !id(item.job_id) : typeof item.reason !== 'string' || !['cleared', 'job_reassigned'].includes(item.reason) || item.job_id !== '')) throw new ApiError('invalid_response', 'The service returned invalid focus history.');
      previous = item.revision;
    }
    return value as Page<WorkerPrimaryHistoryEntry>;
  }
  async workerPause(workerId: string, signal?: AbortSignal): Promise<WorkerPause> {
    if (!id(workerId)) throw new ApiError('invalid_request', 'The worker identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`workers/${encodeURIComponent(workerId)}/pause`), signal, undefined, 8 << 10, assessmentJSON);
    if (!validWorkerPause(value, workerId)) throw new ApiError('invalid_response', 'The service returned invalid worker pause state.');
    return value;
  }
  async workerPauseHistory(workerId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<Page<WorkerPauseHistoryEntry>> {
    if (!id(workerId)) throw new ApiError('invalid_request', 'The worker identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`workers/${encodeURIComponent(workerId)}/pause/history`), coordinationQuery(options)), signal, undefined, maxBytes, assessmentJSON);
    if (!coordinationPage(value, defaultLimit(options))) throw new ApiError('invalid_response', 'The service returned invalid pause history.');
    let previous = Infinity;
    for (const item of value.items) {
      if (!record(item) || !keys(item, ['worker_id', 'revision', 'paused', 'reason', 'actor_id', 'actor_worker_id', 'created_at']) || item.worker_id !== workerId || !positive(item.revision) || item.revision >= previous || !validPauseFields(item) || !id(item.actor_id) || item.actor_worker_id !== '' && !id(item.actor_worker_id) || !coordinationTime(item.created_at)) throw new ApiError('invalid_response', 'The service returned invalid pause history.');
      previous = item.revision;
    }
    return value as Page<WorkerPauseHistoryEntry>;
  }
  async notes(options: NoteOptions = {}, signal?: AbortSignal): Promise<Page<NoteSummary>> {
    const query = coordinationQuery(options);
    for (const field of ['worker_id', 'job_id'] as const) if (options[field] !== undefined) { if (!id(options[field])) throw new ApiError('invalid_request', 'The note scope filter is invalid.'); query.set(field, options[field]); }
    if (options.phase !== undefined) { if (!coordinationText(options.phase, 200, true)) throw new ApiError('invalid_request', 'The note phase filter is invalid.'); query.set('phase', options.phase); }
    if (options.kind !== undefined) { if (options.kind !== 'reminder' && options.kind !== 'source') throw new ApiError('invalid_request', 'The note kind filter is invalid.'); query.set('kind', options.kind); }
    if (options.status !== undefined) { if (!['pending', 'consumed', 'dismissed', 'all'].includes(options.status)) throw new ApiError('invalid_request', 'The note status filter is invalid.'); query.set('status', options.status); }
    if (options.include_assigned_jobs !== undefined) { if (typeof options.include_assigned_jobs !== 'boolean' || options.include_assigned_jobs && options.worker_id === undefined) throw new ApiError('invalid_request', 'Assigned-job notes require a worker filter.'); query.set('include_assigned_jobs', String(options.include_assigned_jobs)); }
    const value = await this.finiteGet(withQuery(this.projectRoute('notes'), query), signal, undefined, 512 << 10, assessmentJSON);
    if (!coordinationPage(value, defaultLimit(options))) throw new ApiError('invalid_response', 'The service returned an invalid note page.');
    const seen = new Set<string>(); let previous: NoteSummary | undefined;
    for (const item of value.items) {
      if (!validNoteSummary(item, this.projectId) || seen.has(item.id) || options.job_id !== undefined && item.job_id !== options.job_id || options.phase !== undefined && item.phase !== options.phase || options.kind !== undefined && item.kind !== options.kind || options.status !== 'all' && item.status !== (options.status ?? 'pending') || options.worker_id !== undefined && !options.include_assigned_jobs && item.worker_id !== options.worker_id) throw new ApiError('invalid_response', 'The service returned an invalid note page.');
      if (previous && (coordinationTimeKey(item.created_at) > coordinationTimeKey(previous.created_at) || coordinationTimeKey(item.created_at) === coordinationTimeKey(previous.created_at) && item.id >= previous.id)) throw new ApiError('invalid_response', 'The service returned an unordered note page.');
      seen.add(item.id); previous = item;
    }
    return value as Page<NoteSummary>;
  }
  async note(noteId: string, signal?: AbortSignal): Promise<Note> {
    if (!id(noteId)) throw new ApiError('invalid_request', 'The note identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`notes/${encodeURIComponent(noteId)}`), signal, undefined, 128 << 10, assessmentJSON);
    if (!validNote(value, this.projectId, noteId)) throw new ApiError('invalid_response', 'The service returned an invalid note.');
    return value;
  }
  async noteHistory(noteId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<Page<NoteHistorySummary>> {
    if (!id(noteId)) throw new ApiError('invalid_request', 'The note identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`notes/${encodeURIComponent(noteId)}/history`), coordinationQuery(options)), signal, undefined, 512 << 10, assessmentJSON);
    if (!coordinationPage(value, defaultLimit(options))) throw new ApiError('invalid_response', 'The service returned invalid note history.');
    let previous = Infinity;
    for (const item of value.items) { if (!validNoteSummary(item, this.projectId, true) || item.id !== noteId || item.revision >= previous) throw new ApiError('invalid_response', 'The service returned invalid note history.'); previous = item.revision; }
    return value as Page<NoteHistorySummary>;
  }
  async noteRevision(noteId: string, revision: number, signal?: AbortSignal): Promise<NoteRevision> {
    if (!id(noteId) || !positive(revision)) throw new ApiError('invalid_request', 'The selected note revision is invalid.');
    const value = await this.finiteGet(this.projectRoute(`notes/${encodeURIComponent(noteId)}/history/${revision}`), signal, undefined, 128 << 10, assessmentJSON);
    if (!validNote(value, this.projectId, noteId, true) || value.revision !== revision) throw new ApiError('invalid_response', 'The service returned an invalid note revision.');
    return value as NoteRevision;
  }
  async jobs(options: JobOptions = {}, signal?: AbortSignal): Promise<Page<JobSummary>> {
    const query = pageOptions(options);
    if (options.worker_id !== undefined) { if (!id(options.worker_id)) throw new ApiError('invalid_request', 'The worker filter is invalid.'); query.set('worker_id', options.worker_id); }
    if (options.status !== undefined) { if (!jobStatuses.has(options.status)) throw new ApiError('invalid_request', 'The job status filter is invalid.'); query.set('status', options.status); }
    const value = await this.finiteGet(withQuery(this.projectRoute('jobs'), query), signal);
    if (!validPage(value, defaultLimit(options)) || !value.items.every(validSummary)) throw new ApiError('invalid_response', 'The service returned an invalid job page.');
    return value as Page<JobSummary>;
  }
  async job(jobId: string, signal?: AbortSignal): Promise<Job> {
    if (!id(jobId)) throw new ApiError('invalid_request', 'The job identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`jobs/${encodeURIComponent(jobId)}`), signal);
    if (!validJob(value, this.projectId) || value.id !== jobId) throw new ApiError('invalid_response', 'The service returned a job outside this binding.');
    return value;
  }
  async checkpointLatest(jobId: string, signal?: AbortSignal): Promise<LatestCheckpoint> {
    if (!id(jobId)) throw new ApiError('invalid_request', 'The job identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`jobs/${encodeURIComponent(jobId)}/checkpoints/latest`), signal, undefined, 128 << 10);
    if (!record(value) || !keys(value, ['job_id', 'job_revision', 'checkpoint']) || value.job_id !== jobId || !Number.isSafeInteger(value.job_revision) || Number(value.job_revision) < 1 || value.checkpoint !== null && (!validCheckpoint(value.checkpoint, this.projectId, jobId) || value.checkpoint.job_revision > Number(value.job_revision))) throw new ApiError('invalid_response', 'The service returned an invalid latest checkpoint.');
    return value as unknown as LatestCheckpoint;
  }
  async checkpoints(jobId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<Page<Checkpoint>> {
    if (!id(jobId)) throw new ApiError('invalid_request', 'The job identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`jobs/${encodeURIComponent(jobId)}/checkpoints`), pageOptions(options)), signal, undefined, 4 << 20);
    if (!validPage(value, defaultLimit(options)) || !value.has_more && !!value.cursor) throw new ApiError('invalid_response', 'The service returned an invalid checkpoint page.');
    let previous = Infinity;
    for (const item of value.items) {
      if (!validCheckpoint(item, this.projectId, jobId) || item.sequence >= previous) throw new ApiError('invalid_response', 'The service returned an invalid checkpoint page.');
      previous = item.sequence;
    }
    return value as Page<Checkpoint>;
  }
  async jobUpdates(jobId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<Page<JobUpdate>> {
    if (!id(jobId)) throw new ApiError('invalid_request', 'The job identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`jobs/${encodeURIComponent(jobId)}/updates`), pageOptions(options)), signal, undefined, 4 << 20, assessmentJSON);
    if (!validPage(value, defaultLimit(options)) || !value.has_more && !!value.cursor) throw new ApiError('invalid_response', 'The service returned an invalid update page.');
    let previous = Infinity;
    const seen = new Set<string>();
    for (const item of value.items) {
      if (!validJobUpdate(item, this.projectId, jobId) || item.sequence >= previous || seen.has(item.id)) throw new ApiError('invalid_response', 'The service returned an invalid update page.');
      previous = item.sequence; seen.add(item.id);
    }
    return value as Page<JobUpdate>;
  }
  async jobChecklist(jobId: string, signal?: AbortSignal): Promise<JobChecklist> {
    if (!id(jobId)) throw new ApiError('invalid_request', 'The job identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`jobs/${encodeURIComponent(jobId)}/checklist`), signal, undefined, 128 << 10, assessmentJSON);
    if (!record(value) || !keys(value, ['job_id', 'job_revision', 'revision', 'items']) || value.job_id !== jobId || !positive(value.job_revision) || !nonnegative(value.revision) || !validChecklistItems(value.items) || value.revision === 0 && value.items.length !== 0) throw new ApiError('invalid_response', 'The service returned an invalid current checklist.');
    return value as unknown as JobChecklist;
  }
  async jobChecklistHistory(jobId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<Page<ChecklistHistoryEntry>> {
    if (!id(jobId)) throw new ApiError('invalid_request', 'The job identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`jobs/${encodeURIComponent(jobId)}/checklist/history`), pageOptions(options)), signal, undefined, 4 << 20, assessmentJSON);
    if (!validPage(value, defaultLimit(options)) || !value.has_more && !!value.cursor) throw new ApiError('invalid_response', 'The service returned an invalid checklist history page.');
    let previous = Infinity;
    for (const entry of value.items) {
      if (!validChecklistHistoryEntry(entry) || entry.revision >= previous) throw new ApiError('invalid_response', 'The service returned an invalid checklist history page.');
      previous = entry.revision;
    }
    return value as Page<ChecklistHistoryEntry>;
  }
  async projectMapEntries(options: ProjectMapOptions = {}, signal?: AbortSignal): Promise<Page<ProjectMapSummary>> {
    const query = mapPageQuery(options);
    if (options.kind !== undefined) { if (options.kind !== 'workstream' && options.kind !== 'return_point') throw new ApiError('invalid_request', 'The map kind filter is invalid.'); query.set('kind', options.kind); }
    if (options.archived !== undefined && typeof options.archived !== 'boolean') throw new ApiError('invalid_request', 'The map archive filter is invalid.');
    query.set('archived', String(options.archived ?? false));
    const value = await this.finiteGet(withQuery(this.projectRoute('map/entries'), query), signal, undefined, maxBytes, assessmentJSON);
    if (!validPage(value, options.limit ?? 25) || !value.has_more && !!value.cursor) throw new ApiError('invalid_response', 'The service returned an invalid map page.');
    let previous = '';
    for (const row of value.items) {
      if (!validMapSummary(row) || row.archived !== (options.archived ?? false) || options.kind !== undefined && row.kind !== options.kind || previous !== '' && row.id <= previous) throw new ApiError('invalid_response', 'The service returned an invalid map page.');
      previous = row.id;
    }
    return value as Page<ProjectMapSummary>;
  }
  async projectMapEntry(entryId: string, signal?: AbortSignal): Promise<ProjectMapEntry> {
    if (!id(entryId)) throw new ApiError('invalid_request', 'The map entry identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`map/entries/${encodeURIComponent(entryId)}`), signal, undefined, maxBytes, assessmentJSON);
    if (!validMapEntry(value, this.projectId, entryId)) throw new ApiError('invalid_response', 'The service returned an invalid map entry.');
    return value;
  }
  async projectMapHistory(entryId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<Page<ProjectMapHistorySummary>> {
    if (!id(entryId)) throw new ApiError('invalid_request', 'The map entry identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`map/entries/${encodeURIComponent(entryId)}/history`), mapPageQuery(options)), signal, undefined, maxBytes, assessmentJSON);
    if (!validPage(value, options.limit ?? 25) || !value.has_more && !!value.cursor) throw new ApiError('invalid_response', 'The service returned invalid map history.');
    let previous = Infinity;
    for (const row of value.items) { if (!validMapHistory(row) || row.revision >= previous) throw new ApiError('invalid_response', 'The service returned invalid map history.'); previous = row.revision; }
    return value as Page<ProjectMapHistorySummary>;
  }
  async projectMapRevision(entryId: string, revision: number, signal?: AbortSignal): Promise<ProjectMapRevision> {
    if (!id(entryId) || !positive(revision)) throw new ApiError('invalid_request', 'The selected map revision is invalid.');
    const value = await this.finiteGet(this.projectRoute(`map/entries/${encodeURIComponent(entryId)}/history/${revision}`), signal, undefined, maxBytes, assessmentJSON);
    if (!record(value) || !keys(value, ['revision', 'actor_id', 'created_at', 'state']) || value.revision !== revision || !id(value.actor_id) || !assessmentTime(value.created_at) || !validMapEntry(value.state, this.projectId, entryId) || value.state.revision !== revision) throw new ApiError('invalid_response', 'The service returned an invalid captured map revision.');
    return value as unknown as ProjectMapRevision;
  }
  async jobWorkstream(jobId: string, signal?: AbortSignal): Promise<JobWorkstream> {
    if (!id(jobId)) throw new ApiError('invalid_request', 'The job identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`jobs/${encodeURIComponent(jobId)}/workstream`), signal, undefined, maxBytes, assessmentJSON);
    if (!record(value) || !keys(value, ['job_id', 'workstream_id', 'workstream_title', 'revision']) || value.job_id !== jobId || !nonnegative(value.revision) || !validWorkstreamReference(value) || value.revision === 0 && value.workstream_id !== '') throw new ApiError('invalid_response', 'The service returned an invalid workstream reference.');
    return value as unknown as JobWorkstream;
  }
  async jobWorkstreamHistory(jobId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<Page<JobWorkstreamHistoryEntry>> {
    if (!id(jobId)) throw new ApiError('invalid_request', 'The job identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`jobs/${encodeURIComponent(jobId)}/workstream/history`), mapPageQuery(options)), signal, undefined, maxBytes, assessmentJSON);
    if (!validPage(value, options.limit ?? 25) || !value.has_more && !!value.cursor) throw new ApiError('invalid_response', 'The service returned invalid workstream reference history.');
    let previous = Infinity;
    for (const row of value.items) {
      if (!record(row) || !keys(row, ['revision', 'workstream_id', 'workstream_title', 'job_revision', 'actor_id', 'created_at']) || !positive(row.revision) || row.revision >= previous || !validWorkstreamReference(row) || !positive(row.job_revision) || !id(row.actor_id) || !assessmentTime(row.created_at)) throw new ApiError('invalid_response', 'The service returned invalid workstream reference history.');
      previous = row.revision;
    }
    return value as Page<JobWorkstreamHistoryEntry>;
  }
  async reviewQueue(options: PageOptions = {}, signal?: AbortSignal): Promise<ReviewQueue> {
    const value = await this.finiteGet(withQuery(this.projectRoute('reviews/queue'), pageOptions(options)), signal);
    if (!record(value) || !keys(value, [...reviewMetadata, 'configured', 'items', 'has_more'], ['cursor']) || typeof value.configured !== 'boolean' || !Array.isArray(value.items) || !validReviewPage({ items: value.items, has_more: value.has_more, ...(value.cursor === undefined ? {} : { cursor: value.cursor }) }, options)) throw new ApiError('invalid_response', 'The service returned an invalid review queue.');
    if (value.configured ? !validReviewSnapshot(value, this.projectId) : value.project_id !== this.projectId || value.revision !== 0 || value.snapshot_id !== '' || value.source !== '' || value.observed_at !== '' || value.recorded_at !== '' || value.items.length !== 0 || value.has_more) throw new ApiError('invalid_response', 'The service returned an invalid review queue.');
    if (!value.items.every(validReviewPull) || new Set(value.items.map(item => item.number)).size !== value.items.length) throw new ApiError('invalid_response', 'The service returned an invalid review queue.');
    return value as unknown as ReviewQueue;
  }
  async reviewPull(number: number, snapshot?: string, signal?: AbortSignal): Promise<ReviewPull> {
    if (!pullNumber(number)) throw new ApiError('invalid_request', 'The pull request number is invalid.');
    const query = reviewQuery(snapshot);
    const value = await this.finiteGet(withQuery(this.projectRoute(`reviews/pulls/${number}`), query), signal);
    if (!record(value) || !keys(value, [...reviewMetadata, 'pull']) || !validReviewSnapshot(value, this.projectId, snapshot) || !validReviewPull(value.pull) || value.pull.number !== number) throw new ApiError('invalid_response', 'The service returned an invalid review pull request.');
    return value as unknown as ReviewPull;
  }
  async reviewRuns(number: number, options: ReviewRunOptions = {}, signal?: AbortSignal): Promise<ReviewRuns> {
    if (!pullNumber(number)) throw new ApiError('invalid_request', 'The pull request number is invalid.');
    const query = pageOptions(options);
    for (const [name, value] of reviewQuery(options.snapshot)) query.set(name, value);
    const value = await this.finiteGet(withQuery(this.projectRoute(`reviews/pulls/${number}/runs`), query), signal);
    if (!record(value) || !keys(value, [...reviewMetadata, 'items', 'has_more'], ['cursor']) || !validReviewSnapshot(value, this.projectId, options.snapshot) || !Array.isArray(value.items) || !validReviewPage({ items: value.items, has_more: value.has_more, ...(value.cursor === undefined ? {} : { cursor: value.cursor }) }, options) || !value.items.every(item => validReviewRun(item, number)) || new Set(value.items.map(item => item.id)).size !== value.items.length) throw new ApiError('invalid_response', 'The service returned an invalid review run page.');
    return value as unknown as ReviewRuns;
  }
  async reviewAttempts(number: number, options: ReviewAttemptOptions = {}, signal?: AbortSignal): Promise<ReviewAttempts> {
    if (!pullNumber(number)) throw new ApiError('invalid_request', 'The pull request number is invalid.');
    const query = pageOptions(options);
    for (const [name, value] of reviewQuery(options.snapshot)) query.set(name, value);
    const value = await this.finiteGet(withQuery(this.projectRoute(`reviews/pulls/${number}/attempts`), query), signal);
    if (!record(value) || !keys(value, [...reviewMetadata, 'items', 'has_more'], ['cursor']) || !validReviewSnapshot(value, this.projectId, options.snapshot) || !Array.isArray(value.items) || !validReviewPage({ items: value.items, has_more: value.has_more, ...(value.cursor === undefined ? {} : { cursor: value.cursor }) }, options) || !value.items.every(validReviewAttemptItem) || new Set(value.items.map(item => JSON.stringify([item.source, item.source_attempt_id]))).size !== value.items.length) throw new ApiError('invalid_response', 'The service returned an invalid review attempt page.');
    return value as unknown as ReviewAttempts;
  }
  async reviewRun(runId: string, snapshot?: string, signal?: AbortSignal): Promise<ReviewRunDetail> {
    if (!reviewRunID(runId)) throw new ApiError('invalid_request', 'The review run identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`reviews/runs/${encodeURIComponent(runId)}`), reviewQuery(snapshot)), signal, undefined, 4 << 20);
    if (!record(value) || !keys(value, [...reviewMetadata, 'pull_number', 'run', 'findings']) || !validReviewSnapshot(value, this.projectId, snapshot) || !pullNumber(value.pull_number) || !validReviewRun(value.run, value.pull_number) || value.run.id !== runId || !Array.isArray(value.findings)) throw new ApiError('invalid_response', 'The service returned an invalid review run detail.');
    const run = value.run;
    if (value.findings.length > 100 || value.findings.length > run.found || run.complete && value.findings.length !== run.found || !value.findings.every(item => validReviewFinding(item) && (item.route === undefined || item.route.target_pull_number !== value.pull_number) && (run.source !== 'native-executor' || item.disposition !== 'routed' || item.route?.state === 'active')) || new Set(value.findings.map(item => item.source_finding_id)).size !== value.findings.length || value.findings.filter(item => item.fix_sha !== '').length !== value.run.fixed) throw new ApiError('invalid_response', 'The service returned an invalid review run detail.');
    return value as unknown as ReviewRunDetail;
  }
  async reviewPolicyAssessment(evidenceId: string, signal?: AbortSignal): Promise<ReviewPolicyAssessment> {
    if (!reviewEvidenceID(evidenceId)) throw new ApiError('invalid_request', 'The native evidence identifier is invalid.');
    const value = await this.finiteGet(this.projectRoute(`reviews/evidence/${evidenceId}/policy`), signal, undefined, 16 << 10, assessmentJSON);
    if (!validPolicyAssessment(value, this.projectId, evidenceId)) throw new ApiError('invalid_response', 'The service returned an invalid captured comparison assessment.');
    return value;
  }
  async inbox(worker: string, options: InboxOptions = {}, signal?: AbortSignal): Promise<Page<InboxItem>> {
    if (!id(worker)) throw new ApiError('invalid_request', 'The inbox worker is invalid.');
    const query = pageOptions(options);
    if (options.unread !== undefined) { if (typeof options.unread !== 'boolean') throw new ApiError('invalid_request', 'The inbox filter is invalid.'); if (options.unread) query.set('unread', 'true'); }
    if (options.unacknowledged !== undefined) { if (typeof options.unacknowledged !== 'boolean') throw new ApiError('invalid_request', 'The inbox filter is invalid.'); if (options.unacknowledged) query.set('unacknowledged', 'true'); }
    const value = await this.finiteGet(withQuery(this.projectRoute(`inbox/${encodeURIComponent(worker)}`), query), signal, worker);
    if (!validPage(value, defaultLimit(options)) || !value.items.every((item) => validInboxItem(item, worker))) throw new ApiError('invalid_response', 'The service returned an invalid inbox page.');
    return value as Page<InboxItem>;
  }
  async thread(conversationId: string, options: PageOptions = {}, signal?: AbortSignal): Promise<ConversationPage> {
    if (!id(conversationId)) throw new ApiError('invalid_request', 'The conversation identifier is invalid.');
    const value = await this.finiteGet(withQuery(this.projectRoute(`conversations/${encodeURIComponent(conversationId)}`), pageOptions(options)), signal);
    if (!record(value) || !keys(value, ['conversation_id', 'root_message_id', 'items', 'has_more'], ['cursor']) || value.conversation_id !== conversationId || !id(value.root_message_id) || !validPage({ items: value.items, has_more: value.has_more, ...(value.cursor === undefined ? {} : { cursor: value.cursor }) }, defaultLimit(options)) || !Array.isArray(value.items) || !value.items.every((item) => validMessage(item, conversationId))) throw new ApiError('invalid_response', 'The service returned an invalid conversation page.');
    return value as unknown as ConversationPage;
  }
  async snapshot(worker: string, options: InboxOptions = {}, signal?: AbortSignal): Promise<InboxSnapshot> {
    if (!id(worker)) throw new ApiError('invalid_request', 'The inbox worker is invalid.');
    const query = pageOptions(options);
    if (options.unread !== undefined) { if (typeof options.unread !== 'boolean') throw new ApiError('invalid_request', 'The inbox filter is invalid.'); if (options.unread) query.set('unread', 'true'); }
    if (options.unacknowledged !== undefined) { if (typeof options.unacknowledged !== 'boolean') throw new ApiError('invalid_request', 'The inbox filter is invalid.'); if (options.unacknowledged) query.set('unacknowledged', 'true'); }
    const value = await this.finiteGet(withQuery(this.projectRoute(`inbox/${encodeURIComponent(worker)}/snapshot`), query), signal, worker);
    if (!record(value) || !keys(value, ['inbox', 'event_cursor']) || !cursor(value.event_cursor) || !validPage(value.inbox, defaultLimit(options)) || !value.inbox.items.every((item) => validInboxItem(item, worker))) throw new ApiError('invalid_response', 'The service returned an invalid inbox snapshot.');
    return value as unknown as InboxSnapshot;
  }

  async presentation(signal?: AbortSignal): Promise<PresentationSnapshot> {
    const value = await this.finiteGet(this.projectRoute('presentation'), signal, undefined, 32 << 10, assessmentJSON);
    if (!validPresentationSnapshot(value, this.projectId)) throw new ApiError('invalid_response', 'The service returned an invalid presentation snapshot.');
    return value;
  }
  async presentationRevision(revision: number, signal?: AbortSignal): Promise<PresentationRevision> {
    if (!positive(revision)) throw new ApiError('invalid_request', 'Select a positive presentation revision.');
    const value = await this.finiteGet(this.projectRoute(`presentation/history/${revision}`), signal, undefined, 32 << 10, assessmentJSON);
    if (!record(value) || !keys(value, ['revision', 'actor_id', 'created_at', 'state']) || value.revision !== revision || !id(value.actor_id) || !assessmentTime(value.created_at) || !validPresentation(value.state, this.projectId) || value.state.revision !== revision || value.state.updated_at !== value.created_at) throw new ApiError('invalid_response', 'The service returned an invalid presentation revision.');
    return value as unknown as PresentationRevision;
  }
  async watchProjectMetadata(initial: PresentationSnapshot, callbacks: EventWatchCallbacks<PresentationSnapshot, ProjectMetadataPage>, signal: AbortSignal): Promise<void> {
    this.ensureOpen();
    if (!validPresentationSnapshot(initial, this.projectId)) throw new ApiError('invalid_request', 'Use a verified presentation snapshot.');
    this.activeMetadataWatch?.abort();
    const controller = new AbortController();
    this.activeMetadataWatch = controller;
    const abort = () => controller.abort();
    if (signal.aborted || this.session.signal.aborted) controller.abort();
    else { signal.addEventListener('abort', abort, { once: true }); this.session.signal.addEventListener('abort', abort, { once: true }); }
    let first: PresentationSnapshot | undefined = initial;
    try {
      await runEventWatch<PresentationSnapshot, ProjectMetadataPage>({
        event: 'project-events', snapshot: selected => { if (first) { const value = first; first = undefined; return Promise.resolve(value); } return this.presentation(selected); },
        snapshotCursor: value => value.event_cursor, openStream: (after, selected) => this.openEventStream('project-events', after, 25, selected),
        readFailure: async (response, selected) => { const request = requestBudget([selected], 5000); try { return await readFailure(response, request.signal); } finally { request.close(); } },
        decodePage: data => { const value = assessmentJSON(data); if (!validMetadataPage(value, 25)) throw new ApiError('invalid_response', 'The project event page is invalid.'); return value; },
        pageCursor: page => page.event_cursor, substantive: page => page.items.length > 0,
        parseStreamError: data => { try { return serverError(assessmentJSON(data), 0); } catch { return new ApiError('invalid_response', 'The project stream error is invalid.'); } },
      }, callbacks, controller.signal);
    } finally { signal.removeEventListener('abort', abort); this.session.signal.removeEventListener('abort', abort); if (this.activeMetadataWatch === controller) this.activeMetadataWatch = undefined; }
  }
  async watchMessages(worker: string, options: InboxOptions, callbacks: WatchCallbacks, signal: AbortSignal): Promise<void> {
    this.ensureOpen();
    if (!id(worker)) throw new ApiError('invalid_request', 'The inbox worker is invalid.');
    pageOptions(options);
    if (options.unread !== undefined && typeof options.unread !== 'boolean' || options.unacknowledged !== undefined && typeof options.unacknowledged !== 'boolean') throw new ApiError('invalid_request', 'The inbox filter is invalid.');
    if (signal.aborted || this.session.signal.aborted) return;
    this.activeWatch?.abort();
    const controller = new AbortController();
    this.activeWatch = controller;
    const abort = () => controller.abort();
    if (signal.aborted || this.session.signal.aborted) controller.abort();
    else {
      signal.addEventListener('abort', abort, { once: true });
      this.session.signal.addEventListener('abort', abort, { once: true });
    }
    const source: MessageWatchSource = {
      projectId: this.projectId,
      snapshot: (target, page, selectedSignal) => this.snapshot(target, page, selectedSignal),
      openStream: (target, after, limit, selectedSignal) => this.openEventStream('message-events', after, limit, selectedSignal, target),
      readFailure: async (response, selectedSignal) => {
        const request = requestBudget([selectedSignal], 5000);
        try { return await readFailure(response, request.signal); }
        catch (error) { throw this.transportError(error, request.expired(), request.signal); }
        finally { request.close(); }
      },
      validatePage: (value, target, limit) => validateMessageEventPage(value, this.projectId, target, limit),
      parseStreamError: (data) => { try { return serverError(JSON.parse(data) as unknown, 0); } catch { return new ApiError('invalid_response', 'The event stream returned an invalid error.'); } },
    };
    try {
      if (!this.verifiedInstanceId) await this.connect(controller.signal);
      await runMessageWatch(source, worker, options, callbacks, controller.signal);
    } finally {
      signal.removeEventListener('abort', abort);
      this.session.signal.removeEventListener('abort', abort);
      if (this.activeWatch === controller) this.activeWatch = undefined;
    }
  }

  private projectRoute(suffix: string): string { return `/api/v1/projects/${encodeURIComponent(this.projectId)}${suffix ? `/${suffix}` : ''}`; }
  private async finiteGet(path: string, signal?: AbortSignal, worker?: string, responseMaxBytes = maxBytes, decode: (text: string) => unknown = JSON.parse): Promise<unknown> {
    this.ensureOpen();
    const request = requestBudget([signal, this.session.signal], 5000);
    try {
      if (!this.verifiedInstanceId) throw new ApiError('not_connected', 'Connect to the selected project before reading data.');
      return await this.getJson(path, this.verifiedInstanceId, request.signal, worker, responseMaxBytes, decode);
    } catch (error) { throw this.transportError(error, request.expired(), request.signal); }
    finally { request.close(); }
  }
  private async getJson(path: string, instanceId: string | undefined, signal: AbortSignal, worker?: string, responseMaxBytes = maxBytes, decode: (text: string) => unknown = JSON.parse): Promise<unknown> {
    const response = await fetch(urlFor(path), { method: 'GET', headers: this.headers(instanceId, worker), redirect: 'error', credentials: this.browserSessionID ? 'same-origin' : 'omit', cache: 'no-store', signal });
    if (!response.ok) throw await readFailure(response, signal);
    return readJson(response, signal, responseMaxBytes, decode);
  }
  private headers(instanceId?: string, worker?: string): Headers {
    const value = new Headers({ Accept: 'application/json' });
    if (this.browserSessionID) value.set('X-FC-Browser-Session', this.browserSessionID);
    else value.set('Authorization', `Bearer ${this.token}`);
    if (instanceId) value.set('X-FC-Instance', instanceId);
    if (worker) value.set('X-FC-Worker', worker);
    return value;
  }
  private async openEventStream(feed: string, after: string, limit: number, signal: AbortSignal, worker?: string): Promise<{ response: Response; close: () => void; signal: AbortSignal }> {
    if (worker !== undefined && !id(worker) || !cursor(after) || !Number.isInteger(limit) || limit < 1 || limit > maxItems) throw new ApiError('invalid_request', 'The event stream request is invalid.');
    if (!this.verifiedInstanceId) throw new ApiError('not_connected', 'Connect to the selected project before watching messages.');
    const controller = new AbortController();
    this.streamConnections.add(controller);
    const abort = () => controller.abort();
    if (signal.aborted) controller.abort(); else signal.addEventListener('abort', abort, { once: true });
    let expired = false;
    const timer = setTimeout(() => { expired = true; controller.abort(); }, 5000);
    try {
      const query = new URLSearchParams({ cursor: after, limit: String(limit) });
      const response = await fetch(urlFor(`${this.projectRoute(`${feed}/stream`)}?${query}`), { method: 'GET', headers: this.headers(this.verifiedInstanceId, worker), redirect: 'error', credentials: this.browserSessionID ? 'same-origin' : 'omit', cache: 'no-store', signal: controller.signal });
      clearTimeout(timer);
      return { response, signal: controller.signal, close: () => { signal.removeEventListener('abort', abort); this.streamConnections.delete(controller); controller.abort(); } };
    } catch (error) {
      clearTimeout(timer);
      signal.removeEventListener('abort', abort);
      this.streamConnections.delete(controller);
      throw this.transportError(error, expired, controller.signal);
    }
  }
  private transportError(error: unknown, timedOut: boolean, signal: AbortSignal): ApiError {
    if (error instanceof ApiError) return error;
    if (timedOut) return new ApiError('deadline_exceeded', 'The service request exceeded its five-second deadline.');
    if (signal.aborted) return new ApiError('cancelled', 'The service request was cancelled.');
    return new ApiError('connection_failed', 'The service could not be reached.');
  }
  private ensureOpen(): void {
    if (this.closed) throw new ApiError('cancelled', 'This service session is closed.');
  }
}
