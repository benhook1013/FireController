import {chromium,expect} from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';
let server,browser,phase='configuration';const checks=[],facts={};
const need=value=>{if(!value)throw new Error('assertion');};
try {
 const config=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
 const [admin,tokenA,tokenB]=await Promise.all([config.admin_file,config.credential_file,config.second_credential_file].map(async path=>(await fs.readFile(path,'utf8')).trim()));
 const configs=[{id:config.project,worker:config.worker,name:'Packaged smoke',handle:'packaged-smoke',workerName:'Builder',token:tokenA,tag:'Alpha'},{id:config.second_project,worker:config.second_other_worker,name:'Second project',handle:'second-project',workerName:'Second inspector',token:tokenB,tag:'Beta'}];
 let writes=0;
 const call=async(item,method,path,body,status=200)=>{
  const response=await fetch(config.origin+'/api/v1/projects/'+item.id+path,{method,headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':item.worker,'Idempotency-Key':'multiproject-browser-'+(++writes),'Content-Type':'application/json'},body:JSON.stringify(body),signal:AbortSignal.timeout(5000)});
  need(response.status===status);return response.json();
 };
 phase='synthetic fixtures';
 for(const item of configs){
  item.home='/projects/'+item.handle;item.prefix='/api/v1/projects/'+item.id;
  item.job=await call(item,'POST','/jobs',{title:item.tag+' private job',brief:item.tag+' private brief',worker_id:item.worker},201);
  await call(item,'POST','/notes',{worker_id:item.worker,job_id:item.job.id,phase:'isolation',kind:'reminder',body:item.tag+' private note'},201);
  const approved={expected_revision:0,enabled:true,manifest:{slug:'multiproject-'+item.handle,presentation:{name:item.tag+' approved public'},description:item.tag+' approved narrative'}};
  const preview=await call(item,'POST','/publication/preview',approved);await call(item,'PUT','/publication',{...approved,preview_sha256:preview.preview_sha256});
 }
 server=await chromium.launchServer({headless:true,...(config.executable?{executablePath:config.executable}:{}),timeout:5000,proxy:{server:config.proxy,bypass:'127.0.0.1,localhost'},args:['--disable-background-networking','--no-first-run']});
 console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
 browser=await chromium.connect(server.wsEndpoint(),{timeout:5000});const context=await browser.newContext({viewport:{width:1440,height:1000}});
 let errors=0,external=0,domainWrites=0,requests=0,foreignReads=0,renewals=0,logouts=0;
 const bindingKey='firecontroller.browser-session.v1';
 const binding=async item=>JSON.parse(await item.page.evaluate(key=>sessionStorage.getItem(key),bindingKey));
 const current=async item=>item.page.evaluate(async ({prefix,key})=>{const b=JSON.parse(sessionStorage.getItem(key));const response=await fetch(prefix+'/browser-session',{headers:{'X-FC-Instance':b.instanceID,'X-FC-Browser-Session':b.sessionID},credentials:'same-origin'});return {status:response.status,value:await response.json()};},{prefix:item.prefix,key:bindingKey});
 const privateScope=async(item,other)=>{const body=await item.page.locator('body').innerText();need(![other.tag+' private',other.job.id,admin,tokenA,tokenB].some(value=>body.includes(value)));const b=await binding(item);need(b.projectID===item.id&&b.instanceID===config.instance&&b.selectedworkerID===item.worker);return b;};
 const publicScope=async item=>{const body=await item.page.content();need(![admin,tokenA,tokenB,...configs.flatMap(x=>[x.id,x.worker,x.job.id,x.tag+' private'])].some(value=>body.includes(value)));need(await item.page.evaluate(key=>sessionStorage.getItem(key),bindingKey)===null);};
 phase='two tabs login';
 for(const item of configs){
  item.page=await context.newPage();item.page.setDefaultTimeout(5000);item.page.setDefaultNavigationTimeout(5000);item.page.on('pageerror',()=>errors++);
  await item.page.route('**/*',async route=>{const request=route.request(),url=new URL(request.url());if(url.origin!==config.origin){external++;await route.abort();return;}if(url.pathname.startsWith('/api/'))requests++;const match=/^\/api\/v1\/projects\/([^/]+)/u.exec(url.pathname);if(match&&![item.id,item.handle].includes(decodeURIComponent(match[1]))){foreignReads++;await route.abort();return;}if(request.method()==='POST'&&url.pathname===item.prefix+'/browser-session/renew')renewals++;if(request.method()==='DELETE'&&url.pathname===item.prefix+'/browser-session')logouts++;if(!['GET','HEAD'].includes(request.method())&&!url.pathname.includes('/browser-session')){domainWrites++;await route.abort();return;}await route.continue();});
  await item.page.goto(config.origin+item.home+'/jobs/'+item.job.id);
  await item.page.getByLabel('Project credential',{exact:true}).fill(item.token);await item.page.getByRole('button',{name:'Connect to project',exact:true}).click();
  await expect(item.page.getByRole('heading',{name:item.tag+' private job',exact:true})).toBeVisible();await expect(item.page.getByText(item.tag+' private brief',{exact:true})).toBeVisible();
  await item.page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Workers',exact:true}).click();await item.page.getByRole('button',{name:'Open '+item.workerName+' inbox',exact:true}).click();await expect(item.page.getByRole('heading',{name:item.workerName+' inbox',exact:true})).toBeVisible();
 }
 const [a,b]=configs;const aBinding=await privateScope(a,b),bBinding=await privateScope(b,a);need(aBinding.sessionID!==bBinding.sessionID);
 const cookies=await context.cookies();need(cookies.filter(c=>[aBinding.sessionID,bBinding.sessionID].some(id=>c.name.endsWith(id))).length===2);
 checks.push('Two tabs share one cookie jar but retain distinct projects, selectors and selected workers');
 phase='independent renewals';
 for(const [item,other] of [[a,b],[b,a]]){
  const otherBinding=await binding(other);const before=(await context.cookies()).find(c=>c.name.endsWith(otherBinding.sessionID));need(!!before);
  const renewed=item.page.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname===item.prefix+'/browser-session/renew');await item.page.reload();need((await renewed).status()===200);await expect(item.page.getByRole('heading',{name:item.workerName+' inbox',exact:true})).toBeVisible();
  await privateScope(item,other);need(JSON.stringify(await binding(other))===JSON.stringify(otherBinding));const after=(await context.cookies()).find(c=>c.name===before.name);need(after?.value===before.value);
  const authority=await current(item);need(authority.status===200&&authority.value.project_id===item.id&&authority.value.session_id===(await binding(item)).sessionID&&authority.value.instance_id===config.instance&&authority.value.allow_writes===false&&authority.value.can_edit_presentation===false);
 }
 checks.push('Reload renews each real cookie without changing the other tab binding or cookie');
 phase='tab switching and private scopes';
 for(const [item,other] of [[a,b],[b,a]]){await item.page.bringToFront();await item.page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Jobs',exact:true}).click();await item.page.getByRole('link',{name:item.tag+' private job',exact:true}).click();await expect(item.page.getByText(item.tag+' private brief',{exact:true})).toBeVisible();await expect(item.page.getByText(item.tag+' private note',{exact:true})).toBeVisible();await privateScope(item,other);}
 checks.push('Switching tabs and navigating job detail retain own private job and note context');
 phase='one tab logout';
 const remaining=await binding(b),remainingCookie=(await context.cookies()).find(c=>c.name.endsWith(remaining.sessionID));
 const ended=a.page.waitForResponse(r=>r.request().method()==='DELETE'&&new URL(r.url()).pathname===a.prefix+'/browser-session');await a.page.getByRole('button',{name:'Sign out',exact:true}).click();need((await ended).status()===200);await expect(a.page.getByRole('heading',{name:a.tag+' approved public',exact:true})).toBeVisible();await publicScope(a);
 need(!(await context.cookies()).some(c=>c.name.endsWith(aBinding.sessionID)));need((await context.cookies()).find(c=>c.name===remainingCookie.name)?.value===remainingCookie.value);need(JSON.stringify(await binding(b))===JSON.stringify(remaining));
 await b.page.reload();await expect(b.page.getByText(b.tag+' private brief',{exact:true})).toBeVisible();await privateScope(b,a);const authority=await current(b);need(authority.status===200&&authority.value.project_id===b.id);
 checks.push('Signing out one project expires only its cookie; the other tab reloads authenticated with its selection');
 phase='anonymous reload and final logout';
 await a.page.reload();await expect(a.page.getByRole('heading',{name:a.tag+' approved public',exact:true})).toBeVisible();await publicScope(a);
 const endedB=b.page.waitForResponse(r=>r.request().method()==='DELETE'&&new URL(r.url()).pathname===b.prefix+'/browser-session');await b.page.getByRole('button',{name:'Sign out',exact:true}).click();need((await endedB).status()===200);await expect(b.page.getByRole('heading',{name:b.tag+' approved public',exact:true})).toBeVisible();await publicScope(b);
 need(!(await context.cookies()).some(c=>[aBinding.sessionID,bBinding.sessionID].some(id=>c.name.endsWith(id))));
 checks.push('Anonymous reloads and both final public routes exclude all former private content and bindings');
 Object.assign(facts,{requests,fixture_writes:writes,page_errors:errors,external_requests:external,browser_domain_writes:domainWrites,foreign_project_requests:foreignReads,renewals,logouts});need(!errors&&!external&&!domainWrites&&!foreignReads&&renewals>=3&&logouts===2);
 console.log(JSON.stringify({state:'PASS',checks,facts}));
}catch{console.log(JSON.stringify({state:'FAIL',phase,checks,facts,reason:'assertion or browser operation failed'}));process.exitCode=1;}
finally{await browser?.close().catch(()=>{});await server?.close().catch(()=>{});}
