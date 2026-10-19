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
  const archives = [], details = [];
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
      const detail = url.pathname.match(/\/reviews\/runs\/(review_run_[a-f0-9]{32})$/u);
      if (detail) {
        details.push({name,id:detail[1],snapshot:url.searchParams.get('snapshot')});
        if (!/^review_snapshot_[a-f0-9]{32}$/u.test(url.searchParams.get('snapshot') ?? '')) errors++;
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
    const rounds = page.locator('.main-content').getByRole('region',{name:`PR #${number} review rounds`,exact:true});
    const attempts = page.getByRole('region',{name:`PR #${number} attempts`,exact:true});
    await rounds.waitFor(); await attempts.waitFor();
    await expect(page.locator('.review-pull-summary .review-diff')).toHaveText('0 files · +0 / −0 lines');
    await expect(page.locator('.review-pull-summary .review-state-draft')).toHaveText('Draft');
    await attempts.locator('summary').click();
    await expect.poll(() => rounds.locator('.review-history-row').count()).toBe(20);
    await expect.poll(() => attempts.locator('li').count()).toBe(20);
    need(await rounds.getByRole('region',{name:'Hosted CodeRabbit',exact:true}).count() === 1);
    need(await rounds.getByRole('region',{name:'CLI CodeRabbit',exact:true}).count() === 1);
    need(await page.locator('.review-pull-summary').getByText(`${count} runs · ${count} attempts`,{exact:true}).count() === 0);
    await expect(rounds.getByRole('region',{name:'CLI CodeRabbit',exact:true}).locator('.review-lane-heading')).toContainText('20 runs shown');
    await expect(rounds.getByRole('region',{name:'Hosted CodeRabbit',exact:true}).locator('.review-lane-heading')).toContainText('0 runs shown');
    const stored = await page.evaluate(() => JSON.stringify({session:{...sessionStorage},local:{...localStorage}}));
    need(!stored.includes(token) && !stored.includes(admin));
    return {page,rounds,attempts,name,snapshot};
  };
  const requests = (view,feed) => archives.filter(item => item.name === view.name && item.feed === feed && (!view.snapshot || item.snapshot === view.snapshot));
  const more = async (view,feed,shown) => {
    const section = feed === 'runs' ? view.rounds : view.attempts;
    const before = requests(view,feed).length;
    await section.getByRole('button',{name:feed === 'runs' ? 'Load more review rounds' : 'Load more attempts',exact:true}).click();
    await expect.poll(() => section.locator(feed === 'runs' ? '.review-history-row' : 'li').count()).toBe(shown);
    need(requests(view,feed).length === before+1);
    if (feed === 'runs') await expect(section.getByRole('region',{name:'CLI CodeRabbit',exact:true}).locator('.review-lane-heading')).toContainText(`${shown} runs${shown < count ? ' shown' : ''}`);
  };
  // The real head summary shares the archive fixture. Check rendered geometry,
  // not CSS declarations: a no-overflow page can still crush one round into
  // a character-wide column through inherited mobile grid styles.
  const readableMergeHead = async page => {
    const original = page.viewportSize();
    try {
      const head = page.getByRole('region',{name:'Merge train head',exact:true});
      await head.waitFor();
      const pills = head.locator('.review-round-pill');
      await expect(pills).toHaveCount(3); // Three latest CLI rounds from the shared bounded page.
      await expect(head.getByRole('region',{name:'CLI CodeRabbit',exact:true}).locator('.review-lane-heading')).toContainText('20 runs loaded · latest 3');
      for (const width of [1440,390,320]) {
        await page.setViewportSize({width,height:900});
        need(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        const geometry = await head.evaluate(element => {
          const rect = node => { const box=node.getBoundingClientRect();return {left:box.left,right:box.right,top:box.top,bottom:box.bottom,width:box.width}; };
          const lineCount = node => { const range=document.createRange();range.selectNodeContents(node);return new Set([...range.getClientRects()].filter(box=>box.width>0&&box.height>0).map(box=>Math.round(box.top))).size; };
          const identity=document.querySelector('.project-identity');
          const project=identity.querySelector('.hero-project');
          const container=element.closest('.delivery-hero');
          const link=element.querySelector('.delivery-head-link');
          const linkStyle=getComputedStyle(link);
          return {head:rect(element),identity:rect(identity),project:rect(project),container:rect(container),link:rect(link),linkText:link.textContent,linkChildren:link.childElementCount,linkWeight:linkStyle.fontWeight,linkDecoration:linkStyle.textDecorationLine,joined:getComputedStyle(container).gap === 'normal' || getComputedStyle(container).gap === '0px',twoTone:getComputedStyle(element).backgroundColor !== getComputedStyle(container).backgroundColor || getComputedStyle(element).backgroundImage !== 'none',groups:[...element.querySelectorAll('.review-lane')].map(rect),pills:[...element.querySelectorAll('.review-round-pill')].map(pill=>({pill:rect(pill),stats:rect(pill.querySelector('strong')),date:rect(pill.querySelector('time')),statsLines:lineCount(pill.querySelector('strong')),dateLines:lineCount(pill.querySelector('time')),text:pill.querySelector('strong').textContent,dateText:pill.querySelector('time').textContent}))};
        });
        need(geometry.groups.length === 2);
        need(geometry.joined && geometry.twoTone);
        need(Math.abs((geometry.project.top+geometry.project.bottom)-(geometry.identity.top+geometry.identity.bottom)) <= 2);
        need(geometry.linkText === 'Review queue ↗' && geometry.linkChildren === 0 && geometry.linkWeight === '750' && geometry.linkDecoration.includes('underline'));
        need(geometry.link.right <= geometry.head.right && geometry.head.right-geometry.link.right <= 32 && geometry.link.bottom <= geometry.head.bottom && geometry.head.bottom-geometry.link.bottom <= 32);
        if (width === 1440) need(Math.abs(geometry.head.top-geometry.identity.top) <= 1 && Math.abs(geometry.head.left-geometry.identity.right) <= 1);
        else {
          // A phone must not retain the desktop panel's empty vertical space.
          // Allow taller actual content without fixing one exact CSS height.
          need(geometry.identity.bottom-geometry.identity.top <= Math.max(200,geometry.project.bottom-geometry.project.top+4));
          need(Math.abs(geometry.head.left-geometry.identity.left) <= 1 && Math.abs(geometry.head.right-geometry.identity.right) <= 1 && geometry.head.top >= geometry.identity.bottom);
          need(geometry.groups[1].top >= geometry.groups[0].bottom && Math.abs(geometry.groups[0].left-geometry.groups[1].left) <= 1 && Math.abs(geometry.groups[0].right-geometry.groups[1].right) <= 1);
        }
        for (const item of geometry.pills) {
          need(item.text === '0/0/0' && /^(?:Just now|Future date|\d+m ago|\d+h \d+m ago|\d+d \d+h ago)$/u.test(item.dateText));
          need(item.pill.width >= 60 && item.statsLines === 1 && item.dateLines === 1);
          need(item.stats.left >= item.pill.left && item.stats.right <= item.pill.right && item.date.left >= item.pill.left && item.date.right <= item.pill.right);
        }
      }
    } finally { await page.setViewportSize(original); }
  };
  phase = 'current-first-pages';
  const current = await connect('current');
  await readableMergeHead(current.page);
  need(requests(current,'runs').length === 1 && requests(current,'attempts').length === 1);
  need(details.length === 0); // Summary rendering does not collect every finding body.
  await more(current,'runs',40); await more(current,'attempts',40);
  phase = 'pinned-first-pages';
  const pinned = await connect('pinned',config.snapshot);
  need(requests(pinned,'runs').length === 1 && requests(pinned,'attempts').length === 1);
  await new Promise(resolve => setTimeout(resolve,100));
  need(requests(pinned,'runs').length === 1 && requests(pinned,'attempts').length === 1);
  need(details.length === 0);
  phase = 'inline-empty-findings';
  const pinnedRow = pinned.rounds.locator(`[data-run-id="${config.first_run_id}"]`);
  await pinnedRow.getByRole('button', {name:/^Show findings for cli run /}).click();
  await pinnedRow.getByText('No findings reported for this run.', {exact:true}).waitFor();
  need(details.length === 1 && details[0].name === 'pinned' && details[0].id === config.first_run_id && details[0].snapshot === config.snapshot);
  need(new URL(pinned.page.url()).pathname === path);
  await pinnedRow.getByRole('button', {name:/^Hide findings for cli run /}).click();
  need(await pinnedRow.locator('.review-history-findings').count() === 0 && details.length === 1);

  phase = 'metadata-refresh';
  const raw = await fs.readFile(config.new_observation_file,'utf8'); need(Buffer.byteLength(raw) <= 256<<10);
  const response = await fetch(config.origin+prefix+'/reviews/observations',{method:'POST',headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':config.worker,'Idempotency-Key':'packaged-archive-new-observation','Content-Type':'application/json'},body:raw,signal:AbortSignal.timeout(5000)});
  need(response.status === 201); await response.arrayBuffer();
  await expect.poll(() => current.rounds.locator('.review-history-row').count()).toBe(1);
  await expect.poll(() => current.attempts.locator('li').count()).toBe(1);
  await current.page.locator('.page-heading').getByRole('heading',{name:'Current archive metadata',exact:true}).waitFor();
  need(await current.rounds.getByRole('link',{name:/current-only-run/u}).count() === 1);
  need(await current.attempts.getByText('current-only-attempt',{exact:true}).count() === 1);
  need(await current.rounds.getByRole('button',{name:'Load more review rounds',exact:true}).count() === 0);
  need(await current.attempts.getByRole('button',{name:'Load more attempts',exact:true}).count() === 0);
  need(requests(current,'runs').filter(item => item.snapshot !== config.snapshot).length === 1);
  need(requests(current,'attempts').filter(item => item.snapshot !== config.snapshot).length === 1);
  phase = 'inline-current-findings';
  const currentRow = current.rounds.locator('.review-history-row');
  need(details.length === 1);
  await expect(currentRow.locator('.review-history-counts')).toHaveText('1 found · 0 accepted · 0 routed');
  await currentRow.getByRole('button', {name:'Show findings for cli run current-only-run', exact:true}).click();
  await currentRow.getByRole('heading', {name:'Inline retained finding', exact:true}).waitFor();
  await expect(currentRow.locator('.review-history-findings strong')).toHaveText('inline detail');
  await expect(currentRow.locator('.review-history-findings li').filter({hasText:'Kept in this PR'}).last()).toHaveText('Kept in this PR');
  need(await currentRow.locator('.review-history-findings img').count() === 0 && await currentRow.locator('.review-history-findings a[href^="javascript:"]').count() === 0);
  need(details.length === 2 && details[1].name === 'current' && details[1].snapshot !== config.snapshot && new URL(current.page.url()).pathname === path);
  await expect(current.page.locator('.review-pull-summary .review-diff')).toHaveText('73 files · +6,160 / −136 lines');
  need(await current.page.locator('.review-pull-summary .review-state-draft').count() === 0);
  await expect(pinned.page.locator('.review-pull-summary .review-diff')).toHaveText('0 files · +0 / −0 lines');
  await expect(pinned.page.locator('.review-pull-summary .review-state-draft')).toHaveText('Draft');
  if (config.proof_directory) {
    await fs.mkdir(config.proof_directory, {recursive:true});
    for (const width of [1440,390,320]) {
      await current.page.setViewportSize({width,height:900});
      need(await current.page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await current.page.screenshot({path:`${config.proof_directory}/expanded-history-${width}.png`,fullPage:true});
    }
    await current.page.setViewportSize({width:390,height:900});
  }

  await currentRow.getByRole('button', {name:'Hide findings for cli run current-only-run', exact:true}).click();
  need(await currentRow.locator('.review-history-findings').count() === 0 && details.length === 2);

  need(await pinned.rounds.locator('.review-history-row').count() === 20 && await pinned.attempts.locator('li').count() === 20);

  phase = 'pinned-continuation';
  for (const shown of [40,60,80,100,120,123]) await more(pinned,'runs',shown);
  for (const shown of [40,60,80,100,120,123]) await more(pinned,'attempts',shown);
  need(requests(pinned,'runs').every(item => item.snapshot === config.snapshot));
  need(requests(pinned,'attempts').every(item => item.snapshot === config.snapshot));
  const links = await pinned.rounds.locator('.review-history-details-link').evaluateAll(elements => elements.map(element => element.getAttribute('href')));
  need(new Set(links).size === count && links.some(link => link.includes(config.first_run_id)) && links.some(link => link.includes(config.tail_run_id)));
  need(await pinned.attempts.getByText(config.first_attempt_id,{exact:true}).count() === 1);
  const shared = pinned.attempts.locator('li').filter({hasText:config.tail_attempt_id});
  need(await shared.count() === 2 && await shared.getByText(config.source_a,{exact:true}).count() === 1 && await shared.getByText(config.source_b,{exact:true}).count() === 1);
  need(await pinned.rounds.getByRole('button',{name:'Load more review rounds',exact:true}).count() === 0);
  need(await pinned.attempts.getByRole('button',{name:'Load more attempts',exact:true}).count() === 0);
  for (const view of [current,pinned]) need(await view.page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  // The pinned detail retains its original snapshot while its current hero
  // reads one bounded page for the new comparison. Neither walks that archive.
  const pinnedCurrentHero = archives.filter(item => item.name === pinned.name && item.feed === 'runs' && item.snapshot !== config.snapshot);
  need(pinnedCurrentHero.length === 1 && pinnedCurrentHero[0].cursor === null && pinnedCurrentHero[0].limit === 20);
  need(errors === 0 && external === 0 && forbiddenWrites === 0 && sessionPosts === 2 && archives.length === 21 && details.length === 2);
  await context.close(); await browser.close(); browser=undefined; await server.close(); server=undefined;
  console.log(JSON.stringify({state:'PASS',archive_requests:archives.length,max_limit:20,source_collisions:true,pinned_tail:true,current_reset:true,inline_findings:true,detail_requests:details.length,fixture_writes:1}));
} catch {
  console.log(JSON.stringify({state:'FAIL',phase})); process.exitCode=1;
} finally {
  if(browser) await browser.close().catch(() => {});
  if(server) await server.close().catch(() => {});
}
