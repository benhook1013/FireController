import type {
  Checkpoint, ConversationPage, InboxItem, InboxOptions, InboxSnapshot, Job, JobOptions, LatestCheckpoint,
  JobSummary, Message, MessageEvent, MessageEventPage, Page, PageOptions,
  Project, WatchCallbacks, Worker,
  ReviewAttemptSummary, ReviewFinding, ReviewPull, ReviewPullSummary, ReviewQueue, ReviewRunDetail,
  ReviewRunOptions, ReviewRuns, ReviewRunSummary, ReviewPolicyAssessment,
} from './types';
import { runMessageWatch, type MessageWatchSource } from './events';
import { newSessionID, sessionExpiry, sessionIdentity, sessionSelector } from './browser-session';

const maxBytes = 1 << 20;
const maxItems = 100;
const idPattern = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
const jobStatuses = new Set(['active', 'parked', 'blocked', 'completed']);
const serverErrorCodes = new Set([
  'bootstrap_failed', 'configuration_invalid', 'credential_already_configured', 'empty_edit', 'forbidden',
  'idempotency_conflict', 'idempotency_key_required', 'instance_mismatch', 'internal_error', 'invalid_brief',
  'invalid_cursor', 'invalid_filter', 'invalid_query', 'invalid_revision', 'invalid_status', 'invalid_title',
  'method_not_allowed', 'not_found', 'overloaded', 'provider_unavailable', 'resync_required', 'schema_invalid',
  'schema_unsupported', 'storage_unavailable', 'stream_failed', 'stream_frame_too_large', 'stream_unavailable',
  'unauthorized', 'validation_failed', 'worker_already_configured',
]);

export class ApiError extends Error {
  readonly code: string;
  readonly status: number;

  constructor(code: string, message: string, status = 0) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.status = status;
  }
}

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
function validReviewPull(value: unknown): value is ReviewPullSummary {
  if (!record(value) || !keys(value, ['number', 'title', 'state', 'head_sha', 'base_ref', 'base_sha', 'ci_state', 'mergeability', 'complete', 'run_count', 'attempt_count', 'recent_runs', 'latest_attempts']) || !pullNumber(value.number) || !checkpointText(value.title, 500, true) || (typeof value.state !== 'string' || !['open', 'merged', 'closed', 'unknown'].includes(value.state)) || !sha(value.head_sha) || !checkpointText(value.base_ref, 256, false) || !sha(value.base_sha) || (typeof value.ci_state !== 'string' || !['unknown', 'pending', 'success', 'failure'].includes(value.ci_state)) || (typeof value.mergeability !== 'string' || !['unknown', 'mergeable', 'conflicting'].includes(value.mergeability)) || typeof value.complete !== 'boolean' || !nonnegative(value.run_count) || value.run_count > 100 || !nonnegative(value.attempt_count) || value.attempt_count > 100 || !Array.isArray(value.recent_runs) || value.recent_runs.length > Math.min(3, value.run_count) || !value.recent_runs.every(item => validReviewRun(item, Number(value.number))) || value.run_count > 0 && value.recent_runs.length === 0 || !Array.isArray(value.latest_attempts) || value.latest_attempts.length > Math.min(4, value.attempt_count) || !value.latest_attempts.every(validReviewAttempt) || value.attempt_count > 0 && value.latest_attempts.length === 0) return false;
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
  private browserSessionID?: string;
  private pendingBrowserSessionID?: string;
  private expectedInstanceID?: string;

  constructor(token: string, projectId: string) {
    if (!text(token, 4096) || !id(projectId)) throw new ApiError('invalid_binding', 'Select a valid project and credential.');
    this.token = token;
    this.projectId = projectId;
  }
  get instanceId(): string | undefined { return this.verifiedInstanceId; }

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
    this.token = '';
    this.session.abort();
    this.activeWatch?.abort();
  }

  async connect(signal?: AbortSignal): Promise<Project> {
    this.ensureOpen();
    const budget = requestBudget([signal, this.session.signal], 5000);
    this.verifiedInstanceId = undefined;
    try {
      const value = await this.getJson('/api/v1/instance', this.expectedInstanceID, budget.signal);
      if (!record(value) || !keys(value, ['instance_id']) || !id(value.instance_id)) throw new ApiError('invalid_response', 'The service returned an invalid instance identity.');
      if (this.expectedInstanceID && value.instance_id !== this.expectedInstanceID) throw new ApiError('instance_mismatch', 'The service identity differs from this saved project session.', 409);
      const project = await this.getJson(this.projectRoute(''), value.instance_id, budget.signal);
      if (!validProject(project, this.projectId)) throw new ApiError('invalid_response', 'The service returned a project outside this binding.');
      if (budget.signal.aborted) throw new ApiError('cancelled', 'The browser connection was cancelled.');
      this.verifiedInstanceId = value.instance_id;
      return project;
    } catch (error) { throw this.transportError(error, budget.expired(), budget.signal); }
    finally { budget.close(); }
  }

  async startBrowserSession(signal?: AbortSignal): Promise<{ session_id: string; instance_id: string; project_id: string; expires_at: string }> {
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
      const value = await readJson(response, budget.signal, 4096);
      if (!record(value) || !keys(value, ['session_id', 'instance_id', 'project_id', 'expires_at']) || value.session_id !== selector || value.instance_id !== instance || value.project_id !== this.projectId || !sessionExpiry(value.expires_at) || Date.parse(value.expires_at) <= Date.now()) throw new ApiError('invalid_response', 'The service returned an invalid browser session.');
      if (budget.signal.aborted) throw new ApiError('cancelled', 'The browser connection was cancelled.');
      this.browserSessionID = selector;
      this.pendingBrowserSessionID = undefined;
      this.token = '';
      return value as { session_id: string; instance_id: string; project_id: string; expires_at: string };
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
    } catch (error) { throw this.transportError(error, budget.expired(), budget.signal); }
    finally { budget.close(); }
  }

  async workers(options: PageOptions = {}, signal?: AbortSignal): Promise<Page<Worker>> {
    const value = await this.finiteGet(withQuery(this.projectRoute('workers'), pageOptions(options)), signal);
    if (!validPage(value, defaultLimit(options)) || !value.items.every((item) => validWorker(item, this.projectId))) throw new ApiError('invalid_response', 'The service returned an invalid worker page.');
    return value as Page<Worker>;
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
    if (!record(value) || !keys(value, [...reviewMetadata, 'pull', 'attempts']) || !validReviewSnapshot(value, this.projectId, snapshot) || !validReviewPull(value.pull) || value.pull.number !== number || !Array.isArray(value.attempts) || value.attempts.length !== value.pull.attempt_count || !value.attempts.every(validReviewAttempt) || new Set(value.attempts.map(item => item.source_attempt_id)).size !== value.attempts.length) throw new ApiError('invalid_response', 'The service returned an invalid review pull request.');
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
      openStream: (target, after, limit, selectedSignal) => this.openStream(target, after, limit, selectedSignal),
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
  private async openStream(worker: string, after: string, limit: number, signal: AbortSignal): Promise<{ response: Response; close: () => void }> {
    if (!id(worker) || !cursor(after) || !Number.isInteger(limit) || limit < 1 || limit > maxItems) throw new ApiError('invalid_request', 'The event stream request is invalid.');
    if (!this.verifiedInstanceId) throw new ApiError('not_connected', 'Connect to the selected project before watching messages.');
    const controller = new AbortController();
    const abort = () => controller.abort();
    if (signal.aborted) controller.abort(); else signal.addEventListener('abort', abort, { once: true });
    let expired = false;
    const timer = setTimeout(() => { expired = true; controller.abort(); }, 5000);
    try {
      const query = new URLSearchParams({ cursor: after, limit: String(limit) });
      const response = await fetch(urlFor(`${this.projectRoute('message-events/stream')}?${query}`), { method: 'GET', headers: this.headers(this.verifiedInstanceId, worker), redirect: 'error', credentials: this.browserSessionID ? 'same-origin' : 'omit', cache: 'no-store', signal: controller.signal });
      clearTimeout(timer);
      return { response, close: () => { signal.removeEventListener('abort', abort); controller.abort(); } };
    } catch (error) {
      clearTimeout(timer);
      signal.removeEventListener('abort', abort);
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
