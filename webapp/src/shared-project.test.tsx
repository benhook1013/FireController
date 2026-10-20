import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryRouter, RouterProvider, MemoryRouter } from 'react-router';
import App, { Connect, JobSummaryPanel, leavesActionProject } from './App';
import { canonicalProjectLocation, projectHome, projectMatches } from './project-address';
import { isCurrentProjectView } from './project-metadata';
import { WorkspaceContext, type Session } from './workspace';

const project={id:'project_one',handle:'firemud',name:'FireMUD',github_repository_id:123,repository:'owner/repo'};
afterEach(()=>vi.unstubAllGlobals());
function paint(path:string){
  vi.stubGlobal('sessionStorage',{getItem:()=>null});
  const client=new QueryClient();client.setQueryData(['fc','old','instance','project_one','','job','job_one'],{brief:'CACHED_PRIVATE_BRIEF'});
  const router=createMemoryRouter([{path:'*',element:<App/>}],{initialEntries:[path]});
  try{return renderToStaticMarkup(<QueryClientProvider client={client}><RouterProvider router={router}/></QueryClientProvider>);}
  finally{router.dispose();client.clear();}
}

describe('one public/private project address',()=>{
  it('opens the public project with a header sign-in action and no private cached content',()=>{
    const html=paint('/projects/firemud');
    expect(html).toContain('>Sign in</button>');expect(html).toContain('public-project-skeleton');
    expect(html).not.toContain('<h1>Loading');expect(html).not.toContain('Try again');
    expect(html).not.toContain('Project credential');expect(html).not.toContain('CACHED_PRIVATE_BRIEF');
  });
  it('opens inline sign-in at the same friendly address with the project already selected',()=>{
    const html=paint('/projects/firemud?signin=1');
    expect(html).toContain('Sign in to firemud');expect(html).toContain('Project credential');expect(html).toContain('public-project-skeleton');
    expect(html).not.toContain('<h1>Loading');expect(html).not.toContain('Try again');
    expect(html).not.toContain('id="project-id"');expect(html).not.toContain('CACHED_PRIVATE_BRIEF');
  });
  it('requires login for private destinations without rendering any anonymous private lookup',()=>{
    const fetch=vi.fn();vi.stubGlobal('fetch',fetch);
    const html=paint('/projects/firemud/jobs/job_one?status=open');
    expect(html).toContain('Sign in to firemud');expect(html).toContain('Project credential');expect(html).not.toContain('public-project-skeleton');
    expect(html).not.toContain('CACHED_PRIVATE_BRIEF');expect(fetch).not.toHaveBeenCalled();
  });
  it('accepts a friendly Project value at global sign-in and requires only credentials at a project URL',()=>{
    const common={busy:false,notice:'',onConnect:()=>{},cancel:()=>{}};
    const global=renderToStaticMarkup(<Connect projectId="" {...common}/>),bound=renderToStaticMarkup(<Connect inline projectId="firemud" {...common}/>);
    expect(global).toContain('>Project</label>');expect(global).not.toContain('Project ID');expect(bound).not.toContain('id="project-id"');
  });
  it('preserves intended routes/query/anchors when canonicalizing an opaque alias',()=>{
    expect(canonicalProjectLocation(project,{pathname:'/projects/project_one/jobs/job_one',search:'?status=open',hash:'#checkpoint'})).toBe('/projects/firemud/jobs/job_one?status=open#checkpoint');
    expect(canonicalProjectLocation(project,{pathname:'/',search:'',hash:''})).toBe('/projects/firemud');
    expect(projectHome(project)).toBe('/projects/firemud');
    expect(projectMatches(project,'project_one')).toBe(true);expect(projectMatches(project,'firemud')).toBe(true);expect(projectMatches(project,'another')).toBe(false);
  });
  it('preserves a legacy publication destination only for the exact authenticated initial selector',()=>{
    const location={pathname:'/projects/legacy-public/jobs/job_one',search:'?status=open',hash:'#checkpoint'};
    expect(canonicalProjectLocation(project,location,'legacy-public')).toBe('/projects/firemud/jobs/job_one?status=open#checkpoint');
    expect(canonicalProjectLocation(project,location)).toBe('/projects/firemud?status=open#checkpoint');
    expect(canonicalProjectLocation(project,location,'another-project')).toBe('/projects/firemud?status=open#checkpoint');
    expect(canonicalProjectLocation(project,location,project.id)).toBe('/projects/firemud?status=open#checkpoint');
    expect(canonicalProjectLocation(project,{pathname:'/projects/legacy-public/published/history/2',search:'?generation=public_generation_exact',hash:''},'legacy-public')).toBe('/projects/firemud/published/history/2?generation=public_generation_exact');
    expect(canonicalProjectLocation(project,{...location,pathname:'/projects/another-project/jobs/job_one'},'legacy-public')).toBe('/projects/firemud?status=open#checkpoint');
  });
  it('keeps action recovery and metadata live on the same verified aliases, never another project',()=>{
    for(const selector of ['project_one','firemud'])expect(leavesActionProject(project.id,`/projects/${selector}/inbox`,project.handle)).toBe(false);
    expect(leavesActionProject(project.id,'/projects/firecontroller',project.handle)).toBe(true);
    const lifetime=new AbortController();
    expect(isCurrentProjectView(project.id,'/projects/firemud/jobs',lifetime,lifetime,project.handle)).toBe(true);
    expect(isCurrentProjectView(project.id,'/projects/firemud-else/jobs',lifetime,lifetime,project.handle)).toBe(false);
    expect(isCurrentProjectView(project.id,'/projects/firemud/jobs',lifetime,new AbortController(),project.handle)).toBe(false);
  });
  it('renders the full authored summary in the private detail scope without borrowing another connection cache',()=>{
    const key=['fc','current','instance','project_one',''],client=new QueryClient();
    const value={job_id:'job_one',job_revision:5,revision:2,title:'Shareable result',body:'**Summary prose**.'};
    client.setQueryData([...key,'job-summary','job_one'],value);
    client.setQueryData(['fc','other','instance','project_two','','job-summary','job_one'],{...value,body:'OTHER_PRIVATE_SUMMARY'});
    const session={project,api:{},controller:new AbortController()} as Session;
    try{
      const html=renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key,home:projectHome(project),selectWorker:()=>{},fail:()=>{},ready:false}}><JobSummaryPanel jobId="job_one" jobRevision={5}/></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
      expect(html).toContain('Shareable result');expect(html).toContain('<strong>Summary prose</strong>');expect(html).not.toContain('OTHER_PRIVATE_SUMMARY');
      client.setQueryData([...key,'job-summary','job_one'],{...value,revision:3,title:'',body:''});
      const cleared=renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{session,key,home:projectHome(project),selectWorker:()=>{},fail:()=>{},ready:false}}><JobSummaryPanel jobId="job_one" jobRevision={5}/></WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
      expect(cleared).not.toContain('aria-label="Job summary"');expect(cleared).not.toContain('Summary prose');
    }finally{client.clear();}
  });
});
