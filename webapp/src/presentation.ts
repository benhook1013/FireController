import defaults from '../../internal/presentationmodel/defaults.json';
import { projectEventTypes } from './project-event-types';
const projectEventTypeSet: ReadonlySet<string> = new Set(projectEventTypes);
import type { Presentation, PresentationSnapshot, ProjectMetadataPage } from './types';

export { defaults as presentationDefaults };
export type ThemeToken = keyof typeof defaults.theme;
export type HeadingKey = keyof typeof defaults.headings;
const encoder = new TextEncoder();
const object = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v) && Object.getPrototypeOf(v) === Object.prototype;
const exact = (v: Record<string, unknown>, names: string[]) => Object.keys(v).length === names.length && names.every(k => Object.hasOwn(v, k));
const identity = (v: unknown): v is string => typeof v === 'string' && /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/u.test(v);
const positive = (v: unknown): v is number => Number.isSafeInteger(v) && Number(v) > 0;
const cursor = (v: unknown): v is string => typeof v === 'string' && v.length > 0 && v.length <= 4096 && !/[\s\p{Cc}]/u.test(v);
const name = (v: unknown, max: number): v is string => typeof v === 'string' && v.length > 0 && encoder.encode(v).length <= max && !/^\p{White_Space}|\p{White_Space}$/u.test(v) && !/[\p{Cc}\uD800-\uDFFF]/u.test(v);
// Implicit snapshots preserve the retained project name without repairing it.
const retainedName = (v: unknown): v is string => typeof v === 'string' && v.length > 0 && encoder.encode(v).length <= 200 && !/[\uD800-\uDFFF]/u.test(v);
function stamp(v: unknown): boolean {
  if (typeof v !== 'string' || !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/u.test(v) || !Number.isFinite(Date.parse(v))) return false;
  const [year, month, day] = v.slice(0, 10).split('-').map(Number);
  const calendar = new Date(0); calendar.setUTCFullYear(year, month - 1, day);
  return calendar.getUTCFullYear() === year && calendar.getUTCMonth() === month - 1 && calendar.getUTCDate() === day
    && Number(v.slice(11, 13)) < 24 && Number(v.slice(14, 16)) < 60 && Number(v.slice(17, 19)) < 60
    && (Date.parse(v) !== -62135596800000 || /\.\d*[1-9]/u.test(v));
}

function validContrast(theme: Record<string, unknown>): boolean {
  const luminance = (color: string) => {
    const channels = [1, 3, 5].map(i => { const n = parseInt(color.slice(i, i + 2), 16) / 255; return n <= .04045 ? n / 12.92 : ((n + .055) / 1.055) ** 2.4; });
    return .2126 * channels[0] + .7152 * channels[1] + .0722 * channels[2];
  };
  const pair = (a: ThemeToken, b: ThemeToken, minimum: number) => {
    if (!Object.hasOwn(theme, a) && !Object.hasOwn(theme, b)) return true;
    const x = luminance(String(theme[a] ?? defaults.theme[a])), y = luminance(String(theme[b] ?? defaults.theme[b]));
    return (Math.max(x, y) + .05) / (Math.min(x, y) + .05) >= minimum;
  };
  for (const key of Object.keys(defaults.theme) as ThemeToken[]) {
    if (!key.endsWith('_background')) continue;
    const family = key.slice(0, -'_background'.length);
    const foreground = `${family}_text` as ThemeToken;
    if (!Object.hasOwn(defaults.theme, foreground)) continue;
    if (!pair(foreground, key, 4.5) || !pair('focus', key, 3)) return false;
    const muted = `${family}_muted` as ThemeToken;
    if (Object.hasOwn(defaults.theme, muted) && !pair(muted, key, 4.5)) return false;
  }
  return pair('accent_background', 'canvas_background', 4.5) && pair('accent_background', 'surface_background', 4.5)
    && pair('surface_text', 'accent_soft_background', 4.5) && pair('surface_muted', 'accent_soft_background', 4.5)
    && pair('accent_background', 'accent_soft_background', 4.5) && pair('focus', 'accent_soft_background', 3);
}

export type PresentationConfig = Pick<Presentation, 'name' | 'brand_name' | 'tagline' | 'headings' | 'theme'>;
export function validPresentationConfig(v: unknown): v is PresentationConfig { return validConfig(v); }
function validConfig(v: unknown, implicit = false): v is PresentationConfig {
  if (!object(v) || !exact(v, ['name', 'brand_name', 'tagline', 'headings', 'theme']) || !(implicit ? retainedName(v.name) : name(v.name, 200)) || !name(v.brand_name, 120) || !name(v.tagline, 120) || !object(v.headings) || !object(v.theme)) return false;
  for (const [key, value] of Object.entries(v.headings)) if (!Object.hasOwn(defaults.headings, key) || !name(value, 240)) return false;
  for (const [key, value] of Object.entries(v.theme)) if (!Object.hasOwn(defaults.theme, key) || typeof value !== 'string' || !/^#[0-9a-f]{6}$/u.test(value)) return false;
  if (!validContrast(v.theme)) return false;
  return encoder.encode(JSON.stringify(v).replace(/[<>&\u2028\u2029]/gu, c => `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`)).length <= 16 << 10;
}
export function validPresentation(v: unknown, project: string): v is Presentation {
  if (!object(v) || !exact(v, ['project_id', 'revision', 'name', 'brand_name', 'tagline', 'headings', 'theme', 'updated_at']) || v.project_id !== project || !identity(v.project_id) || !Number.isSafeInteger(v.revision) || Number(v.revision) < 0 || !validConfig({ name: v.name, brand_name: v.brand_name, tagline: v.tagline, headings: v.headings, theme: v.theme }, v.revision === 0)) return false;
  return v.revision === 0 ? v.updated_at === '' && v.brand_name === 'FireController' && v.tagline === 'Project workspace' && Object.keys(v.headings as object).length === 0 && Object.keys(v.theme as object).length === 0 : stamp(v.updated_at);
}
export function validPresentationSnapshot(v: unknown, project: string): v is PresentationSnapshot {
  return object(v) && exact(v, ['presentation', 'event_cursor']) && validPresentation(v.presentation, project) && cursor(v.event_cursor);
}
function metadataResource(event: string, value: unknown): boolean {
  if (event !== 'reviews.rounds_configured') return identity(value);
  if (typeof value !== 'string') return false;
  const match = /^([1-9][0-9]{0,9})\/(hosted|cli)$/u.exec(value);
  return match !== null && match[0] === value && Number(match[1]) <= 2147483647;
}
export function validMetadataPage(v: unknown, limit: number): v is ProjectMetadataPage {
  if (!object(v) || !exact(v, ['items', 'event_cursor', 'has_more']) || !Array.isArray(v.items) || v.items.length > limit || !cursor(v.event_cursor) || typeof v.has_more !== 'boolean' || v.has_more && v.items.length === 0) return false;
  const seen = new Set<string>();
  for (const item of v.items) {
    if (!object(item) || !exact(item, ['event_type', 'resource_id', 'revision', 'event_cursor']) || typeof item.event_type !== 'string' || !projectEventTypeSet.has(item.event_type) || !metadataResource(item.event_type, item.resource_id) || !positive(item.revision) || !cursor(item.event_cursor) || seen.has(item.event_cursor)) return false;
    seen.add(item.event_cursor);
  }
  return !v.has_more || v.items.at(-1)?.event_cursor === v.event_cursor;
}
export function heading(presentation: Presentation | undefined, key: HeadingKey): string { return presentation?.headings[key] ?? defaults.headings[key]; }

// Empty themes preserve exact selector fallbacks. A configured theme resolves
// every role against the same reference palette used by contrast validation.
export function applyPresentationRoot(root: Pick<HTMLElement, 'style'>, document: Pick<Document, 'title'>, presentation?: Presentation): void {
  const configured = presentation !== undefined && Object.keys(presentation.theme).length > 0;
  for (const key of Object.keys(defaults.theme) as ThemeToken[]) {
    const value = configured ? presentation.theme[key] ?? defaults.theme[key] : undefined;
    if (value === undefined) root.style.removeProperty(`--fc-${key}`);
    else root.style.setProperty(`--fc-${key}`, value);
  }
  document.title = presentation ? `${presentation.name} · FireController` : 'FireController';
}
