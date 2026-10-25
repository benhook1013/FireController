import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

let server, browser, phase='configuration';
let errors=0,external=0,writes=0;
try {
  const config=JSON.parse(await fs.readFile(process.argv[2],'utf8'));
  const token=(await fs.readFile(config.credential_file,'utf8')).trim();
  phase='launch';
  server=await chromium.launchServer({headless:true,...(config.executable?{executablePath:config.executable}:{}),timeout:5000,proxy:{server:config.proxy,bypass:'127.0.0.1,localhost'},args:['--disable-background-networking','--no-first-run']});
  console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
  browser=await chromium.connect(server.wsEndpoint(),{timeout:5000});
  const context=await browser.newContext({viewport:{width:1440,height:1000}});
  await context.route('**/*',async route=>{if(new URL(route.request().url()).origin!==config.origin){await route.abort();return;}await route.continue();});
  const page=await context.newPage();page.setDefaultTimeout(5000);page.setDefaultNavigationTimeout(5000);
  page.on('pageerror',()=>errors++);
  page.on('console',message=>{if(message.type()==='error')errors++;});
  page.on('request',request=>{const url=new URL(request.url());if(url.origin!==config.origin){external++;return;}if(request.method()!=='GET'&&!url.pathname.endsWith('/browser-session')&&!url.pathname.endsWith('/browser-session/renew'))writes++;});
  phase='login';
  await page.goto(`${config.origin}/projects/${config.project}`);
  await page.getByLabel('Project credential',{exact:true}).fill(token);
  await page.getByRole('button',{name:'Connect to project',exact:true}).click();
  const queue=page.locator('#review-queue');await queue.waitFor();
  phase='queue';
  const row=queue.locator('#review-pr-42');await row.waitFor();
  const lane=row.getByRole('region',{name:'Subagent Reviews',exact:true});
  await expect(lane.locator('.review-lane-heading')).toContainText('1 run');
  await expect(lane.locator('.review-round-pill')).toHaveCount(1);
  await row.getByRole('link',{name:'Review details for PR #42',exact:true}).click();
  phase='timeline';
  const rounds=page.getByRole('region',{name:'PR #42 review rounds',exact:true});
  await expect(rounds.locator('.review-history-row')).toHaveCount(3);
  await expect(rounds.locator('.review-history-kind')).toHaveText(['CodeRabbit CLI #1','CodeRabbit Hosted #1','Subagent #1']);
  const source=rounds.locator(`[data-run-id="${config.run_id}"]`);
  await expect(source.locator('.review-count-found')).toHaveText('Found: 1');
  await expect(source.locator('.review-count-accepted')).toHaveText('Accepted: 1/0/0');
  await source.getByRole('button',{name:'Show findings for Subagent #1',exact:true}).click();
  phase='finding';
  await expect(source).toContainText('Preserve database invariant');
  await expect(source).toContainText('database proof');
  await expect(source.locator('strong').filter({hasText:'database proof'})).toHaveCount(1);
  await expect(source.locator('code').filter({hasText:'source.go:12'})).toHaveCount(1);
  const storage=await page.evaluate(()=>JSON.stringify({local:{...localStorage},session:{...sessionStorage}}));
  if(storage.includes(token)||errors||external||writes)throw new Error('assertion');
  await context.close();
  console.log(JSON.stringify({state:'PASS',errors,external,writes,rows:3}));
} catch {
  console.log(JSON.stringify({state:'FAIL',phase,errors,external,writes}));process.exitCode=1;
} finally {
  await browser?.close().catch(()=>{});await server?.close().catch(()=>{});
}
