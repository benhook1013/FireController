import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ReviewRunsView } from './review-workspace';
import { PublicReviews } from './public-reviews';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewRunSummary } from './types';
import type { PublicRound } from './public-site-api';
const sizing = vi.hoisted(() => ({ capacity: 8, measured: true }));
const fixture = vi.hoisted(() => ({ items: [] as ReviewRunSummary[], hasMore: true, busy: false, error: undefined }));
vi.mock('./review-pill-capacity', () => ({ useReviewPillCapacity: () => ({ ref: { current: null }, capacity: sizing.capacity, measured: sizing.measured }) }));
vi.mock('./review-archive', async original => ({ ...await original<typeof import('./review-archive')>(), ReviewArchive: class { snapshot = () => fixture; subscribe = () => () => {}; } }));
afterEach(() => { fixture.items = []; sizing.measured = true; });
const stamp = '2026-11-01T00:00:00Z';
const rounds = Array.from({ length: 12 }, (_, i): ReviewRunSummary => ({ id:`run_${i}`,source:'fixture',source_run_id:`round_${i}`,pull_number:1,channel:'cli',head_sha:'a'.repeat(40),reviewer:'Reviewer',scope:'Comparison',coverage_limits:[],outcome:'completed',complete:true,attributable:true,finalized:true,found:i,accepted:0,routed:0,fixed:0,started_at:stamp,finished_at:stamp }));
describe('shared width-sized review previews', () => {
  it('keeps an unmeasured preview disabled instead of requesting the display fallback', () => {
    sizing.measured = false; sizing.capacity = 6;
    const client = new QueryClient();
    try {
      const session = {api:{reviewRuns:vi.fn()},controller:new AbortController()} as unknown as Session;
      renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key:['test'],home:'/projects/test',ready:true,fail:()=>{},selectWorker:()=>{}}}><ReviewRunsView number={1} snapshot="snapshot" head={'a'.repeat(40)} attempts={[]} expected={12} channelTotals={{cli:12,hosted:0,manual:0,subagent:0}} /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
      const query = client.getQueryCache().getAll().find(query=>query.queryKey.includes('cli'))!;
      expect((query.options as { enabled?: boolean }).enabled).toBe(false);
      expect(query.state.fetchStatus).toBe('idle');
      expect(session.api.reviewRuns).not.toHaveBeenCalled();
    } finally { client.clear(); }
  });

  for (const compact of [false, true]) for (const capacity of [8, 3, 6, 12]) it(`renders the newest ${capacity} rounds (compact=${compact}), preserving full-history access`, () => {
    sizing.capacity = capacity; fixture.items = [...rounds].reverse();
    const client = new QueryClient();
    try {
      const session = {api:{reviewRuns:vi.fn()},controller:new AbortController()} as unknown as Session;
      const html = renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key:['test'],home:'/projects/test',ready:true,fail:()=>{},selectWorker:()=>{}}}><ReviewRunsView compact={compact} number={1} snapshot="snapshot" head={'a'.repeat(40)} attempts={[]} expected={12} channelTotals={{cli:12,hosted:0,manual:0,subagent:0}} /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
      expect([...html.matchAll(/aria-label="Open cli run round_(\d+):/gu)].map(match=>Number(match[1]))).toEqual(rounds.slice(-capacity).map(run=>run.found));
      expect(html).toContain('12 runs');
      const query = client.getQueryCache().getAll().find(query=>query.queryKey.includes('cli'))!;
      expect(query.queryKey.at(-1)).toBe(capacity);
      expect((query.options as { enabled?: boolean }).enabled).toBe(true);
      const publicRounds: PublicRound[] = rounds.map(run=>({key:run.id,found:run.found,accepted:0,routed:0,complete:true,observed_at:stamp,duration_milliseconds:0}));
      const published = renderToStaticMarkup(<PublicReviews reviews={[{number:1,title:'Published PR',url:'https://github.com/example/project/pull/1',lifecycle:'open',hosted:[],cli:publicRounds}]} />);
      expect(published.match(/aria-label="Published CLI round:/gu)).toHaveLength(capacity);
      if (capacity < 12) expect(published).toContain('href="#published-history"');
    } finally { client.clear(); }
  });
});
