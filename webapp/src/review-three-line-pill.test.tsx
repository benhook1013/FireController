import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ReviewRunsView, reviewRunPillStatus } from './review-workspace';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewAttemptSummary, ReviewRunSummary } from './types';
import type { ReviewProgressItem, ReviewRoundProgress } from './review-progress';

const archive = vi.hoisted(() => ({ items: [] as ReviewRunSummary[], hasMore: false, busy: false, error: undefined }));
vi.mock('./review-archive', async original => ({ ...await original<typeof import('./review-archive')>(), ReviewArchive: class { snapshot = () => archive; subscribe = () => () => {}; } }));
const head='a'.repeat(40), stamp='2026-11-15T05:34:45Z';
const completed: ReviewRunSummary={id:'run',source:'native-executor',source_run_id:'evidence',pull_number:78,channel:'cli',head_sha:head,reviewer:'coderabbit',scope:'admitted comparison',coverage_limits:[],outcome:'completed',complete:true,attributable:true,finalized:true,found:2,accepted:1,routed:0,fixed:1,started_at:stamp,finished_at:'2026-11-15T05:42:08Z'};
const attempt: ReviewAttemptSummary={source_attempt_id:`review_attempt_${'a'.repeat(32)}`,channel:'cli',head_sha:head,state:'active',started_at:stamp,finished_at:'',duration_seconds:0,source_run_id:'evidence',trigger_id:'',provider_review_id:''};
const progress:ReviewRoundProgress={channel:'cli',configured:true,revision:1,epoch:1,closed:false,minimum:0,maximum:7,taper:2,used:1,dry:0,tapered:false,pending:0,pending_execution:0,pending_adjudication:0};
afterEach(()=>{archive.items=[];});
function markup(runs:ReviewRunSummary[], attempts:ReviewAttemptSummary[]=[], channelProgress?:ReviewRoundProgress){
 archive.items=runs;
 const client=new QueryClient(); const session={api:{reviewRuns:vi.fn()},controller:new AbortController()} as unknown as Session;
 try{return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key:['fixture'],home:'/projects/project',ready:true,fail:()=>{},selectWorker:()=>{}}}><ReviewRunsView number={78} snapshot="snapshot" head={head} attempts={attempts} recentRuns={runs} expected={runs.length} channelTotals={{cli:runs.filter(x=>x.complete).length,hosted:0,manual:0,subagent:0}} progress={channelProgress?{pull_number:78,channels:[channelProgress]} as ReviewProgressItem:undefined}/></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);}finally{client.clear();}
}
describe('three-line review state pills',()=>{
 it('shows counts, age, then each captured result status',()=>{
  const html=markup([completed]);
  expect(html).toMatch(/<strong>2\/1\/0<\/strong><time[^>]*>[\s\S]*?<\/time><span class="review-pill-status">Finished<\/span>/u);
  expect(reviewRunPillStatus({...completed,fixed:0})).toBe('Fixes pending');
  expect(reviewRunPillStatus({...completed,finalized:false})).toBe('Needs adjudication');
  expect(reviewRunPillStatus({...completed,attributable:false})).toBe('Incomplete');
 });
 it('keeps real partial-case semantics without completed credit',()=>{
  const partial={...completed,outcome:'failed' as const,complete:false,attributable:false,finalized:false,found:15,accepted:0,fixed:0,retained_undecided:15};
  const html=markup([partial],[{...attempt,state:'failed',finished_at:partial.finished_at,reason:'coverage_incomplete'}],{...progress,retained_adjudication_pending:true});
  expect(html).toContain('<strong>15/0/0</strong>');expect(html).toContain('Partial · Needs adjudication');expect(html).toContain('Needs adjudication');expect(html).toContain('No completed round');
  expect(html).not.toContain('>Finished</span>');expect(html).not.toContain('review-pill-zero');
 });
 it('never lets a stale completion replace an active attempt with matching source identity',()=>{
  const html=markup([completed],[attempt],{...progress,closed:true});
  expect(html).toContain('aria-label="cli execution: Reviewing"');
  expect(html).toContain('aria-label="cli current review state"');
  expect(html).not.toContain('>Closed</span>');
 });
 it('puts waiting and uncertain states inside independent unknown-count pills',()=>{
  const wait=markup([],[],{...progress,wait:{paused:true}});
  expect(wait).toMatch(/review-pill-status[\s\S]*?Paused/u);expect(wait).toContain('<strong>?/?/?</strong>');
  const uncertain=markup([completed],[{...attempt,state:'uncertain'}],progress);
  expect(uncertain).toMatch(/review-pill-status[\s\S]*?Needs agent attention/u);
  expect(uncertain).not.toContain('<h4>CLI CodeRabbit<span');
 });
});

describe('aggregate channel holds do not relabel a clean result',()=>{
 it.each([{pending_adjudication:1,pending:1},{fixes_pending:true}])('keeps latest clean result finished while older work stays outstanding: %j', gate=>{
  const older={...completed,id:'older',source_run_id:'older',fixed:0};
  const html=markup([older,completed],[{...attempt,state:'completed',finished_at:completed.finished_at}],{...progress,...gate});
  const latest=html.split('aria-label="Open cli run evidence:')[1].split('</a>')[0];
  expect(latest).toContain('review-pill-status">Finished');
  expect(latest).not.toContain('review-pill-status">Fixes pending');
  expect(latest).not.toContain('review-pill-status">Needs adjudication');
  expect(html).toContain('aria-label="cli current review state"');
  expect(html).toContain('review-pill-status">Fixes pending');
 });
});
