import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

let browser, server, page, config;
let phase='configuration', releaseHeld;
let external=0, errors=0, domainWrites=0, sessionPosts=0, sessionDeletes=0, injectedFailures=0, injectedConsole=0;
let metadataA=0,presentationA=0,deliveryA=0,fixtureCalls=0;
const need=value=>{if(!value)throw new Error('assertion');};
try{
  config=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
  const [admin,token,targetToken,defaults]=await Promise.all([fs.readFile(config.admin_file,'utf8'),fs.readFile(config.credential_file,'utf8'),fs.readFile(config.target_credential_file,'utf8'),fs.readFile(config.defaults_file,'utf8')]);
  const prefix='/api/v1/projects/'+config.project,targetPrefix='/api/v1/projects/'+config.target_project;
  const fixture=async(method,project,worker,path,body,status=200)=>{
    const response=await fetch(config.origin+'/api/v1/projects/'+project+path,{method,headers:{Authorization:'Bearer '+admin.trim(),'X-FC-Instance':config.instance,'X-FC-Worker':worker,'Idempotency-Key':'refresh-browser-fixture-'+(++fixtureCalls),'Content-Type':'application/json'},body:JSON.stringify(body),signal:AbortSignal.timeout(5000)});
    need(response.status===status);return response.json();
  };
  // Begin with canonical semantic keys and use a known contrast-valid fixture.
  // Every rendered family has dark text on white; focus contrasts all surfaces.
  const theme={...JSON.parse(defaults).theme,focus:'#767676',accent_background:'#000000',accent_text:'#fffffe',accent_soft_background:'#ffffff'};
  for(const family of ['canvas','surface','header','hero','active','blocked','parked','completed']){
    theme[family+'_background']='#ffffff';theme[family+'_text']='#000000';
    if(['canvas','surface','header','hero'].includes(family))theme[family+'_muted']='#000000';
  }
  phase='fixture';
  await fixture('PUT',config.project,config.worker,'/presentation',{expected_revision:0,presentation:{name:'Refresh project A',brand_name:'Initial brand A',theme}});
  const initialJob=await fixture('POST',config.project,config.worker,'/jobs',{title:'Initial A job',brief:'Synthetic initial project work.',worker_id:config.worker},201);
  const targetTheme={...theme,header_background:'#e4f0e4',header_text:'#27452d',header_muted:'#27452d'};
  await fixture('PUT',config.target_project,config.target_worker,'/presentation',{expected_revision:0,presentation:{name:'Refresh project B',brand_name:'Isolated brand B',theme:targetTheme}});
  const targetJob=await fixture('POST',config.target_project,config.target_worker,'/jobs',{title:'B only job',brief:'Synthetic independent project.',worker_id:config.target_worker},201);

  server=await chromium.launchServer({headless:true,...(config.executable?{executablePath:config.executable}:{}),timeout:5000,proxy:{server:config.proxy,bypass:'127.0.0.1,localhost'},args:['--disable-background-networking','--no-first-run']});
  console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
  browser=await chromium.connect(server.wsEndpoint(),{timeout:5000});
  const context=await browser.newContext({viewport:{width:1440,height:900}});page=await context.newPage();
  page.setDefaultTimeout(5000);page.setDefaultNavigationTimeout(5000);
  let holdNext=false,heldReady,heldDone,injectedRequest;
  const armHold=()=>{
    let ready,done;heldReady=new Promise(resolve=>{ready=resolve;});heldDone=new Promise(resolve=>{done=resolve;});
    let release;const gate=new Promise(resolve=>{release=resolve;});releaseHeld=release;holdNext={ready,done,gate};
  };
  context.on('request',request=>{
    const url=new URL(request.url());if(url.origin!==config.origin){external++;return;}
    if(url.pathname===prefix+'/project-events/stream')metadataA++;
    if(url.pathname===prefix+'/presentation')presentationA++;
    if(url.pathname===prefix+'/delivery')deliveryA++;
    if(['GET','HEAD'].includes(request.method()))return;
    if([prefix+'/browser-session',targetPrefix+'/browser-session'].includes(url.pathname)&&request.method()==='POST')sessionPosts++;
    else if([prefix+'/browser-session',targetPrefix+'/browser-session'].includes(url.pathname)&&request.method()==='DELETE')sessionDeletes++;
    else domainWrites++;
  });
  await context.route('**/*',async route=>{
    const request=route.request(),url=new URL(request.url());
    if(url.origin!==config.origin){await route.abort();return;}
    const ownSession=[prefix+'/browser-session',targetPrefix+'/browser-session'].includes(url.pathname)&&['POST','DELETE'].includes(request.method());
    if(!['GET','HEAD'].includes(request.method())&&!ownSession){await route.abort();return;}
    if(request.method()==='GET'&&url.pathname===prefix+'/project-events/stream'){
      injectedRequest=request;injectedFailures++;await route.fulfill({status:400,contentType:'application/json',body:JSON.stringify({error:{code:'invalid_response'}})});return;
    }
    if(holdNext&&request.method()==='GET'&&url.pathname===prefix+'/presentation'){
      const held=holdNext;holdNext=false;
      const response=await route.fetch({timeout:5000}),body=await response.body();need(response.status()===200);
      held.ready();await held.gate;
      try{await route.fulfill({response,body});held.done('delivered');}catch{held.done('cancelled');}finally{await response.dispose();}
      return;
    }
    await route.continue();
  });
  page.on('pageerror',()=>errors++);
  page.on('console',message=>{if(message.type()!=='error')return;const location=message.location().url;if(injectedRequest&&injectedConsole===0&&location===injectedRequest.url()&&/^Failed to load resource: the server responded with a status of 400 \(Bad Request\)$/u.test(message.text()))injectedConsole++;else errors++;});
  page.on('response',response=>{if(response.status()>=400&&!(response.status()===400&&new URL(response.url()).pathname===prefix+'/project-events/stream'))errors++;});
  const paint=()=>page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  const screenshot=async name=>{if(config.diagnostic_directory){await fs.mkdir(config.diagnostic_directory,{recursive:true,mode:0o700});await page.screenshot({path:config.diagnostic_directory+'/'+name+'.png',fullPage:false,mask:[page.locator('input,textarea')],timeout:2000});}};
  const connect=async(project,credential,name)=>{
    await page.getByLabel('Project ID',{exact:true}).fill(project);await page.getByLabel('Project credential',{exact:true}).fill(credential.trim());
    const accepted=page.waitForResponse(response=>response.request().method()==='POST'&&new URL(response.url()).pathname==='/api/v1/projects/'+project+'/browser-session');
    await page.getByRole('button',{name:'Connect to project',exact:true}).click();need((await accepted).status()===200);
    await page.getByRole('heading',{name,exact:true}).waitFor();await page.getByRole('heading',{name:'Delivery',exact:true}).waitFor();
  };
  const disconnect=async project=>{
    const ended=page.waitForResponse(response=>response.request().method()==='DELETE'&&new URL(response.url()).pathname==='/api/v1/projects/'+project+'/browser-session');
    await page.getByRole('button',{name:'Disconnect',exact:true}).click();need((await ended).status()===200);await page.getByRole('button',{name:'Connect to project',exact:true}).waitFor();
  };
  phase='connect-a';await page.goto(config.origin);await connect(config.project,token,'Refresh project A');
  await page.getByRole('link',{name:'Initial A job',exact:true}).waitFor();await expect(page.locator('.brand')).toContainText('Initial brand A');
  phase='closed-feed';await page.getByText('Project updates disconnected.',{exact:false}).waitFor();need(metadataA===1&&presentationA===1&&deliveryA===1);
  phase='missed-events';
  const changedTheme={...theme,header_background:'#e0eaf4',header_text:'#183451',header_muted:'#183451'};
  await fixture('PUT',config.project,config.worker,'/presentation',{expected_revision:1,presentation:{name:'Refreshed project A',brand_name:'Refreshed brand A',theme:changedTheme}});
  const changedJob=await fixture('POST',config.project,config.worker,'/jobs',{title:'A job from missed event',brief:'Synthetic post-disconnection work.',worker_id:config.worker},201);
  await paint();await expect(page.getByRole('heading',{name:'Refresh project A',exact:true})).toBeVisible();await expect(page.locator('.brand')).toContainText('Initial brand A');
  need(await page.getByRole('link',{name:'A job from missed event',exact:true}).count()===0&&presentationA===1&&deliveryA===1&&metadataA===1);
  phase='held-refresh';armHold();
  const refresh=page.getByRole('button',{name:'Refresh visible data',exact:true});await refresh.click();await heldReady;
  await expect(refresh).toHaveAttribute('aria-busy','true');await expect(refresh).toBeDisabled();await paint();
  await expect(refresh).toHaveAttribute('aria-busy','true');await expect(refresh).toBeDisabled();
  await expect(page.getByRole('heading',{name:'Refresh project A',exact:true})).toBeVisible();await screenshot('refresh-held-a-1440');
  releaseHeld();need(await heldDone==='delivered');releaseHeld=undefined;
  phase='refresh-result';await expect(refresh).toBeEnabled();await expect(refresh).toHaveAttribute('aria-busy','false');
  await page.getByRole('heading',{name:'Refreshed project A',exact:true}).waitFor();await expect(page.locator('.brand')).toContainText('Refreshed brand A');await page.getByRole('link',{name:'A job from missed event',exact:true}).waitFor();
  need(await page.evaluate(()=>document.documentElement.style.getPropertyValue('--fc-header_background').trim())==='#e0eaf4');
  need(metadataA===1&&presentationA===2&&deliveryA===2);await expect(page.getByText('Project updates disconnected.',{exact:false})).toBeVisible();
  await page.setViewportSize({width:390,height:900});need(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await screenshot('refresh-recovered-a-390');
  phase='late-refresh';armHold();await refresh.click();await heldReady;await expect(refresh).toBeDisabled();
  await disconnect(config.project);
  phase='connect-b';await connect(config.target_project,targetToken,'Refresh project B');await page.getByRole('link',{name:'B only job',exact:true}).waitFor();
  await expect(page.locator('.brand')).toContainText('Isolated brand B');
  releaseHeld();const lateResult=await heldDone;releaseHeld=undefined;need(['delivered','cancelled'].includes(lateResult));
  phase='late-result';await paint();await expect(page.getByRole('heading',{name:'Refresh project B',exact:true})).toBeVisible();await expect(page.locator('.brand')).toContainText('Isolated brand B');
  await expect(page.getByRole('link',{name:'B only job',exact:true})).toBeVisible();
  need(await page.getByRole('link',{name:'Initial A job',exact:true}).count()===0&&await page.getByRole('link',{name:'A job from missed event',exact:true}).count()===0);
  need(await page.evaluate(()=>document.documentElement.style.getPropertyValue('--fc-header_background').trim())==='#e4f0e4');
  const text=await page.locator('body').innerText();need(![config.project,config.worker,initialJob.id,changedJob.id,targetJob.id].some(value=>text.includes(value)));
  need(new URL(page.url()).pathname==='/projects/'+config.target_project&&metadataA===1&&presentationA===3);
  await page.setViewportSize({width:320,height:900});need(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await screenshot('refresh-isolated-b-320');
  phase='logout';await disconnect(config.target_project);
  need(fixtureCalls===6&&sessionPosts===2&&sessionDeletes===2&&domainWrites===0&&errors===0&&external===0&&injectedFailures===1&&injectedConsole<=1);
  await context.close();await browser.close();browser=undefined;await server.close();server=undefined;
  console.log(JSON.stringify({state:'PASS',fixture_calls:fixtureCalls,session_posts:sessionPosts,session_deletes:sessionDeletes,domain_writes:domainWrites,errors,external,injected_failures:injectedFailures,injected_console:injectedConsole,metadata_a:metadataA,presentation_a:presentationA,late_result:lateResult}));
}catch(error){
  if(releaseHeld)releaseHeld();
  const line=Number(/refresh_browser\.mjs:(\d+)/u.exec(String(error?.stack))?.[1]??0);
  if(page&&config?.diagnostic_directory)await page.screenshot({path:config.diagnostic_directory+'/refresh-failure.png',fullPage:false,mask:[page.locator('input,textarea')],timeout:2000}).catch(()=>{});
  console.log(JSON.stringify({state:'FAIL',phase,line}));process.exitCode=1;
}finally{
  if(releaseHeld)releaseHeld();
  if(browser)await browser.close().catch(()=>{});
  if(server)await server.close().catch(()=>{});
}
