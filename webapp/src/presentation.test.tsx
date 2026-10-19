import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryObserver } from '@tanstack/react-query';
import { Api } from './api';
import { applyPresentationRoot, heading, presentationDefaults, validMetadataPage, validPresentationSnapshot, validPresentationConfig } from './presentation';
import { PresentationContext, useProjectHeading } from './presentation-context';
import { isCurrentProjectView, refreshProjectMetadata } from './project-metadata';
import type { Presentation, PresentationSnapshot, ProjectMetadataPage } from './types';

const project = { id: 'project_one', name: 'Project one', github_repository_id: 1, repository: 'owner/repo' };
const presentation: Presentation = { project_id: project.id, revision: 0, name: project.name, brand_name: 'FireController', tagline: 'Project workspace', headings: {}, theme: {}, updated_at: '' };
const snapshot: PresentationSnapshot = { presentation, event_cursor: 'snapshot_cursor' };
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
const flush = async () => { for (let i = 0; i < 60; i++) await Promise.resolve(); };
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); vi.useRealTimers(); });

describe('project appearance and metadata boundary', () => {
  it('validates editable configuration without fabricated state identity or timestamps', () => {
    const config = { name: 'Project title', brand_name: 'Project brand', tagline: 'Project tagline', headings: { jobs: 'Current work' }, theme: { accent_text: '#fffffe' } };
    expect(validPresentationConfig(config)).toBe(true);
    expect(validPresentationConfig({ ...config, project_id: project.id })).toBe(false);
    expect(validPresentationConfig({ ...config, revision: 1, updated_at: '2026-10-17T12:00:00Z' })).toBe(false);
    expect(validPresentationConfig({ ...config, theme: { surface_text: '#f7f6f3' } })).toBe(false);
    expect(validPresentationConfig({ ...config, headings: { jobs: 'Untrusted\nheading' } })).toBe(false);
  });
  it('validates bounded project queue groups without changing queue or review rules', () => {
    const config={name:'Project',brand_name:'Brand',tagline:'Tagline',headings:{},theme:{},review_groups:[{title:'Foundation',pull_numbers:[1,2]},{title:'Delivery',pull_numbers:[9]}]};
    expect(validPresentationConfig(config)).toBe(true);
    expect(validPresentationConfig({...config,review_groups:null})).toBe(false);
    expect(validPresentationConfig({...config,review_groups:[{title:'One',pull_numbers:[1]},{title:'Two',pull_numbers:[1]}]})).toBe(false);
    expect(validPresentationConfig({...config,review_groups:[{title:'One',pull_numbers:[]}]})).toBe(false);
    expect(validPresentationConfig({...config,review_groups:[{title:'One',pull_numbers:[2147483648]}]})).toBe(false);
    expect(validPresentationSnapshot({...snapshot,presentation:{...presentation,review_groups:config.review_groups}},project.id)).toBe(false);
    expect(validPresentationSnapshot({...snapshot,presentation:{...presentation,revision:1,updated_at:'2026-10-19T00:00:00Z',review_groups:config.review_groups}},project.id)).toBe(true);
    expect(validPresentationConfig({...config,theme:{merged_text:'#f2ebf8'}})).toBe(false);
    expect(validPresentationConfig({...config,theme:{merged_rail_text:'#7042a0'}})).toBe(false);
  });
  it('preserves the approved fallback colors and layout and covers the shared token vocabulary', async () => {
    const fs = await vi.importActual<{ readFileSync(path: URL, encoding: 'utf8'): string }>('node:fs');
    const styles = fs.readFileSync(new URL('./styles.css', import.meta.url), 'utf8');
    const base = styles.split('\n/* Bounded presentation text. */')[0];
    // Baseline keeps the selected Inter 600 header/five-pixel gap and removes
    // the redundant left hero eyebrow and permission pill as requested. The
    // mobile title now scales below 390px so ordinary words fit at 320px;
    // the 390px and desktop title sizes remain unchanged.
    const original = base.replace(/var\(--fc-[a-z_]+,([^()]+)\)/gu, '$1');
    const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(original));
    expect(Array.from(new Uint8Array(hash), byte => byte.toString(16).padStart(2, '0')).join('')).toBe('be741206be6ffabd6407a5a3277b3a32efa212bc3413b853a92615f05bcb8af2');
    const modularStyles = styles + fs.readFileSync(new URL('./review-queue.css', import.meta.url), 'utf8');
    const tokens = new Set(Array.from(modularStyles.matchAll(/--fc-([a-z_]+)/gu), match => match[1]));
    expect([...tokens].sort()).toEqual(Object.keys(presentationDefaults.theme).sort());
    // Decorative shadows may stay literal. All informative declarations use
    // bounded roles, including colors added by stored-review views.
    for (const declaration of base.matchAll(/([a-z-]+):([^;{}]+)/gu)) {
      if (declaration[1] === 'box-shadow' && !declaration[2].includes('inset')) continue;
      const unbound = declaration[2].replace(/var\(--fc-[a-z_]+,[^()]+\)/gu, '');
      expect(unbound, declaration[0]).not.toMatch(/#[0-9a-f]{3,8}\b|\bwhite\b/iu);
    }
  });

  it('validates implicit defaults and rejects malformed configuration and metadata shapes', () => {
    expect(validPresentationSnapshot(snapshot, project.id)).toBe(true);
    const changed = { ...presentation, revision: 1, updated_at: '2026-10-16T01:00:00.000000000Z', theme: { surface_text: '#000000' }, headings: { jobs: '<literal>' } };
    expect(validPresentationSnapshot({ presentation: changed, event_cursor: 'new_cursor' }, project.id)).toBe(true);
    for (const value of [
      { ...presentation, revision: null }, { ...presentation, revision: 1.1 }, { ...presentation, revision: 1, updated_at: '' },
      { ...changed, updated_at: '2026-02-30T01:00:00Z' }, { ...changed, updated_at: '0001-01-01T00:00:00Z' },
      { ...presentation, project_id: 'foreign' }, { ...presentation, theme: null }, { ...changed, theme: { extra: '#ffffff' } },
      { ...changed, theme: { surface_text: 'red' } }, { ...changed, theme: { surface_text: '#FFFFFF' } }, { ...changed, theme: { surface_text: '#f7f6f3' } },
      { ...changed, theme: { surface_text: 'url(https://example.test)' } }, { ...changed, headings: { jobs: null } },
      { ...changed, theme: { accent_soft_background: '#000000' } },
      { ...changed, headings: { jobs: 'x\n' } }, { ...changed, headings: { unowned: 'title' } }, { ...changed, brand_name: '' },
      { ...presentation, updated_at: changed.updated_at }, { ...presentation, headings: { jobs: 'override at zero' } },
    ]) expect(validPresentationSnapshot({ presentation: value, event_cursor: 'cursor' }, project.id), JSON.stringify(value)).toBe(false);
    const page: ProjectMetadataPage = { items: [{ event_type: 'map.entry_updated', resource_id: 'entry_one', revision: 2, event_cursor: 'event_cursor' }], event_cursor: 'event_cursor', has_more: true };
    expect(validMetadataPage(page, 25)).toBe(true);
    for (const value of [{ ...page, cursor: 'message_cursor' }, { ...page, event_cursor: null }, { ...page, items: null }, { ...page, has_more: true, items: [] }, { ...page, event_cursor: 'wrong_last' }, { ...page, items: [...page.items, ...page.items] }, { ...page, items: [{ ...page.items[0], event_type: 'message.sent' }] }, { ...page, items: [{ ...page.items[0], revision: Number.MAX_SAFE_INTEGER + 1 }] }, { ...page, items: [{ ...page.items[0], body: 'not metadata' }] }]) expect(validMetadataPage(value, 25)).toBe(false);
  });

  it('accepts canonical round-control resources without widening other event identities', () => {
    const event = { event_type: 'reviews.rounds_configured', resource_id: '32/hosted', revision: 1, event_cursor: 'round_cursor' };
    const page = { items: [event], event_cursor: event.event_cursor, has_more: false };
    for (const resource_id of ['1/cli', '32/hosted', '32/cli', '2147483647/hosted']) {
      expect(validMetadataPage({ ...page, items: [{ ...event, resource_id }] }, 25), resource_id).toBe(true);
    }
    for (const resource_id of ['0/cli', '01/cli', '-1/cli', '+1/cli', '1.0/cli', '1e2/cli', '2147483648/cli', '99999999999999999999/cli', '1/CLI', '1/unknown', '1/cli/more', '1//cli', '1%2fcli', '../cli', '1/cli\n', 'record_one', null]) {
      expect(validMetadataPage({ ...page, items: [{ ...event, resource_id }] }, 25), String(resource_id)).toBe(false);
    }
    for (const event_type of ['map.entry_updated', 'coordination.imported', 'review.observed', 'review.evidence.finalized']) {
      expect(validMetadataPage({ ...page, items: [{ ...event, event_type }] }, 25), event_type).toBe(false);
      expect(validMetadataPage({ ...page, items: [{ ...event, event_type, resource_id: 'record_one' }] }, 25), event_type).toBe(true);
    }
    for (const item of [{ ...event, payload: { channel: 'hosted' } }, { ...event, revision: 0 }, { ...event, event_cursor: '' }]) {
      expect(validMetadataPage({ ...page, items: [item] }, 25)).toBe(false);
    }
    expect(validMetadataPage({ ...page, extra: true }, 25)).toBe(false);
    expect(validMetadataPage({ ...page, items: [event, event] }, 25)).toBe(false);
  });

  it('preserves bounded well-formed retained names only at implicit revision zero', () => {
    const retainedNames = ['  Retained\tname\n\u0000\u0085  ', ' \t\n ', ' \t' + '🔥'.repeat(49) + '\n '];
    expect(new TextEncoder().encode(retainedNames[2]).length).toBe(200);
    for (const name of retainedNames) {
      expect(validPresentationSnapshot({ presentation: { ...presentation, name }, event_cursor: 'retained_cursor' }, project.id)).toBe(true);
      expect(validPresentationSnapshot({ presentation: { ...presentation, name, revision: 1, updated_at: '2026-10-16T01:00:00Z' }, event_cursor: 'configured_cursor' }, project.id)).toBe(false);
    }
    // A canonical astral name is valid at both revisions; the limit is UTF-8
    // bytes, and valid pairs must not be mistaken for lone surrogates.
    for (const revision of [0, 1]) {
      const state = { ...presentation, revision, updated_at: revision ? '2026-10-16T01:00:00Z' : '' };
      expect(validPresentationSnapshot({ presentation: { ...state, name: '🔥'.repeat(50) }, event_cursor: 'cursor' }, project.id)).toBe(true);
      for (const name of ['', null, '🔥'.repeat(50) + 'a', '\ud800', '\udfff', 'retained\ud800name']) {
        expect(validPresentationSnapshot({ presentation: { ...state, name }, event_cursor: 'cursor' }, project.id)).toBe(false);
      }
    }
    for (const change of [
      { project_id: 'foreign' }, { brand_name: 'Different' }, { tagline: 'Different' }, { headings: null }, { theme: null },
      { headings: { jobs: 'Different' } }, { theme: { surface_text: '#000000' } }, { updated_at: '2026-10-16T01:00:00Z' },
    ]) expect(validPresentationSnapshot({ presentation: { ...presentation, name: retainedNames[0], ...change }, event_cursor: 'cursor' }, project.id)).toBe(false);
  });

  it('reads an exact retained snapshot through Api.presentation without rewriting text or issuing mutations', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    const name = ' \tRetained\n<img src=x>\u0000 ';
    const retained = { presentation: { ...presentation, name }, event_cursor: 'retained_cursor' };
    const requests: { method: string; url: string }[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input); requests.push({ method: init?.method ?? 'GET', url });
      if (url.endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (url.endsWith('/project_one')) return json(project);
      return json(retained);
    }));
    const api = new Api('private-test-token', project.id);
    try {
      await api.connect();
      const result = await api.presentation();
      expect(result).toEqual(retained); expect(result.presentation.name).toBe(name);
      expect(requests).toEqual([
        { method: 'GET', url: 'https://service.example/api/v1/instance' },
        { method: 'GET', url: 'https://service.example/api/v1/projects/project_one' },
        { method: 'GET', url: 'https://service.example/api/v1/projects/project_one/presentation' },
      ]);
      const doc = { title: '' };
      applyPresentationRoot({ style: { removeProperty: () => '', setProperty: () => {} } } as unknown as HTMLElement, doc, result.presentation);
      expect(doc.title).toBe(name + ' · FireController');
      const html = renderToStaticMarkup(<h1>{result.presentation.name}</h1>);
      expect(html).toContain('&lt;img src=x&gt;'); expect(html).not.toContain('<img');
    } finally { api.close(); }
  });

  it('pairs every review-card label and count with its status background while preserving default colors', async () => {
    const fs = await vi.importActual<{ readFileSync(path: URL, encoding: 'utf8'): string }>('node:fs');
    const styles = fs.readFileSync(new URL('./styles.css', import.meta.url), 'utf8');
    const theme: Record<string, string> = { focus: '#808080', accent_background: '#ffffff', accent_text: '#000000', accent_soft_background: '#000000' };
    for (const family of ['canvas', 'surface', 'header', 'hero', 'active', 'blocked', 'parked', 'completed', 'merged']) {
      theme[`${family}_background`] = '#000000'; theme[`${family}_text`] = '#ffffff';
      if (['canvas', 'surface', 'header', 'hero'].includes(family)) theme[`${family}_muted`] = '#ffffff';
    }
    const retainedTheme = {...theme}; delete retainedTheme.merged_background; delete retainedTheme.merged_text;
    expect(validPresentationSnapshot({presentation:{...presentation,revision:1,updated_at:'2026-10-16T01:00:00Z',theme:retainedTheme},event_cursor:'cursor'},project.id)).toBe(true);
    theme.merged_rail='#000000'; theme.merged_rail_text='#ffffff';
    // Both status surfaces may legally differ from the surrounding dark panel.
    theme.blocked_background = theme.parked_background = '#ffffff';
    theme.blocked_text = theme.parked_text = '#000000';
    expect(validPresentationSnapshot({ presentation: { ...presentation, revision: 1, updated_at: '2026-10-16T01:00:00Z', theme }, event_cursor: 'cursor' }, project.id)).toBe(true);
    const property = (selector: string, name: string) => {
      const rules = [...styles.replace(/\/\*[\s\S]*?\*\//gu, '').matchAll(/([^{}]+)\{([^{}]+)\}/gu)].filter(rule => rule[1].split(',').map(value => value.trim()).includes(selector));
      return rules.flatMap(rule => rule[2].split(';')).filter(value => value.startsWith(`${name}:`)).at(-1)?.slice(name.length + 1);
    };
    const labels = [['', '#5f4f57'], ['>strong', '#a52a2c'], ['>strong small', '#9b7771'], [' .review-channel', '#a25a4b'], [' .review-warning', '#9d592d']];
    for (const outcome of ['failed', 'uncertain', 'active', 'posting', 'prepared']) {
      const role = ['failed', 'uncertain'].includes(outcome) ? 'blocked' : 'parked';
      const card = `.review-round.review-outcome-${outcome}`;
      for (const [descendant, fallback] of labels) {
        const value = property(card + descendant, 'color');
        expect(value, card + descendant).toBe(`var(--fc-${role}_text,${fallback})`);
        const token = value?.match(/^var\(--fc-([a-z_]+),/u)?.[1];
        expect(theme[token ?? ''], card + descendant).toBe('#000000');
      }
      // Hover must keep the corresponding status pair, with the original
      // override-free hover color retained as its selector-specific fallback.
      expect(property(`${card}:hover`, 'background')).toBe(`var(--fc-${role}_background,#f4dfd5)`);
    }
  });

  it('resets root overrides/title without changing fallback CSS or retaining another project', () => {
    const properties = new Map<string, string>();
    const root = { style: { setProperty: (key: string, value: string) => properties.set(key, value), removeProperty: (key: string) => { properties.delete(key); return ''; } } };
    const doc = { title: 'FireController' };
    applyPresentationRoot(root as unknown as HTMLElement, doc, { ...presentation, name: 'One', theme: { surface_text: '#000000' } });
    expect(properties.get('--fc-surface_text')).toBe('#000000'); expect(doc.title).toBe('One · FireController');
    const partial = { ...presentation, name: 'Two', revision: 1, updated_at: '2026-10-16T01:00:00Z', theme: { header_background: '#000000' } };
    expect(validPresentationSnapshot({ presentation: partial, event_cursor: 'cursor' }, project.id)).toBe(true);
    applyPresentationRoot(root as unknown as HTMLElement, doc, partial);
    expect(properties.size).toBe(Object.keys(presentationDefaults.theme).length);
    expect(properties.get('--fc-surface_text')).toBe(presentationDefaults.theme.surface_text);
    expect(properties.get('--fc-header_background')).toBe('#000000');
    expect(properties.get('--fc-header_muted')).toBe(presentationDefaults.theme.header_muted);
    // An explicitly empty theme also clears every variable, without waiting
    // for a disconnect or retaining the previous project's resolved colors.
    applyPresentationRoot(root as unknown as HTMLElement, doc, { ...presentation, name: 'Three' });
    expect(properties.size).toBe(0); expect(doc.title).toBe('Three · FireController');
    applyPresentationRoot(root as unknown as HTMLElement, doc);
    expect(properties.size).toBe(0); expect(doc.title).toBe('FireController');
    const one = new AbortController(), two = new AbortController();
    expect(isCurrentProjectView(project.id, '/projects/project_one/map', one, one)).toBe(true);
    expect(isCurrentProjectView(project.id, '/projects/project_one_extra', one, one)).toBe(false);
    expect(isCurrentProjectView(project.id, '/projects/foreign', one, one)).toBe(false);
    expect(isCurrentProjectView(project.id, '/projects/project_one', one, two)).toBe(false);
    one.abort(); expect(isCurrentProjectView(project.id, '/projects/project_one', one, one)).toBe(false);
  });

  it('renders configured headings literally with exact shared defaults', () => {
    function Headings() { return <><h2>{useProjectHeading('jobs')}</h2><h3>{useProjectHeading('hero_title')}</h3></>; }
    const html = renderToStaticMarkup(<PresentationContext.Provider value={{ ...presentation, headings: { jobs: '<img src=x>', hero_title: '<script>Literal</script>' } }}><Headings /></PresentationContext.Provider>);
    expect(html).toContain('&lt;img src=x&gt;'); expect(html).toContain('&lt;script&gt;Literal&lt;/script&gt;'); expect(html).not.toContain('<img');
    expect(heading(undefined, 'hero_title')).toBe(presentationDefaults.headings.hero_title);
  });

  it('uses scoped strict GET snapshots and rejects unknown/duplicate/private response fields', async () => {
    vi.stubGlobal('location', { origin: 'https://service.example' });
    let body = JSON.stringify(snapshot);
    const requests: { url: string; headers: Headers }[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input); requests.push({ url, headers: new Headers(init?.headers) });
      if (url.endsWith('/instance')) return json({ instance_id: 'instance_one' });
      if (url.endsWith('/project_one')) return json(project);
      return new Response(body, { headers: { 'content-type': 'application/json' } });
    }));
    const api = new Api('private-test-token', project.id); await api.connect();
    await expect(api.presentation()).resolves.toEqual(snapshot);
    expect(requests.at(-1)?.url).toBe('https://service.example/api/v1/projects/project_one/presentation');
    expect(requests.at(-1)?.headers.get('x-fc-instance')).toBe('instance_one');
    expect(requests.at(-1)?.headers.get('x-fc-worker')).toBeNull();
    for (const invalid of [JSON.stringify({ ...snapshot, private: 'body' }), JSON.stringify({ ...snapshot, presentation: { ...presentation, theme: null } }), JSON.stringify(snapshot).replace('"revision":0', '"revision":0,"revision":0'), JSON.stringify(snapshot).replace('Project one', '\\ud800')]) {
      body = invalid; await expect(api.presentation()).rejects.toMatchObject({ code: 'invalid_response' });
    }
    api.close();
  });

  it('cancels pre-event map reads, marks inactive pages stale and never fetches unrelated bodies', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
    const key = ['fc', 'session', 'instance', project.id, 'worker'];
    let late!: (value: string) => void; let calls = 0; let oldSignal: AbortSignal | undefined;
    const old = new Promise<string>(resolve => { late = resolve; });
    const observer = new QueryObserver(client, { queryKey: [...key, 'map-entry', 'entry_one'], queryFn: ({ signal }) => { if (++calls === 1) { oldSignal = signal; return old; } return Promise.resolve('new'); } });
    const unsubscribe = observer.subscribe(() => {});
    client.setQueryData([...key, 'map', 'inactive'], 'older page'); client.setQueryData([...key, 'reviews'], 'untouched');
    try {
      await refreshProjectMetadata(client, key, { items: [{ event_type: 'map.entry_updated', resource_id: 'entry_one', revision: 2, event_cursor: 'cursor' }], event_cursor: 'cursor', has_more: false }, new AbortController().signal);
      expect(oldSignal?.aborted).toBe(true); expect(calls).toBe(2); expect(client.getQueryData([...key, 'map-entry', 'entry_one'])).toBe('new');
      expect(client.getQueryState([...key, 'map', 'inactive'])?.isInvalidated).toBe(true);
      expect(client.getQueryState([...key, 'reviews'])?.isInvalidated).toBe(false);
      late('old'); await flush(); expect(client.getQueryData([...key, 'map-entry', 'entry_one'])).toBe('new');
    } finally { unsubscribe(); client.clear(); }
  });
});
