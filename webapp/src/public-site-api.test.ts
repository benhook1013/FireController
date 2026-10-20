import { afterEach, describe, expect, it, vi } from 'vitest';
import { PublicSiteApi, validatePublicChange, validatePublicSnapshot, type PublicSnapshot } from './public-site-api';
import { publicProjectLocation } from './public-site-api';

const generation = `public_generation_${'a'.repeat(32)}`;
const published = '2026-10-17T12:00:00Z';
function snapshot(revision = 1): PublicSnapshot {
  return { slug: 'showcase', generation, revision, published_at: published, cursor: `${generation}:${revision}`, content: { slug: 'showcase', presentation: { name: 'Showcase', brand_name: 'Showcase', tagline: 'A shared project', headings: {}, theme: {} }, description: 'Approved overview\nSecond line.', links: [{ key: 'source', label: 'Source', url: 'https://example.com/source' }], project_map: [], progress: [], worker_lanes: [], reviews: [{ number: 4, title: 'Completed work', url: 'https://github.com/example/project/pull/4', lifecycle: 'merged', cli: [], hosted: [{ key: `public_round_${'b'.repeat(32)}`, found: 2, accepted: 1, routed: 1, complete: true, observed_at: '', duration_milliseconds: null }] }] } };
}
function json(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } }); }
function frame(revision: number, extra: Record<string, unknown> = {}) { const cursor = `${generation}:${revision}`; return new TextEncoder().encode(`event: public-project\nid: ${cursor}\ndata: ${JSON.stringify({ revision, published_at: published, cursor, ...extra })}\n\n`); }
async function flush() { for (let i = 0; i < 80; i++) await Promise.resolve(); }
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('anonymous project address resolution', () => {
  it.each(['firemud','project_one','legacy-public'])('resolves only the enabled publication slug for %s without cookies or private identity', async address => {
    const fetch=vi.fn().mockResolvedValue(new Response(JSON.stringify({slug:'published-showcase'}),{headers:{'content-type':'application/json'}}));vi.stubGlobal('fetch',fetch);
    await expect(publicProjectLocation(address,new AbortController().signal)).resolves.toBe('published-showcase');
    expect(fetch.mock.calls[0][0]).toBe(`/api/v1/public/project-locations/${address}`);
    expect(fetch.mock.calls[0][1]).toMatchObject({method:'GET',credentials:'omit',cache:'no-store',redirect:'error',headers:{Accept:'application/json'}});
  });
  it.each([{slug:'published-showcase',project_id:'private'}, {slug:null}, {slug:'UPPER'}, {}, {slug:'../other'}])('rejects any private or malformed location field',async value=>{
    vi.stubGlobal('fetch',vi.fn().mockResolvedValue(new Response(JSON.stringify(value),{headers:{'content-type':'application/json'}})));
    await expect(publicProjectLocation('firemud',new AbortController().signal)).rejects.toBeDefined();
  });
  it('refuses malformed addresses before I/O and preserves unpublished404',async()=>{
    const fetch=vi.fn().mockResolvedValue(new Response('{}',{status:404,headers:{'content-type':'application/json'}}));vi.stubGlobal('fetch',fetch);
    await expect(publicProjectLocation('../other',new AbortController().signal)).rejects.toBeDefined();expect(fetch).not.toHaveBeenCalled();
    await expect(publicProjectLocation('firemud',new AbortController().signal)).rejects.toMatchObject({status:404});
  });
});

describe('anonymous publication DTO boundary', () => {
  it('preserves old payloads and accepts only optional boolean channel window flags', () => {
    const old = snapshot(), before = JSON.stringify(old);
    expect(validatePublicSnapshot(old, 'showcase')).toBe(true);
    expect(JSON.stringify(old)).toBe(before);
    expect(Object.hasOwn(old.content.reviews[0], 'cli_has_more')).toBe(false);
    for (const field of ['cli_has_more', 'hosted_has_more']) {
      for (const flag of [true, false]) {
        const value = snapshot(); Object.assign(value.content.reviews[0], { [field]: flag });
        expect(validatePublicSnapshot(value, 'showcase')).toBe(true);
      }
      for (const flag of [null, 0, 1, 'true', [], {}, undefined]) {
        const value = snapshot(); Object.assign(value.content.reviews[0], { [field]: flag });
        expect(validatePublicSnapshot(value, 'showcase')).toBe(false);
      }
    }
    const unknown = snapshot(); Object.assign(unknown.content.reviews[0], { has_more: true });
    expect(validatePublicSnapshot(unknown, 'showcase')).toBe(false);
  });
  it('retains the 100-round transfer bound even when more rounds are declared', () => {
    for (const channel of ['cli', 'hosted'] as const) {
      const value = snapshot(), review = value.content.reviews[0];
      review[channel] = Array.from({ length: 100 }, (_, i) => ({ ...review.hosted[0], key: `public_round_${i.toString(16).padStart(32, '0')}` }));
      review[`${channel}_has_more`] = true;
      expect(validatePublicSnapshot(value, 'showcase')).toBe(true);
      review[channel].push({ ...review[channel][0], key: `public_round_${'f'.repeat(32)}` });
      expect(validatePublicSnapshot(value, 'showcase')).toBe(false);
    }
  });
  it('accepts only selected public values and explicit unknown round details', () => {
    expect(validatePublicSnapshot(snapshot(), 'showcase')).toBe(true);
    for (const [path, value] of [['project_id', 'PRIVATE_PROJECT'], ['snapshot_id', 'PRIVATE_SNAPSHOT'], ['actor_id', 'PRIVATE_ACTOR'], ['credential', 'PRIVATE_TOKEN']]) {
      expect(validatePublicSnapshot({ ...snapshot(), [path]: value }, 'showcase')).toBe(false);
      expect(validatePublicSnapshot({ ...snapshot(), content: { ...snapshot().content, [path]: value } }, 'showcase')).toBe(false);
    }
    expect(validatePublicSnapshot(snapshot(), 'another-project')).toBe(false);
    expect(validatePublicSnapshot(snapshot(), 'showcase', `public_generation_${'c'.repeat(32)}`)).toBe(false);
    expect(validatePublicSnapshot(snapshot(), 'showcase', generation, 2)).toBe(false);
    expect(validatePublicChange({ revision: 2, published_at: published, cursor: `${generation}:2`, private_revision: 99 }, generation)).toBe(false);
  });
  it('rejects unsafe links, private IDs, malformed counts and calendars', () => {
    for (const unsafe of ['javascript:alert(1)', 'data:text/html,<script>x</script>', 'https://user:secret@example.com', 'https://example.com/\nprivate']) {
      const value = snapshot(); value.content.links[0].url = unsafe; expect(validatePublicSnapshot(value, 'showcase')).toBe(false);
    }
    const value = snapshot(); value.content.reviews[0].hosted[0].key = 'review_evidence_private'; expect(validatePublicSnapshot(value, 'showcase')).toBe(false);
    value.content.reviews[0].hosted[0].key = `public_round_${'b'.repeat(32)}`; value.content.reviews[0].hosted[0].routed = 2; expect(validatePublicSnapshot(value, 'showcase')).toBe(false);
    expect(validatePublicSnapshot({ ...snapshot(), published_at: '2026-02-30T12:00:00Z' }, 'showcase')).toBe(false);
  });
});

describe('public transport has no private connection authority', () => {
  it('reads window flags anonymously in current and frozen content and refuses malformed wire flags', async () => {
    const value = snapshot(); value.content.reviews[0].cli_has_more = true; value.content.reviews[0].hosted_has_more = false;
    const fetcher = vi.fn(async (_url: string, init: RequestInit) => { expect(init.credentials).toBe('omit'); return json(value); });
    vi.stubGlobal('fetch', fetcher);
    const api = new PublicSiteApi('showcase'), signal = new AbortController().signal;
    expect((await api.snapshot(signal)).content.reviews[0]).toEqual(value.content.reviews[0]);
    expect((await api.snapshot(signal, 1, generation)).content.reviews[0]).toEqual(value.content.reviews[0]);
    const raw = JSON.stringify(value);
    for (const invalid of [raw.replace('"cli_has_more":true', '"cli_has_more":null'), raw.replace('"cli_has_more":true', '"cli_has_more":1'), raw.replace('"cli_has_more":true', '"cli_has_more":true,"cli_has_more":true'), raw.replace('"cli_has_more":true', '"private_has_more":true')]) {
      vi.stubGlobal('fetch', vi.fn(async () => new Response(invalid, { headers: { 'content-type': 'application/json' } })));
      await expect(api.snapshot(signal)).rejects.toMatchObject({ code: 'invalid_response' });
    }
  });
  it('accepts decoded Fetch bodies retaining compression headers and bounds decoded bytes', async () => {
    // Fetch exposes decoded body bytes even when the gzip/br response headers
    // and compressed Content-Length remain visible to application code.
    for (const encoding of ['gzip', 'br']) {
      vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(snapshot()), { headers: { 'content-type': 'application/json', 'content-encoding': encoding, 'content-length': '100' } })));
      expect((await new PublicSiteApi('showcase').snapshot(new AbortController().signal)).content.presentation.name).toBe('Showcase');
    }
    vi.stubGlobal('fetch', vi.fn(async () => new Response(' '.repeat(512 << 10) + JSON.stringify(snapshot()), { headers: { 'content-type': 'application/json', 'content-encoding': 'gzip', 'content-length': '100' } })));
    await expect(new PublicSiteApi('showcase').snapshot(new AbortController().signal)).rejects.toMatchObject({ code: 'invalid_response' });
  });
  it('uses credentials omit and public-only routes for current and frozen history', async () => {
    const calls: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => { calls.push(url); expect(init.credentials).toBe('omit'); expect(init.redirect).toBe('error'); expect(init.cache).toBe('no-store'); expect(new Headers(init.headers).has('authorization')).toBe(false); expect(new Headers(init.headers).has('x-fc-browser-session')).toBe(false); return json(snapshot()); }));
    const api = new PublicSiteApi('showcase'), signal = new AbortController().signal;
    expect((await api.snapshot(signal)).content.presentation.name).toBe('Showcase');
    await api.snapshot(signal, 1, generation);
    expect(calls).toEqual(['/api/v1/public/projects/showcase', `/api/v1/public/projects/showcase/history/1?generation=${generation}`]);
  });
  it('rejects duplicate JSON keys and oversized streamed responses', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(snapshot()).replace('"slug":"showcase"', '"slug":"showcase","slug":"showcase"'), { headers: { 'content-type': 'application/json' } })));
    await expect(new PublicSiteApi('showcase').snapshot(new AbortController().signal)).rejects.toMatchObject({ code: 'invalid_response' });
    let canceled = false;
    vi.stubGlobal('fetch', vi.fn(async () => new Response(new ReadableStream({ start(output) { output.enqueue(new Uint8Array((512 << 10) + 1)); }, cancel() { canceled = true; } }), { headers: { 'content-type': 'application/json' } })));
    await expect(new PublicSiteApi('showcase').snapshot(new AbortController().signal)).rejects.toMatchObject({ code: 'invalid_response' });
    expect(canceled).toBe(true);
  });
  it('binds compact history to its generation, decreasing pages and last cursor', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => json({ slug: 'showcase', generation, items: [{ revision: 3, published_at: published }, { revision: 2, published_at: published }], has_more: true, cursor: `${generation}:2` })));
    const api = new PublicSiteApi('showcase'); expect((await api.history(new AbortController().signal, generation)).items).toHaveLength(2);
    await expect(api.history(new AbortController().signal, generation, `${generation}:2`)).rejects.toMatchObject({ code: 'invalid_response' });
    await expect(api.history(new AbortController().signal, `public_generation_${'c'.repeat(32)}`)).rejects.toMatchObject({ code: 'invalid_response' });
  });
  it('bounds finite fetches to five seconds and cancels scope-owned requests', async () => {
    vi.useFakeTimers(); let canceled = false;
    vi.stubGlobal('fetch', vi.fn((_url, init: RequestInit) => new Promise<Response>((_resolve, reject) => { init.signal!.addEventListener('abort', () => { canceled = true; reject(new DOMException('Aborted', 'AbortError')); }, { once: true }); })));
    const pending = new PublicSiteApi('showcase').snapshot(new AbortController().signal).catch(error => error);
    await vi.advanceTimersByTimeAsync(5000); await pending; expect(canceled).toBe(true);
  });
});

describe('quiet public live delivery', () => {
  it.each(['network', 'body network', 'deadline', 'server'])('replays the unprocessed change after an event snapshot %s failure', async kind => {
    vi.useFakeTimers();
    let reads = 0, stream!: ReadableStreamDefaultController<Uint8Array>, failedSignal: AbortSignal | undefined;
    const cursors: string[] = [], seen: number[] = [], statuses: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      expect(init.credentials).toBe('omit'); expect(init.redirect).toBe('error'); expect(init.cache).toBe('no-store');
      expect(new Headers(init.headers).has('authorization')).toBe(false);
      expect(new Headers(init.headers).has('x-fc-browser-session')).toBe(false);
      if (url.includes('/events?')) {
        cursors.push(new URL(url, 'https://public.example').searchParams.get('cursor')!);
        return new Response(new ReadableStream<Uint8Array>({ start(output) {
          stream = output; output.enqueue(frame(2));
          init.signal!.addEventListener('abort', () => { try { output.close(); } catch { /* canceled reader */ } }, { once: true });
        } }), { headers: { 'content-type': 'text/event-stream' } });
      }
      expect(url).toBe('/api/v1/public/projects/showcase');
      reads++;
      if (reads === 1) return json(snapshot());
      if (reads === 2) {
        if (kind === 'network') throw new TypeError('Synthetic network failure');
        if (kind === 'body network') return new Response(new ReadableStream({ start(output) { output.error(new TypeError('Synthetic body failure')); } }), { headers: { 'content-type': 'application/json' } });
        if (kind === 'server') return json({}, 503);
        failedSignal = init.signal!;
        return new Promise<Response>((_resolve, reject) => init.signal!.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true }));
      }
      return json(snapshot(reads - 1));
    }));
    const controller = new AbortController();
    const watch = new PublicSiteApi('showcase').watch({ snapshot: value => { seen.push(value.revision); }, status: value => { statuses.push(value); } }, controller.signal).catch(error => error);
    try {
      await flush();
      if (kind === 'deadline') { await vi.advanceTimersByTimeAsync(5000); expect(failedSignal?.aborted).toBe(true); }
      expect(statuses.at(-1)).toBe('reconnecting'); expect(seen).toEqual([1]); expect(reads).toBe(2);
      await vi.advanceTimersByTimeAsync(999); expect(cursors).toEqual([`${generation}:1`]);
      await vi.advanceTimersByTimeAsync(1); await flush();
      expect(cursors).toEqual([`${generation}:1`, `${generation}:1`]); expect(seen).toEqual([1, 2]); expect(reads).toBe(3);
      stream.enqueue(frame(2)); await flush(); expect(reads).toBe(3);
      stream.enqueue(frame(3)); await flush(); expect(seen).toEqual([1, 2, 3]);
    } finally { controller.abort(); await watch; }
    expect(vi.getTimerCount()).toBe(0);
  });

  it('cancels the actual pending event snapshot without a retry or late callback', async () => {
    vi.useFakeTimers(); let reads = 0, streams = 0, pendingSignal: AbortSignal | undefined;
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      if (url.includes('/events?')) {
        streams++;
        return new Response(new ReadableStream({ start(output) { output.enqueue(frame(2)); init.signal!.addEventListener('abort', () => { try { output.close(); } catch { /* canceled reader */ } }, { once: true }); } }), { headers: { 'content-type': 'text/event-stream' } });
      }
      if (++reads === 1) return json(snapshot());
      pendingSignal = init.signal!;
      return new Promise<Response>((_resolve, reject) => init.signal!.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true }));
    }));
    const controller = new AbortController(), seen: number[] = [];
    const watch = new PublicSiteApi('showcase').watch({ snapshot: value => { seen.push(value.revision); }, status() {} }, controller.signal);
    await flush(); expect(reads).toBe(2); expect(pendingSignal?.aborted).toBe(false);
    controller.abort(); await watch; expect(pendingSignal?.aborted).toBe(true);
    await vi.advanceTimersByTimeAsync(10_000); expect(reads).toBe(2); expect(streams).toBe(1); expect(seen).toEqual([1]); expect(vi.getTimerCount()).toBe(0);
  });

  it.each(['malformed', 'not found', 'older revision', 'view callback'])('keeps an event snapshot %s failure terminal', async kind => {
    vi.useFakeTimers(); let reads = 0, streams = 0;
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (url.includes('/events?')) { streams++; return new Response(new ReadableStream({ start(output) { output.enqueue(frame(2)); } }), { headers: { 'content-type': 'text/event-stream' } }); }
      if (++reads === 1) return json(snapshot());
      if (kind === 'not found') return json({}, 404);
      if (kind === 'malformed') return new Response('{', { headers: { 'content-type': 'application/json' } });
      return json(snapshot(kind === 'older revision' ? 1 : 2));
    }));
    const seen: number[] = [], statuses: string[] = [];
    const error = await new PublicSiteApi('showcase').watch({ snapshot: value => {
      if (value.revision === 2 && kind === 'view callback') throw new TypeError('Synthetic view failure');
      seen.push(value.revision);
    }, status: value => { statuses.push(value); } }, new AbortController().signal).catch(value => value);
    expect(error).toMatchObject({ code: kind === 'not found' ? 'not_found' : kind === 'view callback' ? 'callback_failed' : 'invalid_response' });
    expect(statuses.at(-1)).toBe('closed'); expect(seen).toEqual([1]);
    await vi.advanceTimersByTimeAsync(10_000); expect(reads).toBe(2); expect(streams).toBe(1); expect(vi.getTimerCount()).toBe(0);
  });

  it('retains event snapshot 409 resynchronization instead of treating it as a transport failure', async () => {
    vi.useFakeTimers(); let reads = 0; const cursors: string[] = [], seen: number[] = [];
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => {
      if (url.includes('/events?')) {
        cursors.push(new URL(url, 'https://public.example').searchParams.get('cursor')!);
        return new Response(new ReadableStream({ start(output) {
          if (cursors.length === 1) output.enqueue(frame(2));
          init.signal!.addEventListener('abort', () => { try { output.close(); } catch { /* canceled reader */ } }, { once: true });
        } }), { headers: { 'content-type': 'text/event-stream' } });
      }
      reads++; return reads === 2 ? json({}, 409) : json(snapshot(reads === 1 ? 1 : 2));
    }));
    const controller = new AbortController();
    const watch = new PublicSiteApi('showcase').watch({ snapshot: value => { seen.push(value.revision); }, status() {} }, controller.signal);
    try {
      await flush(); expect(reads).toBe(3); expect(seen).toEqual([1, 2]);
      await vi.advanceTimersByTimeAsync(1000); expect(cursors).toEqual([`${generation}:1`, `${generation}:2`]);
    } finally { controller.abort(); await watch; }
    expect(vi.getTimerCount()).toBe(0);
  });

  it('discards a late canceled scope while another public project loads independently', async () => {
    let completeOld!: (value: Response) => void;
    const other = snapshot(); other.slug = 'other-project'; other.content.slug = other.slug; other.generation = `public_generation_${'c'.repeat(32)}`; other.cursor = `${other.generation}:1`;
    vi.stubGlobal('fetch', vi.fn((url: string) => url.endsWith('/showcase') ? new Promise<Response>(resolve => { completeOld = resolve; }) : Promise.resolve(json(other))));
    const controller = new AbortController(), applied: string[] = [];
    const old = new PublicSiteApi('showcase').watch({ snapshot: value => { applied.push(value.slug); }, status() {} }, controller.signal);
    controller.abort();
    expect((await new PublicSiteApi('other-project').snapshot(new AbortController().signal)).slug).toBe('other-project');
    completeOld(json(snapshot())); await old; expect(applied).toEqual([]);
  });
  it('refreshes only after public projection changes and cancels the stream', async () => {
    let output!: ReadableStreamDefaultController<Uint8Array>, streamSignal!: AbortSignal; let reads = 0; const urls: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit) => { urls.push(url); expect(init.credentials).toBe('omit'); if (!url.includes('/events?')) return json(snapshot(++reads)); streamSignal = init.signal!; return new Response(new ReadableStream<Uint8Array>({ start(controller) { output = controller; streamSignal.addEventListener('abort', () => controller.close(), { once: true }); } }), { headers: { 'content-type': 'text/event-stream' } }); }));
    const controller = new AbortController(), seen: number[] = [];
    const watch = new PublicSiteApi('showcase').watch({ snapshot: value => { seen.push(value.revision); }, status() {} }, controller.signal);
    await flush(); expect(reads).toBe(1); output.enqueue(new TextEncoder().encode(': heartbeat\n\n')); await flush(); expect(reads).toBe(1);
    output.enqueue(frame(2)); await flush(); expect(seen).toEqual([1, 2]);
    output.enqueue(frame(2)); await flush(); expect(reads).toBe(2);
    controller.abort(); await watch; expect(streamSignal.aborted).toBe(true);
    expect(urls.every(url => url.startsWith('/api/v1/public/projects/showcase'))).toBe(true);
  });
  it('rejects private event fields without fetching another snapshot', async () => {
    let reads = 0;
    vi.stubGlobal('fetch', vi.fn(async (url: string) => { if (!url.includes('/events?')) { reads++; return json(snapshot()); } return new Response(new ReadableStream({ start(controller) { controller.enqueue(frame(2, { actor_id: 'PRIVATE_ACTOR' })); } }), { headers: { 'content-type': 'text/event-stream' } }); }));
    await expect(new PublicSiteApi('showcase').watch({ snapshot() {}, status() {} }, new AbortController().signal)).rejects.toMatchObject({ code: 'invalid_response' }); expect(reads).toBe(1);
  });
  it('resnapshots after replay expiry and closes on unpublish', async () => {
    vi.useFakeTimers(); let streams = 0, reads = 0;
    vi.stubGlobal('fetch', vi.fn(async (url: string) => { if (!url.includes('/events?')) return json(snapshot(++reads)); streams++; return streams === 1 ? json({ error: { code: 'resync_required' } }, 409) : new Response('event: error\ndata: {"error":{"code":"not_found"}}\n\n', { headers: { 'content-type': 'text/event-stream' } }); }));
    const seen: number[] = []; const watch = new PublicSiteApi('showcase').watch({ snapshot: value => { seen.push(value.revision); }, status() {} }, new AbortController().signal).catch(error => error);
    await flush(); await vi.advanceTimersByTimeAsync(1000); const error = await watch;
    expect(error).toMatchObject({ code: 'not_found', status: 404 }); expect(seen).toEqual([1, 2]); expect(streams).toBe(2);
  });
});
