import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider, QueryObserver } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router';
import { Api } from './api';
import { refreshProjectMetadata } from './project-metadata';
import { githubThreadContext, type ReviewSourceReadiness } from './review-source-readiness';
import { ReviewGitHubThreads } from './review-workspace';
import { WorkspaceContext, type Session } from './workspace';
import type { Project, ReviewPullSummary } from './types';
import type { ReviewFronts } from './review-fronts';

const project: Project = { id:'project_one', name:'Project', github_repository_id:17, repository:'owner/project' };
const snapshot = `review_snapshot_${'a'.repeat(32)}`;
const stamp = '2026-11-03T01:02:03.123456789Z';
const key = ['fixture','instance_one','project_one'];
const pull: ReviewPullSummary = { number:3381,title:'Change',state:'open',head_sha:'a'.repeat(40),base_sha:'b'.repeat(40),base_ref:'develop',ci_state:'success',mergeability:'mergeable',complete:true,run_count:0,run_counts:{hosted:0,cli:0,manual:0,subagent:0},attempt_count:0,recent_runs:[],latest_attempts:[] };
const fronts: ReviewFronts = { project_id:project.id,repository_id:17,configured:true,configuration_revision:3,policy_revision:2,snapshot_id:snapshot,queue:[3381],channels:[] };
type ReadinessFixture = ReviewSourceReadiness & {policy_revision:number;evaluated_at:string;state:'held'|'unconfigured';channels:{channel:'cli'|'hosted';covered:boolean;pending:boolean}[];ci:{state:string;reason:string;selection:string;selected_commit_sha:string;merge_state:string;observed_at:string;required:number;satisfied:number;failed:number;pending:number;unproven:number}};
const fixture = (): ReadinessFixture => ({project_id:project.id,refresh_id:`review_refresh_${'c'.repeat(32)}`,source:'observed_comparison',configuration_revision:3,policy_revision:2,evaluated_at:'2026-11-03T02:00:00Z',state:'held',channels:[{channel:'cli',covered:true,pending:false}],ci:{state:'not_required',reason:'',selection:'',selected_commit_sha:'',merge_state:'verified',observed_at:stamp,required:0,satisfied:0,failed:0,pending:0,unproven:0},source_observed_at:'2026-11-03T01:01:00Z',reasons:['unresolved_current_threads','unresolved_outdated_threads'],target:{repository_id:17,repository_name:'owner/project',pull_number:3381,head_sha:pull.head_sha,base_sha:pull.base_sha,base_ref:'develop',parent_pull_number:0,parent_sha:pull.base_sha,observed_at:'2026-11-03T01:01:00Z'},review_state:{state:'complete',reason:'',observed_at:stamp,pull_state:'OPEN',draft:false,mergeable:'MERGEABLE',merge_state_status:'CLEAN',review_decision:null,total_threads:7,unresolved_current:2,unresolved_outdated:1}});
const json = (value: unknown, status=200) => new Response(JSON.stringify(value),{status,headers:{'content-type':'application/json'}});
const clients: Api[] = [], caches: QueryClient[] = [];
async function api() {
  vi.stubGlobal('location',{origin:'https://service.example'});
  const client = new Api('private-fixture-token',project.id); clients.push(client);
  vi.stubGlobal('fetch',vi.fn(async(input:RequestInfo|URL)=>String(input).endsWith('/instance')?json({instance_id:'instance_one'}):json(project)));
  await client.connect(); return client;
}
afterEach(()=>{clients.splice(0).forEach(client=>client.close());caches.splice(0).forEach(cache=>cache.clear());vi.unstubAllGlobals();});

describe('retained all-provider GitHub thread context',()=>{
  it('reads one bounded existing readiness GET without refreshing or changing source',async()=>{
    const client=await api(), source=fixture();
    const fetch=vi.fn(async()=>json(source));vi.stubGlobal('fetch',fetch);
    const got=await client.reviewSourceReadiness(3381);
    expect(got!.review_state.observed_at).toBe(stamp);expect(got!.target.observed_at).toBe(source.target.observed_at);
    expect(fetch).toHaveBeenCalledTimes(1);
    const [url,request]=fetch.mock.calls[0] as unknown as [string,RequestInit];
    expect(new URL(url).pathname).toBe('/api/v1/projects/project_one/reviews/source/3381/readiness');expect(new URL(url).search).toBe('');expect(request.method??'GET').toBe('GET');expect(request.body).toBeUndefined();
    const unknown=fixture();unknown.reasons=['review_state_unproven'];unknown.review_state={state:'unproven',reason:'review_state_provider_unavailable',observed_at:stamp,pull_state:'',draft:false,mergeable:'',merge_state_status:'',review_decision:null,total_threads:0,unresolved_current:0,unresolved_outdated:0};
    vi.stubGlobal('fetch',vi.fn(async()=>json(unknown)));expect((await client.reviewSourceReadiness(3381))!.review_state.state).toBe('unproven');
  });
  it('rejects malformed or foreign facts instead of manufacturing a count',async()=>{
    const client=await api();
    for(const change of [
      (s:ReviewSourceReadiness)=>{s.project_id='project_other';},
      (s:ReviewSourceReadiness)=>{s.target.pull_number=3382;},
      (s:ReviewSourceReadiness)=>{s.target.repository_name='https://foreign.example/a';},
      (s:ReviewSourceReadiness)=>{s.review_state.unresolved_current=-1;},
      (s:ReviewSourceReadiness)=>{s.review_state.unresolved_outdated=8;},
      (s:ReviewSourceReadiness)=>{s.review_state.total_threads=10001;},
      (s:ReviewSourceReadiness)=>{s.review_state.observed_at='2026-02-31T00:00:00Z';},
      (s:ReviewSourceReadiness)=>{s.target.head_sha='abc123';},
      (s:ReviewSourceReadiness)=>{s.source_observed_at=stamp;},
    ]){const value=fixture();change(value);vi.stubGlobal('fetch',vi.fn(async()=>json(value)));await expect(client.reviewSourceReadiness(3381)).rejects.toMatchObject({code:'invalid_response'});}
    const fetch=vi.fn();vi.stubGlobal('fetch',fetch);await expect(client.reviewSourceReadiness(0)).rejects.toMatchObject({code:'invalid_request'});expect(fetch).not.toHaveBeenCalled();
  });
  it('includes outdated threads and the original observed comparison without provider arithmetic',()=>{
    const value=githubThreadContext(fixture(),project,pull,snapshot,fronts)!;
    expect(value.count).toBe(3);expect(value.url).toBe('https://github.com/owner/project/pull/3381');
    expect(value.title).toContain(`as of ${stamp}`);expect(value.title).toContain('2 current and 1 outdated unresolved; 7 total threads');expect(value.title).toContain(`head ${pull.head_sha}; base develop at ${pull.base_sha}`);expect(value.title).toContain('not a live GitHub refresh');
    const outdated=fixture();outdated.review_state.unresolved_current=0;expect(githubThreadContext(outdated,project,pull,snapshot,fronts)?.count).toBe(1);
  });
  it('stays silent for none, zero, unproven or nonapplicable selected/history comparisons',()=>{
    expect(githubThreadContext(undefined,project,pull,snapshot,fronts)).toBeUndefined();
    for(const change of [
      (s:ReviewSourceReadiness)=>{s.review_state.unresolved_current=0;s.review_state.unresolved_outdated=0;},
      (s:ReviewSourceReadiness)=>{s.review_state.state='unproven';},
      (s:ReviewSourceReadiness)=>{s.state='unconfigured';},
      (s:ReviewSourceReadiness)=>{s.target.head_sha='d'.repeat(40);},
      (s:ReviewSourceReadiness)=>{s.target.base_sha='d'.repeat(40);},
      (s:ReviewSourceReadiness)=>{s.target.base_ref='main';},
      (s:ReviewSourceReadiness)=>{s.target.repository_id=99;},
      (s:ReviewSourceReadiness)=>{s.target.repository_name='owner/other';},
      (s:ReviewSourceReadiness)=>{s.configuration_revision=2;},
      (s:ReviewSourceReadiness)=>{s.reasons=['source_comparison_changed'];},
      (s:ReviewSourceReadiness)=>{s.reasons=['source_configuration_changed'];},
    ]){const value=fixture();change(value);expect(githubThreadContext(value,project,pull,snapshot,fronts)).toBeUndefined();}
    expect(githubThreadContext(fixture(),project,pull,`review_snapshot_${'d'.repeat(32)}`,fronts)).toBeUndefined();
    expect(githubThreadContext(fixture(),project,pull,snapshot,undefined)).toBeUndefined();
    expect(githubThreadContext(fixture(),project,pull,snapshot,{...fronts,project_id:'project_other'})).toBeUndefined();
    expect(githubThreadContext(fixture(),{...project,repository:'https://evil.example'},pull,snapshot,fronts)).toBeUndefined();
  });
  it('renders only positive applicable counts and keeps unavailable reads quiet without polling',async()=>{
    const client=await api(), cache=new QueryClient({defaultOptions:{queries:{retry:false}}});caches.push(cache);
    const session={api:client,project,controller:new AbortController()} as Session;
    const render=(selected=snapshot)=>renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={cache}><WorkspaceContext.Provider value={{session,key,home:'/projects/project_one',selectWorker(){},fail(){throw new Error('optional context failed the page');},ready:true}}><ReviewGitHubThreads pull={pull} snapshot={selected} fronts={fronts}/></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
    expect(render()).toBe('');
    const query=cache.getQueryCache().getAll()[0];
    const options=query.options as typeof query.options & {refetchInterval?:unknown;refetchOnWindowFocus?:unknown};
    expect(options.retry).toBe(false);expect(options.refetchInterval).toBeUndefined();expect(options.refetchOnWindowFocus).toBe(false);
    const fetch=vi.fn(async()=>json(fixture()));vi.stubGlobal('fetch',fetch);
    await cache.fetchQuery({...query.options,queryKey:query.queryKey});
    const html=render();expect(html).toContain('3 unresolved GitHub threads');expect(html).toContain('href="https://github.com/owner/project/pull/3381"');expect(html).toContain(stamp);expect(html).not.toContain('Greptile');
    render();expect(fetch).toHaveBeenCalledTimes(1);
    const one=fixture();one.review_state.unresolved_current=0;cache.setQueryData(query.queryKey,one);expect(render()).toContain('1 unresolved GitHub thread ↗');
    const zero=fixture();zero.review_state.unresolved_current=0;zero.review_state.unresolved_outdated=0;cache.setQueryData(query.queryKey,zero);expect(render()).toBe('');
    expect(render(`review_snapshot_${'d'.repeat(32)}`)).toBe('');expect(fetch).toHaveBeenCalledTimes(1);
    cache.setQueryData(query.queryKey,fixture());vi.stubGlobal('fetch',vi.fn(async()=>json({error:{code:'reviews_readiness_unavailable',message:'No retained readiness'}},409)));
    await expect(cache.fetchQuery({...query.options,queryKey:query.queryKey,staleTime:0})).resolves.toBeNull();expect(render()).toBe('');
  });
  it('keeps optional absence successful through event invalidation and later displays retained facts',async()=>{
    const client=await api(), cache=new QueryClient({defaultOptions:{queries:{retry:false,staleTime:Infinity}}});caches.push(cache);
    const queryKey=[...key,'reviews','current','source-readiness',pull.number,snapshot,pull.head_sha,pull.base_sha,pull.base_ref,fronts.configuration_revision];
    const session={api:client,project,controller:new AbortController()} as Session;
    const render=()=>renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={cache}><WorkspaceContext.Provider value={{session,key,home:'/projects/project_one',selectWorker(){},fail(){throw new Error('optional absence failed the page');},ready:true}}><ReviewGitHubThreads pull={pull} snapshot={snapshot} fronts={fronts}/></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
    cache.setQueryData(queryKey,null);
    const observer=new QueryObserver(cache,{queryKey,queryFn:()=>client.reviewSourceReadiness(3381),retry:false,staleTime:Infinity});
    const stop=observer.subscribe(()=>{});
    const events={items:[{event_type:'review.observed' as const,resource_id:'review_workspace',revision:2,event_cursor:'cursor_2'}],event_cursor:'cursor_2',has_more:false};
    try{
      const fetch=vi.fn(async()=>json({error:{code:'reviews_readiness_unavailable',message:'Explicitly refresh selected source with readiness before assessing it.'}},409));vi.stubGlobal('fetch',fetch);
      await expect(refreshProjectMetadata(cache,key,events,new AbortController().signal)).resolves.toBeUndefined();
      expect(cache.getQueryState(queryKey)?.status).toBe('success');expect(cache.getQueryData(queryKey)).toBeNull();expect(render()).toBe('');expect(fetch).toHaveBeenCalledTimes(1);
      vi.stubGlobal('fetch',vi.fn(async()=>json(fixture())));
      await expect(refreshProjectMetadata(cache,key,events,new AbortController().signal)).resolves.toBeUndefined();
      expect(render()).toContain('3 unresolved GitHub threads');expect(cache.getQueryState(queryKey)?.status).toBe('success');
      for(const [response,code,status] of [
        [json({error:{code:'forbidden'}},403),'forbidden',403],
        [json({error:{code:'instance_mismatch'}},409),'instance_mismatch',409],
        [json({error:{code:'reviews_readiness_unavailable'}},500),'reviews_readiness_unavailable',500],
        [json({error:{code:'not_found'}},404),'not_found',404],
        [json({...fixture(),review_state:{...fixture().review_state,total_threads:-1}}),'invalid_response',0],
      ] as const){
        const fetch=vi.fn(async()=>response);vi.stubGlobal('fetch',fetch);
        // The shared invalidator deliberately tolerates not_found404; the API
        // still preserves it as a real refusal rather than optional absence.
        if(code==='not_found') await expect(client.reviewSourceReadiness(3381)).rejects.toMatchObject({code,status});
        else await expect(refreshProjectMetadata(cache,key,events,new AbortController().signal)).rejects.toMatchObject({code,status});
        expect(fetch).toHaveBeenCalledTimes(1);
      }
    }finally{stop();}
  });

});
