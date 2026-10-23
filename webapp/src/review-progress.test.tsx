import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { Api } from './api';
import { roundProgressState, roundProgressStopReason, validRoundProgress, type ReviewRoundProgress } from './review-progress';
import { ReviewProgressView } from './review-progress-view';

const project = { id: 'project_one', name: 'Progress', github_repository_id: 1, repository: 'owner/project' };
const current: ReviewRoundProgress = { channel: 'hosted', configured: true, revision: 3, epoch: 2, closed: false, minimum: 0, maximum: 5, taper: 1, used: 2, dry: 0, tapered: false, pending: 1 };
const absent: ReviewRoundProgress = { channel: 'cli', configured: false, revision: 0, epoch: 0, closed: false, minimum: 0, maximum: 0, taper: 0, used: 0, dry: 0, tapered: false, pending: 0 };
const result = { project_id: project.id, evaluated_at: '2026-10-19T00:00:00Z', items: [{ pull_number: 31, channels: [current, absent] }] };
const clients: Api[] = [];
function json(value: unknown) { return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } }); }
async function connected() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = new Api('private-progress-token', project.id); clients.push(api);
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => String(input).endsWith('/instance') ? json({ instance_id: 'instance_one' }) : json(project)));
  await api.connect(); return api;
}
afterEach(() => { clients.splice(0).forEach(api => api.close()); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('current round progress', () => {
  it('uses one bounded GET and binds results to the requested project, pulls and channel order', async () => {
    const api = await connected(); const fetch = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => json(result)); vi.stubGlobal('fetch', fetch);
    expect(await api.reviewProgress([31])).toEqual(result);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(String(fetch.mock.calls[0][0])).toContain('/reviews/rounds/progress?pull_numbers=31');
    expect(fetch.mock.calls[0][1]?.method ?? 'GET').toBe('GET');
  });
  it('refuses invalid selectors before any network request', async () => {
    const api = await connected(); const fetch = vi.fn(); vi.stubGlobal('fetch', fetch);
    for (const value of [[], [0], [-1], [1.5], [2147483648], [31,31], Array.from({length:101}, (_,i)=>i+1)]) await expect(api.reviewProgress(value)).rejects.toMatchObject({code:'invalid_request'});
    expect(fetch).not.toHaveBeenCalled();
  });
  it('refuses malformed or cross-scoped progress instead of displaying invented limits', async () => {
    const api = await connected();
    const badChannels = [{...current,used:6},{...current,pending:-1},{...current,pending:33},{...current,epoch:4},{...current,taper:0},{...current,configured:false},{...current,origin_key:'PRIVATE'}, {...absent,maximum:8}];
    const bad = [ {...result,project_id:'project_other'}, {...result,evaluated_at:'not-time'}, {...result,items:[]}, {...result,items:[{...result.items[0],pull_number:32}]}, {...result,items:[{pull_number:31,channels:[absent,current]}]}, ...badChannels.map(channel=>({...result,items:[{pull_number:31,channels:[channel,absent]}]})) ];
    for (const value of bad) { vi.stubGlobal('fetch', vi.fn(async()=>json(value))); await expect(api.reviewProgress([31])).rejects.toMatchObject({code:'invalid_response'}); }
  });
  it('retains the ordinary deadline and cancellation without a second request', async () => {
    const api = await connected(); vi.useFakeTimers();
    const fetch=vi.fn((_input: RequestInfo|URL, init: RequestInit)=>new Promise<Response>((_resolve,reject)=>{init.signal!.addEventListener('abort',()=>reject(new DOMException('aborted','AbortError')),{once:true});}));vi.stubGlobal('fetch',fetch);
    const pending=api.reviewProgress([31]).catch(error=>error);await vi.advanceTimersByTimeAsync(5000);expect(await pending).toMatchObject({code:'deadline_exceeded'});expect(fetch).toHaveBeenCalledTimes(1);
    const selected=new AbortController();const cancelled=api.reviewProgress([31],selected.signal).catch(error=>error);selected.abort();expect(await cancelled).toMatchObject({code:'cancelled'});expect(fetch).toHaveBeenCalledTimes(2);
  });
  it('keeps historical run counts separate and does not turn allowance into permission to dispatch', () => {
    const html=renderToStaticMarkup(<ReviewProgressView value={current}/>);
    expect(html).toContain('Up to 3 more reviews');expect(html).not.toContain('of 5 used');expect(html).toContain('1 in flight');expect(html).toContain('Taper 0/1');expect(html).not.toMatch(/Ready|Idle|run recorded|coverage/i);
    expect(renderToStaticMarkup(<ReviewProgressView value={absent}/>)).toBe('');
    expect(roundProgressState({...current,closed:true})).toBe('No reviews remaining');
    expect(roundProgressState({...current,tapered:true,dry:1})).toBe('No reviews remaining');
    expect(roundProgressState({...current,used:5})).toBe('No reviews remaining');
    const exact=renderToStaticMarkup(<ReviewProgressView value={{...current,minimum:5}}/>);expect(exact).toContain('At least 3 more');
  });
  it('renders a minimum-only rule without fabricating an exhausted maximum', async () => {
    const value: ReviewRoundProgress = {...current,maximum_unbounded:true,maximum:0,minimum:2001,used:2000,dry:0};
    expect(validRoundProgress(value,'hosted')).toBe(true);
    expect(roundProgressState(value)).toBe('More rounds allowed');
    const html=renderToStaticMarkup(<ReviewProgressView value={value}/>);
    expect(html).toContain('No maximum');expect(html).toContain('At least 1 more');expect(html).toContain('Taper 0/1');
    expect(html).not.toMatch(/remaining|of 0 used|Allowance used/u);
    const api=await connected();const response={...result,items:[{pull_number:31,channels:[value,absent]}]};
    vi.stubGlobal('fetch',vi.fn(async()=>json(response)));expect(await api.reviewProgress([31])).toEqual(response);
    expect(roundProgressState({...current,maximum:0,used:0,maximum_unbounded:false})).toBe('No reviews remaining');
  });
  it('does not present an adopted exhausted allowance as zero completed history', () => {
    const tapered = renderToStaticMarkup(<ReviewProgressView value={{...current, maximum:0, used:0, dry:1, taper:1, tapered:true, pending:0}}/>);
    expect(tapered).toContain('No reviews remaining'); expect(tapered).toContain('Taper met'); expect(tapered).not.toMatch(/of 0|Taper 1\/1/u);
    const exhausted = renderToStaticMarkup(<ReviewProgressView value={{...current, maximum:0, used:0, pending:0}}/>);
    expect(exhausted).toContain('No reviews remaining'); expect(exhausted).not.toMatch(/of 0|Taper 0\/1/u);
  });
  it('keeps an added minimum visible when the retained clean streak already meets its target', () => {
    for (const dry of [1, 2]) {
      const value: ReviewRoundProgress = { ...current, minimum: 1, used: 0, dry, taper: 1, tapered: false, pending: 0 };
      expect(validRoundProgress(value, 'hosted')).toBe(true);
      const html = renderToStaticMarkup(<ReviewProgressView value={value} />);
      expect(html).toContain('Up to 5 more reviews');
      expect(html).toContain('At least 1 more');
      expect(html).toContain('Clean streak target met');
      expect(html).toContain(`title="Consecutive eligible rounds with no accepted findings: ${dry}; target 1.`);
      expect(html).not.toMatch(/Taper [12]\/1|No reviews remaining|Taper met/u);
    }
    const pendingStreak = renderToStaticMarkup(<ReviewProgressView value={{ ...current, minimum: 1, used: 0, dry: 1, taper: 2, pending: 0 }} />);
    expect(pendingStreak).toContain('Taper 1/2');
    expect(pendingStreak).toContain('At least 1 more');
    expect(pendingStreak).not.toContain('Clean streak target met');
  });
  it('requires an explicit boolean and zero placeholder and rejects unsafe counters', () => {
    const value={...current,maximum_unbounded:true,maximum:0,minimum:Number.MAX_SAFE_INTEGER,used:Number.MAX_SAFE_INTEGER,dry:0};
    expect(validRoundProgress(value,'hosted')).toBe(true);
    for(const bad of [{...value,maximum:1},{...value,maximum_unbounded:false},{...value,maximum_unbounded:null},{...value,maximum_unbounded:1},{...value,used:Number.MAX_SAFE_INTEGER+1},{...value,dry:Number.MAX_SAFE_INTEGER+1},{...absent,maximum_unbounded:true}]) expect(validRoundProgress(bad,bad.channel)).toBe(false);
  });

  it('accepts cumulative finite ceilings while retaining safe bounds and comparison guards', async () => {
    const value: ReviewRoundProgress = {...current,minimum:1001,maximum:2001,used:1000,dry:2,pending:0};
    expect(validRoundProgress(value,'hosted')).toBe(true);
    const api=await connected();const response={...result,items:[{pull_number:31,channels:[value,absent]}]};
    vi.stubGlobal('fetch',vi.fn(async()=>json(response)));expect(await api.reviewProgress([31])).toEqual(response);
    const edge: ReviewRoundProgress={...value,minimum:Number.MAX_SAFE_INTEGER-1,maximum:Number.MAX_SAFE_INTEGER,used:Number.MAX_SAFE_INTEGER-1,dry:0};
    expect(validRoundProgress(edge,'hosted')).toBe(true);
    expect(renderToStaticMarkup(<ReviewProgressView value={edge}/>)).toContain('Up to 1 more review');
    for(const bad of [{...value,maximum:Number.MAX_SAFE_INTEGER+1},{...value,minimum:value.maximum+1},{...value,used:value.maximum+1},{...value,pending:33},{...value,epoch:value.revision+1}]) expect(validRoundProgress(bad,'hosted')).toBe(false);
  });

  it('renders accepted findings resetting a retained streak without resetting cumulative use', () => {
    const value: ReviewRoundProgress={...current,channel:'cli',minimum:1001,maximum:1002,used:1001,dry:0,taper:3,tapered:false,pending:0};
    expect(validRoundProgress(value,'cli')).toBe(true);
    expect(roundProgressState(value)).toBe('More rounds allowed');
    const html=renderToStaticMarkup(<ReviewProgressView value={value}/>);
    expect(html).toContain('Up to 1 more review');expect(html).toContain('Taper 0/3');
    expect(html).not.toMatch(/Taper met|Clean streak target met|No reviews remaining/u);
  });

  it('uses one stopped label with a distinct accessible reason for each channel stop', () => {
    const cases: [ReviewRoundProgress, string][] = [
      [{ ...current, closed: true }, 'Manually closed'],
      [{ ...current, tapered: true, dry: 1 }, 'Taper met'],
      [{ ...current, used: 5 }, 'Maximum reached'],
      [{ ...current, closed: true, tapered: true, dry: 1, used: 5 }, 'Manually closed'],
    ];
    for (const [value, reason] of cases) {
      const html = renderToStaticMarkup(<ReviewProgressView value={value} />);
      expect(roundProgressState(value)).toBe('No reviews remaining');
      expect(roundProgressStopReason(value)).toBe(reason);
      expect(html).toContain('<details><summary class="review-progress-state">No reviews remaining</summary><span>' + reason + '</span></details>');
      expect(html.match(/No reviews remaining/gu)).toHaveLength(1);
      expect(html).not.toMatch(/Up to|At least|Taper 0\/1|Review closed/);
    }
    expect(roundProgressStopReason(current)).toBeUndefined();
    expect(roundProgressStopReason({ ...current, maximum_unbounded: true, maximum: 0 })).toBeUndefined();
  });

});
