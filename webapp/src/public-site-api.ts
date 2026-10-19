import { ApiError } from './api';
import { runEventWatch, type EventWatchCallbacks } from './events';
import { presentationDefaults, type HeadingKey, type ThemeToken } from './presentation';

export interface PublicPresentation { name: string; brand_name: string; tagline: string; headings: Partial<Record<HeadingKey, string>>; theme: Partial<Record<ThemeToken, string>> }
export interface PublicLink { key: string; label: string; url: string }
export interface PublicCard { key: string; title: string; description: string; phase: string; status: string; links: PublicLink[] }
export interface PublicLane { key: string; label: string; headline: string; status: string; milestones: string[] }
export interface PublicRound { key: string; found: number; accepted: number; routed: number; complete: boolean; observed_at: string; duration_milliseconds: number | null }
export interface PublicReview { number: number; title: string; url: string; lifecycle: 'unknown' | 'open' | 'closed' | 'merged'; cli: PublicRound[]; hosted: PublicRound[]; cli_has_more?: boolean; hosted_has_more?: boolean }
export interface PublicContent { slug: string; presentation: PublicPresentation; description: string; links: PublicLink[]; project_map: PublicCard[]; progress: PublicCard[]; worker_lanes: PublicLane[]; reviews: PublicReview[] }
export interface PublicSnapshot { slug: string; generation: string; revision: number; published_at: string; cursor: string; content: PublicContent }
export interface PublicChange { revision: number; published_at: string; cursor: string }
export interface PublicHistory { slug: string; generation: string; items: { revision: number; published_at: string }[]; cursor?: string; has_more: boolean }
const encoder = new TextEncoder();
const maximumBytes = 512 << 10;
const object = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v);
const exact = (v: Record<string, unknown>, keys: string[], optional: string[] = []) => keys.every(k => Object.hasOwn(v, k)) && Object.keys(v).every(k => keys.includes(k) || optional.includes(k));
const positive = (v: unknown): v is number => Number.isSafeInteger(v) && Number(v) > 0;
const nonnegative = (v: unknown): v is number => Number.isSafeInteger(v) && Number(v) >= 0;
const text = (v: unknown, max: number, required = false, multiline = false): v is string => typeof v === 'string' && encoder.encode(v).length <= max && (!required || v.trim().length > 0) && !(multiline ? /[^\P{Cc}\n\t]|[\uD800-\uDFFF]/u : /[\p{Cc}\uD800-\uDFFF]/u).test(v);
export const validPublicSlug = (v: unknown): v is string => typeof v === 'string' && /^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$/u.test(v);
export const validPublicGeneration = (v: unknown): v is string => typeof v === 'string' && /^public_generation_[a-f0-9]{32}$/u.test(v);
const key = (v: unknown): v is string => typeof v === 'string' && /^[a-z][a-z0-9_-]{0,63}$/u.test(v);
const cursorFor = (generation: string, revision: number) => `${generation}:${revision}`;
function stamp(v: unknown): v is string {
  if (typeof v !== 'string' || !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/u.test(v) || !Number.isFinite(Date.parse(v))) return false;
  const [year, month, day] = v.slice(0, 10).split('-').map(Number), date = new Date(0);
  date.setUTCFullYear(year, month - 1, day);
  return year > 0 && date.getUTCFullYear() === year && date.getUTCMonth() === month - 1 && date.getUTCDate() === day && Number(v.slice(11, 13)) < 24 && Number(v.slice(14, 16)) < 60 && Number(v.slice(17, 19)) < 60 && (Date.parse(v) !== -62135596800000 || /\.\d*[1-9]/u.test(v));
}
export function publicURL(v: unknown): v is string {
  if (typeof v !== 'string' || encoder.encode(v).length > 2048 || /[\s\p{Cc}\\\uD800-\uDFFF]/u.test(v)) return false;
  try { const url = new URL(v); return ['http:', 'https:'].includes(url.protocol) && !!url.hostname && !url.username && !url.password; } catch { return false; }
}
function links(v: unknown): v is PublicLink[] {
  if (!Array.isArray(v) || v.length > 12) return false;
  const seen = new Set<string>();
  return v.every(item => object(item) && exact(item, ['key', 'label', 'url']) && key(item.key) && !seen.has(item.key) && !!seen.add(item.key) && text(item.label, 120, true) && publicURL(item.url));
}
function presentation(v: unknown): v is PublicPresentation {
  return object(v) && exact(v, ['name', 'brand_name', 'tagline', 'headings', 'theme']) && text(v.name, 200, true) && text(v.brand_name, 120, true) && text(v.tagline, 120, true) && object(v.headings) && object(v.theme)
    && Object.entries(v.headings).every(([k, value]) => Object.hasOwn(presentationDefaults.headings, k) && text(value, 240, true))
    && Object.entries(v.theme).every(([k, value]) => Object.hasOwn(presentationDefaults.theme, k) && typeof value === 'string' && /^#[0-9a-f]{6}$/u.test(value));
}
function content(v: unknown, slug: string): v is PublicContent {
  if (!object(v) || !exact(v, ['slug', 'presentation', 'description', 'links', 'project_map', 'progress', 'worker_lanes', 'reviews']) || v.slug !== slug || !presentation(v.presentation) || !text(v.description, 8192, false, true) || !links(v.links)) return false;
  const seen = new Set<string>();
  for (const cards of [v.project_map, v.progress]) {
    if (!Array.isArray(cards) || cards.length > 100 || !cards.every(item => object(item) && exact(item, ['key', 'title', 'description', 'phase', 'status', 'links']) && key(item.key) && !seen.has(item.key) && !!seen.add(item.key) && text(item.title, 240, true) && text(item.description, 8192, false, true) && text(item.phase, 160) && text(item.status, 240) && links(item.links))) return false;
  }
  if (!Array.isArray(v.worker_lanes) || v.worker_lanes.length > 50 || !v.worker_lanes.every(item => object(item) && exact(item, ['key', 'label', 'headline', 'status', 'milestones']) && key(item.key) && !seen.has(item.key) && !!seen.add(item.key) && text(item.label, 120, true) && text(item.headline, 240) && text(item.status, 240) && Array.isArray(item.milestones) && item.milestones.length <= 20 && item.milestones.every(item => text(item, 240, true)))) return false;
  const numbers = new Set<number>(), rounds = new Set<string>();
  const validRounds = (items: unknown): items is PublicRound[] => Array.isArray(items) && items.length <= 100 && items.every(item => object(item) && exact(item, ['key', 'found', 'accepted', 'routed', 'complete', 'observed_at', 'duration_milliseconds']) && typeof item.key === 'string' && /^public_round_[a-f0-9]{32}$/u.test(item.key) && !rounds.has(item.key) && !!rounds.add(item.key) && nonnegative(item.found) && nonnegative(item.accepted) && nonnegative(item.routed) && item.accepted + item.routed <= item.found && typeof item.complete === 'boolean' && (item.observed_at === '' || stamp(item.observed_at)) && (item.duration_milliseconds === null || nonnegative(item.duration_milliseconds)));
  return Array.isArray(v.reviews) && v.reviews.length <= 100 && v.reviews.every(item => {
    if (!object(item) || !exact(item, ['number', 'title', 'url', 'lifecycle', 'cli', 'hosted'], ['cli_has_more', 'hosted_has_more']) || ['cli_has_more', 'hosted_has_more'].some(field => Object.hasOwn(item, field) && typeof item[field] !== 'boolean') || !positive(item.number) || item.number > 2147483647 || numbers.has(item.number) || !text(item.title, 240, true) || !publicURL(item.url) || !['unknown', 'open', 'closed', 'merged'].includes(String(item.lifecycle)) || !validRounds(item.cli) || !validRounds(item.hosted)) return false;
    const url = new URL(item.url), parts = url.pathname.split('/');
    if (url.protocol !== 'https:' || url.host !== 'github.com' || url.search || url.hash || parts.length !== 5 || parts[3] !== 'pull' || parts[4] !== String(item.number) || !parts.slice(1, 3).every(p => /^[A-Za-z0-9_.-]{1,100}$/u.test(p) && p !== '.' && p !== '..')) return false;
    numbers.add(item.number); return true;
  });
}
export function validatePublicSnapshot(v: unknown, slug: string, generation?: string, revision?: number): v is PublicSnapshot {
  return object(v) && exact(v, ['slug', 'generation', 'revision', 'published_at', 'cursor', 'content']) && v.slug === slug && validPublicSlug(v.slug) && validPublicGeneration(v.generation) && (generation === undefined || generation === v.generation) && positive(v.revision) && (revision === undefined || revision === v.revision) && stamp(v.published_at) && v.cursor === cursorFor(v.generation, v.revision) && content(v.content, slug);
}
export function validatePublicChange(v: unknown, generation: string): v is PublicChange {
  return validPublicGeneration(generation) && object(v) && exact(v, ['revision', 'published_at', 'cursor']) && positive(v.revision) && stamp(v.published_at) && v.cursor === cursorFor(generation, v.revision);
}
function invalid(): ApiError { return new ApiError('invalid_response', 'The public project response is invalid.'); }
function parse(raw: string): unknown {
	if (encoder.encode(raw).length > maximumBytes) throw invalid();
  const result: unknown = JSON.parse(raw), tokens = raw.match(/"(?:\\.|[^"\\])*"|[{}\[\]:,]|true|false|null|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/gu) ?? [];
  let at = 0;
  function visit(depth: number): void {
    if (depth > 32) throw invalid();
    const token = tokens[at++];
    if (token === '{') {
      const seen = new Set<string>();
      if (tokens[at] === '}') { at++; return; }
      while (true) { const name = JSON.parse(tokens[at++]) as string; if (seen.has(name) || tokens[at++] !== ':') throw invalid(); seen.add(name); visit(depth + 1); const next = tokens[at++]; if (next === '}') return; if (next !== ',') throw invalid(); }
    } else if (token === '[') { if (tokens[at] === ']') { at++; return; } while (true) { visit(depth + 1); const next = tokens[at++]; if (next === ']') return; if (next !== ',') throw invalid(); } }
    else if (token === undefined) throw invalid();
  }
  visit(0); if (at !== tokens.length) throw invalid(); return result;
}
async function boundedJSON(response: Response, signal: AbortSignal): Promise<unknown> {
  // Fetch exposes decoded bytes while retaining compression metadata. Bound
  // those actual decoded bytes rather than refusing normal proxy compression.
  if (response.headers.get('content-type')?.split(';')[0].trim().toLowerCase() !== 'application/json' || !response.body) throw invalid();
  const reader = response.body.getReader(); let total = 0; const chunks: Uint8Array[] = [];
  try { while (true) { if (signal.aborted) throw new ApiError('cancelled', 'The public read ended.'); const value = await reader.read(); if (value.done) break; total += value.value.byteLength; if (total > maximumBytes) throw invalid(); chunks.push(value.value); } }
  finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
  const bytes = new Uint8Array(total); let offset = 0; for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
  try { return parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes)); } catch { throw invalid(); }
}
function failure(status: number): ApiError { return new ApiError(status === 409 ? 'resync_required' : status === 404 ? 'not_found' : 'public_unavailable', 'This published project is unavailable.', status); }
export class PublicSiteApi {
  readonly path: string;
  constructor(readonly slug: string) { if (!validPublicSlug(slug)) throw invalid(); this.path = `/api/v1/public/projects/${slug}`; }
  private async read(path: string, signal: AbortSignal): Promise<unknown> {
    const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 5000), combined = AbortSignal.any([signal, controller.signal]);
    try { const response = await fetch(path, { method: 'GET', credentials: 'omit', redirect: 'error', cache: 'no-store', headers: { Accept: 'application/json' }, signal: combined }); if (!response.ok) { await response.body?.cancel(); throw failure(response.status); } return await boundedJSON(response, combined); }
    finally { clearTimeout(timer); controller.abort(); }
  }
  async snapshot(signal: AbortSignal, revision?: number, generation?: string): Promise<PublicSnapshot> {
    if (revision !== undefined && (!positive(revision) || !validPublicGeneration(generation))) throw invalid();
    const value = await this.read(revision === undefined ? this.path : `${this.path}/history/${revision}?generation=${generation}`, signal);
    if (!validatePublicSnapshot(value, this.slug, generation, revision)) throw invalid(); return value;
  }
  async history(signal: AbortSignal, generation: string, cursor?: string): Promise<PublicHistory> {
    if (!validPublicGeneration(generation) || cursor !== undefined && (typeof cursor !== 'string' || !positive(Number(cursor.split(':')[1])) || cursor !== cursorFor(generation, Number(cursor.split(':')[1])))) throw invalid();
    const value = await this.read(`${this.path}/history?limit=20${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, signal);
    if (!object(value) || !exact(value, ['slug', 'generation', 'items', 'has_more'], ['cursor']) || value.slug !== this.slug || value.generation !== generation || typeof value.has_more !== 'boolean' || !Array.isArray(value.items) || value.items.length > 20) throw invalid();
    let previous = cursor ? Number(cursor.split(':')[1]) : Infinity;
    for (const item of value.items) { if (!object(item) || !exact(item, ['revision', 'published_at']) || !positive(item.revision) || item.revision >= previous || !stamp(item.published_at)) throw invalid(); previous = item.revision; }
    if (value.has_more ? !value.items.length || value.cursor !== cursorFor(generation, previous) : Object.hasOwn(value, 'cursor')) throw invalid();
    return value as unknown as PublicHistory;
  }
  async watch(callbacks: Pick<EventWatchCallbacks<PublicSnapshot, PublicChange>, 'snapshot' | 'status'>, signal: AbortSignal): Promise<void> {
    let current: PublicSnapshot;
    return runEventWatch<PublicSnapshot, PublicChange>({
      snapshot: async selected => current = await this.snapshot(selected), snapshotCursor: value => value.cursor,
      openStream: async (cursor, selected) => {
        const controller = new AbortController(), combined = AbortSignal.any([selected, controller.signal]), timer = setTimeout(() => controller.abort(), 5000);
        try { const response = await fetch(`${this.path}/events?cursor=${encodeURIComponent(cursor)}`, { method: 'GET', credentials: 'omit', redirect: 'error', cache: 'no-store', headers: { Accept: 'text/event-stream' }, signal: combined }); return { response, close: () => controller.abort() }; }
        catch (error) { controller.abort(); throw error; } finally { clearTimeout(timer); }
      },
      readFailure: async response => { await response.body?.cancel(); return failure(response.status); },
      decodePage: raw => { const value = parse(raw); if (!validatePublicChange(value, current.generation)) throw invalid(); return value; },
      pageCursor: value => value.cursor, substantive: () => true,
      parseStreamError: raw => { const value = parse(raw); if (!object(value) || !exact(value, ['error']) || !object(value.error) || !exact(value.error, ['code']) || !['resync_required', 'not_found'].includes(String(value.error.code))) return invalid(); return failure(value.error.code === 'not_found' ? 404 : 409); }, event: 'public-project',
    }, { ...callbacks, events: async change => {
      if (change.revision <= current.revision) return;
      let next: PublicSnapshot;
      try { next = await this.snapshot(signal); }
      catch (error) {
        // Transport failures belong to reconnect handling, rather than being
        // mistaken for failures in the view callback below. Keep API errors
        // (including replay expiry) and caller cancellation unchanged.
        if (!signal.aborted && (error instanceof TypeError || error instanceof DOMException && ['AbortError', 'NetworkError', 'TimeoutError'].includes(error.name))) throw new ApiError('stream_unavailable', 'The public project could not be refreshed.');
        throw error;
      }
      if (next.generation !== current.generation) throw failure(409);
      if (next.revision < change.revision) throw invalid();
      current = next;
      if (!signal.aborted) await callbacks.snapshot(next);
    } }, signal);
  }
}
