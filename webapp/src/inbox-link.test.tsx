import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Inbox, inboxWorkerSelector } from './App';
import { WorkspaceContext, type Session } from './workspace';

describe('worker inbox deep links',()=>{
  it('accepts only one explicit valid worker identity',()=>{
    expect(inboxWorkerSelector(new URLSearchParams())).toBe('');
    expect(inboxWorkerSelector(new URLSearchParams('worker_id=worker_target'))).toBe('worker_target');
    for(const query of ['worker_id=','worker_id=../other','worker_id=one&worker_id=two']) expect(inboxWorkerSelector(new URLSearchParams(query))).toBeUndefined();
  });
  it('does not expose the previous worker inbox or composer while a target link resolves',()=>{
    const client=new QueryClient();const key=['fc','connection','instance','project','worker_old'];
    const session={project:{id:'project'},worker:{id:'worker_old',name:'Previous worker'},controller:new AbortController(),api:{worker:vi.fn()}} as unknown as Session;
    try{
      const html=renderToStaticMarkup(<MemoryRouter initialEntries={['/projects/project/inbox?worker_id=worker_target']}><QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key,home:'/projects/project',selectWorker:vi.fn(),fail:vi.fn(),ready:false}}><Inbox /></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
      expect(html).toContain('page-skeleton');expect(html).not.toContain('Previous worker');expect(html).not.toContain('<textarea');
    }finally{client.clear();}
  });
});
