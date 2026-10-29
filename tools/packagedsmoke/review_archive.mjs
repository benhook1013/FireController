import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

// Only disposable synthetic fixture inputs enter this driver. Never print
// request headers, credentials, captured response bodies or raw diagnostics.
let server, browser, releaseRefresh;
let phase = 'configuration';
let selectedProjectStatus = 0;
const need = value => { if (!value) { const error = new Error('assertion'); Error.captureStackTrace(error,need); throw error; } };
try {
  const config = JSON.parse(await fs.readFile(process.argv[2], 'utf8'));
  const [admin, token] = await Promise.all([config.admin_file, config.credential_file].map(async file => (await fs.readFile(file, 'utf8')).trim()));
  const number = Number(config.pull_number), count = Number(config.count);
  need(Number.isSafeInteger(number) && number > 0 && count === 123);
  const prefix = `/api/v1/projects/${config.project}`;
  const path = `/projects/${config.project}/reviews/pulls/${number}`;
  const archives = [], details = [], queueReads = [], controlReads = [];
  let errors = 0, external = 0, forbiddenWrites = 0, sessionPosts = 0;
  phase = 'launch';
  server = await chromium.launchServer({ headless:true, ...(config.executable ? {executablePath:config.executable} : {}), timeout:5000, proxy:{server:config.proxy,bypass:'127.0.0.1,localhost'}, args:['--disable-background-networking','--no-first-run'] });
  console.log(JSON.stringify({event:'browser_started',pid:server.process().pid}));
  browser = await chromium.connect(server.wsEndpoint(),{timeout:5000});
  const contexts = [];
  const connect = async (name,snapshot) => {
    // Independent cookie/storage jars keep both explicit fixture logins and
    // prevent intentional same-context recovery from bypassing the second.
    const context = await browser.newContext({viewport:{width:390,height:900}});
    contexts.push(context);
    await context.route('**/*',async route => { if (new URL(route.request().url()).origin !== config.origin) { await route.abort(); return; } await route.continue(); });
    const page = await context.newPage(); page.setDefaultTimeout(5000); page.setDefaultNavigationTimeout(5000);
    page.on('pageerror',() => errors++);
    page.on('console',message => { if (message.type() === 'error') errors++; });
    page.on('response',response => { if (response.status() >= 400) errors++; });
    page.on('request',request => {
      const url = new URL(request.url());
      if (url.origin !== config.origin) { external++; return; }
      if (url.pathname === prefix+'/reviews/queue') queueReads.push({view:url.searchParams.get('view'),limit:Number(url.searchParams.get('limit')),cursor:url.searchParams.get('cursor')});
      if (url.pathname === prefix+'/reviews/fronts' || url.pathname === prefix+'/reviews/rounds/progress') controlReads.push(url.pathname);
      if (request.method() !== 'GET') {
        if (url.pathname === prefix+'/browser-session' && request.method() === 'POST') sessionPosts++;
        else if (!(url.pathname === prefix+'/browser-session' && request.method() === 'DELETE') && !(url.pathname === prefix+'/browser-session/renew' && request.method() === 'POST')) forbiddenWrites++;
      }
      const detail = url.pathname.match(/\/reviews\/runs\/(review_run_[a-f0-9]{32})$/u);
      if (detail) {
        details.push({name,id:detail[1],snapshot:url.searchParams.get('snapshot')});
        if (url.searchParams.get('view') !== 'display') errors++;
        if (!/^review_snapshot_[a-f0-9]{32}$/u.test(url.searchParams.get('snapshot') ?? '')) errors++;
      }
      const match = url.pathname.match(/\/reviews\/pulls\/[1-9][0-9]*\/(runs|attempts)$/u);
      if (match) {
        const limit = Number(url.searchParams.get('limit'));
        const view = url.searchParams.get('view'), channel = url.searchParams.get('channel');
        archives.push({name,feed:match[1],snapshot:url.searchParams.get('snapshot'),cursor:url.searchParams.get('cursor'),limit,view,channel});
        const bounded = match[1] === 'attempts' ? limit === 20 && view === null && channel === null : view === 'timeline' ? limit === 25 && channel === null : view === null && limit === 6 && ['cli','hosted','manual','subagent'].includes(channel);
        if (!bounded || !/^review_snapshot_[a-f0-9]{32}$/u.test(url.searchParams.get('snapshot') ?? '')) errors++;
      }
    });
    await page.goto(config.origin+path+(snapshot ? `?snapshot=${snapshot}` : ''));
    // The project-scoped sign-in form already binds this deep link's project.
    await page.getByLabel('Project credential',{exact:true}).fill(token);
    // Buffer this same project response before delivering it to the browser.
    // Chromium may retire a response's inspector body during canonical navigation;
    // that must not turn a successful login into an observer-only smoke failure.
    let identity, identityStatus;
    await page.route(config.origin+prefix, async route => {
      const response = await route.fetch({timeout:5000,maxRedirects:0,maxRetries:0});
      identityStatus = response.status(); identity = await response.json();
      await route.fulfill({response});
    }, {times:1});
    await page.getByRole('button',{name:'Connect to project',exact:true}).click();
    await expect.poll(() => identityStatus).toBe(200);
    selectedProjectStatus=identityStatus;
    need(identity.id === config.project && typeof identity.handle === 'string' && identity.handle.length > 0);
    const home = `/projects/${encodeURIComponent(identity.handle)}`, canonicalPath = `${home}/reviews/pulls/${number}`;
    await expect(page).toHaveURL(config.origin+canonicalPath+(snapshot ? `?snapshot=${snapshot}` : ''));
    const rounds = page.locator('.main-content').getByRole('region',{name:`PR #${number} review rounds`,exact:true});
    const attempts = page.getByRole('region',{name:`PR #${number} attempts`,exact:true});
    await rounds.waitFor(); await attempts.waitFor();
    await expect(page.locator('.review-detail-overview .review-diff')).toHaveText('0 files · +0 / −0 lines');
    await expect(page.locator('.review-detail-overview .review-state-draft')).toHaveText('Draft');
    await attempts.locator('summary').click();
    await expect.poll(() => rounds.locator('.review-history-row').count()).toBe(25);
    await expect.poll(() => attempts.locator('li').count()).toBe(20);
    await expect(rounds.locator('.review-timeline')).toHaveCount(1);
    need(await rounds.locator('.review-lanes').count() === 0);
    await expect(rounds.locator('.review-history-kind')).toHaveText(Array.from({length:25},(_,i)=>`CodeRabbit CLI #${count-24+i}`));
    await expect(rounds.locator('.section-heading')).toContainText(`${count} runs`);
    const continuation = rounds.locator('.review-timeline-continuation');
    await expect(continuation).toContainText(`25 of ${count} rounds`);
    need(await rounds.evaluate(section => section.querySelector('.review-timeline-continuation').compareDocumentPosition(section.querySelector('.review-timeline')) & Node.DOCUMENT_POSITION_FOLLOWING));
    need(await page.locator('.review-detail-overview').getByText(`${count} runs · ${count} attempts`,{exact:true}).count() === 0);
    const stored = await page.evaluate(() => JSON.stringify({session:{...sessionStorage},local:{...localStorage}}));
    need(!stored.includes(token) && !stored.includes(admin));
    return {page,rounds,attempts,name,snapshot,home,path:canonicalPath};
  };
  const requests = (view,feed) => archives.filter(item => item.name === view.name && item.feed === feed && (feed !== 'runs' || item.view === 'timeline') && (!view.snapshot || item.snapshot === view.snapshot));
  const more = async (view,feed,shown) => {
    const section = feed === 'runs' ? view.rounds : view.attempts;
    const before = requests(view,feed).length;
    await section.getByRole('button',{name:feed === 'runs' ? 'Load earlier rounds' : 'Load more attempts',exact:true}).click();
    await expect.poll(() => section.locator(feed === 'runs' ? '.review-history-row' : 'li').count()).toBe(shown);
    need(requests(view,feed).length === before+1);
    if (feed === 'runs') {
      await expect(section.locator('.section-heading')).toContainText(`${count} runs`);
      await expect(section.locator('.review-history-kind')).toHaveText(Array.from({length:shown},(_,i)=>`CodeRabbit CLI #${count-shown+1+i}`));
      if(shown<count) await expect(section.locator('.review-timeline-continuation')).toContainText(`${shown} of ${count} rounds`);
    }
  };
  // The real head summary shares the archive fixture. Check rendered geometry,
  // not CSS declarations: a no-overflow page can still crush one round into
  // a character-wide column through inherited mobile grid styles.
  const readableMergeHead = async page => {
    const original = page.viewportSize();
    try {
      const head = page.getByRole('region',{name:'Review queue head',exact:true});
      await head.waitFor();
      const pills = head.locator('.review-round-pill');
      await expect(pills).toHaveCount(6); // Shared queue row shows the latest six from the bounded archive page.
      await expect(head.getByRole('region',{name:'CLI CodeRabbit',exact:true}).locator('.review-lane-heading')).toContainText(`${Number(config.count)} runs`);
      for (const width of [1440,390,320]) {
        await page.setViewportSize({width,height:900});
        need(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        const geometry = await head.evaluate(element => {
          const rect = node => { const box=node.getBoundingClientRect();return {left:box.left,right:box.right,top:box.top,bottom:box.bottom,width:box.width}; };
          const lineCount = node => { const range=document.createRange();range.selectNodeContents(node);return new Set([...range.getClientRects()].filter(box=>box.width>0&&box.height>0).map(box=>Math.round(box.top))).size; };
          const identity=document.querySelector('.project-identity');
          const project=identity.querySelector('.hero-project');
          const container=element.closest('.delivery-hero');
          const link=element.querySelector('[aria-label="Jump to review queue"]');
          const linkStyle=getComputedStyle(link);
          return {head:rect(element),row:rect(element.querySelector('.review-queue-row')),identity:rect(identity),project:rect(project),container:rect(container),square:getComputedStyle(container).borderTopLeftRadius === '0px' && getComputedStyle(element).borderTopLeftRadius === '0px',roundedPills:[...element.querySelectorAll('.review-round-pill')].every(pill=>parseFloat(getComputedStyle(pill).borderTopLeftRadius)>0),link:rect(link),linkText:link.textContent,linkChildren:link.childElementCount,linkWeight:linkStyle.fontWeight,linkFamily:linkStyle.fontFamily,linkDecoration:linkStyle.textDecorationLine,joined:getComputedStyle(container).gap === 'normal' || getComputedStyle(container).gap === '0px',twoTone:getComputedStyle(element).backgroundColor !== getComputedStyle(container).backgroundColor || getComputedStyle(element).backgroundImage !== 'none',groups:[...element.querySelectorAll('.review-lane')].map(rect),pills:[...element.querySelectorAll('.review-round-pill')].filter(pill=>pill.getClientRects().length>0).map(pill=>({pill:rect(pill),stats:rect(pill.querySelector('strong')),date:rect(pill.querySelector('time')),statsLines:lineCount(pill.querySelector('strong')),dateLines:lineCount(pill.querySelector('time')),text:pill.querySelector('strong').textContent,dateText:pill.querySelector('time').textContent}))};
        });
        if(config.proof_directory){await fs.mkdir(config.proof_directory,{recursive:true,mode:0o700});await fs.writeFile(config.proof_directory+'/merge-head-geometry.json',JSON.stringify({width,head:geometry.head,row:geometry.row,link:geometry.link,weight:geometry.linkWeight,family:geometry.linkFamily,pills:geometry.pills.map(item=>({pill:item.pill,stats:item.stats,date:item.date,statsLines:item.statsLines,dateLines:item.dateLines}))}),{mode:0o600});}
        need(geometry.groups.length === 2 && geometry.pills.length>=1 && geometry.pills.length<=6);
        need(geometry.joined && geometry.twoTone && geometry.square && geometry.roundedPills);
        need(Math.abs((geometry.project.top+geometry.project.bottom)-(geometry.identity.top+geometry.identity.bottom)) <= 2);
        need(geometry.linkText === 'Review queue ↗' && geometry.linkChildren === 0 && geometry.linkWeight === '700' && geometry.linkFamily === 'ui-sans-serif, system-ui, sans-serif' && geometry.linkDecoration.includes('underline'));
        need(geometry.row.left >= geometry.head.left && geometry.row.right <= geometry.head.right && geometry.row.top >= geometry.head.top && geometry.row.bottom <= geometry.head.bottom);
        need(geometry.link.right <= geometry.row.right && geometry.row.right-geometry.link.right <= 32 && geometry.link.bottom <= geometry.row.bottom && geometry.row.bottom-geometry.link.bottom <= 32);
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
          need(item.pill.width >= Math.max(item.stats.width,item.date.width)+16 && item.statsLines === 1 && item.dateLines === 1);
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
  await more(current,'runs',50); await more(current,'attempts',40);
  phase = 'pinned-first-pages';
  const pinned = await connect('pinned',config.snapshot);
  need(requests(pinned,'runs').length === 1 && requests(pinned,'attempts').length === 1);
  await new Promise(resolve => setTimeout(resolve,100));
  need(requests(pinned,'runs').length === 1 && requests(pinned,'attempts').length === 1);
  need(details.length === 0);
  phase = 'inline-empty-findings';
  const pinnedRow = pinned.rounds.locator(`[data-run-id="${config.first_run_id}"]`);
  await pinnedRow.getByRole('button', {name:'Show findings for CodeRabbit CLI #123',exact:true}).click();
  await pinnedRow.getByText('No findings reported for this run.', {exact:true}).waitFor();
  need(details.length === 1 && details[0].name === 'pinned' && details[0].id === config.first_run_id && details[0].snapshot === config.snapshot);
  need(new URL(pinned.page.url()).pathname === pinned.path);
  await pinnedRow.getByRole('button', {name:'Hide findings for CodeRabbit CLI #123',exact:true}).click();
  need(await pinnedRow.locator('.review-history-findings').count() === 0 && details.length === 1);

  phase = 'metadata-refresh';
  // Hold the replacement summaries after the new queue metadata arrives.
  // Populated history and its original links must survive the transition.
  const refreshGate = new Promise(resolve => { releaseRefresh = resolve; });
  let heldRefreshReads = 0;
  const refreshPattern = config.origin+prefix+`/reviews/pulls/${number}/*`;
  await current.page.route(refreshPattern, async route => {
    const url = new URL(route.request().url());
    if (!/\/(runs|attempts)$/u.test(url.pathname) || url.searchParams.get('snapshot') === config.snapshot) return route.fallback();
    const response = await route.fetch();
    heldRefreshReads++;
    await refreshGate;
    await route.fulfill({response});
  });
  const raw = await fs.readFile(config.new_observation_file,'utf8'); need(Buffer.byteLength(raw) <= 256<<10);
  const response = await fetch(config.origin+prefix+'/reviews/observations',{method:'POST',headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':config.worker,'Idempotency-Key':'packaged-archive-new-observation','Content-Type':'application/json'},body:raw,signal:AbortSignal.timeout(5000)});
  need(response.status === 201); await response.arrayBuffer();
  await current.page.locator('.review-detail-overview').getByRole('heading',{name:`#${number} Current archive metadata`,exact:true}).waitFor();
  await expect.poll(() => heldRefreshReads).toBeGreaterThanOrEqual(3);
  await expect(current.rounds.locator('.review-history-row')).toHaveCount(50);
  await expect(current.attempts.locator('li')).toHaveCount(40);
  await expect(current.page.locator('.delivery-merge-head .review-round-pill')).toHaveCount(6);
  await expect(current.page.locator('.delivery-merge-head .review-round-placeholder')).toHaveCount(0);
  need(await current.rounds.locator('.review-history-details-link').evaluateAll((links,snapshot)=>links.every(link=>new URL(link.href).searchParams.get('snapshot')===snapshot),config.snapshot));
  releaseRefresh(); releaseRefresh=undefined;
  await current.page.unrouteAll({behavior:'wait'});
  await expect.poll(() => current.rounds.locator('.review-history-row').count()).toBe(1);
  await expect.poll(() => current.attempts.locator('li').count()).toBe(1);
  await current.page.locator('.review-detail-overview').getByRole('heading',{name:`#${number} Current archive metadata`,exact:true}).waitFor();
  await expect(current.rounds.locator('.review-history-kind')).toHaveText('CodeRabbit CLI #1');
  await expect(current.rounds.getByRole('link',{name:'Open CodeRabbit CLI #1 run details',exact:true})).toHaveCount(1);
  need(await current.attempts.getByText('current-only-attempt',{exact:true}).count() === 1);
  need(await current.rounds.getByRole('button',{name:'Load earlier rounds',exact:true}).count() === 0);
  need(await current.attempts.getByRole('button',{name:'Load more attempts',exact:true}).count() === 0);
  need(requests(current,'runs').filter(item => item.snapshot !== config.snapshot).length === 1);
  need(requests(current,'attempts').filter(item => item.snapshot !== config.snapshot).length === 1);
  phase = 'inline-current-findings';
  const currentRow = current.rounds.locator('.review-history-row');
  need(details.length === 1);
  await expect(currentRow.locator('.review-history-counts > span').first()).toHaveText('Found: 1');
  await expect(currentRow.locator('.review-count-accepted')).toHaveText('Accepted: 0/0/0');
  await expect(currentRow.locator('.review-history-counts > span').last()).toHaveText('Routed: 0');
  await currentRow.getByRole('button', {name:'Show findings for CodeRabbit CLI #1', exact:true}).click();
  await currentRow.getByRole('heading', {name:'Inline retained finding', exact:true}).waitFor();
  const finding = currentRow.locator('.review-finding-compact');
  await expect(finding.locator('.review-severity-low')).toHaveText('Minor');
  await expect(finding.locator('.review-finding-compact-rationale strong')).toHaveText('inline detail');
  await expect(finding.locator('.review-finding-compact-rationale li')).toHaveText('Kept in this PR');
  await expect(finding.locator('.review-finding-compact-decision')).toContainText('Reviewed source proof.');
  const disclosures = finding.locator(':scope > details');
  await expect(disclosures).toHaveCount(3);
  need(await disclosures.evaluateAll(items=>items.every(item=>!item.open)));
  const full = finding.locator('.review-finding-disclosure:not(.review-finding-technical):not(.review-finding-raw)');
  await full.locator('summary').click();
  await expect(full.locator('strong')).toHaveText('inline detail');
  await expect(full.locator('li')).toHaveText('Kept in this PR');
  const technical = finding.locator('.review-finding-technical');
  need(await technical.locator('dl').isVisible() === false);
  await technical.locator('summary').click();
  await expect(technical.getByText('Fixture reviewer',{exact:true})).toBeVisible();
  const originalFields = finding.locator('.review-finding-raw');
  need(await originalFields.locator('pre').isVisible() === false);
  await originalFields.locator('summary').click();
  await expect(originalFields.locator('pre')).toContainText('"severity": "low"');
  await expect(originalFields.locator('pre')).toContainText('Useful **inline detail**');
  need(await currentRow.locator('.review-history-findings img').count() === 0 && await currentRow.locator('.review-history-findings a[href^="javascript:"]').count() === 0);
  need(details.length === 2 && details[1].name === 'current' && details[1].snapshot !== config.snapshot && new URL(current.page.url()).pathname === current.path);
  await expect(current.page.locator('.review-detail-overview .review-diff')).toHaveText('73 files · +6,160 / −136 lines');
  need(await current.page.locator('.review-detail-overview .review-state-draft').count() === 0);
  await expect(pinned.page.locator('.review-detail-overview .review-diff')).toHaveText('0 files · +0 / −0 lines');
  await expect(pinned.page.locator('.review-detail-overview .review-state-draft')).toHaveText('Draft');
  if (config.proof_directory) {
    await fs.mkdir(config.proof_directory, {recursive:true});
    for (const width of [1440,390,320]) {
      await current.page.setViewportSize({width,height:900});
      need(await current.page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await current.page.screenshot({path:`${config.proof_directory}/expanded-history-${width}.png`,fullPage:true});
    }
    await current.page.setViewportSize({width:390,height:900});
  }

  await currentRow.getByRole('button', {name:'Hide findings for CodeRabbit CLI #1', exact:true}).click();
  need(await currentRow.locator('.review-history-findings').count() === 0 && details.length === 2);

  phase = 'recent-queue';
  const reviewPath = `${current.home}/reviews`;
  await current.page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Review queue', exact:true}).click();
  await expect.poll(() => {
    const url = new URL(current.page.url());
    return url.pathname + url.hash;
  }).toBe(`${current.home}#review-queue`);
  const queue = current.page.locator('#review-queue');
  await expect(queue.locator('.review-queue-row')).toHaveCount(5);
  need(JSON.stringify(await queue.locator('.review-queue-row').evaluateAll(rows=>rows.map(row=>row.id))) === JSON.stringify(['review-pr-123','review-pr-203','review-pr-204','review-pr-205','review-pr-206']));
  await expect(queue.locator('.review-count-key')).toContainText('last 48 hours and the latest two known merges');
  const provenance = queue.locator('.review-technical-details');
  need(await provenance.getAttribute('open') === null);
  need(await provenance.locator('dl').isVisible() === false && !(await provenance.innerText()).includes('As of'));
  const technicalReads=queueReads.length;
  await provenance.getByLabel('Technical details',{exact:true}).click();
  await expect(provenance.getByText('Snapshot origin timestamp',{exact:true})).toBeVisible();
  await expect(provenance.getByText('Saved locally',{exact:true})).toBeVisible();
  await current.page.keyboard.press('Escape');
  need(await provenance.getAttribute('open') === null && queueReads.length===technicalReads);
  need(await queue.locator('.review-pull-heading .badge').count() === 0);
  need(await queue.getByText('Open',{exact:true}).count() === 0);
  for (const merged of [203,204,206]) {
    const facts = queue.locator(`#review-pr-${merged} .review-pull-facts`);
    await expect(facts.locator(':scope > .review-state-merged')).toHaveCount(1);
    await expect(facts.locator(':scope > .review-state-merged')).toHaveText('Merged');
    await expect(facts.locator('.review-ci')).toHaveCount(0);
    need(await facts.locator('.review-mergeability').count()===0);
  }
  need(await queue.locator('#review-pr-205 .review-pull-facts > .badge:not(.review-fact-pill)').count() === 0);
  await expect(queue.locator('#review-pr-205 .review-ci')).toHaveText('Required CI: Success');
  await expect(queue.locator('#review-pr-205 .review-mergeability')).toHaveText('Merge: Unknown');
  need(await queue.locator('.review-queue-panel').evaluate(panel=>getComputedStyle(panel).borderTopLeftRadius === '0px'));
  if (config.proof_directory) for (const width of [1440,390]) {
    await current.page.setViewportSize({width,height:900});
    need(await current.page.evaluate(()=>document.documentElement.scrollWidth <= innerWidth));
    await current.page.screenshot({path:`${config.proof_directory}/recent-queue-${width}.png`,fullPage:true});
  }
  phase = 'merged-history';
  const controlsBefore = controlReads.length;
  await queue.locator('.delivery-section-heading').getByRole('link',{name:'Queue history ↗',exact:true}).click();
  await expect(queue.getByRole('heading',{name:'Queue history',exact:true})).toBeVisible();
  await expect(queue.locator('.review-queue-row')).toHaveCount(5);
  need(JSON.stringify(await queue.locator('.review-queue-row').evaluateAll(rows=>rows.map(row=>row.id))) === JSON.stringify(['review-pr-206','review-pr-204','review-pr-203','review-pr-202','review-pr-201']));
  await expect(queue.locator('#review-pr-201 .review-round-pill')).toHaveCount(1);
  need(controlReads.length === controlsBefore);
  need(queueReads.every(read=>read.limit === 20 && read.cursor === null && (read.view === 'recent' || read.view === 'merged')));
  need(queueReads.filter(read=>read.view === 'merged').length === 1);
  await expect(queue.getByRole('link',{name:'Back to review queue ↗',exact:true})).toHaveAttribute('href',reviewPath);
  // The server must serve this route on a direct reload, before the SPA runs.
  const reloaded = await current.page.reload();
  need(reloaded.status() === 200 && reloaded.headers()['content-type'].startsWith('text/html'));
  await expect(queue.locator('.review-queue-row')).toHaveCount(5);
  need(JSON.stringify(await queue.locator('.review-queue-row').evaluateAll(rows=>rows.map(row=>row.id))) === JSON.stringify(['review-pr-206','review-pr-204','review-pr-203','review-pr-202','review-pr-201']));
  // A fresh application also re-reads the current hero's two shared controls.
  await expect.poll(() => controlReads.length).toBe(controlsBefore + 2);
  need(queueReads.filter(read=>read.view === 'merged').length === 2);
  if (config.proof_directory) for (const width of [1440,390]) {
    await current.page.setViewportSize({width,height:900});
    need(await current.page.evaluate(()=>document.documentElement.scrollWidth <= innerWidth));
    await current.page.screenshot({path:`${config.proof_directory}/merged-queue-${width}.png`,fullPage:true});
  }
  await queue.locator('#review-pr-201').getByRole('link',{name:'Review history for PR #201',exact:true}).click();
  const original = current.page.locator('.main-content').getByRole('region',{name:'PR #201 review rounds',exact:true});
  await original.getByRole('button',{name:'Show findings for CodeRabbit CLI #1',exact:true}).click();
  await expect(original.getByRole('heading',{name:'Original merged finding',exact:true})).toBeVisible();
  await expect(original.locator('.review-finding-compact-rationale strong')).toHaveText('retained evidence');
  await original.locator('.review-finding-technical > summary').click();
  await expect(original.locator('.review-finding-technical')).toContainText('Original retained reviewer');
  need(details.length === 3 && details[2].snapshot !== config.snapshot);

  need(await pinned.rounds.locator('.review-history-row').count() === 25 && await pinned.attempts.locator('li').count() === 20);

  phase = 'pinned-continuation';
  for (const shown of [50,75,100,123]) await more(pinned,'runs',shown);
  for (const shown of [40,60,80,100,120,123]) await more(pinned,'attempts',shown);
  need(requests(pinned,'runs').every(item => item.snapshot === config.snapshot));
  need(requests(pinned,'attempts').every(item => item.snapshot === config.snapshot));
  const links = await pinned.rounds.locator('.review-history-details-link').evaluateAll(elements => elements.map(element => element.getAttribute('href')));
  need(new Set(links).size === count && links.at(-1).includes(config.first_run_id) && links[0].includes(config.tail_run_id));
  need(await pinned.attempts.getByText(config.first_attempt_id,{exact:true}).count() === 1);
  const shared = pinned.attempts.locator('li').filter({hasText:config.tail_attempt_id});
  need(await shared.count() === 2 && await shared.getByText(config.source_a,{exact:true}).count() === 1 && await shared.getByText(config.source_b,{exact:true}).count() === 1);
  need(await pinned.rounds.getByRole('button',{name:'Load earlier rounds',exact:true}).count() === 0);
  need(await pinned.attempts.getByRole('button',{name:'Load more attempts',exact:true}).count() === 0);
  for (const view of [current,pinned]) need(await view.page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  // The pinned detail retains its original snapshot while its current hero
  // reads one bounded page for the new comparison. Neither walks that archive.
  const pinnedCurrentHero = archives.filter(item => item.name === pinned.name && item.feed === 'runs' && item.channel === 'cli' && item.snapshot !== config.snapshot);
  need(pinnedCurrentHero.length === 1 && pinnedCurrentHero[0].cursor === null && pinnedCurrentHero[0].limit === 6);
  // Reload restores the current hero and may invalidate its first page once
  // when the live workspace snapshot arrives. Neither read follows a cursor.
  need(errors === 0 && external === 0 && forbiddenWrites === 0 && sessionPosts === 2 && archives.length >= 26 && archives.length <= 27 && details.length === 3);
  await Promise.all(contexts.map(context=>context.close())); await browser.close(); browser=undefined; await server.close(); server=undefined;
  console.log(JSON.stringify({state:'PASS',archive_requests:archives.length,max_limit:25,source_collisions:true,pinned_tail:true,current_reset:true,inline_findings:true,queue_window:true,merged_history:true,detail_requests:details.length,fixture_writes:1}));
} catch (error) {
  // Retain only the fixed phase and bounded source line, never error text.
  const line = /review_archive\.mjs:(\d+):\d+/u.exec(error?.stack ?? '');
  const driverLine = line ? Number(line[1]) : 0;
  console.log(JSON.stringify({state:'FAIL',phase,project_status:selectedProjectStatus,failure_kind:error?.name === 'SyntaxError' ? 'invalid_json' : error?.name === 'TimeoutError' ? 'timeout' : 'assertion',driver_line:Number.isInteger(driverLine) && driverLine > 0 && driverLine <= 10000 ? driverLine : 0})); process.exitCode=1;
} finally {
  releaseRefresh?.();
  if(browser) await browser.close().catch(() => {});
  if(server) await server.close().catch(() => {});
}
