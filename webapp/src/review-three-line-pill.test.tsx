import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ReviewRunsView, reviewRunPillStatus } from './review-workspace';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewAttemptSummary, ReviewRunSummary } from './types';
import type { HostedLaneHold, ReviewProgressItem, ReviewRoundProgress } from './review-progress';

const sizing = vi.hoisted(() => ({ capacity: 6 }));
vi.mock('./review-pill-capacity', () => ({ useReviewPillCapacity: () => ({ ref: { current: null }, capacity: sizing.capacity, measured: true }) }));
const archive = vi.hoisted(() => ({ items: [] as ReviewRunSummary[], hasMore: false, busy: false, error: undefined }));
vi.mock('./review-archive', async original => ({ ...await original<typeof import('./review-archive')>(), ReviewArchive: class { snapshot = () => archive; subscribe = () => () => {}; } }));
const head='a'.repeat(40), stamp='2026-11-15T05:34:45Z';
const completed: ReviewRunSummary={id:'run',source:'native-executor',source_run_id:'evidence',pull_number:78,channel:'cli',head_sha:head,reviewer:'coderabbit',scope:'admitted comparison',coverage_limits:[],outcome:'completed',complete:true,attributable:true,finalized:true,current_accepted:1,current_pending_fixes:0,found:2,accepted:1,routed:0,fixed:1,started_at:stamp,finished_at:'2026-11-15T05:42:08Z'};
const attempt: ReviewAttemptSummary={source_attempt_id:`review_attempt_${'a'.repeat(32)}`,channel:'cli',head_sha:head,state:'active',started_at:stamp,finished_at:'',duration_seconds:0,source_run_id:'evidence',trigger_id:'',provider_review_id:''};
const progress:ReviewRoundProgress={channel:'cli',configured:true,revision:1,epoch:1,closed:false,minimum:0,maximum:7,taper:2,used:1,dry:0,tapered:false,pending:0,pending_execution:0,pending_adjudication:0};
afterEach(()=>{archive.items=[];sizing.capacity=6;});
function markup(runs:ReviewRunSummary[], attempts:ReviewAttemptSummary[]=[], channelProgress?:ReviewRoundProgress, position?:{text:string;description?:string}, hostedLaneHold?:HostedLaneHold){
 archive.items=runs;
 const client=new QueryClient(); const session={api:{reviewRuns:vi.fn()},controller:new AbortController()} as unknown as Session;
 try{return renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key:['fixture'],home:'/projects/project',ready:true,fail:()=>{},selectWorker:()=>{}}}><ReviewRunsView positions={position?{cli:position,hosted:position}:undefined} number={78} snapshot="snapshot" head={head} attempts={attempts} recentRuns={runs} expected={runs.length} channelTotals={{cli:runs.filter(x=>x.complete && x.channel==='cli').length,hosted:runs.filter(x=>x.complete && x.channel==='hosted').length,manual:0,subagent:0}} progress={channelProgress?{pull_number:78,channels:[channelProgress],hostedLaneHold} as ReviewProgressItem:undefined}/></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);}finally{client.clear();}
}
describe('three-line review state pills',()=>{
 it('shows counts, each captured result status, then age',()=>{
  const html=markup([completed]);
  expect(html).toMatch(/<strong>2\/1\/0<\/strong><span class="review-pill-status">Finished<\/span><time[^>]*>[\s\S]*?<\/time>/u);
  expect(reviewRunPillStatus({...completed,fixed:0,current_pending_fixes:1})).toBe('Fixes pending');
  expect(reviewRunPillStatus({...completed,finalized:false})).toBe('Needs adjudication');
  expect(reviewRunPillStatus({...completed,attributable:false})).toBe('Incomplete');
 });
 it('keeps confirmed running counts, elapsed time and status as distinct lines',()=>{
  const evidence={attempt_id:attempt.source_attempt_id,head_sha:head,observed_at:stamp,kind:'cli_process_spawned' as const,process_started_at:stamp};
  const html=markup([],[{...attempt,source_run_id:''}],{...progress,activity_evidence:evidence});
  expect(html).toMatch(/<strong>\?\/\?\/\?<\/strong><span class="review-pill-status">Running<\/span><time[^>]*>[\s\S]*?<\/time>/u);
  expect(html).not.toContain('?/?/?Running');
 });
 it('keeps real partial-case semantics without completed credit',()=>{
  const partial={...completed,outcome:'failed' as const,complete:false,attributable:false,finalized:false,found:15,accepted:0,fixed:0,retained_undecided:15};
  const html=markup([partial],[{...attempt,state:'failed',finished_at:partial.finished_at,reason:'coverage_incomplete'}],{...progress,retained_adjudication_pending:true});
  expect(html).toContain('<strong>15/?/?</strong>');expect(html).toContain('>Adjudicating</span>');expect(html).not.toContain('aria-label="cli current review state"');expect(html).toContain('No completed round');
  expect(html).not.toContain('>Finished</span>');expect(html).not.toContain('review-pill-zero');
  expect(markup([{...partial,finalized:true,retained_undecided:0,accepted:3,routed:2}])).toContain('<strong>15/3/2</strong>');
 });
 it('separates an unsettled returned partial result from failed-attempt attention',()=>{
  const partial={...completed,found:3,accepted:1,routed:0,fixed:0,retained_undecided:1,outcome:'failed' as const,complete:false,attributable:false,finalized:false};
  const html=markup([partial],[{...attempt,state:'failed',finished_at:partial.finished_at,reason:'capture_retained'}]);
  expect(html).toContain('>Adjudicating</span>');
  expect(html).toContain('aria-label="cli current review state"');
  expect(html).toContain('Check attempt');
  expect(html).toContain('0 runs');
  expect(html).not.toContain('review-pill-running');
  const compact=markup([{...partial,retained_undecided:undefined}],[{...attempt,state:'failed',finished_at:partial.finished_at,reason:'capture_retained'}]);
  expect(compact).toContain('review-pill-status">Status unknown</span>');
  expect(compact).not.toContain('Adjudication unknown');
  expect(compact).not.toContain('Partial · Needs adjudication');
 });
 it('never lets a stale completion replace an active attempt with matching source identity',()=>{
  const html=markup([completed],[attempt],{...progress,closed:true});
  expect(html).toContain('aria-label="cli execution: Review active"');
  expect(html).toContain('aria-label="cli current review state"');
  expect(html).not.toContain('>Closed</span>');
 });
 it('keeps waiting and uncertain states as lane text without invented review pills',()=>{
  const wait=markup([],[],{...progress,wait:{paused:true}});
  expect(wait).toMatch(/review-lane-current-state[\s\S]*?Paused/u);expect(wait).not.toContain('<strong>?/?/?</strong>');
  expect(wait).toContain('review-lane-current-state');
  const uncertain=markup([completed],[{...attempt,state:'uncertain'}],progress);
  expect(uncertain).toMatch(/review-lane-current-state[\s\S]*?Check attempt/u);
  expect(uncertain).toContain('review-lane-current-state');
 });
});

describe('aggregate channel holds do not relabel a clean result',()=>{
 it.each([{pending_adjudication:1,pending:1},{fixes_pending:true}])('keeps latest clean result finished while older work stays outstanding: %j', gate=>{
  const older={...completed,id:'older',source_run_id:'older',fixed:0,current_pending_fixes:1};
  const html=markup([older,completed],[{...attempt,state:'completed',finished_at:completed.finished_at}],{...progress,...gate});
  const latest=html.split('aria-label="Open cli run evidence:')[1].split('</a>')[0];
  expect(latest).toContain('review-pill-status">Finished');
  expect(latest).not.toContain('review-pill-status">Fixes pending');
  expect(latest).not.toContain('review-pill-status">Needs adjudication');
  expect(html).not.toContain('aria-label="cli current review state"');
  expect(html.match(/review-pill-status">Fixes pending/gu)).toHaveLength(1);
 });
});


describe('adjudication indicator age', () => {
 it.each(['cli', 'hosted'] as const)('uses the exact returned %s review start, not preparation or finish', channel => {
  const run = {...completed, channel, finalized:false, display_start_at:'2026-11-15T05:35:00Z'};
  const html=markup([run],[{...attempt,channel,state:'completed',finished_at:run.finished_at}],{...progress,channel,pending:1,pending_adjudication:1});
  expect(html).not.toContain(`aria-label="${channel} current review state"`);
  const indicator=html.split(`aria-label="Open ${channel} run evidence:`)[1].split('</a>')[0];
  expect(indicator).toContain('<span class="review-pill-status">Adjudicating</span>');
  expect(indicator).toContain('dateTime="2026-11-15T05:35:00Z"');
  expect(indicator).not.toContain(`dateTime="${stamp}"`);
  expect(indicator).not.toContain(`dateTime="${run.finished_at}"`);
 });
 it('keeps the retained partial start on its real review, not the lane indicator', () => {
  const run={...completed,outcome:'failed' as const,complete:false,attributable:false,finalized:false,retained_undecided:1};
  const html=markup([run],[{...attempt,state:'failed',finished_at:run.finished_at}],{...progress,retained_adjudication_pending:true});
  expect(html).not.toContain('aria-label="cli current review state"');
  expect(html.split('aria-label="Open cli run evidence:')[1]).toContain(`dateTime="${stamp}"`);
 });
 it.each(['different-run', 'different-head', 'settled', 'active'])('does not invent an adjudication start for %s', scenario => {
  const run={...completed,finalized:scenario==='settled'};
  const a: ReviewAttemptSummary={...attempt,state:scenario==='active'?'active':'completed',finished_at:scenario==='active'?'':run.finished_at,source_run_id:scenario==='different-run'?'unrelated':run.source_run_id,head_sha:scenario==='different-head'?'b'.repeat(40):head};
  const html=markup([run],[a],{...progress,pending:1,pending_adjudication:1});
  const indicator=html.split('aria-label="cli current review state"')[1]?.split('</h4>')[0];
  if(scenario==='active') expect(indicator).not.toContain('Needs adjudication');
  else expect(indicator).toBeUndefined();
 });
});


describe('complete persisted current-state mapping in both review lanes',()=>{
 for (const channel of ['cli','hosted'] as const) {
  for (const withCompleted of [false,true]) {
   const cases: [string, Partial<ReviewRoundProgress>][] = [
    ['Queued',{pending:1,pending_execution:1}],
    ['Needs adjudication',{pending:1,pending_adjudication:1}],
    ['Fixes pending',{fixes_pending:true}],
    ['Closed',{closed:true}],
    ['Cap reached',{used:7}],
    ['Closed',{tapered:true}],
   ];
   it.each(cases)(`${channel} current %s uses the correct lane presentation with completed=${withCompleted}`, (label, changes)=>{
    const runs=withCompleted?[{...completed,channel}]:[];
    const attempts=withCompleted?[{...attempt,channel,state:'completed' as const,finished_at:completed.finished_at}]:[];
    const html=markup(runs,attempts,{...progress,channel,...changes});
    const current=html.split(`aria-label="${channel} current review state"`)[1]?.split('</span></div>')[0];
    if (label === 'Closed' || label === 'Cap reached') {
     expect(current).toBeUndefined();
     expect(html).toContain('No reviews remaining');
    } else if (label === 'Needs adjudication' || label === 'Fixes pending') {
     expect(current).toBeUndefined();
     if (label === 'Needs adjudication') expect(html).toContain('1 awaiting adjudication');
    } else {
     expect(current).toBeDefined();
     expect(html).toContain('review-lane-current-state');
     expect(current).not.toContain('<strong>?/?/?</strong>');
     expect(current).toContain(label);
    }
    if(withCompleted) expect(html).toContain('review-pill-status">Finished');
   });
  }
  it(`${channel} retains real execution ahead of stale completed/capped state`,()=>{
   const active={...attempt,channel,trigger_id:channel==='hosted'?'42':''};
   const base={attempt_id:active.source_attempt_id,head_sha:head,observed_at:stamp};
   const activity_evidence=channel==='cli'?{...base,kind:'cli_process_spawned' as const,process_started_at:stamp}:{...base,kind:'hosted_processing_observed' as const,trigger_id:'42',comment_updated_at:stamp,source_comment_id:42,provider_run_id:'11111111-2222-4333-8444-555555555555'};
   const html=markup([{...completed,channel}],[active],{...progress,channel,closed:true,used:7,activity_evidence});
   expect(html).toContain('review-pill-running');
   expect(html).toContain('review-pill-status">Running');
   expect(html).not.toContain(`aria-label="${channel} current review state"`);
  });
  it(`${channel} shows its retained provider wait despite older completed results`,()=>{
   const wait={paused:false,baseline_attempt_id:attempt.source_attempt_id,retry_not_before:'2099-01-01T00:00:00Z'};
   const html=markup([{...completed,channel}],[{...attempt,channel,state:'completed',finished_at:completed.finished_at}],{...progress,channel,wait});
   expect(html).toContain(`aria-label="${channel} current review state"`);
   expect(html).toContain(channel==='cli'?'Quota wait':'Check in ');
   expect(html).toContain('review-pill-status">Finished');
  });
 }
});


it.each(['cli','hosted'] as const)('keeps persisted results and dependency holds inside %s pills, not PR facts',channel=>{
 for (const label of ['Results pending','Dependency hold']) {
  for (const runs of [[],[{...completed,channel}]]) {
   const html=markup(runs,[],{...progress,channel},{text:label});
   const lane=html.split(`aria-label="${channel} current review state"`)[1];
   expect(html).toContain('review-lane-current-state');
   expect(lane).not.toContain('<strong>?/?/?</strong>');
   expect(lane).toContain(label);
   expect(html).not.toContain('review-pull-facts');
  }
 }
});


describe('terminal channel state stays in lane progress', () => {
 it.each([{closed:true},{used:7},{tapered:true,dry:2}])('does not add a redundant terminal pill: %j', terminal => {
  const html=markup([completed],[],{...progress,...terminal});
  expect(html).toContain('No reviews remaining');
  expect(html).not.toContain('aria-label="cli current review state"');
  expect(html).not.toMatch(/>Closed<|>Cap reached</u);
  expect(html).toMatch(/review-pill-status">Finished<\/span><time/u);
 });
 it.each(['hosted', 'cli'] as const)('hides the old %s countdown after closure', channel => {
  const wait = { paused: false, baseline_attempt_id: attempt.source_attempt_id, retry_not_before: '2099-01-01T00:00:00Z' };
  const html = markup([{...completed, channel}], [], {...progress, channel, closed:true, wait, pending:1, pending_execution:1});
  expect(html).toContain('No reviews remaining');
  expect(html).toContain('review-pill-status">Finished');
  expect(html).not.toContain(`aria-label="${channel} current review state"`);
  expect(html).not.toMatch(/Check in |Quota wait/u);
 });
 it('preserves actionable pending adjudication despite closed rounds', () => {
  const html=markup([completed],[],{...progress,closed:true,pending:1,pending_adjudication:1});
  expect(html).not.toContain('aria-label="cli current review state"');
  expect(html).toContain('1 awaiting adjudication');
 });
});

it('keeps additional pending adjudications in the round summary', () => {
 const run={...completed,finalized:false};
 const html=markup([run],[{...attempt,state:'completed',finished_at:run.finished_at}],{...progress,pending:2,pending_adjudication:2});
 expect(html).not.toContain('aria-label="cli current review state"');
 expect(html).toContain('<span class="review-pill-status">Adjudicating</span>');
 expect(html).toContain('2 awaiting adjudication');
});


describe('lane state never displaces actual review history', () => {
 it.each([1, 2, 4, 6, 8, 12])('preserves all %i measured slots for real captures', capacity => {
  sizing.capacity=capacity;
  const runs=Array.from({length:12}, (_,i)=>({...completed,id:`run-${i}`,source_run_id:`source-${i}`}));
  for (const state of [{wait:{paused:true}}, {closed:true}, {used:7}, {pending:1,pending_adjudication:1}]) {
   const html=markup(runs,[],{...progress,...state});
   expect([...html.matchAll(/aria-label="Open cli run /gu)]).toHaveLength(capacity);
   expect(html).not.toContain('review-pill-current');
   expect(html).not.toContain('<strong>?/?/?</strong>');
  }
 });
});


describe('honest review-start labels',()=>{
 it.each([undefined,'2026-11-15T05:35:00Z'])('marks existing Hosted start proxy as assumed without changing its timestamp',display_start_at=>{
  const run={...completed,channel:'hosted' as const,display_start_at};
  const html=markup([run],[],{...progress,channel:'hosted'});
  const label=display_start_at?'Assumed start (CodeRabbit activity seen)':'Assumed start (review requested)';
  expect(html).toContain(label);
  expect(html).toContain(`dateTime="${display_start_at||stamp}" title="${label}: `);
  expect(html).not.toContain(`dateTime="${run.finished_at}"`);
 });
 it('does not label proven CLI process start as assumed',()=>{
  const html=markup([{...completed,display_start_at:'2026-11-15T05:35:00Z'}]);
  expect(html).toContain('title="Started: ');
  expect(html).not.toContain('Assumed start');
 });
 it('uses publication age when the returned Hosted findings have no start evidence',()=>{
  const run={...completed,source:'github-hosted-observer',channel:'hosted' as const,started_at:completed.finished_at,finalized:false};
  const html=markup([run]);
  expect(html).toContain('>Adjudicating</span>');
  expect(html).toContain('Start time unknown');
  expect(html).toContain(`dateTime="${run.finished_at}" title="Published (start unknown): `);
  expect(html).not.toContain('Assumed start');
  const requested = '2026-11-15T05:34:00Z';
  const matched = markup([{...run,display_request_at:requested}]);
  expect(matched).toContain(`dateTime="${requested}" title="Assumed start (review requested): `);
  expect(matched).not.toContain(`dateTime="${run.finished_at}"`);
  const observed = '2026-11-15T05:35:00Z';
  const started = markup([{...run,display_request_at:requested,display_start_at:observed}]);
  expect(started).toContain(`dateTime="${observed}" title="Assumed start (CodeRabbit activity seen): `);
 });
 it('uses an explicitly labeled finish fallback and shows a dash if both times are missing',()=>{
  const run={...completed,started_at:''};
  const finished = markup([run]);
  expect(finished).toContain(`dateTime="${run.finished_at}" title="Finished (start unknown): `);
  const undated = markup([{...run,finished_at:''}]);
  expect(undated).toContain('Start time unknown');
  expect(undated).not.toContain('<time');
 });
});


describe('current finding obligations remain distinct from frozen counts', () => {
 it.each(['native-executor', 'github-hosted-observer', 'worker-subagent', 'imported'])("uses proven current dispositions for %s", source => {
  const corrected = {...completed, source, found:3, accepted:3, fixed:2, current_accepted:2, current_pending_fixes:0};
  const html = markup([corrected]);
  expect(html).toContain('<strong>3/3/0</strong>');
  expect(html).toContain('review-pill-status">Finished</span>');
  expect(html).toContain('2 currently accepted; 0 fixes pending.');
  if (source !== 'github-hosted-observer') expect(html).toContain(`dateTime="${stamp}"`);
  else expect(html).toContain('Start time unknown');
  expect(reviewRunPillStatus({...corrected, current_accepted:3, current_pending_fixes:1})).toBe('Fixes pending');
 });
 it('never derives completion or pending from frozen subtraction when current inventory is absent', () => {
  for (const fixed of [0, 1, 2]) {
   const unknown = {...completed, fixed, current_accepted:undefined, current_pending_fixes:undefined};
   expect(reviewRunPillStatus(unknown)).toBe('Reviewed');
   expect(markup([unknown])).toContain('Current fix inventory unavailable.');
   expect(markup([unknown])).not.toContain('Fix status unknown');
  }
 });
 it('keeps partial retained adjudication and fix obligations without completed credit', () => {
  const partial = {...completed, outcome:'failed' as const, complete:false, attributable:false, finalized:false, retained_undecided:0, accepted:3, current_accepted:2, current_pending_fixes:0};
  expect(reviewRunPillStatus(partial)).toBe('Finished');
  expect(reviewRunPillStatus({...partial,current_pending_fixes:1})).toBe('Fixes pending');
  expect(reviewRunPillStatus({...partial,retained_undecided:1})).toBe('Needs adjudication');
  expect(reviewRunPillStatus({...partial,current_pending_fixes:undefined})).toBe('Reviewed');
  const fixes = markup([{...partial,found:17,accepted:13,current_pending_fixes:13}], [], {...progress,fixes_pending:true});
  expect(fixes).not.toContain('aria-label="cli current review state"');
  expect(fixes).toContain('review-pill-partial');
  expect(fixes).toContain('<strong>17/13/0</strong><span class="review-pill-status">Fixes pending</span>');
 });
});


it('does not celebrate frozen zero acceptance after a current acceptance becomes pending', () => {
 const zero = {...completed, accepted:0, fixed:0, current_accepted:0, current_pending_fixes:0};
 expect(markup([zero])).toContain('review-pill-zero');
 const pending = {...zero, current_accepted:1, current_pending_fixes:1};
 expect(markup([pending])).toContain('review-pill-status">Fixes pending');
 expect(markup([pending])).not.toContain('review-pill-zero');
 expect(markup([{...zero,current_accepted:undefined,current_pending_fixes:undefined}])).not.toContain('review-pill-zero');
});

it('preserves a proven uncertain review as one partial pill with its original start and tap details',()=>{
 const start='2026-11-15T05:35:10Z';
 const signal={attempt_id:attempt.source_attempt_id,head_sha:head,observed_at:start,kind:'cli_process_spawned' as const,process_started_at:start};
 const uncertain={...attempt,state:'uncertain' as const,source_run_id:'',reason:'capture_retention_failed'};
 const html=markup([],[uncertain],{...progress,closed:true,activity_evidence:signal});
 expect(html).toMatch(/<strong>\?\/\?\/\?<\/strong><span class="review-pill-status">Needs attention<\/span><time[^>]*dateTime="2026-11-15T05:35:10Z"/u);
 expect(html).toContain('review-pill-partial');
 expect(html).toContain('popoverTarget='); expect(html).toContain('popover="auto"');
 expect(html).toContain('The review result could not be retained');
 expect(html).not.toContain('aria-label="cli current review state"');
 expect(html).not.toContain('review-pill-running'); expect(html).toContain('0 runs');
 expect(html).not.toContain(`dateTime="${stamp}"`);
 for(const wrong of [{...signal,attempt_id:'review_attempt_other'}, {...signal,head_sha:'f'.repeat(40)}]){
  expect(markup([],[uncertain],{...progress,activity_evidence:wrong})).not.toContain('review-pill-partial');
 }
 expect(markup([],[uncertain],progress)).not.toContain('review-pill-partial');
});

it('keeps hosted uncertain evidence matched to its trigger and replaces it with its exact result',()=>{
 const signal={attempt_id:attempt.source_attempt_id,head_sha:head,observed_at:stamp,kind:'hosted_processing_observed' as const,comment_updated_at:stamp,source_comment_id:12,provider_run_id:'11111111-2222-4333-8444-555555555555',trigger_id:'42'};
 const uncertain={...attempt,channel:'hosted' as const,state:'uncertain' as const,source_run_id:'',trigger_id:'42',reason:'hosted_finished_inventory_unproven'};
 const current={...progress,channel:'hosted' as const,activity_evidence:signal};
 const html=markup([],[uncertain],current);
 expect(html).toContain('review-pill-partial'); expect(html).toContain('Import incomplete');
 expect(html).not.toContain('aria-label="hosted current review state"');
 const mismatched=markup([],[{...uncertain,trigger_id:'43'}],current);
 expect(mismatched).toContain('review-pill-partial');
 expect(mismatched).toContain('Start time unknown');
 expect(mismatched).not.toContain('<time');
 const returned=markup([{...completed,channel:'hosted'}],[{...uncertain,source_run_id:completed.source_run_id}],current);
 expect(returned).not.toContain('review-pill-partial'); expect(returned).toContain('<strong>2/1/0</strong>');
});

it('shows an older Hosted capacity holder and identifies it from another waiting row', () => {
 const holder: ReviewAttemptSummary = {...attempt,channel:'hosted',state:'uncertain',head_sha:'b'.repeat(40),
   source_run_id:'',trigger_id:'42',reason:'hosted_finished_inventory_unproven'};
 const current = {...progress,channel:'hosted' as const,used:0};
 const newer: ReviewAttemptSummary = {...holder,source_attempt_id:`review_attempt_${'c'.repeat(32)}`,state:'completed',head_sha:head,
   reason:undefined,finished_at:'2026-11-15T06:00:00Z'};
 const blocked = markup([], [newer], current, undefined, {pull_number:78,attempt:holder});
 expect(blocked.match(/review-pill-status">Import incomplete/g)).toHaveLength(1);
 expect(blocked).toContain('CodeRabbit finished. FireController could not verify all findings.');
 expect(blocked).toContain('This attempt still holds the Hosted lane.');
 expect(blocked).toContain('Review head bbbbbbbb is older');
 expect(blocked).toContain('0 runs');
 expect(blocked).not.toContain('>Running</span>');
 const waiting = markup([], [], current, undefined, {pull_number:87,attempt:holder});
 expect(waiting).toContain('Waiting on #87');
 expect(waiting).toContain('CodeRabbit finished #87, but its results could not be verified.');
 expect(waiting).not.toContain('review-pill-status">Import incomplete');
 expect(markup([], [], {...current,closed:true}, undefined, {pull_number:87,attempt:holder})).not.toContain('Waiting on #87');
 expect(markup([], [newer], current)).not.toContain('holds the Hosted lane');
});

it('shows proven finished Hosted capture without inventing a start or treating trigger-only attempts as reviews',()=>{
 const proven={...attempt,channel:'hosted' as const,state:'uncertain' as const,source_run_id:'',trigger_id:'42',reason:'hosted_finished_inventory_unproven'};
 const html=markup([],[proven]);
 expect(html).toContain('review-pill-partial'); expect(html).toContain('Import incomplete'); expect(html).toContain('Start time unknown');
 expect(html).not.toContain('<time'); expect(html).not.toContain('aria-label="hosted current review state"');
 for(const change of [{trigger_id:''},{trigger_id:'0'},{trigger_id:'00'},{trigger_id:'invalid'},{state:'posting' as const},{reason:'provider_outcome_unknown'},{channel:'cli' as const}]){
  expect(markup([],[{...proven,...change}])).not.toContain('review-pill-partial');
 }
 const recovered = markup([{...completed,channel:'hosted'}],[{...proven,source_run_id:completed.source_run_id}]);
 expect(recovered).not.toContain('review-pill-partial');
 expect(recovered).not.toContain('Import incomplete');
});

it('prefers a clearly estimated matched request submission over unknown time, never preparation',()=>{
 const proven={...attempt,channel:'hosted' as const,state:'uncertain' as const,source_run_id:'',trigger_id:'42',reason:'hosted_finished_inventory_unproven'};
 const submitted='2026-11-15T05:35:10Z';
 const submission={attempt_id:proven.source_attempt_id,head_sha:head,trigger_id:'42',submitted_at:submitted};
 const current={...progress,channel:'hosted' as const,submission_evidence:submission};
 const html=markup([],[proven],current);
 expect(html).toContain('Estimated start (request submission acknowledged)');
 expect(html).toContain(`dateTime="${submitted}"`); expect(html).toContain('review-pill-age">~ ');
 expect(html).not.toContain(`dateTime="${stamp}"`); expect(html).not.toContain('Start time unknown');
 for(const change of [{attempt_id:'review_attempt_'+'f'.repeat(32)},{head_sha:'f'.repeat(40)},{trigger_id:'43'},{submitted_at:'invalid'}]){
  const rejected=markup([],[proven],{...current,submission_evidence:{...submission,...change}});
  expect(rejected).toContain('Start time unknown'); expect(rejected).not.toContain('<time');
 }
 const signal={attempt_id:proven.source_attempt_id,head_sha:head,observed_at:'2026-11-15T05:36:00Z',kind:'hosted_processing_observed' as const,comment_updated_at:submitted,source_comment_id:12,provider_run_id:'11111111-2222-4333-8444-555555555555',trigger_id:'42'};
 const observed=markup([],[proven],{...current,activity_evidence:signal});
 expect(observed).toContain(`dateTime="${signal.observed_at}"`); expect(observed).not.toContain('Estimated start');
});

it('dates the incomplete-import label from the exact request after the Hosted slot is released',()=>{
 const released={...attempt,channel:'hosted' as const,state:'failed' as const,source_run_id:'',trigger_id:'42',reason:'hosted_finished_incomplete'};
 const submitted='2026-11-15T05:35:10Z';
 const submission={attempt_id:released.source_attempt_id,head_sha:head,trigger_id:'42',submitted_at:submitted};
 const current={...progress,channel:'hosted' as const,submission_evidence:submission};
 const html=markup([],[released],current);
 expect(html).toContain('Import incomplete');
 expect(html).toContain('Request acknowledged');
 expect(html).toContain(`dateTime="${submitted}"`);
 expect(html).toContain('0 runs');
 expect(html).not.toContain('review-pill-running');
 expect(markup([],[released],{...current,submission_evidence:{...submission,trigger_id:'43'}})).not.toContain('<time');
});
