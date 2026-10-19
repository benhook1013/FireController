import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';
import {writeFileSync, renameSync} from 'node:fs';

// All inputs are synthetic, private files produced by browser_test.go. Neither
// Playwright diagnostics nor request/credential bytes are printed on failure.
let server, browser;
let phase = 'configuration';
let checkpoint = '';
let operation = 'workload';
let publishProgress = () => {};
let stopProgress = () => {};
let progressFailed = false;
let counters = () => ({});
let failureClass = '';
let workerHistoryBefore = -1, workerHistoryAfter = -1;
let sendStatus = 0, replyStatus = 0, conversationMatch = false;
let lastHTTPStatus = 0;
let lastAllowWrites = false, lastCanEditPresentation = false;
let renewalProof = { renewal_response_received:false, renewal_response_ok:false, current_read_ok:false, current_shape_valid:false, current_identity_matches:false, current_read_only:false, current_expiry_valid:false, current_expiry_nondecreasing:false, storage_matches_current:false };
let failureSnapshot = async () => ({});
let diagnostics = () => ({});
const need = (value) => { if (!value) throw new Error('assertion'); };
try {
  const config = JSON.parse(await fs.readFile(process.argv[2], 'utf8'));
  need(typeof config.progress_file === 'string' && config.progress_file.startsWith('/'));
  let previousProgress = '';
  publishProgress = () => {
    const observed = {phase, checkpoint, operation, counters:{...counters(), ...diagnostics(), http_status:lastHTTPStatus, allow_writes:lastAllowWrites, can_edit_presentation:lastCanEditPresentation}};
    const fingerprint = JSON.stringify(observed);
    if (fingerprint === previousProgress) return;
    try {
      writeFileSync(config.progress_file + '.tmp', JSON.stringify({...observed, observed_unix_ms:Date.now()}), {mode:0o600});
      renameSync(config.progress_file + '.tmp', config.progress_file);
      previousProgress = fingerprint;
    } catch { progressFailed = true; }
  };
  publishProgress();
  const progressTimer = setInterval(publishProgress, 250);
  progressTimer.unref();
  stopProgress = () => clearInterval(progressTimer);
  const [admin, token] = await Promise.all([config.admin_file, config.credential_file].map(async path => (await fs.readFile(path, 'utf8')).trim()));
  phase = 'launch';
  server = await chromium.launchServer({ headless: true, ...(config.executable ? { executablePath: config.executable } : {}), timeout: 5000, proxy: { server: config.proxy, bypass: '127.0.0.1,localhost' }, args: ['--disable-background-networking', '--no-first-run'] });
  console.log(JSON.stringify({event: 'browser_started', pid: server.process().pid}));
  browser = await chromium.connect(server.wsEndpoint(), {timeout: 5000});
  const context = await browser.newContext({ viewport: { width: 390, height: 900 } });
  const page = await context.newPage();
  await page.clock.install({time: new Date()});
  page.setDefaultTimeout(5000);
  page.setDefaultNavigationTimeout(5000);
  const prefix = '/api/v1/projects/' + config.project;
  const home = '/projects/' + config.project;
  const bindingKey = 'firecontroller.browser-session.v1';
  const loginAuthority = async (loginResponse, allowWrites, canEditPresentation) => {
    const posted = loginResponse.request().postDataJSON();
    need(posted && Object.keys(posted).length === 1 && typeof posted.session_id === 'string' && /^[a-f0-9]{32}$/u.test(posted.session_id));
    // The page has accepted this login; inspect actual cookie authority rather
    // than an unbounded CDP response-finished/body observer.
    const current = await page.evaluate(async ({prefix, bindingKey, sessionID, project, instance}) => {
      const binding = JSON.parse(sessionStorage.getItem(bindingKey));
      if (binding?.sessionID !== sessionID || binding.projectID !== project || binding.instanceID !== instance) throw new Error('login session binding differs');
      const response = await fetch(prefix + '/browser-session', {headers:{'X-FC-Browser-Session':sessionID, 'X-FC-Instance':instance}, credentials:'same-origin', cache:'no-store', redirect:'error', signal:AbortSignal.timeout(5000)});
      return {status:response.status, view:response.status === 200 ? await response.json() : null, binding};
    }, {prefix, bindingKey, sessionID:posted.session_id, project:config.project, instance:config.instance});
    const view = current.view, binding = current.binding;
    const fields = ['session_id', 'instance_id', 'project_id', 'expires_at', 'allow_writes', 'can_edit_presentation'];
    const bindingFields = ['version', 'projectID', 'instanceID', 'sessionID', 'expiresAt', 'selectedworkerID'];
    need(current.status === 200 && view && typeof view === 'object' && !Array.isArray(view) && Object.keys(view).length === fields.length && fields.every(field => Object.hasOwn(view, field)));
    need(view.session_id === posted.session_id && view.project_id === config.project && view.instance_id === config.instance && view.allow_writes === allowWrites && view.can_edit_presentation === canEditPresentation);
    need(typeof view.expires_at === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/u.test(view.expires_at) && Number.isFinite(Date.parse(view.expires_at)) && Date.parse(view.expires_at) > Date.now() && Date.parse(view.expires_at) <= Date.now() + 86400000 + 300000);
    need(binding.version === 1 && Object.keys(binding).length === bindingFields.length && bindingFields.every(field => Object.hasOwn(binding, field)) && binding.expiresAt === view.expires_at && typeof binding.selectedworkerID === 'string' && (binding.selectedworkerID === '' || /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/u.test(binding.selectedworkerID)));
    return view;
  };

  failureSnapshot = async () => {
    if (phase === 'live-reviews') {
      const snapshot = {
        updated_pull: await page.getByRole('link',{name:'Live review updated',exact:true}).count(),
        hosted_run_link: await page.getByRole('link',{name:'Open hosted run live-hosted-round',exact:true}).count(),
        history_sections: await page.locator('.review-history').count(),
        hosted_sections: await page.getByRole('region',{name:'Hosted CodeRabbit',exact:true}).count(),
        review_alerts: await page.getByRole('alert').count(),
        run_fetches: reviewRunReads,
      };
      if (config.diagnostic_directory) {
        await fs.mkdir(config.diagnostic_directory,{recursive:true,mode:0o700});
        await page.screenshot({path:config.diagnostic_directory+'/review-failure.png',mask:[page.locator('input,textarea,code,.full-text,.message-preview')],timeout:2000});
        await fs.writeFile(config.diagnostic_directory+'/review-failure.json',JSON.stringify({phase,checkpoint,...snapshot}),{mode:0o600});
      }
      return snapshot;
    }
    if (phase === 'delivery-overview') {
      // Persist local fixed counters first: optional DOM/indicator/screenshot
      // failures must not erase the checkpoint that explains the real failure.
      const snapshot = {
        fixture_writes:fixtureWrites, action_writes:actionWrites, forbidden_writes:forbiddenWrites,
        delivery_reads:deliveryReads, job_brief_reads:jobBriefReads, job_archive_reads:jobArchiveReads,
        presence_reads:presenceReads, presence_finished:presenceFinished, presence_header_errors:presenceHeaderErrors,
        contact_history_reads:contactHistoryReads, node_clock_ms:Date.now(),
        worker_history_before: workerHistoryBefore, worker_history_after: workerHistoryAfter,
      };
      const persist = async () => {
        if (!config.diagnostic_directory) return;
        await fs.mkdir(config.diagnostic_directory,{recursive:true,mode:0o700});
        await fs.writeFile(config.diagnostic_directory+'/delivery-failure.json',JSON.stringify({phase,checkpoint,...snapshot}),{mode:0o600});
      };
      await persist();
      try {
        snapshot.browser_clock_ms=await page.evaluate(()=>Date.now());
        snapshot.browser_hidden=await page.evaluate(()=>document.visibilityState==='hidden');
        snapshot.delivery_heading=await page.getByRole('heading',{name:'Delivery',exact:true}).count();
        snapshot.inspector_lane=await page.locator('.delivery-lane[aria-label="Inspector jobs"]').count();
        snapshot.active_job=await page.getByText('Inspector active work',{exact:true}).count();
        snapshot.latest_done=await page.locator('.delivery-checkpoint p').filter({hasText:'Latest checkpoint is visible.'}).count();
        snapshot.latest_next=await page.locator('.delivery-checkpoint p').filter({hasText:'Continue from the latest checkpoint.'}).count();
        snapshot.earlier_done=await page.locator('.delivery-checkpoint p').filter({hasText:'Earlier checkpoint only.'}).count();
        snapshot.inbox_sidebar=await page.locator('.sidebar').count();
        snapshot.inbox_heading=await page.getByRole('heading',{name:'Builder inbox',exact:true}).count();
        snapshot.readonly_new_message=await page.getByRole('button',{name:'New message',exact:true}).count();
        snapshot.contact_labels=await page.locator('.worker-contact').count();
        if (await page.locator('.feed-card .status-dot').count()) snapshot.indicators=await indicatorColors();
      } catch { snapshot.dom_diagnostics_unavailable=true; }
      await persist();
      if (config.diagnostic_directory) await page.screenshot({path:config.diagnostic_directory+'/delivery-failure.png',mask:[page.locator('input,textarea,code,.full-text,.message-preview')],timeout:2000}).catch(()=>{});
      return snapshot;
    }
    if (phase === 'reload') {
      if (config.diagnostic_directory) {
        await fs.mkdir(config.diagnostic_directory,{recursive:true,mode:0o700});
        await fs.writeFile(config.diagnostic_directory+'/reload-failure.json',JSON.stringify({phase,checkpoint,...renewalProof}),{mode:0o600});
      }
      return renewalProof;
    }
    if (phase !== 'explicit-actions-uncertain') return {};
    const snapshot = {
      sent_links: await page.getByRole('link',{name:'Open sent conversation',exact:true}).count(),
      retry_buttons: await page.getByRole('button',{name:'Retry exact send',exact:true}).count(),
      send_buttons: await page.getByRole('button',{name:'Send message',exact:true}).count(),
      composer_forms: await page.locator('.browser-composer form').count(),
      message_matches: await page.getByText('Uncertain browser message',{exact:true}).count(),
      conversation_items: await page.locator('.conversation-list > li').count(),
      alerts: await page.getByRole('alert').count(),
      inbox_route: new URL(page.url()).pathname === home+'/inbox',
      conversation_route: new URL(page.url()).pathname.startsWith(home+'/inbox/'),
    };
    if (config.diagnostic_directory) {
      await fs.mkdir(config.diagnostic_directory,{recursive:true,mode:0o700});
      await page.screenshot({path:config.diagnostic_directory+'/failure.png', mask:[page.locator('input,textarea,code,.full-text,.message-preview')], timeout:2000});
      await fs.writeFile(config.diagnostic_directory+'/failure.json',JSON.stringify({phase,checkpoint,...snapshot}),{mode:0o600});
    }
    return snapshot;
  };
  let fixtureWrites = 0;
  const write = async (method, path, body, status = 200, sender = config.worker) => {
    const response = await fetch(config.origin + prefix + path, {
      method, headers: {'Authorization':'Bearer '+admin, 'X-FC-Instance':config.instance, 'X-FC-Worker':sender, 'Idempotency-Key':'packaged-browser-write-' + (fixtureWrites + 1), 'Content-Type':'application/json'},
      body:JSON.stringify(body), signal:AbortSignal.timeout(5000),
    });
    need(response.status === status);
    const receipt = await response.json();
    need(receipt && Object.keys(receipt).length > 0);
    fixtureWrites++;
    return receipt;
  };
  let deliveryFixtureWrites = 0;
  const seedDelivery = async (method, path, body, status = 200, sender = config.worker) => {
    const response = await fetch(config.origin + prefix + path, {
      method, headers: {'Authorization':'Bearer '+admin, 'X-FC-Instance':config.instance, 'X-FC-Worker':sender, 'Idempotency-Key':'packaged-delivery-fixture-' + (++deliveryFixtureWrites), 'Content-Type':'application/json'},
      body:JSON.stringify(body), signal:AbortSignal.timeout(5000),
    });
    need(response.status === status);
    const receipt = await response.json();
    need(receipt && Object.keys(receipt).length > 0);
    return receipt;
  };
  const deliveryFixture = { jobs: {} };
  deliveryFixture.jobs.builderActive = (await seedDelivery('POST', '/jobs', {title:'Builder primary delivery', brief:'Private delivery identifier never rendered.', worker_id:config.worker}, 201)).id;
  deliveryFixture.jobs.inspectorActive = (await seedDelivery('POST', '/jobs', {title:'Inspector active work', brief:'Fixture brief.', worker_id:config.other_worker}, 201)).id;
  deliveryFixture.jobs.inspectorBlocked = (await seedDelivery('POST', '/jobs', {title:'Inspector blocked work', brief:'Fixture brief.', worker_id:config.other_worker}, 201)).id;
  deliveryFixture.jobs.inspectorParked = (await seedDelivery('POST', '/jobs', {title:'Inspector parked work', brief:'Fixture brief.', worker_id:config.other_worker}, 201)).id;
  deliveryFixture.jobs.unassigned = (await seedDelivery('POST', '/jobs', {title:'Unassigned parked work', brief:'Fixture brief.', worker_id:''}, 201)).id;
  await seedDelivery('PATCH', '/jobs/' + deliveryFixture.jobs.inspectorBlocked, {expected_revision:1, status:'blocked'});
  await seedDelivery('PATCH', '/jobs/' + deliveryFixture.jobs.inspectorParked, {expected_revision:1, status:'parked'});
  await seedDelivery('PATCH', '/jobs/' + deliveryFixture.jobs.unassigned, {expected_revision:1, status:'parked'});
  await seedDelivery('POST', '/jobs/' + deliveryFixture.jobs.inspectorActive + '/checkpoints', {expected_job_revision:1, done:'Earlier checkpoint only.', next:'Earlier next step.'}, 201);
  await seedDelivery('POST', '/jobs/' + deliveryFixture.jobs.inspectorActive + '/checkpoints', {expected_job_revision:1, done:'Latest checkpoint is visible.', next:'Continue from the latest checkpoint.'}, 201);
  await seedDelivery('PUT', '/workers/' + config.other_worker + '/pause', {expected_revision:0, paused:true, reason:'Paused for synthetic dependency review.'});
  await seedDelivery('PUT', '/workers/' + config.worker + '/primary-job', {expected_revision:0, job_id:deliveryFixture.jobs.builderActive});
  let sessionPosts = 0, sessionDeletes = 0, sessionRenewals = 0, terminalResponses = 0, forbiddenWrites = 0, actionWrites = 0, errors = 0, external = 0;
 let explicitActions = false;
 let controlledMessageFailure = false;
 let controlledAccessFailure = false;
 let controlledProgressAccessFailure = false, progressAccessFailures = 0;
 const expectedMissingSessionRoute = url => { if ((!controlledAccessFailure && !controlledProgressAccessFailure) || !url) return false; try { const value = new URL(url); return value.origin === config.origin && ((controlledAccessFailure && ['/api/v1/instance', prefix + '/browser-session'].includes(value.pathname)) || (controlledProgressAccessFailure && value.pathname === prefix + '/reviews/rounds/progress')); } catch { return false; } };
  counters = () => ({session_posts:sessionPosts, session_deletes:sessionDeletes, session_renewals:sessionRenewals, terminal_responses:terminalResponses, errors, forbidden_writes:forbiddenWrites, action_writes:actionWrites, external});
  let presentationReads = 0, metadataStreams = 0, inboxStreams = 0, injectedResponses = 0, injectedConsoleErrors = 0, noteArchiveReads = 0;
  let deliveryReads = 0, jobBriefReads = 0, jobArchiveReads = 0, reviewRunReads = 0;
  let presenceReads = 0, presenceFinished = 0, presenceHeaderErrors = 0, contactHistoryReads = 0;
  let injectedRequest;
  diagnostics = () => ({ presentation_reads:presentationReads, metadata_streams:metadataStreams, inbox_streams:inboxStreams, injected_metadata_failures:injectedResponses, injected_console_errors:injectedConsoleErrors, errors, external, presence_reads:presenceReads, presence_finished:presenceFinished, presence_header_errors:presenceHeaderErrors, delivery_reads:deliveryReads, job_brief_reads:jobBriefReads, job_archive_reads:jobArchiveReads, contact_history_reads:contactHistoryReads });
  page.on('pageerror', () => errors++);
  page.on('console', value => {
    if (value.type() !== 'error' || expectedMissingSessionRoute(value.location().url) || (controlledMessageFailure && value.location().url === config.origin + prefix + '/messages')) return;
    if (injectedRequest && injectedConsoleErrors === 0 && value.location().url === injectedRequest.url() && /^Failed to load resource: the server responded with a status of 400 \(Bad Request\)$/u.test(value.text())) injectedConsoleErrors++;
    else errors++;
  });
  page.on('response', value => { if (value.status() >= 400) { if (value.status() === 401 && expectedMissingSessionRoute(value.url())) terminalResponses++; else if (value.request() === injectedRequest && value.status() === 400) injectedResponses++; else errors++; } });
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.origin !== config.origin) { external++; return; }
    if (request.method() === 'GET') {
      if (url.pathname === prefix + '/workers/presence') {
        presenceReads++;
        const headers=request.headers();
        if (Object.hasOwn(headers,'x-fc-worker') || Object.hasOwn(headers,'x-fc-session')) presenceHeaderErrors++;
      }
      if (/\/(?:history|runs|attempts)(?:\/|$)/u.test(url.pathname.slice(prefix.length))) contactHistoryReads++;
      if (url.pathname === prefix + '/presentation') presentationReads++;
      if (url.pathname === prefix + '/notes' && url.searchParams.get('status') === 'all') noteArchiveReads++;
      if (url.pathname === prefix + '/delivery') deliveryReads++;
      if (url.pathname === prefix + '/reviews/pulls/32/runs') reviewRunReads++;
      if (url.pathname === prefix + '/jobs' && url.searchParams.get('status') === 'all') jobArchiveReads++;
      if (/\/jobs\/[^/]+$/u.test(url.pathname.slice(prefix.length))) jobBriefReads++;
      if (url.pathname === prefix + '/project-events/stream') metadataStreams++;
      if (url.pathname === prefix + '/message-events/stream') inboxStreams++;
      return;
    }
    if (url.pathname === prefix + '/browser-session' && request.method() === 'POST') {
      sessionPosts++;
      try { const body=JSON.parse(request.postData()); if (Object.keys(body).length!==1 || typeof body.session_id!=='string' || !body.session_id) forbiddenWrites++; }
      catch { forbiddenWrites++; }
    }
    else if (url.pathname === prefix + '/browser-session/renew' && request.method() === 'POST') { sessionRenewals++; if (request.postData() !== '{}' || request.headers()['authorization'] || request.headers()['x-fc-instance'] !== config.instance) forbiddenWrites++; }
    else if (url.pathname === prefix + '/browser-session' && request.method() === 'DELETE') sessionDeletes++;
    else if (explicitActions && ((request.method() === 'POST' && (url.pathname === prefix + '/messages' || /^messages\/[^/]+\/(?:seen|ack)$/.test(url.pathname.slice(prefix.length + 1)))) || (request.method() === 'PUT' && url.pathname === prefix + '/presentation'))) actionWrites++;
    else forbiddenWrites++;
  });
  page.on('requestfinished', request => {
    if (request.method()==='GET' && request.url().startsWith(config.origin+prefix+'/workers/presence?')) presenceFinished++;
  });
  await context.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== config.origin) { await route.abort(); return; }
    if (!injectedRequest && route.request().method() === 'GET' && url.pathname === prefix + '/project-events/stream') {
      injectedRequest = route.request();
      await route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ error: { code: 'invalid_response' } }) });
      return;
    }
    await route.continue();
  });
  const appearance = () => page.evaluate(() => ({
    title: document.title, brand: document.querySelector('.brand')?.textContent.trim(),
    background: getComputedStyle(document.documentElement).backgroundColor,
    overrides: [...document.documentElement.style].filter(name => name.startsWith('--fc-')),
    accent: document.documentElement.style.getPropertyValue('--fc-accent_text'),
  }));
  const indicatorColors = () => page.evaluate(() => ({
    connection: getComputedStyle(document.querySelector('.mast .header-utilities .connection-note'), '::before').backgroundColor,
    connectionBorder: getComputedStyle(document.querySelector('.mast .header-utilities .connection-note'), '::before').borderColor,
    connectionText: getComputedStyle(document.querySelector('.mast .header-utilities .connection-note')).color,
    canvas: getComputedStyle(document.documentElement).backgroundColor,
    header: getComputedStyle(document.querySelector('.mast')).backgroundColor,
    connectionSurface: getComputedStyle(document.querySelector('.mast')).backgroundColor,
    status: getComputedStyle(document.querySelector('.feed-card .status-dot')).backgroundColor,
    online: document.querySelector('.feed-card .status-dot').classList.contains('online'),
    surface: getComputedStyle(document.querySelector('.feed-card')).backgroundColor,
  }));
  const captureLayouts = async (name, widths) => {
    for (const width of widths) {
      await page.setViewportSize({width, height:900});
      need(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      need(await page.locator('.mast .header-utilities .connection-note').count() === 1);
      need(await page.locator('.connection-note').count() === 1);
      need(await page.locator('.connection-card .connection-note').count() === 0);
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      need(await page.locator('.mast').evaluate(element => Math.abs(element.getBoundingClientRect().top) <= 1));
      await page.evaluate(() => window.scrollTo(0, 0));
      need(await page.locator('.mast').evaluate(element => Math.abs(element.getBoundingClientRect().top) <= 1));
      await expect(page.getByRole('button', {name:'Refresh visible data', exact:true})).toBeVisible();
      if (config.diagnostic_directory) {
        await fs.mkdir(config.diagnostic_directory, {recursive:true, mode:0o700});
        await page.screenshot({path:config.diagnostic_directory + '/' + name + '-' + width + '.png', fullPage:true, timeout:2000});
      }
    }
    await page.setViewportSize({width:390, height:900});
  };
  const defaults = async () => {
    const value = await appearance();
    need(value.brand === 'FireController' && value.background === 'rgb(232, 231, 228)' && value.overrides.length === 0);
  };
  phase = 'connect';
  checkpoint = 'root';
  await page.goto(config.origin);
  checkpoint = 'project-input';
  await page.getByLabel('Project ID', {exact:true}).fill(config.project);
  checkpoint = 'credential-input';
  await page.getByLabel('Project credential', {exact:true}).fill(token);
  need(await page.getByRole('checkbox').count() === 0);
  checkpoint = 'submit';
  const readOnlyLogin=page.waitForResponse(response=>response.request().method()==='POST' && new URL(response.url()).pathname===prefix+'/browser-session');
  await page.getByRole('button', {name:'Connect to project', exact:true}).click();
  checkpoint = 'session-response';
  const readOnlyResponse=await readOnlyLogin; lastHTTPStatus=readOnlyResponse.status();
  checkpoint = 'session-status';
  need(lastHTTPStatus===200);
  checkpoint = 'heading';
  await page.getByRole('heading', {name:'Packaged smoke', exact:true}).waitFor();
  checkpoint = 'capabilities-read';
  const readOnlyCapabilities=await loginAuthority(readOnlyResponse, false, false);
  lastAllowWrites=readOnlyCapabilities.allow_writes===true; lastCanEditPresentation=readOnlyCapabilities.can_edit_presentation===true;
  checkpoint = 'capabilities-check';
  need(readOnlyCapabilities.allow_writes===false && readOnlyCapabilities.can_edit_presentation===false);

  need(await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Settings',exact:true}).count()===0);
  need(await page.locator('.sidebar').count() === 0);
  need(await page.locator('.mast .header-utilities .connection-note').count() === 1);
  need(await page.locator('.connection-note').count() === 1);
  need(await page.locator('.connection-card .connection-note').count() === 0);
  await expect(page.locator('.page-skeleton')).toHaveCount(0);
  phase = 'delivery-overview'; checkpoint = 'initial-snapshot';
  await page.getByRole('heading', {name:'Delivery', exact:true}).waitFor();
  checkpoint = 'delivery-heading';
  const builderLane = page.locator('.delivery-lane[aria-label="Builder jobs"]');
  const inspectorLane = page.locator('.delivery-lane[aria-label="Inspector jobs"]');
  const unassignedLane = page.locator('.delivery-lane[aria-label="Unassigned jobs"]');
  await expect(builderLane.getByText('Builder primary delivery', {exact:true})).toBeVisible();
  checkpoint = 'builder-job';
  await expect(builderLane.getByText('Primary focus', {exact:true})).toBeVisible();
  checkpoint = 'builder-primary';
  await expect(inspectorLane.getByText('Paused', {exact:true})).toBeVisible();
  checkpoint = 'inspector-paused';
  await expect(inspectorLane.getByText('Paused for synthetic dependency review.', {exact:true})).toBeVisible();
  checkpoint = 'inspector-pause-reason';
  await expect(unassignedLane.getByText('Unassigned parked work', {exact:true})).toBeVisible();
  checkpoint = 'unassigned-job';
  const inspectorStatuses = await inspectorLane.locator('.delivery-job-state .badge').allTextContents();
  need(inspectorStatuses.join('|') === 'Active|Blocked|Parked');
  checkpoint = 'status-order';
  checkpoint = 'latest-checkpoint-done';
  const latestDone = inspectorLane.locator('.delivery-checkpoint p').filter({hasText:'Latest checkpoint is visible.'});
  await expect(latestDone).toHaveCount(1);
  await expect(latestDone).toContainText('Latest checkpoint is visible.');
  checkpoint = 'latest-checkpoint-next';
  const latestNext = inspectorLane.locator('.delivery-checkpoint p').filter({hasText:'Continue from the latest checkpoint.'});
  await expect(latestNext).toHaveCount(1);
  await expect(latestNext).toContainText('Continue from the latest checkpoint.');
  checkpoint = 'latest-checkpoint';
  need(await inspectorLane.locator('.delivery-checkpoint p').filter({hasText:'Earlier checkpoint only.'}).count() === 0);
  checkpoint = 'old-checkpoint-absent';
  need(deliveryReads === 1 && jobBriefReads === 0 && jobArchiveReads === 0);
  checkpoint = 'overview-request-budget';
  need(await page.locator('.delivery-lane').count() === 3);
  checkpoint = 'lane-count';
  need(await page.getByRole('button', {name:/^(?:Previous|Next) (?:workers|jobs)$/, exact:true}).count() === 0);
  need(await page.getByRole('button', {name:'Load more work', exact:true}).count() === 0);
  const deliveryBodyText = await page.locator('body').innerText();
  need(![config.project, config.worker, config.other_worker, ...Object.values(deliveryFixture.jobs)].some(id => deliveryBodyText.includes(id)));
  checkpoint = 'private-identifiers-absent';
  // Actual mounted contact query: one shared read for all loaded lanes, no
  // browser agent attribution, and no fleet/board/history refresh while hidden.
  checkpoint = 'presence-initial';
  await expect.poll(() => presenceFinished, {timeout:5000}).toBe(1);
  need(presenceReads===1 && presenceHeaderErrors===0);
  await expect(page.locator('.worker-contact')).toHaveCount(0);
  const contactReadBoundary={delivery:deliveryReads,brief:jobBriefReads,archive:jobArchiveReads,history:contactHistoryReads};
  const sameContactReadBoundary=()=>deliveryReads===contactReadBoundary.delivery && jobBriefReads===contactReadBoundary.brief && jobArchiveReads===contactReadBoundary.archive && contactHistoryReads===contactReadBoundary.history;
  const restoreContactVisibility=()=>page.evaluate(()=>globalThis.__fcSmokeRestoreContactVisibility?.());
  await page.evaluate(()=>{
    if (document.visibilityState!=='visible' || Object.hasOwn(globalThis,'__fcSmokeRestoreContactVisibility')) throw new Error('visibility fixture unavailable');
    const original=Object.getOwnPropertyDescriptor(document,'visibilityState');
    globalThis.__fcSmokeRestoreContactVisibility=()=>{
      if (original) Object.defineProperty(document,'visibilityState',original);else delete document.visibilityState;
      delete globalThis.__fcSmokeRestoreContactVisibility;
      document.dispatchEvent(new Event('visibilitychange'));
    };
    Object.defineProperty(document,'visibilityState',{configurable:true,get:()=> 'hidden'});
    document.dispatchEvent(new Event('visibilitychange'));
  });
  try {
    checkpoint = 'presence-hidden';
    need(await page.evaluate(()=>document.visibilityState==='hidden'));
    const hiddenAt=performance.now();
    // This native client contact stays in Node; no credential or connection ID
    // enters page execution. It performs a GET, not a new fixture/domain write.
    const contact=await fetch(config.origin+prefix+'/workers/'+config.worker,{
      headers:{Authorization:'Bearer '+admin,'X-FC-Instance':config.instance,'X-FC-Worker':config.worker,'X-FC-Session':'dddddddddddddddddddddddddddddddd'},
      signal:AbortSignal.timeout(5000),
    });
    need(contact.status===200 && (await contact.json()).id===config.worker);
    // Timer callbacks need not fire at their exact requested delay. Prove the
    // full interval with monotonic elapsed time, including early wakeups.
    const hiddenUntil=hiddenAt+15500;
    while (performance.now()<hiddenUntil) await new Promise(resolve=>setTimeout(resolve,Math.max(1,Math.ceil(hiddenUntil-performance.now()))));
    need(performance.now()-hiddenAt>=15500 && presenceReads===1 && presenceFinished===1 && presenceHeaderErrors===0 && sameContactReadBoundary());
    await expect(page.locator('.worker-contact')).toHaveCount(0);
    checkpoint = 'presence-visible';
    await restoreContactVisibility();
    need(await page.evaluate(()=>document.visibilityState==='visible'));
    await expect.poll(()=>presenceFinished,{timeout:8000}).toBe(2);
    await expect(builderLane.locator('.worker-contact')).toContainText('Last contact');
    need(presenceReads===2 && presenceHeaderErrors===0 && sameContactReadBoundary());
    need(await inspectorLane.locator('.worker-contact').count()===0);
  } finally {
    // Restore the original property even when a contact/timing assertion fails.
    await restoreContactVisibility().catch(()=>{});
  }
  await captureLayouts('delivery', [1440, 390, 320]);
  checkpoint = 'layout-capture';
  need(await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Notes',exact:true}).count() === 0);
  checkpoint = 'notes-nav-absent';
  checkpoint = 'inbox-nav-click';
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Inbox',exact:true}).click();
  await page.getByRole('button',{name:'Select worker Builder',exact:true}).click();
  checkpoint = 'inbox-worker-selected';
  await page.getByRole('heading',{name:'Builder inbox',exact:true}).waitFor();
  checkpoint = 'worker-inbox-workers-nav';
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Workers',exact:true}).click();
  const builderInbox = page.getByRole('button',{name:'Open Builder inbox',exact:true});
  await builderInbox.waitFor();
  workerHistoryBefore = await page.evaluate(() => history.state.idx);
  checkpoint = 'worker-inbox-open';
  await builderInbox.click();
  await page.getByRole('heading',{name:'Builder inbox',exact:true}).waitFor();
  workerHistoryAfter = await page.evaluate(() => history.state.idx);
  checkpoint = 'worker-inbox-history';
  need(Number.isInteger(workerHistoryBefore) && workerHistoryAfter === workerHistoryBefore + 1);
  checkpoint = 'worker-inbox-back';
  await page.goBack();
  await page.waitForURL(config.origin + home + '/workers');
  await page.getByRole('button',{name:'Open Builder inbox',exact:true}).waitFor();
  need(await page.evaluate(() => history.state.idx) === workerHistoryBefore);
  checkpoint = 'worker-inbox-return';
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Inbox',exact:true}).click();
  await page.getByRole('heading',{name:'Builder inbox',exact:true}).waitFor();
  checkpoint = 'inbox-heading';
  need(await page.locator('.sidebar').count() === 1);
  checkpoint = 'inbox-sidebar';
  need(await page.getByRole('button',{name:'New message',exact:true}).count()===0);
  checkpoint = 'inbox-readonly-composer';
  need(actionWrites===0 && fixtureWrites===0 && forbiddenWrites===0);
  checkpoint = 'inbox-readonly-writes';
  checkpoint = 'default-appearance';
  await defaults();
  checkpoint = 'default-title';
  need((await appearance()).title === 'Packaged smoke · FireController');
  checkpoint = 'default-indicator-colors';
  const defaultIndicators = await indicatorColors();
  checkpoint = 'default-indicator-contrast';
  need(defaultIndicators.connection === 'rgba(0, 0, 0, 0)' && defaultIndicators.connectionBorder === 'rgb(243, 203, 200)' && defaultIndicators.connectionText === 'rgb(243, 203, 200)' && defaultIndicators.connectionText !== defaultIndicators.connectionSurface && defaultIndicators.status === (defaultIndicators.online ? 'rgb(103, 128, 93)' : 'rgb(169, 164, 160)'));
  phase = 'metadata-failure';
  checkpoint = 'metadata-notice';
  const reconnect = page.getByRole('button', {name:'Reconnect project updates', exact:true});
  await reconnect.waitFor();
  checkpoint = 'metadata-initial-counts';
  need(metadataStreams === 1 && presentationReads === 1 && injectedResponses === 1);
  await page.getByRole('button', {name:'Select worker Builder', exact:true}).click();
  checkpoint = 'metadata-inbox-ready';
  await page.getByText('Live inbox connected', {exact:true}).waitFor();
  need(metadataStreams === 1 && inboxStreams === 1);
  checkpoint = 'none';

  const binding = await page.evaluate(key => sessionStorage.getItem(key), bindingKey);
  need(binding && !binding.includes(token) && JSON.parse(binding).selectedworkerID === config.worker);
  const sessionID = JSON.parse(binding).sessionID;
  const cookieName = 'fc-local-browser-' + sessionID;
  const cookie = (await context.cookies()).find(value => value.name === cookieName);
  need(cookie?.httpOnly && cookie.sameSite === 'Strict' && sessionRenewals === 0);

  // A DOM-only text fixture measures the normal single-word title without
  // changing project presentation or reducing the long-brand fixture below.
  phase = 'normal-title-mobile-layout';
  for (const width of [320, 390]) {
    await page.setViewportSize({width, height:900});
    need(await page.locator('.hero-project h1').evaluate(element => {
      const original = element.textContent;
      try {
        element.textContent = 'FireController';
        const range = document.createRange(); range.selectNodeContents(element);
        const rects = [...range.getClientRects()];
        const box = element.getBoundingClientRect();
        return rects.length === 1 && rects[0].left >= box.left - 1 && rects[0].right <= box.right + 1
          && document.documentElement.scrollWidth <= innerWidth;
      } finally { element.textContent = original; }
    }));
  }
  await page.setViewportSize({width:390, height:900});

  phase = 'fixture-update';
  const title = 'UnbrokenProjectTitle'.repeat(8) + '<literal>';
  need(Buffer.byteLength(title) <= 200);
  // This writer is separate from page execution and uses the authenticated
  // public API, not SQL or browser mutation privileges.
  // A complete valid semantic theme covers the same-background indicator
  // regression; accent-only overrides leave the old fallback dots visible.
  const theme = {focus:'#767676', accent_background:'#000000', accent_text:'#fffffe', accent_soft_background:'#ffffff'};
  for (const family of ['canvas', 'surface', 'header', 'hero', 'active', 'blocked', 'parked', 'completed']) {
    theme[family + '_background'] = '#ffffff';
    theme[family + '_text'] = '#000000';
    if (['canvas', 'surface', 'header', 'hero'].includes(family)) theme[family + '_muted'] = '#000000';
  }
  const brand = 'UnbrokenProjectBrand'.repeat(5) + '<literal>';
  const receipt = await write('PUT', '/presentation', {expected_revision:0, presentation:{name:title, brand_name:brand, theme, review_groups:[{title:'Smoke release',pull_numbers:[31,32]}]}});
  need(receipt.revision === 1 && receipt.outcome === 'updated');
  await page.getByRole('heading', {name:'Packaged smoke', exact:true}).waitFor();
  need(metadataStreams === 1 && presentationReads === 1);
  phase = 'metadata-reconnect';
  await reconnect.click();
  const heading = page.getByRole('heading', {name:title, exact:true});
  await heading.waitFor();
  await expect.poll(async () => (await appearance()).accent, {timeout:5000}).toBe('#fffffe');
  need((await appearance()).brand === brand && (await appearance()).title === title + ' · FireController');
  const themedIndicators = await indicatorColors();
  need(themedIndicators.connection === 'rgb(0, 0, 0)' && themedIndicators.connection !== themedIndicators.connectionSurface);
  need(themedIndicators.status === 'rgb(0, 0, 0)' && themedIndicators.status !== themedIndicators.surface);
  need(await page.locator('literal').count() === 0);
  const fits = await heading.evaluate(element => {
    const range = document.createRange(); range.selectNodeContents(element);
    const rectangles = [...range.getClientRects()];
    const parent = element.closest('.hero-project').getBoundingClientRect();
    if (rectangles.length === 0 || rectangles.some(rect => rect.left < parent.left - 1 || rect.right > parent.right + 1) || document.documentElement.scrollWidth > innerWidth) return false;
    // Glyphs can extend beyond an ordinary, visible-overflow line box. Check
    // vertical bounds against actual clipping ancestors instead of font metrics.
    const clips = value => ['hidden', 'clip', 'auto', 'scroll'].includes(value);
    for (let ancestor = element; ancestor; ancestor = ancestor.parentElement) {
      const style = getComputedStyle(ancestor);
      if (!clips(style.overflowX) && !clips(style.overflowY)) continue;
      const box = ancestor.getBoundingClientRect();
      const left = box.left + ancestor.clientLeft, top = box.top + ancestor.clientTop;
      const right = left + ancestor.clientWidth, bottom = top + ancestor.clientHeight;
      if (rectangles.some(rect => clips(style.overflowX) && (rect.left < left - 1 || rect.right > right + 1)
        || clips(style.overflowY) && (rect.top < top - 1 || rect.bottom > bottom + 1))) return false;
    }
    return true;
  });
  checkpoint = 'long-brand-layout';
  need(fits);
  need(await page.locator('.brand').evaluate(element => {
    const range = document.createRange(); range.selectNodeContents(element);
    return document.documentElement.scrollWidth <= innerWidth && [...range.getClientRects()].every(rect => rect.left >= -1 && rect.right <= innerWidth + 1);
  }));
  checkpoint = 'none';

  await expect(reconnect).toHaveCount(0);
  await page.getByText('Live inbox connected', {exact:true}).waitFor();
  need(metadataStreams === 2 && presentationReads === 2 && inboxStreams === 1 && sessionPosts === 1);
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === binding);
  await expect(page.getByRole('button', {name:'Select worker Builder', exact:true})).toHaveAttribute('aria-pressed', 'true');
  phase = 'metadata-live-update';
  const liveReceipt = await write('PUT', '/presentation', {expected_revision:1, presentation:{name:title, brand_name:'Smoke live <literal>', theme, review_groups:[{title:'Smoke release',pull_numbers:[31,32]}]}});
  need(liveReceipt.revision === 2 && liveReceipt.outcome === 'updated');
  await expect.poll(async () => (await appearance()).brand, {timeout:5000}).toBe('Smoke live <literal>');
  need(metadataStreams === 2 && presentationReads === 3 && inboxStreams === 1 && sessionPosts === 1);

  // Mutate through a separate fixture client only after the relevant browser
  // view has rendered its preceding state. No Refresh click or browser reload
  // may be used to observe these changes; the project stream owns delivery.
  phase = 'live-jobs';
  const selectedWorker = async () => JSON.parse(await page.evaluate(key => sessionStorage.getItem(key), bindingKey)).selectedworkerID;
  const selectedBinding = await page.evaluate(key => sessionStorage.getItem(key), bindingKey);
  need(JSON.parse(selectedBinding).selectedworkerID === config.worker);
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Jobs', exact:true}).click();
  await page.getByLabel('Job status', {exact:true}).waitFor();
  await expect(page.getByLabel('Job status', {exact:true})).toHaveValue('open');
  await expect(page.getByLabel('Job worker', {exact:true})).toHaveValue('');
  need(await page.locator('.sidebar').count() === 0);
  const completedJob = await write('POST', '/jobs', {title:'Completed job sentinel', brief:'Synthetic completed job', worker_id:config.worker}, 201);
  await write('PATCH', '/jobs/' + completedJob.id, {expected_revision:1, status:'completed'});
  await expect(page.getByRole('link', {name:'Completed job sentinel', exact:true})).toHaveCount(0);
  await page.getByLabel('Job status', {exact:true}).selectOption('completed');
  await page.getByRole('link', {name:'Completed job sentinel', exact:true}).waitFor();
  await page.getByLabel('Job status', {exact:true}).selectOption('open');
  await expect(page.getByRole('link', {name:'Completed job sentinel', exact:true})).toHaveCount(0);
  await page.getByLabel('Job worker', {exact:true}).selectOption(config.other_worker);
  need(await selectedWorker() === config.worker);
  await page.getByRole('button', {name:'Show jobs for all workers', exact:true}).click();
  await expect(page.getByLabel('Job worker', {exact:true})).toHaveValue('');
  need(await selectedWorker() === config.worker);
  const job = await write('POST', '/jobs', {title:'Live job before', brief:'Live brief before', worker_id:config.worker}, 201);
  await page.getByRole('link', {name:'Live job before', exact:true}).waitFor();
  await write('PATCH', '/jobs/' + job.id, {expected_revision:1, title:'Live job list updated'});
  await page.getByRole('link', {name:'Live job list updated', exact:true}).click();
  await page.getByText('Live brief before', {exact:true}).waitFor();
  await write('PATCH', '/jobs/' + job.id, {expected_revision:2, title:'Live job detail updated', brief:'Live brief <literal> updated'});
  await page.getByRole('heading', {name:'Live job detail updated', exact:true}).waitFor();
  await page.getByText('Live brief <literal> updated', {exact:true}).waitFor();

  phase = 'live-workers';
  checkpoint = 'selected-builder-before-workers';
  need(await selectedWorker() === config.worker);
  checkpoint = 'workers-nav';
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Workers', exact:true}).click();
  checkpoint = 'builder-inbox-link';
  await page.getByRole('button', {name:'Open Builder inbox', exact:true}).waitFor();
  checkpoint = 'create-worker';
  await write('POST', '/workers', {name:'Live newcomer'});
  checkpoint = 'newcomer-visible';
  await page.getByRole('button', {name:'Open Live newcomer inbox', exact:true}).waitFor();
  checkpoint = 'selection-preserved';
  need(await selectedWorker() === config.worker);
  checkpoint = 'builder-details';
  await page.getByRole('link', {name:'Read Builder details', exact:true}).click();
  checkpoint = 'pause-default';
  await page.getByText('No pause reason recorded.', {exact:true}).waitFor();
  checkpoint = 'primary-existing';
  await page.getByRole('link', {name:'Open primary job ' + deliveryFixture.jobs.builderActive, exact:true}).waitFor();
  checkpoint = 'pause-update';
  await write('PUT', '/workers/' + config.worker + '/pause', {expected_revision:0, paused:true, reason:'Live pause <literal>'});
  checkpoint = 'pause-visible';
  await page.getByText('Live pause <literal>', {exact:true}).waitFor();
  checkpoint = 'primary-update';
  await write('PUT', '/workers/' + config.worker + '/primary-job', {expected_revision:1, job_id:job.id});
  checkpoint = 'primary-link';
  await page.getByRole('link', {name:'Open primary job ' + job.id, exact:true}).waitFor();
  checkpoint = 'selected-builder-after-update';
  need(await selectedWorker() === config.worker);

  phase = 'live-notes';
  checkpoint = 'worker-history-link';
  const emptyWorkerHistory = page.getByRole('link', {name:'Notes history', exact:true});
  await emptyWorkerHistory.waitFor();
  checkpoint = 'worker-history-empty';
  need(await page.getByRole('region', {name:'Pending notes', exact:true}).count() === 0 && noteArchiveReads === 0);
  need(await emptyWorkerHistory.getAttribute('href') === home + '/notes?status=all&worker_id=' + config.worker);
  const workerArchive = page.waitForResponse(value => value.request().method() === 'GET' && new URL(value.url()).pathname === prefix + '/notes' && new URL(value.url()).searchParams.get('status') === 'all' && new URL(value.url()).searchParams.get('limit') === '20' && !new URL(value.url()).searchParams.has('cursor') && new URL(value.url()).searchParams.get('worker_id') === config.worker);
  checkpoint = 'worker-history-open';
  await emptyWorkerHistory.click();
  checkpoint = 'worker-history-response';
  need((await workerArchive).status() === 200 && noteArchiveReads === 1);
  checkpoint = 'worker-history-filters';
  await expect(page.getByLabel('Worker ID', {exact:true})).toHaveValue(config.worker);
  await expect(page.getByRole('form', {name:'Note filters', exact:true}).getByRole('combobox', {name:/^Status/})).toHaveValue('all');
  checkpoint = 'worker-history-phase-filter';
  await page.getByLabel('Phase', {exact:true}).fill('live-proof');
  const filtered = page.waitForResponse(value => value.request().method() === 'GET' && new URL(value.url()).pathname === prefix + '/notes' && new URL(value.url()).searchParams.get('phase') === 'live-proof');
  await page.getByRole('button', {name:'Apply note filters', exact:true}).click();
  need((await filtered).status() === 200);
  checkpoint = 'worker-note-create';
  const noteBody = {worker_id:config.worker, job_id:'', phase:'live-proof', kind:'reminder', body:'Live note before'};
  const note = await write('POST', '/notes', noteBody, 201);
  await page.getByText('Live note before', {exact:true}).waitFor();
  await write('POST', '/notes', {...noteBody, phase:'other-proof', body:'Excluded note sentinel'}, 201);
  need(await page.getByText('Excluded note sentinel', {exact:true}).count() === 0);
  await write('PATCH', '/notes/' + note.id, {expected_revision:1, body:'Live note list updated'});
  await page.getByText('Live note list updated', {exact:true}).waitFor();
  need(await page.getByText('Excluded note sentinel', {exact:true}).count() === 0);
  await page.getByRole('link', {name:'Read note ' + note.id, exact:true}).click();
  await page.getByRole('heading', {name:'Full body', exact:true}).waitFor();
  await write('PATCH', '/notes/' + note.id, {expected_revision:2, body:'Live note detail <literal> updated'});
  await page.getByText('Live note detail <literal> updated', {exact:true}).waitFor();

  phase = 'contextual-notes';
  const pendingPanel = page.getByRole('region', {name:'Pending notes', exact:true});
  const archiveReadsBeforeEmptyJob = noteArchiveReads;
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Jobs', exact:true}).click();
  await page.getByRole('link', {name:'Live job detail updated', exact:true}).click();
  checkpoint = 'job-empty';
  const emptyJobHistory = page.getByRole('link', {name:'Notes history', exact:true});
  await emptyJobHistory.waitFor();
  await expect(pendingPanel).toHaveCount(0);
  need(await emptyJobHistory.getAttribute('href') === home + '/notes?status=all&job_id=' + job.id);
  need(noteArchiveReads === archiveReadsBeforeEmptyJob);
  const archiveReadsBeforeJob = noteArchiveReads;
  const jobArchive = page.waitForResponse(value => value.request().method() === 'GET' && new URL(value.url()).pathname === prefix + '/notes' && new URL(value.url()).searchParams.get('status') === 'all' && new URL(value.url()).searchParams.get('limit') === '20' && !new URL(value.url()).searchParams.has('cursor') && new URL(value.url()).searchParams.get('job_id') === job.id);
  await emptyJobHistory.click();
  need((await jobArchive).status() === 200 && noteArchiveReads === archiveReadsBeforeJob + 1);
  await expect(page.getByLabel('Job ID', {exact:true})).toHaveValue(job.id);
  await expect(page.getByRole('form', {name:'Note filters', exact:true}).getByRole('combobox', {name:/^Status/})).toHaveValue('all');
  await page.goBack();
  await emptyJobHistory.waitFor();
  checkpoint = 'job-create';
  const jobNote = await write('POST', '/notes', {worker_id:'', job_id:job.id, phase:'', kind:'reminder', body:'Context job pending'}, 201);
  checkpoint = 'job-appeared';
  await pendingPanel.getByText('Context job pending', {exact:true}).waitFor();
  const jobPanelBox = await pendingPanel.boundingBox();
  const briefBox = await page.getByRole('heading', {name:'Job brief', exact:true}).boundingBox();
  need(jobPanelBox && briefBox && jobPanelBox.y < briefBox.y);
  await captureLayouts('job-pending-notes', [1440, 390, 320]);
  checkpoint = 'job-read';
  await pendingPanel.getByRole('link', {name:'Read note', exact:true}).click();
  checkpoint = 'job-pending-detail';
  await page.getByText('reminder · pending', {exact:true}).waitFor();
  checkpoint = 'job-open-history';
  await page.getByRole('button', {name:'Show note history', exact:true}).click();
  checkpoint = 'job-history-loaded';
  await page.getByRole('link', {name:'Read revision 1', exact:true}).waitFor();
  checkpoint = 'job-scope-return';
  await page.locator('.panel.brief').getByRole('link', {name:job.id, exact:true}).click();
  await pendingPanel.getByText('Context job pending', {exact:true}).waitFor();
  checkpoint = 'job-consume';
  await write('PATCH', '/notes/' + jobNote.id, {expected_revision:1, status:'consumed'});
  await expect(pendingPanel).toHaveCount(0);
  checkpoint = 'consumed-history-return';
  await page.goBack();
  await page.getByText('reminder · consumed', {exact:true}).waitFor();
  await page.getByRole('button', {name:'Show note history', exact:true}).click();
  await page.getByRole('link', {name:'Read revision 1', exact:true}).click();
  await page.getByRole('heading', {name:'Captured note revision 1', exact:true}).waitFor();
  await page.getByText('reminder · pending', {exact:true}).waitFor();
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Inbox', exact:true}).click();
  await page.getByRole('heading', {name:'Builder inbox', exact:true}).waitFor();
  need(await selectedWorker() === config.worker);
  checkpoint = 'worker-create';
  const workerNote = await write('POST', '/notes', {worker_id:config.worker, job_id:'', phase:'', kind:'source', body:'Context worker pending'}, 201);
  checkpoint = 'worker-appeared';
  await pendingPanel.getByText('Context worker pending', {exact:true}).waitFor();
  const workerPanelBox = await pendingPanel.boundingBox();
  const inboxBox = await page.locator('.main-content > .panel:not(.contextual-notes)').first().boundingBox();
  need(workerPanelBox && inboxBox && workerPanelBox.y < inboxBox.y);
  need(await page.locator('.sidebar').count() === 1);
  await captureLayouts('inbox-pending-notes', [1440, 390, 320]);
  checkpoint = 'worker-dismiss';
  await write('PATCH', '/notes/' + workerNote.id, {expected_revision:1, status:'dismissed', status_reason:'Synthetic dismissal proof'});
  await expect(pendingPanel.getByText('Context worker pending', {exact:true})).toHaveCount(0);
  // The original library fixture stays pending: viewing it did not consume it.
  await pendingPanel.getByText('Live note detail <literal> updated', {exact:true}).waitFor();
  need(forbiddenWrites === 0 && actionWrites === 0);
  await pendingPanel.getByRole('link', {name:'View all notes and history', exact:true}).click();
  await page.getByRole('link', {name:'Read note ' + workerNote.id, exact:true}).click();
  await page.getByText('source · dismissed', {exact:true}).waitFor();
  await page.getByText('Synthetic dismissal proof', {exact:true}).waitFor();
  await page.getByRole('button', {name:'Show note history', exact:true}).click();
  await page.getByRole('link', {name:'Read revision 1', exact:true}).click();
  await page.getByRole('heading', {name:'Captured note revision 1', exact:true}).waitFor();
  await page.getByText('source · pending', {exact:true}).waitFor();

  phase = 'live-reviews';
  checkpoint = 'reviews-open';
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Reviews', exact:true}).click();
  checkpoint = 'unconfigured';
  await page.locator('#review-queue').getByRole('heading', {name:'No review queue yet', exact:true}).waitFor();
  const pull = (number, state, title) => ({number, title, state, head_sha:'a'.repeat(40), base_ref:'main', base_sha:'b'.repeat(40), ci_state:'unknown', mergeability:'unknown', complete:true, runs:[], attempts:[]});
  const merged = pull(31, 'merged', 'Retained merged smoke PR');
  const open = pull(32, 'open', 'Live review before');
  const observation = (revision, queue) => ({expected_revision:revision, repository_id:991, source:'packaged-browser', observed_at:new Date().toISOString(), queue, retired:[]});
  checkpoint = 'seed-observation';
  await write('POST', '/reviews/observations', observation(0, [merged, open]), 201);
  checkpoint = 'initial-open-pull';
  await page.getByRole('link', {name:'Live review before', exact:true}).waitFor();
  checkpoint = 'initial-merged-pull';
  await page.getByRole('link', {name:'Retained merged smoke PR', exact:true}).waitFor();
  const finished = new Date().toISOString();
  const run = {source_run_id:'live-hosted-round', channel:'hosted', head_sha:open.head_sha, reviewer:'Synthetic reviewer', scope:'Synthetic comparison', outcome:'completed', attributable:true, complete:true, started_at:finished, finished_at:finished, found:0, accepted:0, routed:0, finalized:true, coverage_limits:[], findings:[]};
  checkpoint = 'updated-observation';
  await write('POST', '/reviews/observations', observation(1, [merged, {...open, title:'Live review updated', runs:[run]}]), 201);
  checkpoint = 'updated-open-pull';
  await page.getByRole('link', {name:'Live review updated', exact:true}).waitFor();
  checkpoint = 'updated-merged-pull';
  await page.getByRole('link', {name:'Retained merged smoke PR', exact:true}).waitFor();
  checkpoint = 'hosted-run';
  await page.locator('#review-queue').getByRole('region', {name:'Hosted CodeRabbit', exact:true}).getByRole('link', {name:/^Open hosted run live-hosted-round:/}).waitFor();
  checkpoint = 'current-round-grants';
  await write('POST', '/reviews/rounds/32/hosted', {expected_revision:0,action:'grant',minimum:0,maximum:5}, 200);
  await write('POST', '/reviews/rounds/32/cli', {expected_revision:0,action:'grant',minimum:2,maximum:4}, 200);
  await expect(page.locator('#review-queue [aria-label="Current hosted round limits"]')).toContainText('0 of 5 used');
  await expect(page.locator('#review-queue [aria-label="Current cli round limits"]')).toContainText('Minimum 2 more');
  checkpoint = 'merged-retention';
  need(await page.locator('.review-state-merged').filter({hasText:'Merged'}).count() === 1);
  checkpoint = 'delivery-queue';
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Delivery', exact:true}).click();
  checkpoint = 'queue-heading';
  const deliveryQueue = page.locator('#review-queue');
  await expect(deliveryQueue.getByRole('heading', {name:'Reviews', exact:true})).toBeVisible();
  await expect(deliveryQueue.getByRole('heading', {name:'Smoke release', exact:true})).toBeVisible();
  need(await deliveryQueue.locator('.review-queue-row').evaluateAll(rows=>rows.map(row=>row.querySelector('.review-number').textContent).join('|')) === '#31|#32');
  checkpoint = 'queue-pull';
  await expect(deliveryQueue.getByRole('link', {name:'Live review updated', exact:true})).toBeVisible();
  await expect(deliveryQueue.locator('.review-state-merged').filter({hasText:'Merged'})).toBeVisible();
  checkpoint = 'merge-head';
  await expect(page.getByRole('region', {name:'Merge train head'}).getByRole('link', {name:'#32 · Live review updated', exact:true})).toBeVisible();
  checkpoint = 'hosted-summary';
  const queueHistory = deliveryQueue.getByRole('region', {name:'PR #32 review rounds', exact:true});
  const queueHosted = queueHistory.getByRole('region', {name:'Hosted CodeRabbit', exact:true});
  await expect(queueHosted.getByRole('link', {name:/^Open hosted run live-hosted-round:/})).toBeVisible();
  await expect(queueHosted.locator('.review-lane-heading')).toContainText('1 run');
  const hero = page.getByRole('region', {name:'Merge train head', exact:true});
  const jump = hero.getByRole('link', {name:'Jump to review queue', exact:true});
  await expect(jump).toHaveAttribute('href', `/projects/${config.project}#review-pr-32`);
  await expect(jump).toHaveText('Review queue ↗');
  need(await jump.locator('*').count() === 0);
  const jumpStyle = await jump.evaluate(element => { const style = getComputedStyle(element); return {color:style.color, weight:style.fontWeight, decoration:style.textDecorationLine}; });
  const configuredInk = [1, 3, 5].map(offset => Number.parseInt(theme.surface_text.slice(offset, offset + 2), 16)).join(', ');
  need(jumpStyle.color === `rgb(${configuredInk})` && jumpStyle.weight === '750' && jumpStyle.decoration.includes('underline'));
  const mergedFacts = deliveryQueue.locator('#review-pr-31 .review-pull-facts');
  const openFacts = deliveryQueue.locator('#review-pr-32 .review-pull-facts');
  await expect(mergedFacts).not.toContainText('Mergeability');
  await expect(mergedFacts).toContainText('CI: Unknown');
  await expect(mergedFacts).toContainText('CI status is unknown.');
  await expect(openFacts).toContainText('Mergeability: Unknown');
  await expect(openFacts).toContainText('Mergeability is unknown.');
  const headRow = page.locator('#review-pr-32');
  const headJumpPosition = () => headRow.evaluate(element => {
    const box = element.getBoundingClientRect(), mast = document.querySelector('.mast').getBoundingClientRect();
    const margin = getComputedStyle(element).scrollMarginTop;
    const maximum = Math.max(0, document.documentElement.scrollHeight - innerHeight);
    const target = Math.max(0, Math.min(maximum, scrollY + box.top - parseFloat(margin)));
    return {belowHeader:box.top >= mast.bottom - 1, inViewport:box.top < innerHeight && box.bottom > mast.bottom, atRowTarget:Math.abs(scrollY - target) <= 2, margin};
  });
  const expectedHeadPosition = {belowHeader:true, inViewport:true, atRowTarget:true, margin:'130px'};
  await expect(hero.getByRole('region', {name:'Hosted CodeRabbit', exact:true}).locator('.review-lane-heading')).toContainText('1 run');
  checkpoint = 'current-round-limits-shared';
  await expect(hero.locator('[aria-label="Current hosted round limits"]')).toContainText('0 of 5 used');
  // Imported recorded history does not count against a new native grant.
  await expect(queueHosted.locator('[aria-label="Current hosted round limits"]')).toContainText('0 of 5 used');
  checkpoint = 'current-round-close-live';
  await write('POST', '/reviews/rounds/32/hosted', {expected_revision:1,action:'close'}, 200);
  await expect(hero.locator('[aria-label="Current hosted round limits"]')).toContainText('Review closed');
  await expect(queueHosted.locator('[aria-label="Current hosted round limits"]')).toContainText('Review closed');
  await expect(queueHosted.locator('.review-lane-heading')).toContainText('1 run');
  const age = queueHosted.locator('.review-round-pill time');
  await expect(age).toHaveText(/^(?:Just now|Future date|\d+m ago|\d+h \d+m ago|\d+d \d+h ago)$/u);
  await expect(age).toHaveAttribute('datetime', finished);
  await captureLayouts('delivery-populated', [1440, 390, 320]);
  await jump.click();
  await expect.poll(() => new URL(page.url()).hash).toBe('#review-pr-32');
  checkpoint = 'queue-jump-visible';
  await expect.poll(headJumpPosition).toEqual(expectedHeadPosition);
  if (config.diagnostic_directory) {
    await fs.mkdir(config.diagnostic_directory, {recursive:true, mode:0o700});
    await page.screenshot({path:config.diagnostic_directory + '/delivery-head-jump-390.png', timeout:2000});
  }
  checkpoint = 'reviews-return';
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Reviews', exact:true}).click();
  checkpoint = 'reviews-heading';
  await page.getByRole('heading', {name:'Reviews', exact:true}).waitFor();
  checkpoint = 'selected-worker';
  need(await selectedWorker() === config.worker);
  checkpoint = 'sanitized-literal';
  need(await page.locator('literal').count() === 0);
  checkpoint = 'reviews-url';
  need(new URL(page.url()).pathname === home + '/reviews');
  checkpoint = 'reload-head-spa';
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Delivery', exact:true}).click();
  await jump.click();
  await expect.poll(() => new URL(page.url()).hash).toBe('#review-pr-32');
  await expect.poll(headJumpPosition).toEqual(expectedHeadPosition);

  phase = 'reload';
  checkpoint = 'reload-before';
  const renewalResponsePromise = page.waitForResponse(value => value.request().method() === 'POST' && new URL(value.url()).pathname === prefix + '/browser-session/renew');
  // Hold only the existing reload queue request: the hash must survive before
  // any target row exists, then scroll after its ordinary async response.
  let releaseReloadQueue, reloadQueueHeld = false;
  const reloadQueueGate = new Promise(resolve => { releaseReloadQueue = resolve; });
  const queuePattern = config.origin + prefix + '/reviews/queue?*';
  const holdReloadQueue = async route => { if (route.request().method() === 'GET') { reloadQueueHeld = true; await reloadQueueGate; } await route.fallback(); };
  await page.route(queuePattern, holdReloadQueue);
  const reloadQueueResponsePromise = page.waitForResponse(value => value.request().method() === 'GET' && new URL(value.url()).pathname === prefix + '/reviews/queue');
  try {
    await page.reload();
    checkpoint = 'reload-after-navigation';
    await page.getByRole('heading', {name:title, exact:true}).waitFor();
    await expect.poll(() => reloadQueueHeld).toBe(true);
    need(new URL(page.url()).hash === '#review-pr-32');
    await expect(headRow).toHaveCount(0);
  } finally { releaseReloadQueue(); }
  need((await reloadQueueResponsePromise).status() === 200);
  await page.unroute(queuePattern, holdReloadQueue);
  await expect.poll(headJumpPosition).toEqual(expectedHeadPosition);
  checkpoint = 'reload-heading-visible';
  await expect.poll(() => sessionRenewals, {timeout:5000}).toBe(1);
  checkpoint = 'reload-session-renewed';
  const renewalResponse = await renewalResponsePromise;
  renewalProof.renewal_response_received = true; renewalProof.renewal_response_ok = renewalResponse.status() === 200;
  need(renewalProof.renewal_response_ok);
  // Chromium may retire the CDP body handle independently of the page's
  // successful renewal. Validate actual current cookie authority instead.
  const current = await page.evaluate(async ({prefix, sessionID, project, instance, bindingKey}) => {
    const binding = JSON.parse(sessionStorage.getItem(bindingKey));
    if (binding?.sessionID !== sessionID || binding.projectID !== project || binding.instanceID !== instance) throw new Error('current session binding differs');
    const response = await fetch(prefix + '/browser-session', {headers:{'X-FC-Browser-Session':sessionID, 'X-FC-Instance':instance}, credentials:'same-origin', cache:'no-store', redirect:'error', signal:AbortSignal.timeout(5000)});
    if (response.status !== 200) return {status:response.status, view:null};
    return {status:response.status, view:await response.json()};
  }, {prefix, sessionID, project:config.project, instance:config.instance, bindingKey});
  const currentView = current.view;
  const originalBinding = JSON.parse(selectedBinding);
  const fields = ['session_id', 'instance_id', 'project_id', 'expires_at', 'allow_writes', 'can_edit_presentation'];
  const currentObject = !!currentView && typeof currentView === 'object' && !Array.isArray(currentView);
  const currentShape = currentObject && Object.keys(currentView).length === fields.length && fields.every(field => Object.hasOwn(currentView, field));
  const currentExpiry = typeof currentView?.expires_at === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/u.test(currentView.expires_at) && Number.isFinite(Date.parse(currentView.expires_at));
  renewalProof = {
    renewal_response_received:true,
    renewal_response_ok:renewalResponse.status() === 200,
    current_read_ok:current.status === 200,
    current_shape_valid:currentShape,
    current_identity_matches:currentShape && currentView.session_id === sessionID && currentView.project_id === config.project && currentView.instance_id === config.instance,
    current_read_only:currentShape && currentView.allow_writes === false && currentView.can_edit_presentation === false,
    current_expiry_valid:currentExpiry && Date.parse(currentView.expires_at) > Date.now() && Date.parse(currentView.expires_at) <= Date.now() + 86400000 + 300000,
    // Cookies use Unix seconds; a same-second renewal need not change expiry.
    current_expiry_nondecreasing:currentExpiry && Date.parse(currentView.expires_at) >= Date.parse(originalBinding.expiresAt),
    storage_matches_current:false,
  };
  checkpoint = 'reload-current-authority';
  need(Object.entries(renewalProof).filter(([name]) => name !== 'storage_matches_current').every(([, value]) => value === true));
  checkpoint = 'reload-expiry-persistence';
  try { await expect.poll(async () => JSON.parse(await page.evaluate(key => sessionStorage.getItem(key), bindingKey)).expiresAt, {timeout:5000}).toBe(currentView.expires_at); renewalProof.storage_matches_current = true; } catch { /* retain bounded diagnostics for the refused assertion */ }
  checkpoint = 'reload-expiry-evidence';
  need(renewalProof.storage_matches_current);
  checkpoint = 'reload-binding-retained';
  const retained = JSON.parse(await page.evaluate(key => sessionStorage.getItem(key), bindingKey));
  const bindingFields = ['version', 'projectID', 'instanceID', 'sessionID', 'expiresAt', 'selectedworkerID'];
  need(retained.version === 1 && Object.keys(retained).length === bindingFields.length && bindingFields.every(field => Object.hasOwn(retained, field)));
  need(retained.projectID === config.project && retained.instanceID === config.instance && retained.sessionID === sessionID && retained.selectedworkerID === config.worker);
  checkpoint = 'reload-expiry-window';
  need(Date.parse(retained.expiresAt) > Date.now() && Date.parse(retained.expiresAt) <= Date.now() + 86400000 + 300000);
  checkpoint = 'reload-cookie-rotated';
  const resumed = (await context.cookies()).find(value => value.name === cookieName);
  need(resumed?.value !== cookie.value && resumed?.httpOnly && resumed?.sameSite === 'Strict' && sessionPosts === 1);
  checkpoint = 'reload-presentation-restored';
  await expect.poll(async () => (await appearance()).accent, {timeout:5000}).toBe('#fffffe');
  checkpoint = 'reload-sidebar-absent';
  need(await page.locator('.sidebar').count() === 0);
  checkpoint = 'reload-connection-live';
  await page.locator('.mast .connection-live').waitFor();

  checkpoint = 'reload-metadata-live';
  await expect(page.getByRole('status').filter({hasText:/^Project updates disconnected/})).toHaveCount(0);
  checkpoint = 'reload-reader-counts';
  need((await appearance()).brand === 'Smoke live <literal>' && metadataStreams >= 3 && metadataStreams <= 4 && presentationReads === 4 && inboxStreams >= 2 && inboxStreams <= 3);

  // Renewal replaces streams that opened before its cookie response. Capture
  // this reader phase before clock advancement and later writer sessions.
  const metadataCounts = {presentation_reads:presentationReads, metadata_streams:metadataStreams, inbox_streams:inboxStreams};

  phase = 'renewal-activity';
  // Advance only Chromium's clock across client throttle admission. The real
  // daemon still issues and checks its ordinary24-hour cookies.
  await page.clock.fastForward(300001);
  await new Promise(resolve => setTimeout(resolve, 100));
  need(sessionRenewals === 1); // Timers and SSE alone are not user activity.
  // Control only this disposable document's visibility getter; this is client
  // admission proof, not a physical-device background/lifecycle assertion.
  await page.evaluate(() => { Object.defineProperty(document, 'visibilityState', {configurable:true, value:'hidden'}); document.dispatchEvent(new Event('visibilitychange')); });
  await page.keyboard.press('Shift');
  await page.mouse.click(385, 895);
  await new Promise(resolve => setTimeout(resolve, 100));
  need(sessionRenewals === 1);
  await page.evaluate(() => { delete document.visibilityState; document.dispatchEvent(new Event('visibilitychange')); });
  await expect.poll(() => sessionRenewals, {timeout:5000}).toBe(2);
  await page.keyboard.press('Shift');
  await page.mouse.click(385, 895);
  await new Promise(resolve => setTimeout(resolve, 100));
  need(sessionRenewals === 2 && sessionPosts === 1);
  const privateStorage = await page.evaluate(() => JSON.stringify({session:{...sessionStorage}, local:{...localStorage}}));
  need(!privateStorage.includes(token) && !privateStorage.includes(admin) && !privateStorage.includes('allow_writes') && !privateStorage.includes('can_edit_presentation'));

  phase = 'disconnect';
  await page.getByRole('button', {name:'Disconnect', exact:true}).click();
  await page.getByRole('button', {name:'Connect to project', exact:true}).waitFor();
  await expect.poll(async () => (await context.cookies()).some(value => value.name === cookieName), {timeout:5000}).toBe(false);
  await defaults();
  need((await appearance()).title === 'FireController');
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === null);
  need(sessionPosts === 1 && sessionDeletes === 1 && forbiddenWrites === 0 && errors === 0 && external === 0);

  phase = 'explicit-actions-connect';
 checkpoint='root-url';
 await page.waitForURL(config.origin + '/');
 checkpoint='root-form';
 await expect(page.getByLabel('Project ID', {exact:true})).toHaveValue('');
 const actionRootHistoryIndex=await page.evaluate(()=>history.state.idx);
  explicitActions = true;
 checkpoint='project-input';
  await page.getByLabel('Project ID', {exact:true}).fill(config.project);
 checkpoint='credential-input';
  await page.getByLabel('Project credential', {exact:true}).fill(admin);
  need(await page.getByRole('checkbox').count()===0);
 checkpoint='submit';
  const adminLogin=page.waitForResponse(response=>response.request().method()==='POST' && new URL(response.url()).pathname===prefix+'/browser-session');
  await page.getByRole('button', {name:'Connect to project',exact:true}).click();
  checkpoint='session-response';
  const adminResponse=await adminLogin;lastHTTPStatus=adminResponse.status();
  checkpoint='session-status';
  need(lastHTTPStatus===200);
  checkpoint='heading';
  await page.getByRole('heading', {name:title,exact:true}).waitFor();
  checkpoint='capabilities-read';
  const adminCapabilities=await loginAuthority(adminResponse, true, true);
  lastAllowWrites=adminCapabilities.allow_writes===true; lastCanEditPresentation=adminCapabilities.can_edit_presentation===true;
  checkpoint='capabilities-check';
  need(adminCapabilities.allow_writes===true && adminCapabilities.can_edit_presentation===true);
  need(actionWrites===0 && forbiddenWrites===0);

  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Inbox',exact:true}).click();
  await page.getByRole('button', {name:'Select worker Builder',exact:true}).click();
  const incoming = await write('POST','/messages',{recipient_worker_id:config.worker,body:'Action incoming <literal>',reply_to_message_id:''},201,config.other_worker);
  const row = page.locator('.inbox-list > li').filter({hasText:'Action incoming <literal>'});
  await row.waitFor();
  need(await row.getByText('Unread',{exact:true}).count() === 1);
  await row.getByRole('button',{name:'Mark seen',exact:true}).click();
  await row.getByText('Seen',{exact:true}).waitFor();
  await row.getByRole('button',{name:'Acknowledge',exact:true}).click();
  await row.getByText('Acknowledged',{exact:true}).waitFor();

  phase = 'explicit-actions-send'; checkpoint = 'composer-open';
  await page.getByRole('button',{name:'New message',exact:true}).click();
  checkpoint = 'recipient';
  await page.getByLabel('Recipient worker ID',{exact:true}).fill(config.other_worker);
  checkpoint = 'send-body';
  await page.getByLabel('Message',{exact:true}).fill('Browser send <literal>');
  const sent = page.waitForResponse(r=>r.request().method()==='POST' && new URL(r.url()).pathname===prefix+'/messages');
  checkpoint = 'send-dispatch';
  await page.getByRole('button',{name:'Send message',exact:true}).click();
  checkpoint = 'send-response';
  sendStatus = (await sent).status(); checkpoint = 'send-status';
  need(sendStatus===201);
  checkpoint = 'conversation-open';
  await row.getByRole('link').click();
  checkpoint = 'conversation-body';
  await page.getByText('Action incoming <literal>',{exact:true}).waitFor();
  checkpoint = 'reply-open';
  await page.getByRole('button',{name:'Reply',exact:true}).click();
  checkpoint = 'reply-body';
  await page.getByLabel('Reply message',{exact:true}).fill('Browser reply <literal>');
  const replied = page.waitForResponse(r=>r.request().method()==='POST' && new URL(r.url()).pathname===prefix+'/messages');
  checkpoint = 'reply-dispatch';
  await page.getByRole('button',{name:'Send reply',exact:true}).click();
  checkpoint = 'reply-response';
  const replyResponse = await replied;
  replyStatus = replyResponse.status(); checkpoint = 'reply-status';
  need(replyStatus===201);
  checkpoint = 'reply-binding';
  const replyRequest = replyResponse.request().postDataJSON();
  need(replyRequest.reply_to_message_id===incoming.id && replyRequest.recipient_worker_id===config.other_worker && replyRequest.body==='Browser reply <literal>');
  const originalConversation = config.origin + home + '/inbox/' + incoming.conversation_id;
  need(page.url()===originalConversation);
  checkpoint = 'reply-visible';
  await page.locator('.conversation-list').getByText('Browser reply <literal>',{exact:true}).waitFor();
  need(page.url()===originalConversation);
  conversationMatch = true;

  phase = 'explicit-actions-settings';
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Settings',exact:true}).click();
  await page.getByLabel('Project name',{exact:true}).fill('Browser configured project');
  await page.getByLabel('Brand name',{exact:true}).fill('Browser configured brand');
  const settingsSaved = page.waitForResponse(r=>r.request().method()==='PUT' && new URL(r.url()).pathname===prefix+'/presentation');
  await page.getByRole('button',{name:'Save appearance',exact:true}).click();
  need((await settingsSaved).status()===200);
  await expect.poll(async()=>(await appearance()).brand).toBe('Browser configured brand');
  await page.reload();
  await page.getByRole('heading',{name:'Browser configured project',exact:true}).waitFor();
  need(new URL(page.url()).pathname===home+'/settings');
  await expect.poll(() => sessionRenewals, {timeout:5000}).toBe(3);
  await page.getByLabel('Project name',{exact:true}).waitFor();
  need(await page.locator('literal').count()===0);
  need(!(await page.evaluate(()=>JSON.stringify(sessionStorage))).includes(admin));
  need(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));

  phase = 'explicit-actions-uncertain'; checkpoint = 'compose';
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Inbox',exact:true}).click();
  await page.getByRole('button',{name:'New message',exact:true}).click();
  await page.getByLabel('Recipient worker ID',{exact:true}).fill(config.other_worker);
  await page.getByLabel('Message',{exact:true}).fill('Uncertain browser message');
  let committed, releaseSend;
  const committedSend = new Promise(resolve=>{committed=resolve;});
  const releaseResponse = new Promise(resolve=>{releaseSend=resolve;});
  let originalWire, retryWire, heldReceipt;
  await page.route(config.origin+prefix+'/messages',async route=>{
    if (route.request().method()!=='POST') {await route.continue();return;}
    const wire={key:route.request().headers()['idempotency-key'],body:route.request().postData(),worker:route.request().headers()['x-fc-worker']};
    if (originalWire) {retryWire=wire;await route.continue();return;}
    originalWire=wire;
    const response=await route.fetch();
    need(response.status()===201);
    heldReceipt=await response.json();committed();
    await releaseResponse;
    await route.abort('failed').catch(()=>{});
  });
  controlledMessageFailure=true; checkpoint = 'dispatch';
  await page.getByRole('button',{name:'Send message',exact:true}).click();
  await committedSend; checkpoint = 'switch-view';
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Workers',exact:true}).click();
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Inbox',exact:true}).click();
  releaseSend(); checkpoint = 'retry-ready';
  const retry = page.getByRole('button',{name:'Retry exact send',exact:true});
  // The retained action must expose retry after remount without a manual
  // composer-open click that could mask disappearing success feedback.
  await expect(retry).toBeEnabled();
  // Pop to the original root route: canceling the explicit discard prompt must
  // preserve this project and the original pending key/body, without a reload.
  checkpoint = 'navigation-guard';
  const rootDelta=await page.evaluate(root=>history.state.idx-root,actionRootHistoryIndex);
  need(rootDelta>0);
  const exitDialog=page.waitForEvent('dialog');
  await page.evaluate(delta=>history.go(-delta),rootDelta);
  await (await exitDialog).dismiss();
  await expect(retry).toBeEnabled();
  need(new URL(page.url()).pathname===home+'/inbox');
  checkpoint = 'retry-response';
  const retryResponse=page.waitForResponse(r=>r.request().method()==='POST' && new URL(r.url()).pathname===prefix+'/messages');
  await retry.click();
  const recovered=await retryResponse;
  need(recovered.status()===201 && (await recovered.json()).id===heldReceipt.id);
  checkpoint = 'retry-identity';
  need(JSON.stringify(originalWire)===JSON.stringify(retryWire));
  checkpoint = 'conversation-link';
  await page.getByRole('link',{name:'Open sent conversation',exact:true}).click();
  checkpoint = 'conversation-body';
  await page.getByText('Uncertain browser message',{exact:true}).waitFor();
  need(await page.getByText('Uncertain browser message',{exact:true}).count()===1);
  controlledMessageFailure=false; checkpoint = 'disconnect';
  await page.getByRole('button',{name:'Disconnect',exact:true}).click();
  await page.getByRole('button',{name:'Connect to project',exact:true}).waitFor();
  checkpoint = 'action-counters';
  need(sessionPosts===2 && sessionDeletes===2 && sessionRenewals===3 && actionWrites===7 && forbiddenWrites===0 && errors===0 && external===0);

  phase = 'renewal-missing-cookie'; checkpoint = 'root-form';
  // Disconnect renders its old project-route form before navigation replaces
  // it. Wait for the destination form before filling either required input.
  await page.waitForURL(config.origin + '/');
  await expect(page.getByLabel('Project ID', {exact:true})).toHaveValue('');
  checkpoint = 'project-input';
  explicitActions = false;
  await page.getByLabel('Project ID', {exact:true}).fill(config.project);
  checkpoint = 'credential-input';
  await page.getByLabel('Project credential', {exact:true}).fill(token);
  checkpoint = 'submit';
  await page.getByRole('button', {name:'Connect to project', exact:true}).click();
  checkpoint = 'heading';
  await page.getByRole('heading', {name:'Browser configured project',exact:true}).waitFor();
  checkpoint = 'binding';
  const missingBinding = JSON.parse(await page.evaluate(key => sessionStorage.getItem(key), bindingKey));
  need(sessionPosts === 3 && sessionRenewals === 3);
  checkpoint = 'remove-cookie';
  await context.clearCookies({name:'fc-local-browser-' + missingBinding.sessionID});
  controlledAccessFailure = true; checkpoint = 'reload';
  await page.reload();
  await page.getByRole('button', {name:'Connect to project',exact:true}).waitFor();
  checkpoint = 'cleanup';
  await expect.poll(() => sessionDeletes, {timeout:5000}).toBe(3);
  checkpoint = 'storage';
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === null);
  checkpoint = 'counters';
  need(terminalResponses > 0 && sessionRenewals === 3 && actionWrites === 7 && forbiddenWrites === 0 && errors === 0 && external === 0);

  phase = 'progress-access-ended'; checkpoint = 'isolated-progress-refusal';
  controlledAccessFailure = false;
  controlledProgressAccessFailure = true;
  await page.route(config.origin + prefix + '/reviews/rounds/progress?*', async route => {
    progressAccessFailures++;
    await route.fulfill({status:401,contentType:'application/json',body:JSON.stringify({error:{code:'unauthorized',message:'Synthetic progress-only access refusal.'}})});
  });
  // Access failure preserves the selected project route for reconnect.
  checkpoint = 'reconnect-input';
  await page.getByLabel('Project ID', {exact:true}).fill(config.project);
  await page.getByLabel('Project credential', {exact:true}).fill(token);
  await page.getByRole('button', {name:'Connect to project',exact:true}).click();
  checkpoint = 'progress-refusal';
  await expect.poll(() => progressAccessFailures).toBe(1);
  checkpoint = 'session-cleanup';
  await expect.poll(() => sessionDeletes).toBe(4);
  await page.getByRole('button', {name:'Connect to project',exact:true}).waitFor();
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === null);
  need(sessionPosts === 4 && sessionRenewals === 3 && actionWrites === 7 && forbiddenWrites === 0 && errors === 0 && external === 0);

  phase = 'cleanup'; operation = 'cleanup'; checkpoint = 'context-close'; publishProgress();
  await context.close();
  checkpoint = 'browser-close'; publishProgress();
  await browser.close(); browser = undefined;
  checkpoint = 'server-close'; publishProgress();
  await server.close(); server = undefined;
  need(!progressFailed);
  console.log(JSON.stringify({state:'PASS', fixture_writes:fixtureWrites, session_posts:sessionPosts, session_deletes:sessionDeletes, session_renewals:sessionRenewals, missing_session_refusals:terminalResponses, progress_access_refusals:progressAccessFailures, action_writes:actionWrites, metadata_reconnect:true, injected_metadata_failures:injectedResponses, injected_console_errors:injectedConsoleErrors, ...metadataCounts}));
} catch (error) {
  failureClass = error?.name === 'TimeoutError' || /timeout|timed out|exceeded.*limit/i.test(error?.message || '') ? 'timeout' : error?.message === 'assertion' ? 'assertion' : 'driver';
  const failureName = ['TimeoutError','Error','AssertionError','TypeError','ReferenceError','RangeError'].includes(error?.name) ? error.name : 'Other';
  operation = 'failure-snapshot'; publishProgress();
  const snapshot = await failureSnapshot().catch(()=>({diagnostic_failed:true}));
  console.log(JSON.stringify({state:'FAIL', phase, checkpoint, failure_class:failureClass, failure_name:failureName, http_status:lastHTTPStatus, allow_writes:lastAllowWrites, can_edit_presentation:lastCanEditPresentation, send_status:sendStatus, reply_status:replyStatus, conversation_match:conversationMatch, ...counters(), ...diagnostics(), ...snapshot}));
  process.exitCode = 1;
} finally {
  phase = 'cleanup'; operation = 'cleanup';
  if (browser) { checkpoint = 'browser-close'; publishProgress(); await browser.close().catch(() => {}); }
  if (server) { checkpoint = 'server-close'; publishProgress(); await server.close().catch(() => {}); }
  stopProgress();
}
