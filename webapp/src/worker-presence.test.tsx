import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { Api } from './api';
import { WorkerPresenceView, dismissWorkerPresence } from './worker-presence-view';
import { presenceSelection, validWorkerPresence, type WorkerPresence } from './worker-presence';
const worker='worker_'+'a'.repeat(32), other='worker_'+'b'.repeat(32);
const project={id:'project_one',name:'Contact',github_repository_id:1,repository:'owner/project'};
const value:WorkerPresence={project_id:project.id,evaluated_at:'2026-10-19T00:00:10Z',observed_since:'2026-10-19T00:00:00Z',complete:true,items:[{worker_id:worker,attached:true,last_seen_at:'2026-10-19T00:00:05Z'}]};
const json=(v:unknown)=>new Response(JSON.stringify(v),{headers:{'content-type':'application/json'}});
let api:Api|undefined;
async function connected(){vi.stubGlobal('location',{origin:'https://service.example'});api=new Api('synthetic-private-token',project.id);vi.stubGlobal('fetch',vi.fn(async(input:RequestInfo|URL)=>String(input).endsWith('/instance')?json({instance_id:'instance_one'}):json(project)));await api.connect();return api;}
afterEach(()=>{api?.close();api=undefined;vi.unstubAllGlobals();vi.useRealTimers();});
describe('worker client contact',()=>{
 it('bounds appended-board contact without changing the loaded board',()=>{const workers=Array.from({length:150},(_,n)=>'worker_'+n.toString(16).padStart(32,'0')).reverse();const before=[...workers];expect(presenceSelection(workers)).toHaveLength(100);expect(presenceSelection(workers)[0]).toBe('worker_'+'0'.repeat(32));expect(workers).toEqual(before);});
 it('uses one bounded project read without a worker/session header',async()=>{
  const api=await connected();const fetch=vi.fn(async(_input:RequestInfo|URL,_init?:RequestInit)=>json(value));vi.stubGlobal('fetch',fetch);
  expect(await api.workerPresence([worker])).toEqual(value);expect(fetch).toHaveBeenCalledTimes(1);expect(String(fetch.mock.calls[0][0])).toContain('/workers/presence?worker_ids='+worker);
  expect(new Headers(fetch.mock.calls[0][1]?.headers).has('X-FC-Worker')).toBe(false);expect(new Headers(fetch.mock.calls[0][1]?.headers).has('X-FC-Session')).toBe(false);
 });
 it('rejects invalid selectors before network and foreign or expanded contact summaries',async()=>{
  const api=await connected();const fetch=vi.fn();vi.stubGlobal('fetch',fetch);
  for(const ids of [[],[worker,worker],['worker_bad'],[worker+' '],Array.from({length:101},(_,n)=>'worker_'+n.toString(16).padStart(32,'0'))])await expect(api.workerPresence(ids)).rejects.toMatchObject({code:'invalid_request'});
  expect(fetch).not.toHaveBeenCalled();
  for(const result of [{...value,project_id:'project_other'},{...value,principal_id:'private'},{...value,items:[]},{...value,items:[{...value.items[0],worker_id:other}]},{...value,items:[{...value.items[0],session_id:'private'}]},{...value,items:[{...value.items[0],last_seen_at:null}]},{...value,observed_since:'bad-date'}]){fetch.mockResolvedValue(json(result));await expect(api.workerPresence([worker])).rejects.toMatchObject({code:'invalid_response'});}
 });
 it('keeps unknown, attached and last contact distinct from job progress',()=>{
  const unknown=renderToStaticMarkup(<WorkerPresenceView value={undefined} worker={worker} name="Builder"/>);
  expect(unknown).toContain('Contact unknown');expect(unknown).toContain('does not mean this worker is stopped or paused');expect(unknown).not.toContain('Invalid Date');
  const restarted=renderToStaticMarkup(<WorkerPresenceView value={{...value,items:[{worker_id:worker,attached:false,last_seen_at:null}]}} worker={worker} name="Builder"/>);
  expect(restarted).toContain('Contact unknown');expect(restarted).toContain('clear when the controller restarts');expect(restarted).not.toContain('Last contact');
  const outside=renderToStaticMarkup(<WorkerPresenceView value={{...value,complete:false,items:[]}} worker={worker} name="Builder"/>);
  expect(outside).toContain('Contact unknown');expect(outside).toContain('Some client contact could not be retained');
  const attached=renderToStaticMarkup(<WorkerPresenceView value={value} worker={worker} name="Builder"/>);expect(attached).toContain('aria-label="Builder: Active"');expect(attached).toContain('Client attached');expect(attached).not.toContain('Working');expect(attached).toContain('does not indicate model activity');
  expect(validWorkerPresence({...value,evaluated_at:'2026-10-18T00:00:00Z'},project.id,[worker])).toBe(true); // Server wall-clock rollback is informational, not corrupt contact.
  const recent={...value,complete:false,items:[{...value.items[0],attached:false}]};expect(validWorkerPresence(recent,project.id,[worker])).toBe(true);
  const html=renderToStaticMarkup(<WorkerPresenceView value={recent} worker={worker} name="Builder"/>);expect(html).toContain('Last contact');expect(html).toContain('Some client contact could not be retained');expect(html).not.toContain('Client attached');
 });
 it('keeps old contact active without a cutoff and lets explicit pause take precedence',()=>{
  const old={...value,items:[{worker_id:worker,attached:false,last_seen_at:'2001-01-01T00:00:00Z'}]};
  const active=renderToStaticMarkup(<WorkerPresenceView value={old} worker={worker} name="Builder"/>);
  expect(active).toContain('aria-label="Builder: Active"');expect(active).toContain('Last contact');expect(active).toContain('worker-presence-active');
  const paused=renderToStaticMarkup(<WorkerPresenceView value={value} worker={worker} name="Builder" paused reason="Waiting <literal>" reasonTruncated/>);
  expect(paused).toContain('aria-label="Builder: Paused"');expect(paused).toContain('Waiting &lt;literal&gt;…');expect(paused).toContain('Last contact');expect(paused).toContain('Client attached');expect(paused).not.toContain('Builder: Active');
  expect(renderToStaticMarkup(<WorkerPresenceView worker={worker} name="Builder" paused/>)).toContain('This worker is explicitly paused.');
 });
 it('dismisses the same details on outside pointer/click and Escape and removes listeners',()=>{
  class TestNode {}
  vi.stubGlobal('Node',TestNode);
  const document=new EventTarget(),focus=vi.fn(),summary=new TestNode(),content=new TestNode(),outside=new TestNode();
  const details=Object.assign(new TestNode(),{ownerDocument:document,open:false,contains:(node:TestNode)=>[details,summary,content].includes(node),querySelector:()=>({focus})});
  const cleanup=dismissWorkerPresence(details as unknown as HTMLDetailsElement);
  const dispatch=(type:string,target:TestNode,key?:string)=>{const event=new Event(type,{cancelable:true});Object.defineProperties(event,{target:{value:target},key:{value:key}});document.dispatchEvent(event);return event;};
  try {
   details.open=true;dispatch('pointerdown',content);dispatch('click',summary);expect(details.open).toBe(true);
   dispatch('pointerdown',outside);expect(details.open).toBe(false);expect(focus).not.toHaveBeenCalled();
   details.open=true;dispatch('click',outside);expect(details.open).toBe(false);
   details.open=true;dispatch('focusin',content);expect(details.open).toBe(true);dispatch('focusin',outside);expect(details.open).toBe(false);
   details.open=true;expect(dispatch('keydown',content,'Enter').defaultPrevented).toBe(false);expect(details.open).toBe(true);
   expect(dispatch('keydown',content,'Escape').defaultPrevented).toBe(true);expect(details.open).toBe(false);expect(focus).toHaveBeenCalledOnce();
   cleanup();details.open=true;dispatch('click',outside);dispatch('keydown',outside,'Escape');expect(details.open).toBe(true);
  } finally { cleanup(); }
 });
 it('keeps normal request deadlines and cancellation with no retry',async()=>{
  const api=await connected();vi.useFakeTimers();const fetch=vi.fn((_input:RequestInfo|URL,init:RequestInit)=>new Promise<Response>((_resolve,reject)=>{init.signal!.addEventListener('abort',()=>reject(new DOMException('aborted','AbortError')),{once:true});}));vi.stubGlobal('fetch',fetch);
  const pending=api.workerPresence([worker]).catch(e=>e);await vi.advanceTimersByTimeAsync(5000);expect(await pending).toMatchObject({code:'deadline_exceeded'});
  const abort=new AbortController();const cancelled=api.workerPresence([worker],abort.signal).catch(e=>e);abort.abort();expect(await cancelled).toMatchObject({code:'cancelled'});expect(fetch).toHaveBeenCalledTimes(2);
 });
});
