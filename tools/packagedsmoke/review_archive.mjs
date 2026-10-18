import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

// Only disposable synthetic fixture inputs enter this driver. Never print
// request headers, credentials, captured response bodies or raw diagnostics.
let server, browser;
let phase = 'configuration';
const need = value => { if (!value) throw new Error('assertion'); };
try {
  const config = JSON.parse(await fs.readFile(process.argv[2], 'utf8'));
  const [admin, token] = await Promise.all([config.admin_file, config.credential_file].map(async file => (await fs.readFile(file, 'utf8')).trim()));
  const number = Number(config.pull_number), count = Number(config.count);
  need(Number.isSafeInteger(number) && number > 0 && count === 123);
  const prefix = `/api/v1/projects/${config.project}`;
  const path = `/projects/${config.project}/reviews/pulls/${number}`;
  const archives = [];
  let errors = 0, external = 0, forbiddenWrites = 0, sessionPosts = 0;
  phase = 'launch';
  server = await chromium.launchServer({ headless:true, ...(config.executable ? {executablePath:config.executable} : {}), timeout:5000, proxy:{server:config.proxy,bypass:'127.0.0.1,localhost'}, args:['--disable-background-networking','--no-first-run'] });
  console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
  browser = await chromium.connect(server.wsEndpoint(),{timeout:5000});
  const context = await browser.newContext({viewport:{width:390,height:900}});
  await context.route('**/*',async route => { if (new URL(route.request().url()).origin !== config.origin) { await route.abort(); return; } await route.continue(); });
  const connect = async (name,snapshot) => {
    const page = await context.newPage(); page.setDefaultTimeout(5000); page.setDefaultNavigationTimeout(5000);
    page.on('pageerror',() => errors++);
    page.on('console',message => { if (message.type() === 'error') errors++; });
    page.on('response',response => { if (response.status() >= 400) errors++; });
    page.on('request',request => {
      const url = new URL(request.url());
      if (url.origin !== config.origin) { external++; return; }
      if (request.method() !== 'GET') {
        if (url.pathname === prefix+'/browser-session' && request.method() === 'POST') sessionPosts++;
        else if (!(url.pathname === prefix+'/browser-session' && request.method() === 'DELETE') && !(url.pathname === prefix+'/browser-session/renew' && request.method() === 'POST')) forbiddenWrites++;
      }
      const match = url.pathname.match(/\/reviews\/pulls\/[1-9][0-9]*\/(runs|attempts)$/u);
      if (match) {
        const limit = Number(url.searchParams.get('limit'));
        archives.push({name,feed:match[1],snapshot:url.searchParams.get('snapshot'),cursor:url.searchParams.get('cursor'),limit});
        if (limit !== 20 || !/^review_snapshot_[a-f0-9]{32}$/u.test(url.searchParams.get('snapshot') ?? '')) errors++;
      }
    });
    await page.goto(config.origin+path+(snapshot ? `?snapshot=${snapshot}` : ''));
    await page.getByLabel('Project ID',{exact:true}).fill(config.project);
    await page.getByLabel('Project credential',{exact:true}).fill(token);
    await page.getByRole('button',{name:'Connect to project',exact:true}).click();
    const rounds = page.getByRole('region',{name:`PR #${number} review rounds`,exact:true});
    const attempts = page.getByRole('region',{name:`PR #${number} attempts`,exact:true});
    await rounds.waitFor(); await attempts.waitFor();
    await attempts.locator('summary').click();
    await expect.poll(() => rounds.locator('.review-round-pill').count()).toBe(20);
    await expect.poll(() => attempts.locator('li').count()).toBe(20);
    need(await rounds.getByRole('region',{name:'Hosted CodeRabbit',exact:true}).count() === 1);
    need(await rounds.getByRole('region',{name:'CLI CodeRabbit',exact:true}).count() === 1);
    need(await page.locator('.review-pull-summary').getByText(`${count} runs · ${count} attempts`,{exact:true}).count() === 1);
    const stored = await page.evaluate(() => JSON.stringify({session:{...sessionStorage},local:{...localStorage}}));
    need(!stored.includes(token) && !stored.includes(admin));
    return {page,rounds,attempts,name};
  };
  const requests = (view,feed) => archives.filter(item => item.name === view.name && item.feed === feed);
  const more = async (view,feed,shown) => {
    const section = feed === 'runs' ? view.rounds : view.attempts;
    const before = requests(view,feed).length;
    await section.getByRole('button',{name:feed === 'runs' ? 'Load more review rounds' : 'Load more attempts',exact:true}).click();
    await expect.poll(() => section.locator(feed === 'runs' ? '.review-round-pill' : 'li').count()).toBe(shown);
    need(requests(view,feed).length === before+1);
  };
  phase = 'current-first-pages';
  const current = await connect('current');
  need(requests(current,'runs').length === 1 && requests(current,'attempts').length === 1);
  await more(current,'runs',40); await more(current,'attempts',40);
  phase = 'pinned-first-pages';
  const pinned = await connect('pinned',config.snapshot);
  need(requests(pinned,'runs').length === 1 && requests(pinned,'attempts').length === 1);
  await new Promise(resolve => setTimeout(resolve,100));
  need(requests(pinned,'runs').length === 1 && requests(pinned,'attempts').length === 1);

  phase = 'metadata-refresh';
  const raw = await fs.readFile(config.new_observation_file,'utf8'); need(Buffer.byteLength(raw) <= 256<<10);
  const response = await fetch(config.origin+prefix+'/reviews/observations',{method:'POST',headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':config.worker,'Idempotency-Key':'packaged-archive-new-observation','Content-Type':'application/json'},body:raw,signal:AbortSignal.timeout(5000)});
  need(response.status === 201); await response.arrayBuffer();
  await expect.poll(() => current.rounds.locator('.review-round-pill').count()).toBe(1);
  await expect.poll(() => current.attempts.locator('li').count()).toBe(1);
  await current.page.locator('.page-heading').getByRole('heading',{name:'Current archive metadata',exact:true}).waitFor();
  need(await current.rounds.getByRole('link',{name:/current-only-run/u}).count() === 1);
  need(await current.attempts.getByText('current-only-attempt',{exact:true}).count() === 1);
  need(await current.rounds.getByRole('button',{name:'Load more review rounds',exact:true}).count() === 0);
  need(await current.attempts.getByRole('button',{name:'Load more attempts',exact:true}).count() === 0);
  need(requests(current,'runs').filter(item => item.snapshot !== config.snapshot).length === 1);
  need(requests(current,'attempts').filter(item => item.snapshot !== config.snapshot).length === 1);
  need(await pinned.rounds.locator('.review-round-pill').count() === 20 && await pinned.attempts.locator('li').count() === 20);

  phase = 'pinned-continuation';
  for (const shown of [40,60,80,100,120,123]) await more(pinned,'runs',shown);
  for (const shown of [40,60,80,100,120,123]) await more(pinned,'attempts',shown);
  need(requests(pinned,'runs').every(item => item.snapshot === config.snapshot));
  need(requests(pinned,'attempts').every(item => item.snapshot === config.snapshot));
  const links = await pinned.rounds.locator('.review-round-pill').evaluateAll(elements => elements.map(element => element.getAttribute('href')));
  need(new Set(links).size === count && links.some(link => link.includes(config.first_run_id)) && links.some(link => link.includes(config.tail_run_id)));
  need(await pinned.attempts.getByText(config.first_attempt_id,{exact:true}).count() === 1);
  const shared = pinned.attempts.locator('li').filter({hasText:config.tail_attempt_id});
  need(await shared.count() === 2 && await shared.getByText(config.source_a,{exact:true}).count() === 1 && await shared.getByText(config.source_b,{exact:true}).count() === 1);
  need(await pinned.rounds.getByRole('button',{name:'Load more review rounds',exact:true}).count() === 0);
  need(await pinned.attempts.getByRole('button',{name:'Load more attempts',exact:true}).count() === 0);
  for (const view of [current,pinned]) need(await view.page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  need(errors === 0 && external === 0 && forbiddenWrites === 0 && sessionPosts === 2 && archives.length === 20);
  await context.close(); await browser.close(); browser=undefined; await server.close(); server=undefined;
  console.log(JSON.stringify({state:'PASS',archive_requests:archives.length,max_limit:20,source_collisions:true,pinned_tail:true,current_reset:true,fixture_writes:1}));
} catch {
  console.log(JSON.stringify({state:'FAIL',phase})); process.exitCode=1;
} finally {
  if(browser) await browser.close().catch(() => {});
  if(server) await server.close().catch(() => {});
}
