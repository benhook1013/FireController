import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import { currentReviewFronts, reviewQueueBadges, type ReviewFronts } from './review-fronts';
import type { ReviewPullSummary } from './types';
import type { ReviewProgressItem, ReviewRoundProgress } from './review-progress';

const snapshot='review_snapshot_a';
const sha='a'.repeat(40),base='b'.repeat(40),time='2026-10-19T13:00:00Z';
const front=(channel:'cli'|'hosted',number:number)=>({channel,state:'ready' as const,pull_number:number,head_sha:sha,base_sha:base,base_ref:'main',source_observed_at:time,checked_at:time});
const value=():ReviewFronts=>({project_id:'project_one',configured:true,repository_id:123,configuration_revision:2,policy_revision:1,snapshot_id:snapshot,queue:[1,2,3,4],channels:[front('hosted',2),front('cli',3)]});
const pull=(number:number):ReviewPullSummary=>({number,title:'Work',state:'open',head_sha:sha,base_sha:base,base_ref:'main',ci_state:'unknown',mergeability:'unknown',complete:false,run_count:0,run_counts:{hosted:0,cli:0,subagent:0,manual:0},attempt_count:0,recent_runs:[],latest_attempts:[]});
const control=(channel:'cli'|'hosted',closed=false):ReviewRoundProgress=>({channel,configured:true,revision:1,epoch:1,closed,minimum:0,maximum:5,taper:2,used:0,dry:0,tapered:false,pending:0});
afterEach(()=>vi.unstubAllGlobals());

describe('current review position badges',()=>{
  it('keeps channel fronts independent and only marks established later positions queued',()=>{
    expect(reviewQueueBadges(pull(1),snapshot,value())).toEqual([]);
    expect(reviewQueueBadges(pull(2),snapshot,value())[0]?.text).toBe('Review front · Hosted');
    expect(reviewQueueBadges(pull(3),snapshot,value())[0]?.text).toBe('Review front · CLI');
    expect(reviewQueueBadges(pull(4),snapshot,value())[0]?.text).toBe('Queued');
  });
  it('rejects a contradictory loaded front for all later positions in that channel',()=>{
    const v=value();v.channels=[front('hosted',2)];
    const current=currentReviewFronts(snapshot,[{...pull(2),head_sha:'c'.repeat(40)},pull(4)],v);
    expect(reviewQueueBadges(pull(4),snapshot,current)).toEqual([]);
    expect(currentReviewFronts('review_snapshot_old',[pull(2)],v)).toBeUndefined();
    // A blocker outside the loaded page is still an established prefix hold.
    v.channels=[{...front('hosted',2),state:'held',reason:'review_evidence_pending'}];
    expect(reviewQueueBadges(pull(4),snapshot,currentReviewFronts(snapshot,[pull(4)],v))[0]?.text).toBe('Queued');
  });
  it('uses current controls for closure without converting missing controls into completion',()=>{
    const progress:ReviewProgressItem={pull_number:2,channels:[control('hosted',true),control('cli',true)]};
    expect(reviewQueueBadges(pull(2),snapshot,value(),progress)[0]?.text).toBe('Review closed');
    progress.channels[1]={...control('cli'),configured:false,revision:0,epoch:0,maximum:0,taper:0};
    expect(reviewQueueBadges(pull(2),snapshot,value(),progress)).toEqual([]);
  });
  it('does not close an unbounded grant at its required zero placeholder',()=>{
    const progress:ReviewProgressItem={pull_number:2,channels:[{...control('hosted'),maximum_unbounded:true,maximum:0,minimum:3000,used:2000},control('cli',true)]};
    expect(reviewQueueBadges(pull(2),snapshot,value(),progress)[0]?.text).toBe('Review front · Hosted');
    progress.channels[0]={...progress.channels[0],tapered:true,minimum:0};
    expect(reviewQueueBadges(pull(2),snapshot,value(),progress)[0]?.text).toBe('Review closed');
  });
  it('retains independent local fronts and queued labels under an operator dispatch stop',()=>{
    const v=value();v.channels=v.channels.map(c=>({channel:c.channel,state:'held',reason:'reviews_paused'}));
    const pulls=[{...pull(1),state:'merged' as const},pull(2),pull(3),pull(4)];
    const progress:ReviewProgressItem[]=pulls.map(p=>({pull_number:p.number,channels:[control('hosted',p.number===2),control('cli')]}));
    const current=currentReviewFronts(snapshot,pulls,v,progress);
    expect(reviewQueueBadges(pull(2),snapshot,current,progress[1])[0]?.text).toBe('Review front · CLI');
    expect(reviewQueueBadges(pull(3),snapshot,current,progress[2])[0]?.text).toBe('Review front · Hosted');
    expect(reviewQueueBadges(pull(4),snapshot,current,progress[3])[0]?.text).toBe('Queued');
    expect(v.channels.every(c=>c.pull_number===undefined)).toBe(true);
    const unavailable={...v,channels:v.channels.map(c=>({...c,state:'unknown' as const,reason:'source_frame_unavailable'}))};
    expect(reviewQueueBadges(pull(3),snapshot,currentReviewFronts(snapshot,pulls,unavailable,progress),progress[2])[0]?.text).toBe('Review front · Hosted');
    expect(reviewQueueBadges(pull(3),snapshot,currentReviewFronts(snapshot,pulls.slice(2),v,progress),progress[2])).toEqual([]);
    expect(reviewQueueBadges(pull(3),snapshot,currentReviewFronts(snapshot,pulls,v),progress[2])).toEqual([]);
  });
  it('rejects another snapshot, changed source identity, retired lifecycle and unknown positions',()=>{
    expect(reviewQueueBadges(pull(2),'review_snapshot_old',value())).toEqual([]);
    expect(reviewQueueBadges({...pull(2),head_sha:'c'.repeat(40)},snapshot,value())).toEqual([]);
    expect(reviewQueueBadges({...pull(2),state:'merged'},snapshot,value())).toEqual([]);
    const unknown=value();unknown.channels=unknown.channels.map(c=>({channel:c.channel,state:'unknown',reason:'source_frame_unavailable'}));
    expect(reviewQueueBadges(pull(4),snapshot,unknown)).toEqual([]);
  });
});

async function connected():Promise<Api>{
  vi.stubGlobal('location',{origin:'https://service.example'});
  const json=(body:unknown)=>new Response(JSON.stringify(body),{headers:{'content-type':'application/json'}});
  vi.stubGlobal('fetch',vi.fn(async(input:RequestInfo|URL)=>String(input).endsWith('/instance')?json({instance_id:'instance_one'}):String(input).endsWith('/browser-session')?json({session_id:'a'.repeat(32),instance_id:'instance_one',project_id:'project_one',expires_at:new Date(Date.now()+3600000).toISOString(),allow_writes:false,can_edit_presentation:false}):json({id:'project_one',name:'Project',github_repository_id:123,repository:'owner/repository'})));
  const api=Api.resume('project_one','a'.repeat(32),'instance_one');await api.connect();return api;
}
describe('bounded front response admission',()=>{
  it('uses one local scoped cookie read with no provider or mutation route',async()=>{
    const api=await connected();const read=vi.fn(async()=>new Response(JSON.stringify(value()),{headers:{'content-type':'application/json'}}));vi.stubGlobal('fetch',read);
    expect(await api.reviewFronts()).toEqual(value());expect(read).toHaveBeenCalledTimes(1);
    const [url,init]=read.mock.calls[0] as unknown as [string,RequestInit];
    expect(url).toBe('https://service.example/api/v1/projects/project_one/reviews/fronts');expect(init.method??'GET').toBe('GET');expect(new Headers(init.headers).has('authorization')).toBe(false);expect(new Headers(init.headers).has('x-fc-worker')).toBe(false);api.close();
  });
  it.each([
    (v:Record<string,unknown>)=>({...v,project_id:'another_project'}),
    (v:Record<string,unknown>)=>({...v,queue:[2,2]}),
    (v:Record<string,unknown>)=>({...v,snapshot_id:undefined}),
    (v:Record<string,unknown>)=>({...v,policy_revision:0}),
    (v:Record<string,unknown>)=>({...v,channels:[{...front('cli',3),state:['ready']}]}),
    (v:Record<string,unknown>)=>({...v,channels:[{...front('cli',3),head_sha:'bad'}]}),
    (v:Record<string,unknown>)=>({...v,channels:[{...front('cli',3),source_observed_at:'2026-02-30T00:00:00Z'}]}),
    (v:Record<string,unknown>)=>({...v,channels:[{...front('cli',3),extra:'unknown'}]}),
  ])('refuses malformed or contradictory front claims',async(change)=>{
    const api=await connected();vi.stubGlobal('fetch',vi.fn(async()=>new Response(JSON.stringify(change(value() as unknown as Record<string,unknown>)),{headers:{'content-type':'application/json'}})));
    await expect(api.reviewFronts()).rejects.toMatchObject({code:'invalid_response'});api.close();
  });
});
