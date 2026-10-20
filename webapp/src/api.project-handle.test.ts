import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';

const project = {id:'project_one',handle:'firemud',name:'FireMUD',github_repository_id:123,repository:'owner/repo'};
const summary = {job_id:'job_one',job_revision:4,revision:2,title:'A concise summary',body:'Approved **description**.'};
const json = (value:unknown) => new Response(JSON.stringify(value),{headers:{'content-type':'application/json'}});
afterEach(()=>{vi.unstubAllGlobals();});
async function connected(selector=project.handle) {
  vi.stubGlobal('location',{origin:'https://service.example'});
  const api=new Api('private-token',selector);
  vi.stubGlobal('fetch',vi.fn().mockResolvedValueOnce(json({instance_id:'instance_one'})).mockResolvedValueOnce(json(project)));
  await expect(api.connect()).resolves.toEqual(project);
  return api;
}

describe('friendly project browser binding',()=>{
  it.each(['firemud','project_one','legacy-publication','a'.repeat(64)])('resolves %s once then uses only the immutable ID for sessions and private reads',async selector=>{
    const api=await connected(selector);
    expect(api.projectId).toBe(project.id);
    const requests:{url:string;init:RequestInit}[]=[];
    vi.stubGlobal('fetch',vi.fn(async(input:RequestInfo|URL,init:RequestInit)=>{
      requests.push({url:String(input),init});
      if(init.method==='POST')return json({session_id:JSON.parse(String(init.body)).session_id,instance_id:'instance_one',project_id:project.id,expires_at:'2099-01-01T00:00:00Z',allow_writes:false,can_edit_presentation:false});
      return json({items:[],has_more:false});
    }));
    await api.startBrowserSession();await api.workers();
    expect(requests.map(r=>new URL(r.url).pathname)).toEqual(['/api/v1/projects/project_one/browser-session','/api/v1/projects/project_one/workers']);
    expect(new Headers(requests[1].init.headers).has('authorization')).toBe(false);
    expect(new Headers(requests[1].init.headers).get('x-fc-browser-session')).toMatch(/^[a-f0-9]{32}$/);
    api.close();
  });
  it.each([{...project,handle:null}, {...project,handle:'UPPER'}, {...project,handle:'../other'}, {...project,handle:'a'.repeat(64)}])('refuses malformed handle resolution',async returned=>{
    vi.stubGlobal('location',{origin:'https://service.example'});
    const api=new Api('private-token','firemud');
    const fetch=vi.fn().mockResolvedValueOnce(json({instance_id:'instance_one'})).mockResolvedValueOnce(json(returned));vi.stubGlobal('fetch',fetch);
    await expect(api.connect()).rejects.toMatchObject({code:'invalid_response'});
    expect(fetch).toHaveBeenCalledTimes(2);expect(api.instanceId).toBeUndefined();api.close();
  });
  it('refuses an initial opaque project ID resolving to a different identity',async()=>{
    vi.stubGlobal('location',{origin:'https://service.example'});
    const api=new Api('private-token','project_other');
    vi.stubGlobal('fetch',vi.fn().mockResolvedValueOnce(json({instance_id:'instance_one'})).mockResolvedValueOnce(json(project)));
    await expect(api.connect()).rejects.toMatchObject({code:'invalid_response'});api.close();
  });
  it('refuses a resumed opaque identity being replaced even if a handle happens to match',async()=>{
    vi.stubGlobal('location',{origin:'https://service.example'});
    const api=Api.resume('project-one','a'.repeat(32),'instance_one');
    vi.stubGlobal('fetch',vi.fn().mockResolvedValueOnce(json({instance_id:'instance_one'})).mockResolvedValueOnce(json({...project,handle:'project-one'})));
    await expect(api.connect()).rejects.toMatchObject({code:'invalid_response'});api.close();
  });
});

describe('attached job summary reads',()=>{
  it('uses an explicit bound GET and retains the independently authored summary revision',async()=>{
    const api=await connected();const fetch=vi.fn().mockResolvedValueOnce(json(summary));vi.stubGlobal('fetch',fetch);
    await expect(api.jobSummary('job_one')).resolves.toEqual(summary);
    expect(String(fetch.mock.calls[0][0])).toBe('https://service.example/api/v1/projects/project_one/jobs/job_one/summary');
    expect(fetch.mock.calls[0][1]).toMatchObject({method:'GET',cache:'no-store',redirect:'error'});api.close();
  });
  it('accepts a missing or deliberately cleared summary without a private brief fallback',async()=>{
    const api=await connected();
    for(const revision of [0,3]){
      const empty={...summary,revision,title:'',body:''};vi.stubGlobal('fetch',vi.fn().mockResolvedValueOnce(json(empty)));
      await expect(api.jobSummary('job_one')).resolves.toEqual(empty);
    }api.close();
  });
  it.each([{...summary,job_id:'other'},{...summary,revision:null},{...summary,revision:0},{...summary,title:'a'.repeat(241)},{...summary,body:'é'.repeat(1001)},{...summary,title:'Bad\nheadline'},{...summary,title:'',body:'partial'}])('rejects malformed or cross-job summary responses',async returned=>{
    const api=await connected();vi.stubGlobal('fetch',vi.fn().mockResolvedValueOnce(json(returned)));
    await expect(api.jobSummary('job_one')).rejects.toMatchObject({code:'invalid_response'});api.close();
  });
});

describe('delivery summary admission',()=>{
  it('keeps optional summaries bound to their exact job and revision without requiring fixture changes',async()=>{
    const api=await connected(), at='2026-10-20T12:00:00Z';
    const job={id:'job_one',title:'Private task',worker_id:'',worker_name:'',status:'active',revision:4,brief_preview:'Standing private instructions',brief_preview_truncated:false,updated_at:at,update:null,checklist:null,checkpoint:null};
    const board=(item:unknown)=>({workers:{items:[],has_more:false},jobs:{items:[item],has_more:false}});
    for(const item of [job,{...job,summary}]){vi.stubGlobal('fetch',vi.fn().mockResolvedValueOnce(json(board(item))));await expect(api.delivery()).resolves.toEqual(board(item));}
    for(const item of [null,{...summary,job_id:'other'},{...summary,job_revision:5},{...summary,body:'x'.repeat(2001)}]){
      vi.stubGlobal('fetch',vi.fn().mockResolvedValueOnce(json(board({...job,summary:item}))));
      await expect(api.delivery()).rejects.toMatchObject({code:'invalid_response'});
    }api.close();
  });
});
