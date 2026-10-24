import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';
let browser, server, phase='setup';
const checks=[]; const facts={};
try {
 const config=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
 const [admin,token]=await Promise.all([config.admin_file,config.credential_file].map(async p=>(await fs.readFile(p,'utf8')).trim()));
 const prefix='/api/v1/projects/'+config.project, home='/projects/packaged-smoke';
 let writes=0;
 const call=async(method,path,body,status=200)=>{
  const response=await fetch(config.origin+prefix+path,{method,headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':config.worker,'Idempotency-Key':'accumulated-browser-'+(++writes),'Content-Type':'application/json'},...(body?{body:JSON.stringify(body)}:{}),signal:AbortSignal.timeout(5000)});
  if(response.status!==status) throw new Error('fixture status '+response.status+' expected '+status+' path '+path);
  return response.json();
 };
 const jobs=[]; for(let i=0;i<55;i++) jobs.push(await call('POST','/jobs',{title:'Audit job '+String(i).padStart(2,'0'),brief:'Private brief '+i,worker_id:i%2?config.worker:config.other_worker},201));
 const workers=[]; for(let i=0;i<23;i++) workers.push(await call('POST','/workers',{name:'Audit worker '+String(i).padStart(2,'0')},200));
 const notes=[]; for(let i=0;i<7;i++) notes.push(await call('POST','/notes',{worker_id:config.worker,job_id:jobs[0].id,phase:'audit',kind:'reminder',body:'Audit pending note '+i},201));
 const approved={expected_revision:0,enabled:true,manifest:{slug:'accumulated-browser-public',presentation:{name:'Audit published project'},description:'Approved public narrative'}};
 const preview=await call('POST','/publication/preview',approved); await call('PUT','/publication',{...approved,preview_sha256:preview.preview_sha256});
 server=await chromium.launchServer({headless:true,...(config.executable?{executablePath:config.executable}:{}),timeout:5000,proxy:{server:config.proxy,bypass:'127.0.0.1,localhost'},args:['--disable-background-networking','--no-first-run']});
 console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
 browser=await chromium.connect(server.wsEndpoint(),{timeout:5000});
 const context=await browser.newContext({viewport:{width:1440,height:1000}}), page=await context.newPage();
 page.setDefaultTimeout(5000); page.setDefaultNavigationTimeout(5000);
 const publicPrivacy=async()=>{
  const html=await page.content();
  const forbidden=[admin,token,config.project,config.worker,config.other_worker,'Private brief','Audit pending note','New current note body','Audit job',...jobs.map(j=>j.id),...notes.map(n=>n.id),...workers.map(w=>w.id)];
  if(forbidden.some(value=>value&&html.includes(value)))throw new Error('private sentinel in anonymous rendered content');
 };

 let errors=0,external=0,domainWrites=0; const requests=[];
 page.on('pageerror',()=>errors++);
 await context.route('**/*',async route=>{const r=route.request(),url=new URL(r.url());if(url.origin!==config.origin){external++;await route.abort();return;}if(url.pathname.startsWith('/api/'))requests.push({method:r.method(),path:url.pathname,query:url.search});if(!['GET','HEAD'].includes(r.method())&&!url.pathname.includes('/browser-session')){domainWrites++;await route.abort();return;}await route.continue();});
 phase='anonymous same url'; await page.goto(config.origin+home);
 await expect(page.getByRole('heading',{name:'Audit published project',exact:true})).toBeVisible();
 await publicPrivacy();
 if(requests.some(r=>!r.path.startsWith('/api/v1/public/')))throw new Error('anonymous private read');
 checks.push('Anonymous shared route renders publication without private API reads');
 await page.getByRole('button',{name:'Sign in',exact:true}).click();
 await page.getByLabel('Project credential',{exact:true}).fill(token); await page.getByRole('button',{name:'Connect to project',exact:true}).click();
 await expect(page.getByRole('heading',{name:'Packaged smoke',exact:true})).toBeVisible();
 phase='delivery accumulated'; await expect(page.locator('.delivery-job-list > li')).toHaveCount(50);
 await page.getByRole('button',{name:'Load more work',exact:true}).click(); await expect(page.locator('.delivery-job-list > li')).toHaveCount(55);
 checks.push('Delivery appends 55 jobs and remaining workers');
 phase='jobs filter'; await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Jobs',exact:true}).click();
 const workerFilter=page.getByLabel('Job worker',{exact:true});
 // This fixture has25 workers. Its random target identity may be on page two.
 await expect.poll(()=>workerFilter.locator('option').count()).toBeGreaterThan(1);
 if(await workerFilter.locator(`option[value="${config.worker}"]`).count()===0)await page.getByRole('button',{name:'More workers',exact:true}).click();
 await workerFilter.selectOption(config.worker); await page.getByLabel('Job status',{exact:true}).selectOption('');
 await expect(page.locator('.job-list > li')).toHaveCount(20); await page.getByRole('button',{name:'Next jobs',exact:true}).click(); await expect(page.locator('.job-list > li')).toHaveCount(7);
 const detail=page.locator('.job-list .job-title').first();await detail.click();await expect(page.getByRole('heading',{name:'Job brief',exact:true})).toBeVisible();
 await page.getByRole('link',{name:'Back to jobs',exact:false}).click(); await expect(page.getByLabel('Job worker',{exact:true})).toHaveValue(config.worker);await expect(page.getByLabel('Job status',{exact:true})).toHaveValue('');
 checks.push('Jobs page 20+7 and detail return preserve filters');
 phase='note continuation';await page.goto(config.origin+home+'/jobs/'+jobs[0].id);
 const panel=page.getByRole('region',{name:'Pending notes',exact:true});await expect(panel.locator('.contextual-note-list > li')).toHaveCount(5);
 await page.setViewportSize({width:390,height:900});await panel.scrollIntoViewIfNeeded();
 if(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth))throw new Error('note phone horizontal overflow');
 if(config.diagnostic_directory)await page.screenshot({path:config.diagnostic_directory+'/accumulated-browser-pending-notes-390.png'});
 await page.setViewportSize({width:1440,height:1000});

 await panel.getByRole('button',{name:'Next pending notes',exact:true}).click(); await expect(panel.locator('.contextual-note-list > li')).toHaveCount(2);
 await panel.getByRole('link',{name:'Read note',exact:true}).first().click(); await page.getByRole('button',{name:'Show note history',exact:true}).click();await page.getByRole('link',{name:'Read revision 1',exact:true}).click();
 await expect(page.getByRole('heading',{name:'Captured note revision 1',exact:true})).toBeVisible();
 const nid=new URL(page.url()).pathname.split('/').at(-3);const original=notes.find(n=>n.id===nid);if(!original)throw new Error('note fixture identity');
 await call('PATCH','/notes/'+nid,{expected_revision:1,body:'New current note body'});
 await page.getByRole('button',{name:'Refresh visible data',exact:true}).click(); await expect(page.getByText('New current note body',{exact:true})).toHaveCount(0);
 await page.getByRole('link',{name:'Read current note',exact:true}).click();await expect(page.getByText('New current note body',{exact:true})).toBeVisible();
 checks.push('Contextual pending notes 5+2; immutable selected revision remains fixed after edit and refresh');
 phase='offpage worker selection';await page.goto(config.origin+home+'/workers');
 await expect(page.locator('.worker-list > li')).toHaveCount(20);await page.getByRole('button',{name:'Next workers',exact:true}).click();await expect(page.locator('.worker-list > li')).toHaveCount(5);
 const pick=page.locator('.worker-list button').last(); const selectedName=await pick.locator('strong').innerText();await pick.click();await expect(page.getByRole('heading',{name:selectedName+' inbox',exact:true})).toBeVisible();
 await page.reload();await expect(page.getByRole('heading',{name:selectedName+' inbox',exact:true})).toBeVisible();
 checks.push('Off-page worker selection and inbox survive cookie-backed reload');
 await page.setViewportSize({width:390,height:900});
 if(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth))throw new Error('inbox phone horizontal overflow');
 if(config.diagnostic_directory)await page.screenshot({path:config.diagnostic_directory+'/accumulated-browser-inbox-390.png'});
 await page.setViewportSize({width:1440,height:1000});

 phase='transient restore';
 let rejectResume=true;const resumeRoute=config.origin+prefix+'/browser-session';
 await context.route(resumeRoute,async route=>{if(route.request().method()==='GET'&&rejectResume)await route.fulfill({status:503,contentType:'application/json',body:JSON.stringify({error:{code:'unavailable',message:'Synthetic outage'}})});else await route.fallback();});
 await page.reload();await expect(page.getByRole('heading',{name:'Project connection unavailable',exact:true})).toBeVisible();
 if(await page.getByLabel('Project credential',{exact:true}).count())throw new Error('transient restore discarded saved access');
 rejectResume=false;await page.getByRole('button',{name:'Retry connection',exact:true}).click();await expect(page.getByRole('heading',{name:selectedName+' inbox',exact:true})).toBeVisible();
 await context.unroute(resumeRoute);
 await page.evaluate(()=>{const k='firecontroller.browser-session.v1',v=JSON.parse(sessionStorage.getItem(k));v.expiresAt='2020-01-01T00:00:00Z';sessionStorage.setItem(k,JSON.stringify(v));});
 await page.reload();await expect(page.getByRole('heading',{name:selectedName+' inbox',exact:true})).toBeVisible();
 checks.push('Transient current-session outage preserves retry and selected worker; past expiry hint still restores valid server cookie');

 phase='public private transitions';await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Project',exact:true}).click();await page.getByRole('link',{name:'View public version',exact:false}).click();await expect(page.getByRole('heading',{name:'Audit published project',exact:true})).toBeVisible();
 await page.getByRole('button',{name:'Sign out',exact:true}).click();await expect(page.getByRole('heading',{name:'Audit published project',exact:true})).toBeVisible();if(new URL(page.url()).pathname!==home)throw new Error('signout route');
 await publicPrivacy();
 if(await page.evaluate(()=>sessionStorage.getItem('firecontroller.browser-session.v1'))!==null)throw new Error('signed out binding retained');
 checks.push('Public preview and sign out retain same shared project address without rendered private sentinels');
 phase='protected route login and missing cookie';
 await page.goto(config.origin+home+'/jobs/'+jobs[0].id);await page.getByLabel('Project credential',{exact:true}).fill(token);await page.getByRole('button',{name:'Connect to project',exact:true}).click();
 await expect(page.getByRole('heading',{name:'Audit job 00',exact:true})).toBeVisible();
 if(new URL(page.url()).pathname!==home+'/jobs/'+jobs[0].id)throw new Error('protected destination lost');
 await context.clearCookies();await page.reload();await expect(page.getByLabel('Project credential',{exact:true})).toBeVisible();
 await expect(page.getByText('Private brief 0',{exact:true})).toHaveCount(0);
 if(await page.evaluate(()=>sessionStorage.getItem('firecontroller.browser-session.v1'))!==null)throw new Error('denied session binding retained');
 checks.push('Protected detail login retains destination; actual missing cookie removes binding and private rendered content');

 facts.requests=requests.length;facts.fixture_writes=writes;facts.page_errors=errors;facts.external_requests=external;facts.browser_domain_writes=domainWrites;
 if(errors||external||domainWrites)throw new Error('unexpected browser errors or effects');
 console.log(JSON.stringify({state:'PASS',checks,facts}));
} catch(e) {console.log(JSON.stringify({state:'FAIL',phase,checks,facts,reason:String(e.message).startsWith('fixture status ')?String(e.message).split(' path ')[0]:'assertion or browser operation failed'}));process.exitCode=1;}
finally {await browser?.close().catch(()=>{});await server?.close().catch(()=>{});}
