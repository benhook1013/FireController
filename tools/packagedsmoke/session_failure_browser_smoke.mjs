import {chromium,expect} from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';
let server,browser,phase='configuration',config;const checks=[],facts={};
const need=value=>{if(!value)throw new Error('assertion');};
try {
 config=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
 const [admin,reader,writer]=await Promise.all([config.admin_file,config.credential_file,config.writer_credential_file].map(async file=>(await fs.readFile(file,'utf8')).trim()));
 const prefix='/api/v1/projects/'+config.project,home='/projects/packaged-smoke',key='firecontroller.browser-session.v1';
 let fixtureWrites=0,requests=0,errors=0,external=0,domainWrites=0,renewals=0,failedRenewals=0,logouts=0;const streamCounts=[{project:0,message:0},{project:0,message:0},{project:0,message:0}];
 const call=async(method,path,body,status=200,worker=config.worker)=>{const response=await fetch(config.origin+prefix+path,{method,headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':worker,'Idempotency-Key':'second-session-'+(++fixtureWrites),'Content-Type':'application/json'},body:JSON.stringify(body),signal:AbortSignal.timeout(5000)});need(response.status===status);return response.json();};
 phase='fixture';
 const job=await call('POST','/jobs',{title:'Second session private job',brief:'Second session private brief',worker_id:config.worker},201);
 await call('POST','/messages',{recipient_worker_id:config.worker,body:'Second session private message',reply_to_message_id:''},201,config.other_worker);
 server=await chromium.launchServer({headless:true,...(config.executable?{executablePath:config.executable}:{}),timeout:5000,proxy:{server:config.proxy,bypass:'127.0.0.1,localhost'},args:['--disable-background-networking','--no-first-run']});
 console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
 browser=await chromium.connect(server.wsEndpoint(),{timeout:5000});const context=await browser.newContext({viewport:{width:1440,height:1000}});
 const pages=[];let failNextRenewal=false;
 const binding=async page=>JSON.parse(await page.evaluate(key=>sessionStorage.getItem(key),key));
 const current=async(page,b)=>page.evaluate(async({prefix,b})=>{const r=await fetch(prefix+'/browser-session',{headers:{'X-FC-Instance':b.instanceID,'X-FC-Browser-Session':b.sessionID},credentials:'same-origin',signal:AbortSignal.timeout(5000)});return r.status;},{prefix,b});
 phase='three tabs';
 for(let index=0;index<3;index++){
  const page=await context.newPage();pages.push(page);page.setDefaultTimeout(5000);page.setDefaultNavigationTimeout(5000);page.on('pageerror',()=>errors++);
  await page.route('**/*',async route=>{const r=route.request(),url=new URL(r.url());if(url.origin!==config.origin){external++;await route.abort();return;}if(url.pathname.startsWith('/api/'))requests++;if(r.method()==='POST'&&url.pathname===prefix+'/browser-session/renew'){renewals++;if(index===1&&failNextRenewal){failNextRenewal=false;failedRenewals++;await route.fulfill({status:503,contentType:'application/json',body:JSON.stringify({error:{code:'provider_unavailable',message:'Synthetic transient renewal failure.'}})});return;}}if(r.method()==='DELETE'&&url.pathname===prefix+'/browser-session')logouts++;if(!['GET','HEAD'].includes(r.method())&&!url.pathname.includes('/browser-session')){domainWrites++;await route.abort();return;}await route.continue();});
  page.on('response',r=>{if(r.status()!==200)return;const path=new URL(r.url()).pathname;if(path===prefix+'/project-events/stream')streamCounts[index].project++;if(path===prefix+'/message-events/stream')streamCounts[index].message++;});
  // Deliberately establish independent credentials; ordinary new tabs now
  // recover this project's existing cookie instead of asking for login again.
  await page.goto(config.origin+'/');await page.evaluate(()=>localStorage.removeItem('firecontroller.browser-recovery.v1'));
  await page.goto(config.origin+home+'/jobs/'+job.id);await page.getByLabel('Project credential',{exact:true}).fill(index===2?reader:writer);await page.getByRole('button',{name:'Connect to project',exact:true}).click();await expect(page.getByText('Second session private brief',{exact:true})).toBeVisible();
  const html=await page.content();need(![admin,reader,writer].some(secret=>html.includes(secret)));const durable=await page.evaluate(()=>JSON.stringify(localStorage));need(![admin,reader,writer,'Second session private brief','Second session private message','allow_writes','can_edit_presentation'].some(value=>durable.includes(value)));
 }
 const [detail,inbox,unaffected]=pages;
 await inbox.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Workers',exact:true}).click();await inbox.getByRole('button',{name:'Open Builder inbox',exact:true}).click();await expect(inbox.getByText('Second session private message',{exact:true}).first()).toBeVisible();
 await unaffected.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Jobs',exact:true}).click();
 const bindings=await Promise.all(pages.map(binding));need(new Set(bindings.map(b=>b.sessionID)).size===3);need(bindings[1].selectedworkerID===config.worker);
 await expect.poll(()=>streamCounts[1].project>0&&streamCounts[1].message>0).toBe(true);
 checks.push('Three real-cookie tabs isolate selectors; the revoked identity owns an active project feed and an active worker message feed');
 phase='transient renewal';
 await inbox.bringToFront();const beforeCookies=await context.cookies();failNextRenewal=true;const failed=inbox.waitForResponse(r=>r.request().method()==='POST'&&new URL(r.url()).pathname===prefix+'/browser-session/renew');await inbox.reload();need((await failed).status()===503);await expect(inbox.getByText('Second session private message',{exact:true}).first()).toBeVisible();need((await binding(inbox)).sessionID===bindings[1].sessionID);need((await binding(inbox)).selectedworkerID===config.worker);need(await current(inbox,bindings[1])===200);
 for(const b of bindings){const before=beforeCookies.find(c=>c.name.endsWith(b.sessionID)),after=(await context.cookies()).find(c=>c.name===before.name);need(after?.value===before.value);}
 await inbox.evaluate(()=>window.dispatchEvent(new Event('pageshow')));need(failedRenewals===1&&renewals===1);
 await expect.poll(()=>streamCounts[1].message>=2&&streamCounts[1].project>=2).toBe(true);
 checks.push('A real resume followed by one failed renewal preserves cookie, private inbox, selected worker and restarted feeds without an automatic retry');
 phase='recreated mobile tab';
 await inbox.evaluate(key=>sessionStorage.removeItem(key),key);
 await inbox.reload();await expect(inbox.getByText('Second session private message',{exact:true}).first()).toBeVisible();
 need((await binding(inbox)).sessionID===bindings[1].sessionID);need((await binding(inbox)).selectedworkerID===config.worker);need(await current(inbox,bindings[1])===200);
 need((await binding(detail)).sessionID===bindings[0].sessionID);need((await binding(unaffected)).sessionID===bindings[2].sessionID);
 checks.push('Loss of per-tab metadata restores the matching project from durable public metadata and the original real HttpOnly cookie; another tab keeps its own identity');
 phase='live credential revocation';
 await call('DELETE','/credentials/'+config.writer_credential_id,{});
 for(const page of [detail,inbox]){
  await expect(page.getByLabel('Project credential',{exact:true})).toBeVisible({timeout:5000});need(await binding(page)===null);const html=await page.content();need(!['Second session private brief','Second session private message','Second session private job',admin,reader,writer].some(value=>html.includes(value)));need(await page.getByRole('button',{name:'Sign out',exact:true}).count()===0);
 }
 need(await current(detail,bindings[0])===401&&await current(inbox,bindings[1])===401);need(await current(unaffected,bindings[2])===200);
 need(JSON.stringify(await binding(unaffected))===JSON.stringify(bindings[2]));
 await expect.poll(async()=>{const c=await context.cookies();return !bindings.slice(0,2).some(b=>c.some(cookie=>cookie.name.endsWith(b.sessionID)));}).toBe(true);
 checks.push('Public credential revocation clears both connected tabs through live authorization, removes private DOM and metadata, expires only their cookies and denies their current-session API reads');
 phase='unaffected live update';
 await call('POST','/jobs',{title:'Independent reader remains live',brief:'Independent reader update',worker_id:config.worker},201);await expect(unaffected.getByRole('link',{name:'Independent reader remains live',exact:true})).toBeVisible();need(await current(unaffected,bindings[2])===200);const logout=unaffected.waitForResponse(r=>r.request().method()==='DELETE'&&new URL(r.url()).pathname===prefix+'/browser-session');await unaffected.getByRole('button',{name:'Sign out',exact:true}).click();need((await logout).status()===200);await expect(unaffected.getByLabel('Project credential',{exact:true})).toBeVisible();need(await binding(unaffected)===null);need(!(await context.cookies()).some(c=>bindings.some(b=>c.name.endsWith(b.sessionID))));
 checks.push('The independent credential keeps live job updates and valid access after revocation, then signs out without leaving any test session cookie');
 const watchPage=async page=>{
  page.setDefaultTimeout(5000);page.setDefaultNavigationTimeout(5000);page.on('pageerror',()=>errors++);
  await page.route('**/*',async route=>{const r=route.request(),url=new URL(r.url());if(url.origin!==config.origin){external++;await route.abort();return;}if(url.pathname.startsWith('/api/'))requests++;if(r.method()==='POST'&&url.pathname===prefix+'/browser-session/renew')renewals++;if(r.method()==='DELETE'&&url.pathname===prefix+'/browser-session')logouts++;if(!['GET','HEAD'].includes(r.method())&&!url.pathname.includes('/browser-session')){domainWrites++;await route.abort();return;}await route.continue();});
 };
 phase='API-isolated session eviction';
 const apiBindings=[];let peak=0;
 for(let index=0;index<9;index++){
  const sessionID=crypto.randomUUID().replaceAll('-','');const r=await context.request.post(config.origin+prefix+'/browser-session',{headers:{Authorization:'Bearer '+reader,'X-FC-Instance':config.instance,Origin:config.origin},data:{session_id:sessionID},timeout:5000});need(r.status()===200);const value=await r.json();apiBindings.push({version:1,projectID:config.project,projectHandle:'packaged-smoke',instanceID:config.instance,sessionID,expiresAt:value.expires_at,selectedworkerID:''});
  const cookies=(await context.cookies()).filter(c=>c.name.startsWith('fc-local-browser-'));peak=Math.max(peak,cookies.length);need(cookies.length<=8&&cookies.reduce((sum,c,index)=>sum+c.name.length+c.value.length+1+(index?2:0),0)<=6144);need(cookies.some(c=>c.name.endsWith(sessionID)));
 }
 const retained=await context.cookies(),victims=apiBindings.filter(b=>!retained.some(c=>c.name.endsWith(b.sessionID))),newest=apiBindings.at(-1);need(victims.length>0&&retained.some(c=>c.name.endsWith(newest.sessionID)));
 for(const b of apiBindings){const r=await context.request.get(config.origin+prefix+'/browser-session',{headers:{'X-FC-Instance':config.instance,'X-FC-Browser-Session':b.sessionID},timeout:5000});need(r.status()===(victims.includes(b)?401:200));}
 Object.assign(facts,{api_created_sessions:9,evicted_sessions:victims.length,eviction_cookie_peak:peak});
 checks.push('Nine API-created sessions isolate cookie pruning from browser connection limits: at most eight cookies and 6 KiB, latest preserved, evicted selectors denied and retained selectors authorized');
 phase='evicted and retained browser resume';
 const resume=async b=>{const page=await context.newPage();await watchPage(page);await page.addInitScript(({key,b})=>sessionStorage.setItem(key,JSON.stringify(b)),{key,b});await page.goto(config.origin+home+'/jobs/'+job.id);return page;};
 const victimPage=await resume(victims[0]);await expect(victimPage.getByLabel('Project credential',{exact:true})).toBeVisible();need(await binding(victimPage)===null);need(!(await victimPage.content()).includes('Second session private brief'));
 const survivorPage=await resume(newest);await expect(survivorPage.getByText('Second session private brief',{exact:true})).toBeVisible();need((await binding(survivorPage)).sessionID===newest.sessionID);need(await current(survivorPage,newest)===200);
 if(config.diagnostic_directory){await victimPage.screenshot({path:config.diagnostic_directory+'/second-session-evicted.png'});await survivorPage.screenshot({path:config.diagnostic_directory+'/second-session-survivor.png'});}
 const survivorLogout=survivorPage.waitForResponse(r=>r.request().method()==='DELETE'&&new URL(r.url()).pathname===prefix+'/browser-session');await survivorPage.getByRole('button',{name:'Sign out',exact:true}).click();need((await survivorLogout).status()===200);await expect(survivorPage.getByLabel('Project credential',{exact:true})).toBeVisible();need(await binding(survivorPage)===null);
 for(const b of apiBindings){const r=await context.request.delete(config.origin+prefix+'/browser-session',{headers:{'X-FC-Instance':config.instance,'X-FC-Browser-Session':b.sessionID,Origin:config.origin},data:{},timeout:5000});need(r.status()===200);}
 need(!(await context.cookies()).some(c=>c.name.startsWith('fc-local-browser-')));
 checks.push('Actual browser resume rejects an evicted binding and preserves the newest API-created session; explicit logout plus selected-cookie cleanup leaves no fixture session');
 Object.assign(facts,{requests,fixture_writes:fixtureWrites,page_errors:errors,external_requests:external,browser_domain_writes:domainWrites,renewals,failed_renewals:failedRenewals,logouts,streams:streamCounts});need(!errors&&!external&&!domainWrites&&failedRenewals===1&&renewals===3&&logouts===5);
 console.log(JSON.stringify({state:'PASS',checks,facts}));
}catch(error){if(config?.diagnostic_directory){await fs.mkdir(config.diagnostic_directory,{recursive:true,mode:0o700});await fs.writeFile(config.diagnostic_directory+'/second-session-failure.json',JSON.stringify({phase,checks,facts,error_class:error?.name==='TimeoutError'?'timeout':'assertion'}),{mode:0o600});for(const [index,page]of (browser?.contexts()[0]?.pages()||[]).entries()){await page.screenshot({path:config.diagnostic_directory+'/second-session-'+index+'.png',timeout:2000}).catch(()=>{});}}console.log(JSON.stringify({state:'FAIL',phase,checks,facts,reason:'assertion or browser operation failed'}));process.exitCode=1;}
finally{await browser?.close().catch(()=>{});await server?.close().catch(()=>{});}
