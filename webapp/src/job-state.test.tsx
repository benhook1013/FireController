import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { QueryClient, QueryClientProvider, QueryObserver } from '@tanstack/react-query';
import { Api } from './api';
import { JobDetail, jobProgressHistoryQuery } from './App';
import { contextualNotesQuery } from './contextual-notes';
import { WorkspaceContext, type Session } from './workspace';
import type { Job, JobProgressEntry, NoteSummary } from './types';

const project = {id:'project_one',name:'Selected project',github_repository_id:42,repository:'synthetic/progress'};
const at='2026-10-20T12:00:00Z';
const progress={summary:'Completed **proof**.\n\n- Preserved detail',next:'Read the retained result.',blocker:'Awaiting input.',evidence:['**literal reference**']};
const note: NoteSummary={id:'note_one',project_id:project.id,job_id:'job_one',worker_id:'worker_other',revision:1,phase:'',kind:'reminder',status:'pending',created_at:at,updated_at:at,body_preview:'Exact attached reminder',body_truncated:false};
const job: Job={id:'job_one',project_id:project.id,title:'Current task',brief:'Private **brief**',worker_id:'worker_one',status:'active',revision:4,created_at:at,updated_at:at,progress,checklist:{revision:2,remaining:[{id:'next',text:'Remaining **step**',done:false}],completed:1,total:2},notes:{items:[note],has_more:true,cursor:'notes_next'}};
const entry: JobProgressEntry={id:'progress_one',project_id:project.id,job_id:job.id,job_revision:4,sequence:8,actor_id:'principal_one',worker_id:'worker_one',progress,created_at:at};
const json=(value:unknown)=>new Response(JSON.stringify(value),{headers:{'content-type':'application/json'}});
async function connected() {
  vi.stubGlobal('location',{origin:'https://service.example'});
  const api=new Api('synthetic-private-token',project.id);
  vi.stubGlobal('fetch',vi.fn().mockResolvedValueOnce(json({instance_id:'instance_one'})).mockResolvedValueOnce(json(project)));
  await api.connect();return api;
}
afterEach(()=>vi.unstubAllGlobals());

describe('unified default job context',()=>{
  it('reads one exact job with full progress, unfinished checklist and only directly attached pending notes',async()=>{
    const api=await connected();const fetch=vi.fn().mockResolvedValue(json(job));vi.stubGlobal('fetch',fetch);
    expect(await api.job(job.id)).toEqual(job);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(String(fetch.mock.calls[0][0])).toBe('https://service.example/api/v1/projects/project_one/jobs/job_one');
    expect(fetch.mock.calls[0][1]).toMatchObject({method:'GET',cache:'no-store'});api.close();
  });
  it('reads bounded list progress without loading full working context',async()=>{
    const api=await connected();const compact={id:job.id,title:job.title,worker_id:job.worker_id,status:job.status,revision:job.revision,progress_summary:'A short **result**',blocker:'Waiting',progress_truncated:true,blocker_truncated:false};
    const fetch=vi.fn().mockResolvedValue(json({items:[compact],has_more:false}));vi.stubGlobal('fetch',fetch);
    expect((await api.jobs()).items).toEqual([compact]);expect(fetch).toHaveBeenCalledTimes(1);
    for(const change of [{progress_summary:'🔥'.repeat(61)},{progress_truncated:null},{blocker_truncated:'false'},{progress_summary:'',progress_truncated:true},{blocker:'bad\u0000'},{progress},{brief:'Private detail'},{notes:job.notes}]) {
      vi.stubGlobal('fetch',vi.fn().mockResolvedValue(json({items:[{...compact,...change}],has_more:false})));await expect(api.jobs()).rejects.toMatchObject({code:'invalid_response'});
    }
    api.close();
  });
  it('accepts deliberately empty current progress and absent optional checklist content',async()=>{
    const api=await connected();const empty={...job,progress:{summary:'',next:'',blocker:'',evidence:[]},checklist:{revision:0,remaining:[],completed:0,total:0},notes:{items:[],has_more:false}};
    vi.stubGlobal('fetch',vi.fn().mockResolvedValue(json(empty)));expect(await api.job(job.id)).toEqual(empty);api.close();
  });
  it('rejects malformed progress, inconsistent checklist and foreign or unrelated notes before exposing context',async()=>{
    const api=await connected();
    const cases=[
      {...job,progress:null},{...job,progress:{...progress,unknown:'private'}},{...job,progress:{...progress,evidence:null}},
      {...job,progress:{...progress,summary:'🔥'.repeat(2049)}},{...job,progress:{...progress,next:'\ud800'}},
      {...job,progress:{summary:'x'.repeat(8192),next:'x'.repeat(8192),blocker:'x',evidence:[]}},
      {...job,progress:{...progress,evidence:new Array(11).fill('reference')}},
      {...job,checklist:{...job.checklist,remaining:[{id:'next',text:'Completed',done:true}]}},
      {...job,checklist:{...job.checklist,total:3}},{...job,checklist:{...job.checklist,revision:0}},
      {...job,notes:{items:[{...note,project_id:'foreign'}],has_more:false}},
      {...job,notes:{items:[{...note,job_id:'unrelated_job'}],has_more:false}},
      {...job,notes:{items:[{...note,status:'consumed'}],has_more:false}},
      {...job,notes:{items:[note,note],has_more:false}},
      {...job,notes:{items:Array.from({length:6},(_,index)=>({...note,id:'note_'+index})),has_more:false}},
      {...job,notes:{items:[note],has_more:true}}, {...job,inbox:{items:[]}},
    ];
    for(const value of cases) {vi.stubGlobal('fetch',vi.fn().mockResolvedValue(json(value)));await expect(api.job(job.id)).rejects.toMatchObject({code:'invalid_response'});}
    api.close();
  });
  it('renders the supplied current context without installing eager checkpoint/update/checklist or note reads',()=>{
    const client=new QueryClient();const key=['fc','connection_one','instance_one',project.id,'inbox_actor'];
    client.setQueryData([...key,'job',job.id],job);
    client.setQueryData([...key,'job-summary',job.id],{job_id:job.id,job_revision:4,revision:0,title:'',body:''});
    client.setQueryData([...key,'job-workstream',job.id],{job_id:job.id,workstream_id:'',workstream_title:'',revision:0});
    const notesKey=[...key,'notes',{job_id:job.id,status:'pending',limit:5},''];
    client.setQueryData(notesKey,{items:[],has_more:false});
    client.getQueryCache().find({queryKey:notesKey})!.setState({status:'error',error:new Error('OLD_NOTE_READ_FAILURE')});
    const read=vi.fn();const session={project,controller:new AbortController(),api:{notes:read,job:read,checkpointLatest:read,jobUpdates:read,jobChecklist:read,jobProgressHistory:read}} as unknown as Session;
    const router=createMemoryRouter([{path:'/projects/project_one/jobs/:jobId',element:<JobDetail/>}],{initialEntries:['/projects/project_one/jobs/job_one']});
    try {
      const html=renderToStaticMarkup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key,home:'/projects/project_one',selectWorker:()=>{},fail:()=>{},ready:false}}><RouterProvider router={router}/></WorkspaceContext.Provider></QueryClientProvider>);
      expect(html).toContain('<h3>Progress</h3>');expect(html).toContain('Completed <strong>proof</strong>');expect(html).toContain('Awaiting input.');expect(html).toContain('**literal reference**');
      expect(html).toContain('Remaining <strong>step</strong>');expect(html).toContain('1/2 completed');expect(html).toContain('Exact attached reminder');expect(html).not.toContain('role="alert"');
      expect(html).toContain('Show progress history');expect(html).toContain('Show full checklist and history');
      expect(html).not.toMatch(/Latest checkpoint|Narrative updates|inbox_actor|worker_other/u);
      const domains=client.getQueryCache().getAll().map(q=>q.queryKey[key.length]);
      for(const domain of ['checkpoint-latest','job-updates','checklist-current','job-progress-history','inbox'])expect(domains).not.toContain(domain);
      const notesQuery=client.getQueryCache().find({queryKey:[...key,'notes',{job_id:job.id,status:'pending',limit:5},'']});
      expect(notesQuery?.isActive()).toBe(false);expect(read).not.toHaveBeenCalled();
    }finally{router.dispose();client.clear();}
  });
  it('requests only an explicit note continuation using the exact job selector',async()=>{
    const read=vi.fn().mockResolvedValue({items:[],has_more:false});const session={controller:new AbortController(),api:{notes:read}} as unknown as Session;
    const query=contextualNotesQuery(session,['fc','connection',project.id],{job_id:job.id},'notes_next');
    await query.queryFn({signal:new AbortController().signal});
    expect(read.mock.calls[0][0]).toEqual({job_id:job.id,status:'pending',limit:5,cursor:'notes_next'});expect(read).toHaveBeenCalledTimes(1);
  });
});

describe('explicit unified progress history',()=>{
  it('retains exact ordered content through the bound history route and refuses malformed identities/order',async()=>{
    const api=await connected();const page={items:[entry],cursor:'older',has_more:true};const fetch=vi.fn().mockResolvedValue(json(page));vi.stubGlobal('fetch',fetch);
    expect(await api.jobProgressHistory(job.id,{limit:1,cursor:'selected'})).toEqual(page);
    expect(String(fetch.mock.calls[0][0])).toBe('https://service.example/api/v1/projects/project_one/jobs/job_one/progress/history?limit=1&cursor=selected');
    for(const value of [{items:[{...entry,project_id:'foreign'}],has_more:false},{items:[{...entry,job_id:'other'}],has_more:false},{items:[entry,entry],has_more:false},{items:[entry,{...entry,id:'progress_two',sequence:9}],has_more:false},{items:[{...entry,progress:{...progress,evidence:['']}}],has_more:false},{items:[entry],has_more:true},{items:[entry],has_more:false,cursor:'extra'}]) {
      vi.stubGlobal('fetch',vi.fn().mockResolvedValue(json(value)));await expect(api.jobProgressHistory(job.id)).rejects.toMatchObject({code:'invalid_response'});
    }
    api.close();
  });
  it('opens one cancellable scoped page only when its history query is subscribed',async()=>{
    const read=vi.fn().mockResolvedValue({items:[entry],has_more:false});const session={controller:new AbortController(),api:{jobProgressHistory:read}} as unknown as Session;
    const key=['fc','selected_connection','instance_one',project.id,'worker_one'];const query=jobProgressHistoryQuery(session,key,job.id);
    expect(read).not.toHaveBeenCalled();expect(query.queryKey).toEqual([...key,'job-progress-history',job.id,'']);
    const client=new QueryClient({defaultOptions:{queries:{retry:false}}});const observer=new QueryObserver(client,query);const stop=observer.subscribe(()=>{});
    try{await vi.waitFor(()=>expect(observer.getCurrentResult().isSuccess).toBe(true));expect(read).toHaveBeenCalledTimes(1);expect(read.mock.calls[0][0]).toBe(job.id);expect(read.mock.calls[0][1]).toEqual({limit:20});expect(read.mock.calls[0][2]).toBeInstanceOf(AbortSignal);}finally{stop();client.clear();}
  });
});
