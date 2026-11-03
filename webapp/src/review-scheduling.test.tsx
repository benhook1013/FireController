import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { WorkspaceContext, type Session } from './workspace';
import { BrowserActionMemory } from './browser-actions';
import { ReviewSchedulingBar, ReviewPullSchedulingControl, reviewSchedulingKey } from './review-scheduling';

function fixture(writes:boolean,manager:boolean) {
  const client=new QueryClient({defaultOptions:{queries:{retry:false}}});const key=['scheduling','project_one'];
  const api={browserCapabilities:{allow_writes:writes,can_edit_presentation:manager},reviewScheduling:vi.fn(),pullScheduling:vi.fn(),workers:vi.fn()};
  const session={api,project:{id:'project_one'},worker:null,controller:new AbortController(),actions:new BrowserActionMemory()} as unknown as Session;
  client.setQueryData(reviewSchedulingKey(key),{project_id:'project_one',revision:1,automatic_hosted:true,authorizing_worker_id:'worker_original',updated_at:'2026-11-01T00:00:00Z',reason:'provider_hold',paused_pulls:[{project_id:'project_one',pull_number:7,revision:2,paused:true,reason:'Prepared <fixes>',actor_id:'principal_one',updated_at:'2026-11-01T00:00:00Z'}]});
  const render=()=>renderToStaticMarkup(<QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key,home:'/projects/project_one',ready:true,fail:vi.fn(),selectWorker:vi.fn()}}><ReviewSchedulingBar /><ReviewPullSchedulingControl number={7} /><ReviewPullSchedulingControl number={8} /></WorkspaceContext.Provider></QueryClientProvider>);
  return {client,api,render};
}
describe('compact review scheduling controls',()=>{
  it.each([[false,false],[true,false],[true,true]])('keeps readonly/writer/manager boundaries (%s,%s)',(writes,manager)=>{
    const f=fixture(writes,manager);try {const html=f.render();expect(html).toContain('Automatic Hosted: On');expect(html).toContain('Provider hold');expect(html).toContain('Reviews paused');expect(html).toContain('Prepared &lt;fixes&gt;');expect(html.includes('<summary>Scheduling</summary>')).toBe(manager);expect(html.includes('<summary>Resume reviews</summary>')).toBe(writes);expect(html.includes('Authorizing worker')).toBe(manager);for(const read of [f.api.reviewScheduling,f.api.pullScheduling,f.api.workers]) expect(read).not.toHaveBeenCalled();}finally{f.client.clear();}
  });
  it('omits any guessed off/paused state before the shared project read exists',()=>{
    const f=fixture(true,true);f.client.clear();try {expect(f.render()).not.toContain('Automatic Hosted: Off');expect(f.api.pullScheduling).not.toHaveBeenCalled();}finally{f.client.clear();}
  });
});
