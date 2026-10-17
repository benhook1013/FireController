import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

// All inputs are synthetic, private files produced by browser_test.go. Neither
// Playwright diagnostics nor request/credential bytes are printed on failure.
let server, browser;
let phase = 'configuration';
let checkpoint = '';
let counters = () => ({});
let failureClass = '';
let sendStatus = 0, replyStatus = 0, conversationMatch = false;
let failureSnapshot = async () => ({});
const need = (value) => { if (!value) throw new Error('assertion'); };
try {
  const config = JSON.parse(await fs.readFile(process.argv[2], 'utf8'));
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
  failureSnapshot = async () => {
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
    need(receipt.id || receipt.project_id);
    fixtureWrites++;
    return receipt;
  };
  let sessionPosts = 0, sessionDeletes = 0, sessionRenewals = 0, terminalResponses = 0, forbiddenWrites = 0, actionWrites = 0, errors = 0, external = 0;
 let explicitActions = false;
 let controlledMessageFailure = false;
 let controlledAccessFailure = false;
 const expectedMissingSessionRoute = url => { if (!controlledAccessFailure || !url) return false; try { const value = new URL(url); return value.origin === config.origin && ['/api/v1/instance', prefix + '/browser-session'].includes(value.pathname); } catch { return false; } };
  counters = () => ({session_posts:sessionPosts, session_deletes:sessionDeletes, session_renewals:sessionRenewals, terminal_responses:terminalResponses, errors, forbidden_writes:forbiddenWrites, action_writes:actionWrites, external});
  page.on('pageerror', () => errors++);
  page.on('console', value => { if (value.type() === 'error' && !(expectedMissingSessionRoute(value.location().url)) && !(controlledMessageFailure && value.location().url === config.origin + prefix + '/messages')) errors++; });
  page.on('response', value => { if (value.status() >= 400) { if (value.status() === 401 && expectedMissingSessionRoute(value.url())) terminalResponses++; else errors++; } });
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.origin !== config.origin) { external++; return; }
    if (request.method() === 'GET') return;
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
  await context.route('**/*', async route => {
    if (new URL(route.request().url()).origin !== config.origin) { await route.abort(); return; }
    await route.continue();
  });
  const appearance = () => page.evaluate(() => ({
    title: document.title, brand: document.querySelector('.brand')?.textContent.trim(),
    background: getComputedStyle(document.documentElement).backgroundColor,
    overrides: [...document.documentElement.style].filter(name => name.startsWith('--fc-')),
    accent: document.documentElement.style.getPropertyValue('--fc-accent_text'),
  }));
  const indicatorColors = () => page.evaluate(() => ({
    connection: getComputedStyle(document.querySelector('.connection-note'), '::before').backgroundColor,
    canvas: getComputedStyle(document.documentElement).backgroundColor,
    status: getComputedStyle(document.querySelector('.feed-card .status-dot')).backgroundColor,
    online: document.querySelector('.feed-card .status-dot').classList.contains('online'),
    surface: getComputedStyle(document.querySelector('.feed-card')).backgroundColor,
  }));
  const defaults = async () => {
    const value = await appearance();
    need(value.brand === 'FireController' && value.background === 'rgb(232, 231, 228)' && value.overrides.length === 0);
  };
  phase = 'connect';
  await page.goto(config.origin);
  await page.getByLabel('Project ID', {exact:true}).fill(config.project);
  await page.getByLabel('Project credential', {exact:true}).fill(token);
  need(await page.getByRole('checkbox').count() === 0);
  const readOnlyLogin=page.waitForResponse(response=>response.request().method()==='POST' && new URL(response.url()).pathname===prefix+'/browser-session');
  await page.getByRole('button', {name:'Connect to project', exact:true}).click();
  const readOnlyResponse=await readOnlyLogin; need(readOnlyResponse.status()===200);
  const readOnlyCapabilities=await readOnlyResponse.json(); need(readOnlyCapabilities.allow_writes===false && readOnlyCapabilities.can_edit_presentation===false);
  await page.getByRole('heading', {name:'Packaged smoke', exact:true}).waitFor();
  need(await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Settings',exact:true}).count()===0);
  await page.getByRole('button',{name:'Open Builder inbox',exact:true}).click();
  await page.getByRole('navigation',{name:'Project navigation'}).getByRole('link',{name:'Inbox',exact:true}).click();
  await page.getByRole('heading',{name:'Builder inbox',exact:true}).waitFor();
  need(await page.getByRole('button',{name:'New message',exact:true}).count()===0);
  need(actionWrites===0 && fixtureWrites===0 && forbiddenWrites===0);
  await defaults();
  need((await appearance()).title === 'Packaged smoke · FireController');
  const defaultIndicators = await indicatorColors();
  need(defaultIndicators.connection === 'rgb(103, 118, 89)' && defaultIndicators.status === (defaultIndicators.online ? 'rgb(103, 128, 93)' : 'rgb(169, 164, 160)'));
  const bindingKey = 'firecontroller.browser-session.v1';
  const binding = await page.evaluate(key => sessionStorage.getItem(key), bindingKey);
  need(binding && !binding.includes(token));
  const sessionID = JSON.parse(binding).sessionID;
  const cookieName = 'fc-local-browser-' + sessionID;
  const cookie = (await context.cookies()).find(value => value.name === cookieName);
  need(cookie?.httpOnly && cookie.sameSite === 'Strict' && sessionRenewals === 0);

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
  const receipt = await write('PUT', '/presentation', {expected_revision:0, presentation:{name:title, brand_name:'Smoke <literal>', theme}});

  need(receipt.revision === 1 && receipt.outcome === 'updated');
  phase = 'live-update';
  const heading = page.getByRole('heading', {name:title, exact:true});
  await heading.waitFor();
  await expect.poll(async () => (await appearance()).accent, {timeout:5000}).toBe('#fffffe');
  need((await appearance()).brand === 'Smoke <literal>' && (await appearance()).title === title + ' · FireController');
  const themedIndicators = await indicatorColors();
  need(themedIndicators.connection === 'rgb(0, 0, 0)' && themedIndicators.connection !== themedIndicators.canvas);
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
  need(fits);

  // Mutate through a separate fixture client only after the relevant browser
  // view has rendered its preceding state. No Refresh click or browser reload
  // may be used to observe these changes; the project stream owns delivery.
  phase = 'live-jobs';
  const selected = page.getByRole('button', {name:'Open Builder inbox', exact:true});
  await selected.click();
  const selectedBinding = await page.evaluate(key => sessionStorage.getItem(key), bindingKey);
  need(JSON.parse(selectedBinding).selectedworkerID === config.worker);
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Jobs', exact:true}).click();
  await page.getByLabel('Job status', {exact:true}).waitFor();
  const job = await write('POST', '/jobs', {title:'Live job before', brief:'Live brief before', worker_id:config.worker}, 201);
  await page.getByRole('link', {name:'Live job before', exact:true}).waitFor();
  await write('PATCH', '/jobs/' + job.id, {expected_revision:1, title:'Live job list updated'});
  await page.getByRole('link', {name:'Live job list updated', exact:true}).click();
  await page.getByText('Live brief before', {exact:true}).waitFor();
  await write('PATCH', '/jobs/' + job.id, {expected_revision:2, title:'Live job detail updated', brief:'Live brief <literal> updated'});
  await page.getByRole('heading', {name:'Live job detail updated', exact:true}).waitFor();
  await page.getByText('Live brief <literal> updated', {exact:true}).waitFor();

  phase = 'live-workers';
  need(await selected.getAttribute('aria-pressed') === 'true');
  await write('POST', '/workers', {name:'Live newcomer'});
  await page.getByRole('button', {name:'Open Live newcomer inbox', exact:true}).waitFor();
  need(await selected.getAttribute('aria-pressed') === 'true');
  await page.getByRole('link', {name:'Read Builder details', exact:true}).click();
  await page.getByText('No pause reason recorded.', {exact:true}).waitFor();
  await page.getByText('No primary job selected.', {exact:true}).waitFor();
  await write('PUT', '/workers/' + config.worker + '/pause', {expected_revision:0, paused:true, reason:'Live pause <literal>'});
  await page.getByText('Live pause <literal>', {exact:true}).waitFor();
  await write('PUT', '/workers/' + config.worker + '/primary-job', {expected_revision:0, job_id:job.id});
  await page.getByRole('link', {name:'Open primary job ' + job.id, exact:true}).waitFor();
  need(await selected.getAttribute('aria-pressed') === 'true');

  phase = 'live-notes';
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Notes', exact:true}).click();
  await page.getByLabel('Phase', {exact:true}).fill('live-proof');
  const filtered = page.waitForResponse(value => value.request().method() === 'GET' && new URL(value.url()).pathname === prefix + '/notes' && new URL(value.url()).searchParams.get('phase') === 'live-proof');
  await page.getByRole('button', {name:'Apply note filters', exact:true}).click();
  need((await filtered).status() === 200);
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

  phase = 'live-reviews';
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Reviews', exact:true}).click();
  await page.getByRole('heading', {name:'Review workspace unconfigured', exact:true}).waitFor();
  const pull = (number, state, title) => ({number, title, state, head_sha:'a'.repeat(40), base_ref:'main', base_sha:'b'.repeat(40), ci_state:'unknown', mergeability:'unknown', complete:true, runs:[], attempts:[]});
  const merged = pull(31, 'merged', 'Retained merged smoke PR');
  const open = pull(32, 'open', 'Live review before');
  const observation = (revision, queue) => ({expected_revision:revision, repository_id:991, source:'packaged-browser', observed_at:new Date().toISOString(), queue, retired:[]});
  await write('POST', '/reviews/observations', observation(0, [merged, open]), 201);
  await page.getByRole('link', {name:'Live review before', exact:true}).waitFor();
  await page.getByRole('link', {name:'Retained merged smoke PR', exact:true}).waitFor();
  const finished = new Date().toISOString();
  const run = {source_run_id:'live-hosted-round', channel:'hosted', head_sha:open.head_sha, reviewer:'Synthetic reviewer', scope:'Synthetic comparison', outcome:'completed', attributable:true, complete:true, started_at:finished, finished_at:finished, found:0, accepted:0, routed:0, finalized:true, coverage_limits:[], findings:[]};
  await write('POST', '/reviews/observations', observation(1, [merged, {...open, title:'Live review updated', runs:[run]}]), 201);
  await page.getByRole('link', {name:'Live review updated', exact:true}).waitFor();
  await page.getByRole('link', {name:'Retained merged smoke PR', exact:true}).waitFor();
  await page.getByRole('link', {name:/^Open hosted run live-hosted-round:/}).waitFor();
  need(await page.getByText('merged · retained history', {exact:true}).count() === 1);
  need(await selected.getAttribute('aria-pressed') === 'true');
  need(await page.locator('literal').count() === 0);
  need(new URL(page.url()).pathname === home + '/reviews');

  phase = 'reload';
  await page.reload();
  await page.getByRole('heading', {name:title, exact:true}).waitFor();
  await expect.poll(() => sessionRenewals, {timeout:5000}).toBe(1);
  await expect.poll(async () => JSON.parse(await page.evaluate(key => sessionStorage.getItem(key), bindingKey)).expiresAt, {timeout:5000}).not.toBe(JSON.parse(selectedBinding).expiresAt);
  const retained = JSON.parse(await page.evaluate(key => sessionStorage.getItem(key), bindingKey));
  need(retained.projectID === config.project && retained.instanceID === config.instance && retained.sessionID === sessionID && retained.selectedworkerID === config.worker);
  need(Date.parse(retained.expiresAt) > Date.now() && Date.parse(retained.expiresAt) <= Date.now() + 86400000 + 300000);
  const resumed = (await context.cookies()).find(value => value.name === cookieName);
  need(resumed?.value !== cookie.value && resumed?.httpOnly && resumed?.sameSite === 'Strict' && sessionPosts === 1);
  await expect.poll(async () => (await appearance()).accent, {timeout:5000}).toBe('#fffffe');

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
 const actionRootHistoryIndex=await page.evaluate(()=>history.state.idx);
  explicitActions = true;
  await page.getByLabel('Project ID', {exact:true}).fill(config.project);
  await page.getByLabel('Project credential', {exact:true}).fill(admin);
  need(await page.getByRole('checkbox').count()===0);
  const adminLogin=page.waitForResponse(response=>response.request().method()==='POST' && new URL(response.url()).pathname===prefix+'/browser-session');
  await page.getByRole('button', {name:'Connect to project',exact:true}).click();
  const adminResponse=await adminLogin;need(adminResponse.status()===200);
  const adminCapabilities=await adminResponse.json();need(adminCapabilities.allow_writes===true && adminCapabilities.can_edit_presentation===true);
  need(actionWrites===0 && forbiddenWrites===0);
  await page.getByRole('heading', {name:title,exact:true}).waitFor();
  await page.getByRole('button', {name:'Open Builder inbox',exact:true}).click();
  await page.getByRole('navigation', {name:'Project navigation'}).getByRole('link', {name:'Inbox',exact:true}).click();
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
  await context.close();
  await browser.close(); browser = undefined;
  await server.close(); server = undefined;
  console.log(JSON.stringify({state:'PASS', fixture_writes:fixtureWrites, session_posts:sessionPosts, session_deletes:sessionDeletes, session_renewals:sessionRenewals, missing_session_refusals:terminalResponses, action_writes:actionWrites}));
} catch (error) {
  failureClass = error?.name === 'TimeoutError' ? 'timeout' : error?.message === 'assertion' ? 'assertion' : 'driver';
  const snapshot = await failureSnapshot().catch(()=>({diagnostic_failed:true}));
  console.log(JSON.stringify({state:'FAIL', phase, checkpoint, failure_class:failureClass, send_status:sendStatus, reply_status:replyStatus, conversation_match:conversationMatch, ...counters(), ...snapshot}));
  process.exitCode = 1;
} finally {
  if (browser) await browser.close().catch(() => {});
  if (server) await server.close().catch(() => {});
}
