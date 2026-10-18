import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

let browser, server;
let phase = 'configuration';
let checkpoint = '';
let external = 0;
let editorialReads = 0, compactReads = 0, individualReads = 0;
let sessionPosts=0,sessionDeletes=0,sessionRenewals=0,domainWrites=0,errors=0;
const need = value => { if (!value) throw new Error('assertion'); };
try {
  const config = JSON.parse(await fs.readFile(process.argv[2], 'utf8'));
  const [admin, token] = await Promise.all([config.admin_file, config.credential_file].map(async path => (await fs.readFile(path, 'utf8')).trim()));
  const prefix = '/api/v1/projects/' + config.project;
  let writes = 0;
  const create = async entry => {
    const response = await fetch(config.origin + prefix + '/map/entries', {
      method: 'POST', headers: {'Authorization':'Bearer '+admin, 'X-FC-Instance':config.instance, 'X-FC-Worker':config.worker, 'Idempotency-Key':'packaged-editorial-fixture-' + (++writes), 'Content-Type':'application/json'},
      body: JSON.stringify(entry), signal: AbortSignal.timeout(5000),
    });
    need(response.status === 201);
    return response.json();
  };
  const first = await create({kind:'section', title:'Project intent', description:'## Direction\n\nA long-form **editorial section** with useful context.\n\n- Keep the narrative readable\n- Preserve the stored order\n\n![Synthetic illustration](https://example.invalid/blocked.png)\n\n> Canvas quote for contrast.\n\nInline `canvas sample` code.\n\n```sh\nprintf \'canvas contrast\'\n```\n\n<img src="https://example.invalid/raw.png">', state:'In progress', phases:[], links:[], position:10});
  const second = await create({kind:'section', title:'Project boundaries', description:'## Boundaries\n\nOptional progress metadata is absent here.'+'\n\nLong-form boundaries provide enough natural document content to test section navigation under the sticky header at every viewport. These synthetic paragraphs also exercise readable wrapping without relying on fixed spacer heights.'.repeat(12), state:'', phases:[], links:[], position:20});

  // Every semantic pair remains valid while canvas and surface muted roles differ.
  const theme={canvas_background:'#bcbcbc',canvas_text:'#000000',canvas_muted:'#000000',canvas_border:'#000000',surface_background:'#ffffff',surface_text:'#757575',surface_muted:'#757575',surface_border:'#757575',accent_background:'#000000',accent_text:'#ffffff',accent_soft_background:'#ffffff',header_background:'#ffffff',header_text:'#000000',header_muted:'#000000',hero_background:'#ffffff',hero_text:'#000000',hero_muted:'#000000',focus:'#5a5a5a',active_background:'#bcbcbc',active_text:'#000000',blocked_background:'#bcbcbc',blocked_text:'#000000',parked_background:'#bcbcbc',parked_text:'#000000',completed_background:'#bcbcbc',completed_text:'#000000'};
  const appearance=await fetch(config.origin+prefix+'/presentation',{method:'PUT',headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':config.worker,'Idempotency-Key':'editorial-appearance-fixture','Content-Type':'application/json'},body:JSON.stringify({expected_revision:0,presentation:{name:'Packaged smoke',theme}}),signal:AbortSignal.timeout(5000)});
  need(appearance.status===200&&(await appearance.json()).revision===1);

  server = await chromium.launchServer({headless:true, ...(config.executable ? {executablePath:config.executable} : {}), timeout:5000, proxy:{server:config.proxy, bypass:'127.0.0.1,localhost'}, args:['--disable-background-networking','--no-first-run']});
  console.log(JSON.stringify({event:'browser_started', pid:server.process().pid}));
  browser = await chromium.connect(server.wsEndpoint(), {timeout:5000});
  const context = await browser.newContext({viewport:{width:390,height:900}});
  const page = await context.newPage();
  page.setDefaultTimeout(5000); page.setDefaultNavigationTimeout(5000);
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.origin !== config.origin) { external++; return; }
    if(request.method() !== 'GET'){if(url.pathname===prefix+'/browser-session'&&request.method()==='POST')sessionPosts++;else if(url.pathname===prefix+'/browser-session'&&request.method()==='DELETE')sessionDeletes++;else if(url.pathname===prefix+'/browser-session/renew'&&request.method()==='POST')sessionRenewals++;else domainWrites++;return;}
    if (url.pathname === prefix + '/map/editorial') editorialReads++;
    if (url.pathname === prefix + '/map/entries') compactReads++;
    if (/\/map\/entries\/[^/]+$/u.test(url.pathname.slice(prefix.length))) individualReads++;
  });
  await context.route('**/*', async route => {
    if (new URL(route.request().url()).origin !== config.origin) { await route.abort(); return; }
    const request=route.request(),pathname=new URL(request.url()).pathname;
    if(!['GET','HEAD'].includes(request.method()) && !(pathname===prefix+'/browser-session'&&['POST','DELETE'].includes(request.method())) && !(pathname===prefix+'/browser-session/renew'&&request.method()==='POST')){await route.abort();return;}
    await route.continue();
  });
  page.on('pageerror',()=>errors++);page.on('console',message=>{if(message.type()==='error')errors++;});page.on('response',response=>{if(response.status()>=400)errors++;});
  phase = 'connect'; checkpoint = 'connect-form';
  await page.goto(config.origin);
  await page.getByLabel('Project ID',{exact:true}).fill(config.project);
  await page.getByLabel('Project credential',{exact:true}).fill(token);
  const connected = page.waitForResponse(value => value.request().method() === 'POST' && new URL(value.url()).pathname === prefix + '/browser-session');
  await page.getByRole('button',{name:'Connect to project',exact:true}).click();
  need((await connected).status() === 200);
  await page.getByRole('heading',{name:'Packaged smoke',exact:true}).waitFor();
  phase = 'project-editorial'; checkpoint = 'ordered-content';
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Project',exact:true}).click();
  const entries = page.locator('.project-editorial-entry');
  await expect(entries).toHaveCount(2);
  need((await entries.locator('h3').allTextContents()).join('|') === 'Project intent|Project boundaries');
  await expect(page.locator('.editorial-markdown').getByRole('heading',{name:'Direction',exact:true})).toBeVisible();
  await expect(page.getByText('A long-form editorial section',{exact:false})).toBeVisible();
  await expect(page.getByText('In progress',{exact:true})).toBeVisible();
  await expect(page.getByText('Image: Synthetic illustration',{exact:true})).toBeVisible();
  await expect(page.getByText('Optional progress metadata is absent here.',{exact:true})).toBeVisible();
  need(await page.getByText('Where it stands',{exact:true}).count() === 0 && await page.getByText('Reconsider when',{exact:true}).count() === 0 && await page.getByText('Phases',{exact:true}).count() === 0);
  need(await page.locator('img').count() === 1); // Only the local brand mark; Markdown images render as text.
  const text = await page.locator('body').innerText();
  need(![config.project, config.worker, config.other_worker, first.id, second.id].some(id => text.includes(id)));
  need(editorialReads === 1 && compactReads === 0 && individualReads === 0);
  checkpoint='canvas-markdown';
  const canvasMarkdown = entries.first().locator('.editorial-markdown');
  need(await canvasMarkdown.evaluate(element=>{
    const style=getComputedStyle(element),quote=getComputedStyle(element.querySelector('.editorial-quote'));
    const image=getComputedStyle(element.querySelector('.editorial-image-alt'));
    const inline=getComputedStyle(element.querySelector('p code'));
    const block=getComputedStyle(element.querySelector('.editorial-code-block'));
    return style.color==='rgb(0, 0, 0)' && quote.color==='rgb(0, 0, 0)' && quote.borderLeftColor==='rgb(0, 0, 0)'
      && image.color==='rgb(0, 0, 0)' && inline.color==='rgb(0, 0, 0)' && inline.backgroundColor==='rgb(188, 188, 188)'
      && block.color==='rgb(0, 0, 0)' && block.backgroundColor==='rgb(188, 188, 188)';
  }));

  for (const width of [1440,390,320]) {
    checkpoint = 'layout-' + width;
    await page.setViewportSize({width,height:900});
    need(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    checkpoint='section-anchor-'+width;
    await page.getByRole('navigation',{name:'Project sections',exact:true}).getByRole('link',{name:'Project intent',exact:true}).click();
    await expect.poll(()=>page.locator('.project-editorial-entry h3').first().evaluate(heading=>heading.getBoundingClientRect().top>=document.querySelector('.mast').getBoundingClientRect().bottom),{timeout:5000}).toBe(true);
    checkpoint='layout-'+width;
    need(await page.locator('.mast .header-utilities .connection-note').count() === 1 && await page.locator('.connection-note').count() === 1);
    const colors = await page.evaluate(() => ({note:getComputedStyle(document.querySelector('.mast .header-utilities .connection-note'),'::before').backgroundColor, header:getComputedStyle(document.querySelector('.mast')).backgroundColor}));
    need(colors.note !== colors.header);
    await page.evaluate(() => window.scrollTo(0,document.documentElement.scrollHeight));
    need(await page.locator('.mast').evaluate(element => Math.abs(element.getBoundingClientRect().top) <= 1));
    await expect(page.getByRole('button',{name:'Refresh visible data',exact:true})).toBeVisible();
    if(config.diagnostic_directory){
      await fs.mkdir(config.diagnostic_directory,{recursive:true,mode:0o700});
      await page.screenshot({path:config.diagnostic_directory+'/editorial-viewport-scrolled-'+width+'.png',fullPage:false,mask:[page.locator('input,textarea')],timeout:2000});
      await page.evaluate(()=>window.scrollTo(0,0));
      await page.screenshot({path:config.diagnostic_directory+'/editorial-fullpage-scroll0-'+width+'.png',fullPage:true,mask:[page.locator('input,textarea')],timeout:2000});
      await page.screenshot({path:config.diagnostic_directory+'/editorial-viewport-scroll0-'+width+'.png',fullPage:false,mask:[page.locator('input,textarea')],timeout:2000});
    }

  }
  need(external === 0);
  const changed = await create({kind:'section', title:'Live editorial addition', description:'A new section delivered by the project event stream.', state:'', phases:[], links:[], position:15});
  need(Boolean(changed.id));
  checkpoint = 'live-update';
  await expect(entries).toHaveCount(3);
  need((await entries.locator('h3').allTextContents()).join('|') === 'Project intent|Live editorial addition|Project boundaries');
  need(editorialReads >= 2 && compactReads === 0 && individualReads === 0 && external === 0);
  checkpoint = 'closed-catalogue';
  await page.locator('.editorial-catalogue > summary').click();
  await expect.poll(() => compactReads,{timeout:5000}).toBe(1);
  need(individualReads === 0);
  checkpoint='catalogue-detail';
  await page.locator('.editorial-catalogue').getByRole('link',{name:'Project intent',exact:true}).click();
  await expect(page.locator('.panel.brief')).toBeVisible();
  await expect.poll(() => individualReads,{timeout:5000}).toBe(1);
  const detailMarkdown=page.locator('.panel.brief .editorial-markdown').first();
  need(await detailMarkdown.evaluate(element=>{
    const quote=getComputedStyle(element.querySelector('.editorial-quote'));
    const image=getComputedStyle(element.querySelector('.editorial-image-alt'));
    const inline=getComputedStyle(element.querySelector('p code'));
    const block=getComputedStyle(element.querySelector('.editorial-code-block'));
    return quote.color==='rgb(117, 117, 117)' && quote.borderLeftColor==='rgb(117, 117, 117)'
      && image.color==='rgb(117, 117, 117)' && inline.color==='rgb(117, 117, 117)' && inline.backgroundColor==='rgb(255, 255, 255)'
      && block.color==='rgb(117, 117, 117)' && block.backgroundColor==='rgb(255, 255, 255)';
  }));
  checkpoint='disconnect';
  need(domainWrites===0&&errors===0&&sessionPosts===1);
  const ended=page.waitForResponse(response=>response.request().method()==='DELETE'&&new URL(response.url()).pathname===prefix+'/browser-session');
  await page.getByRole('button',{name:'Disconnect',exact:true}).click();await page.getByRole('button',{name:'Connect to project',exact:true}).waitFor();need((await ended).status()===200&&sessionDeletes===1);
  await context.close();

  // This separate, explicit fixture writer approves only authored public prose.
  // Neither the anonymous browser nor the publication manifest gets credentials
  // or private job, note, message, or map identifiers.
  phase='public-editorial';checkpoint='publication-fixture';
  let publicationCalls=0;
  const fixtureCall=async(method,path,body,status=200)=>{
    const response=await fetch(config.origin+prefix+path,{method,headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':config.worker,'Idempotency-Key':'editorial-public-fixture-'+(++publicationCalls),'Content-Type':'application/json'},body:JSON.stringify(body),signal:AbortSignal.timeout(5000)});
    need(response.status===status);return response.json();
  };
  const privateJob=await fixtureCall('POST','/jobs',{title:'PRIVATE_JOB_SENTINEL',brief:'PRIVATE_BRIEF_SENTINEL',worker_id:config.worker},201);
  await fixtureCall('POST','/notes',{worker_id:config.worker,job_id:privateJob.id,phase:'PRIVATE_PHASE_SENTINEL',kind:'reminder',body:'PRIVATE_NOTE_SENTINEL'},201);
  await fixtureCall('POST','/messages',{recipient_worker_id:config.other_worker,body:'PRIVATE_MESSAGE_SENTINEL',reply_to_message_id:''},201);
  const approvedMarkdown='## Public direction\n\nApproved **editorial prose** with [Project source](https://github.com/example/public-project).\n\n- Public first point\n- Public second point\n\n![Public illustration](https://example.invalid/public-image.png)\n\n<img src="https://example.invalid/public-raw.png" onerror="window.editorialRawExecuted=1">\n\n[Unsafe destination](javascript:alert(1))';
  const manifest={slug:'editorial-smoke',presentation:{name:'Public editorial',brand_name:'Public flame',tagline:'Approved project story',headings:{hero_title:'Public editorial',map:'Public project'},theme},description:'Approved public overview',links:[],project_map:[{key:'direction',title:'Approved direction',description:approvedMarkdown,phase:'',status:'',links:[{key:'guide',label:'Public guide',url:'https://github.com/example/public-project'}]},{key:'boundaries',title:'Approved boundaries',description:'Public boundaries remain second.',phase:'',status:'',links:[]}],progress:[],worker_lanes:[],reviews:[]};
  const approved={expected_revision:0,enabled:true,manifest};
  const preview=await fixtureCall('POST','/publication/preview',approved);
  const publication=await fixtureCall('PUT','/publication',{...approved,preview_sha256:preview.preview_sha256});
  need(publication.revision===1&&publicationCalls===5);

  const anonymous=await browser.newContext({viewport:{width:1440,height:900}});
  await anonymous.addCookies([{name:'private-fixture-cookie',value:'PRIVATE_COOKIE_SENTINEL',url:config.origin,httpOnly:true,sameSite:'Strict'}]);
  await anonymous.addInitScript(()=>{const get=Storage.prototype.getItem;Storage.prototype.getItem=function(key){if(key==='firecontroller.browser-session.v1')throw new Error('private session read');return get.call(this,key);};});
  let publicReads=0,publicSnapshots=0,publicHistory=0,publicPrivateRequests=0,publicWrites=0,publicErrors=0;
  const publicPath='/api/v1/public/projects/editorial-smoke';
  anonymous.on('request',request=>{
    const url=new URL(request.url());
    if(url.origin!==config.origin){external++;return;}
    if(!['GET','HEAD'].includes(request.method()))publicWrites++;
    if(url.pathname.startsWith('/api/')){
      if(!url.pathname.startsWith('/api/v1/public/'))publicPrivateRequests++;
      else{publicReads++;if(url.pathname===publicPath)publicSnapshots++;if(url.pathname.includes('/history'))publicHistory++;const h=request.headers();if(h.authorization||h.cookie||h['x-fc-browser-session']||h['x-fc-worker']||h['x-fc-instance'])publicPrivateRequests++;}
    }
  });
  await anonymous.route('**/*',async route=>{const request=route.request(),url=new URL(request.url());if(url.origin!==config.origin||!['GET','HEAD'].includes(request.method()))await route.abort();else await route.continue();});
  const shared=await anonymous.newPage();shared.setDefaultTimeout(5000);shared.setDefaultNavigationTimeout(5000);
  shared.on('pageerror',()=>publicErrors++);shared.on('console',message=>{if(message.type()==='error')publicErrors++;});shared.on('response',response=>{if(response.status()>=400)publicErrors++;});
  checkpoint='public-content';
  const snapshotResponse=shared.waitForResponse(response=>new URL(response.url()).pathname===publicPath);
  await shared.goto(config.origin+'/public/editorial-smoke');
  checkpoint='public-heading';await shared.getByRole('heading',{name:'Public editorial',exact:true}).waitFor();
  checkpoint='public-snapshot';const response=await snapshotResponse,snapshot=await response.json();
  need(response.status()===200&&response.headers()['cache-control']==='no-store');
  need(snapshot.revision===1&&JSON.stringify(snapshot.content.project_map)===JSON.stringify(manifest.project_map));
  checkpoint='public-order';const publicEntries=shared.locator('#project-map .public-story');
  await expect(publicEntries).toHaveCount(2);
  need((await publicEntries.locator('h3').allTextContents()).join('|')==='Approved direction|Approved boundaries');
  checkpoint='public-markdown';need(await publicEntries.first().locator('.editorial-markdown').evaluate(element=>getComputedStyle(element).color==='rgb(0, 0, 0)'));await expect(shared.getByRole('heading',{name:'Public direction',exact:true})).toBeVisible();
  need(await publicEntries.locator('strong').innerText()==='editorial prose');
  need((await publicEntries.locator('.editorial-list li').allTextContents()).join('|')==='Public first point|Public second point');
  await expect(shared.getByText('Image: Public illustration',{exact:true})).toBeVisible();
  await expect(publicEntries.first().locator('.editorial-markdown')).toContainText('<img src="https://example.invalid/public-raw.png" onerror="window.editorialRawExecuted=1">');
  need(await publicEntries.locator('img,script').count()===0&&await shared.evaluate(()=>window.editorialRawExecuted===undefined));
  need(await shared.getByRole('link',{name:'Unsafe destination',exact:true}).count()===0);
  await expect(shared.getByText('Unsafe destination',{exact:true})).toBeVisible();
  checkpoint='public-links';const approvedLink=shared.getByRole('link',{name:'Project source',exact:true});
  need(await approvedLink.getAttribute('href')==='https://github.com/example/public-project'&&await approvedLink.getAttribute('target')==='_blank');
  need((await approvedLink.getAttribute('rel')).split(' ').includes('noopener'));
  need((await shared.getByRole('link',{name:'Public guide',exact:false}).getAttribute('href'))==='https://github.com/example/public-project');
  checkpoint='public-privacy';const forbidden=['PRIVATE_',config.project,config.worker,config.other_worker,first.id,second.id,changed.id,privateJob.id];
  need(!forbidden.some(value=>JSON.stringify(snapshot).includes(value))&&!forbidden.some(value=>snapshot.content.project_map.some(card=>card.description.includes(value))));
  need(!(await shared.content()).includes('PRIVATE_')&&await shared.getByLabel('Project credential',{exact:true}).count()===0);
  for(const width of [1440,390,320]){
    checkpoint='public-layout-'+width;await shared.setViewportSize({width,height:900});
    need(await shared.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    need(!(await shared.locator('body').innerText()).includes('PRIVATE_'));
    if(config.diagnostic_directory){await shared.evaluate(()=>window.scrollTo(0,0));await shared.screenshot({path:config.diagnostic_directory+'/public-editorial-fullpage-scroll0-'+width+'.png',fullPage:true,timeout:2000});await shared.screenshot({path:config.diagnostic_directory+'/public-editorial-viewport-scroll0-'+width+'.png',fullPage:false,timeout:2000});}
  }
  need(publicReads===2&&publicSnapshots===1&&publicHistory===0&&publicPrivateRequests===0&&publicWrites===0&&publicErrors===0&&external===0);
  await anonymous.close();await browser.close(); browser = undefined; await server.close(); server = undefined;
  console.log(JSON.stringify({state:'PASS',editorial_reads:editorialReads,compact_reads:compactReads,individual_reads:individualReads,external_page_requests:external,fixture_entries:writes,appearance_fixture_calls:1,session_posts:sessionPosts,session_deletes:sessionDeletes,session_renewals:sessionRenewals,domain_writes:domainWrites,errors,publication_fixture_calls:publicationCalls,public_reads:publicReads,public_snapshots:publicSnapshots,public_history:publicHistory,public_private_requests:publicPrivateRequests,public_writes:publicWrites,public_errors:publicErrors,viewports:[1440,390,320]}));
} catch {
  console.log(JSON.stringify({state:'FAIL',phase,checkpoint}));
  process.exitCode = 1;
} finally {
  if (browser) await browser.close().catch(() => {});
  if (server) await server.close().catch(() => {});
}
