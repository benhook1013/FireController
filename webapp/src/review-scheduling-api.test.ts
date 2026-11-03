import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';

const project={id:'project_one',name:'Project',github_repository_id:1,repository:'example/project'};
const clients:Api[]=[];
const json=(value:unknown,status=200)=>new Response(JSON.stringify(value),{status,headers:{'content-type':'application/json'}});
async function connected(writes=true,manager=true) {
  vi.stubGlobal('location',{origin:'https://service.example'});
  vi.stubGlobal('fetch',vi.fn(async(input:RequestInfo|URL,init?:RequestInit)=>String(input).endsWith('/instance')?json({instance_id:'instance_one'}):String(input).endsWith('/browser-session')?json({session_id:JSON.parse(String(init?.body)).session_id,instance_id:'instance_one',project_id:project.id,expires_at:'2099-01-01T00:00:00Z',allow_writes:writes,can_edit_presentation:manager}):json(project)));
  const api=new Api('PRIVATE_TOKEN',project.id);clients.push(api);await api.connect();await api.startBrowserSession();return api;
}
const paused={project_id:project.id,pull_number:7,revision:2,paused:true,reason:'Wait for prepared fixes',actor_id:'principal_one',updated_at:'2026-11-01T00:00:00Z'};
const settings={project_id:project.id,revision:3,automatic_hosted:true,authorizing_worker_id:'worker_one',updated_at:'2026-11-01T00:00:00Z',paused_pulls:[paused]};
afterEach(()=>{clients.splice(0).forEach(api=>api.close());vi.unstubAllGlobals();});

describe('review scheduling browser API',()=>{
  it('reads one bounded project paused inventory and validates exact PR bindings',async()=>{
    const api=await connected();const paths:string[]=[];
    vi.stubGlobal('fetch',vi.fn(async(input:RequestInfo|URL,init?:RequestInit)=>{paths.push(new URL(String(input)).pathname);expect(init?.method).toBe('GET');return json(String(input).includes('/pulls/')?paused:settings);}));
    expect(await api.reviewScheduling()).toEqual(settings);expect(paths).toHaveLength(1);
    expect(await api.pullScheduling(7)).toEqual(paused);
    for(const value of [{...settings,project_id:'other'},{...settings,reason:'made_up'},{...settings,paused_pulls:[paused,paused]},{...settings,paused_pulls:[{...paused,project_id:'other'}]},{...settings,paused_pulls:[{...paused,paused:false}]},{...settings,revision:0}, {...settings,automatic_hosted:null}]) {
      vi.stubGlobal('fetch',vi.fn(async()=>json(value)));await expect(api.reviewScheduling()).rejects.toMatchObject({code:'invalid_response'});
    }
    for(const value of [{...paused,pull_number:8},{...paused,paused:false},{...paused,revision:-1},{...paused,reason:'bad\nreason'}]) {vi.stubGlobal('fetch',vi.fn(async()=>json(value)));await expect(api.pullScheduling(7)).rejects.toMatchObject({code:'invalid_response'});}
  });
  it('keeps human authority, explicit authorizing worker, original key and exact CAS body',async()=>{
    const api=await connected();let writes=0;
    vi.stubGlobal('fetch',vi.fn(async(input:RequestInfo|URL,init?:RequestInit)=>{
      writes++;const headers=new Headers(init?.headers);const body=JSON.parse(String(init?.body));
      expect(init?.method).toBe('POST');expect(init?.credentials).toBe('same-origin');expect(headers.has('authorization')).toBe(false);expect(headers.get('x-fc-browser-session')).toMatch(/^[a-f0-9]{32}$/u);expect(headers.get('x-fc-instance')).toBe('instance_one');expect(headers.get('idempotency-key')).toBe('key-original');
      if(String(input).includes('/pulls/')) {expect(headers.has('x-fc-worker')).toBe(false);expect(body).toEqual({expected_revision:2,paused:false});return json({id:'7',state:'pull_scheduling_updated',revision:3});}
      expect(headers.get('x-fc-worker')).toBe('worker_chosen');expect(body).toEqual({expected_revision:3,automatic_hosted:true});return json({id:project.id,state:'scheduling_configured',revision:4});
    }));
    await api.configureReviewScheduling('worker_chosen',true,3,'key-original');await api.configurePullScheduling(7,false,'discard on resume',2,'key-original');expect(writes).toBe(2);
    await expect(api.configureReviewScheduling('',true,3,'invalid')).rejects.toMatchObject({code:'invalid_request'});expect(writes).toBe(2);
  });
  it('keeps unknown mutation outcomes for exact-key recovery without automatic retry',async()=>{
    const api=await connected();let requests=0;const bodies:string[]=[];
    vi.stubGlobal('fetch',vi.fn(async(_input:RequestInfo|URL,init?:RequestInit)=>{requests++;bodies.push(String(init?.body));expect(new Headers(init?.headers).get('idempotency-key')).toBe('same-key');return requests===1?new Response('{',{status:200,headers:{'content-type':'application/json'}}):json({id:project.id,state:'scheduling_configured',revision:1});}));
    await expect(api.configureReviewScheduling('worker_one',true,0,'same-key')).rejects.toMatchObject({outcomeUnknown:true});expect(requests).toBe(1);
    await api.configureReviewScheduling('worker_one',true,0,'same-key');expect(bodies[0]).toBe(bodies[1]);expect(requests).toBe(2);
  });
  it('enforces read-only versus project-manager capabilities before mutation',async()=>{
    const reader=await connected(false,false);const writer=await connected(true,false);const fetch=vi.fn(async()=>json({id:'7',state:'pull_scheduling_updated',revision:1}));vi.stubGlobal('fetch',fetch);
    await expect(reader.configurePullScheduling(7,true,'',0,'key')).rejects.toMatchObject({code:'forbidden',outcomeUnknown:false});
    await expect(writer.configureReviewScheduling('worker_one',true,0,'key')).rejects.toMatchObject({code:'forbidden',outcomeUnknown:false});expect(fetch).not.toHaveBeenCalled();
    await writer.configurePullScheduling(7,true,'',0,'key');expect(fetch).toHaveBeenCalledTimes(1);
  });
});
