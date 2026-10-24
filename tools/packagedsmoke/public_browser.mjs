import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

// Disposable fixture credentials belong only to this separate API writer.
// Anonymous page requests never receive these headers or a private session.
let server, browser, phase = 'configuration', diagnostics = {};
const need = value => { if (!value) throw new Error('assertion'); };
try {
  const config = JSON.parse(await fs.readFile(process.argv[2], 'utf8'));
  const admin = (await fs.readFile(config.admin_file, 'utf8')).trim();
  const prefix = `/api/v1/projects/${config.project}`;
  let writes = 0, pageErrors = 0, privateRequests = 0, pageWrites = 0, external = 0, publicReads = 0, historyReads = 0, pendingChecks = 0;
  const gate = async (action, kind, fail = false) => {
    const response = await fetch(`${config.gate_origin}/${action}?kind=${kind}${fail ? '&fail=1' : ''}`, { method: action === 'arrived' ? 'GET' : 'POST', signal: AbortSignal.timeout(5000) });
    need(response.status === 204); await response.body?.cancel();
  };
  const call = async (method, path, body, status = 200) => {
    const response = await fetch(config.origin + prefix + path, { method, headers: { Authorization: `Bearer ${admin}`, 'X-FC-Instance': config.instance, 'X-FC-Worker': config.worker, 'Idempotency-Key': `public-browser-${++writes}`, 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal: AbortSignal.timeout(5000) });
    need(response.status === status); return await response.json();
  };
  const publicRead = async path => { const response = await fetch(config.origin + path, { signal: AbortSignal.timeout(5000) }); need(response.status === 200); need(response.headers.get('cache-control') === 'no-store'); return await response.json(); };
  const publicPath = '/api/v1/public/projects/public-smoke';
  phase = 'fixture';
  const privateJob = await call('POST', '/jobs', { title: 'PRIVATE_JOB_SENTINEL', brief: 'PRIVATE_BRIEF_SENTINEL', worker_id: config.worker }, 201);
  await call('POST', '/notes', { worker_id: config.worker, job_id: privateJob.id, phase: 'PRIVATE_PHASE_SENTINEL', kind: 'reminder', body: 'PRIVATE_NOTE_SENTINEL' }, 201);
  await call('POST', '/messages', { recipient_worker_id: config.other_worker, body: 'PRIVATE_MESSAGE_SENTINEL', reply_to_message_id: '' }, 201);
  const pull = { number: 31, title: 'PRIVATE_PR_TITLE_SENTINEL', state: 'merged', head_sha: 'a'.repeat(40), base_ref: 'main', base_sha: 'b'.repeat(40), ci_state: 'unknown', mergeability: 'unknown', complete: true, runs: [], attempts: [] };
  const round = (source, channel) => { const at = new Date().toISOString(); return { source_run_id: source, channel, head_sha: pull.head_sha, reviewer: 'PRIVATE_REVIEWER_SENTINEL', scope: 'PRIVATE_SCOPE_SENTINEL', outcome: 'completed', attributable: true, complete: true, started_at: at, finished_at: at, found: 0, accepted: 0, routed: 0, finalized: true, coverage_limits: ['PRIVATE_COVERAGE_SENTINEL'], findings: [] }; };
  const hosted = [round('PRIVATE_HOSTED_RUN_ONE', 'hosted')], cli = [round('PRIVATE_CLI_RUN_ONE', 'cli')];
  const observe = (revision, runs = [...hosted, ...cli]) => call('POST', '/reviews/observations', { expected_revision: revision, repository_id: 991, source: 'PRIVATE_OBSERVER_SENTINEL', observed_at: new Date().toISOString(), queue: [{ ...pull, runs }], retired: [] }, 201);
  await observe(0);
  const manifest = { slug: 'public-smoke', presentation: { name: 'Public showcase', brand_name: 'Public flame', tagline: 'Shared development', headings: { hero_title: 'Public showcase', map: 'Public roadmap' }, theme: { accent_text: '#fffffe' } }, description: 'Approved public overview <literal>', links: [{ key: 'source', label: 'Project source', url: 'https://github.com/example/public-project' }], project_map: [{ key: 'delivery', title: 'Published roadmap card', description: 'Approved roadmap summary', phase: 'Delivery', status: 'Active', links: [] }], progress: [{ key: 'progress', title: 'Published milestone', description: 'Approved progress summary', phase: 'Build', status: 'Complete', links: [] }], worker_lanes: [{ key: 'builder', label: 'Public builder lane', headline: 'Shipping the public project', status: 'Active', milestones: ['Approved milestone'] }], reviews: [{ number: 31, title: 'Merged public work', url: 'https://github.com/example/public-project/pull/31' }] };
  const approved = { expected_revision: 0, enabled: true, manifest };
  const preview = await call('POST', '/publication/preview', approved);
  const publication = await call('PUT', '/publication', { ...approved, preview_sha256: preview.preview_sha256 });
  need(publication.revision === 1);
  const first = await publicRead(publicPath); need(first.revision === 1 && first.content.reviews[0].lifecycle === 'merged');
  need((await publicRead(`/api/v1/public/project-locations/${config.public_address}`)).slug === 'public-smoke');
  phase = 'launch';
  server = await chromium.launchServer({ headless: true, ...(config.executable ? { executablePath: config.executable } : {}), timeout: 5000, proxy: { server: config.proxy, bypass: '127.0.0.1,localhost' }, args: ['--disable-background-networking', '--no-first-run'] });
  console.log(JSON.stringify({ event: 'browser_started', pid: server.process().pid }));
  browser = await chromium.connect(server.wsEndpoint(), { timeout: 5000 });
  const context = await browser.newContext({ viewport: { width: 1280, height: 960 } });
  await context.addCookies([{ name: 'private-fixture-cookie', value: 'PRIVATE_COOKIE_SENTINEL', url: config.origin, httpOnly: true, sameSite: 'Strict' }]);
  await context.addInitScript(() => { const original = Storage.prototype.getItem; Storage.prototype.getItem = function(key) { if (key === 'firecontroller.browser-session.v1') throw new Error('PRIVATE_SESSION_READ'); return original.call(this, key); }; });
  await context.route('**/*', async route => { if (new URL(route.request().url()).origin !== config.origin) { external++; await route.abort(); } else await route.continue(); });
  context.on('request', request => {
    const url = new URL(request.url());
    if (url.origin !== config.origin) return;
    if (request.method() !== 'GET') pageWrites++;
    if (url.pathname.startsWith('/api/') && !url.pathname.startsWith('/api/v1/public/')) privateRequests++;
    if (url.pathname.startsWith('/api/v1/public/')) { publicReads++; if (url.pathname === publicPath + '/history') historyReads++; const headers = request.headers(); if (headers.authorization || headers.cookie || headers['x-fc-browser-session'] || headers['x-fc-worker'] || headers['x-fc-instance']) privateRequests++; }
  });
  const pendingGeometry = async target => {
    const shell = target.locator('.public-project-skeleton > .public-layout[role="status"]');
    await expect(shell).toBeVisible();
    need(await target.getByRole('button', { name: 'Try again', exact: true }).count() === 0);
    need(await target.locator('.loading-spinner, .public-unavailable').count() === 0);
    need(await shell.locator('h1,h2,h3,h4,button,a,[role="heading"]').count() === 0);
    const geometry = await shell.evaluate(node => {
      const rect = element => { const { x, y, width, height } = element.getBoundingClientRect(); return { x, y, width, height }; };
      const label = node.querySelector(':scope > .visually-hidden'), style = label && getComputedStyle(label);
      const copy = node.cloneNode(true); copy.querySelectorAll('.visually-hidden').forEach(element => element.remove());
      return { viewport: innerWidth, overflow: document.documentElement.scrollWidth > innerWidth,
        hiddenLabel: label?.textContent === 'Loading published project' && style.position === 'absolute' && style.width === '1px' && style.height === '1px' && style.overflow === 'hidden' && style.clip !== 'auto',
        visibleText: copy.textContent.trim(), layout: rect(node), hero: rect(node.querySelector('.public-hero')), navigation: rect(node.querySelector('.public-navigation')),
        content: rect(node.querySelector('.public-skeleton-content')) };
    });
    need(geometry.hiddenLabel && !geometry.overflow && geometry.visibleText === '');
    need(geometry.hero.width >= geometry.viewport * 0.8 && geometry.hero.height >= 150 && geometry.hero.height <= 720);
    need(geometry.navigation.y >= geometry.hero.y + geometry.hero.height && geometry.content.y >= geometry.navigation.y + geometry.navigation.height);
    need(geometry.layout.x >= 0 && geometry.layout.x + geometry.layout.width <= geometry.viewport + 1);
    pendingChecks++; return geometry;
  };
  const sameShellGeometry = (before, after) => {
    for (const section of ['layout', 'hero', 'navigation']) for (const field of ['x', 'width']) need(Math.abs(before[section][field] - after[section][field]) <= 1);
  };
  // The proxy holds actual successful daemon responses. Each boundary is
  // independent; no successful location/snapshot DTO is invented by the test.
  for (const [label, width, height] of [['desktop', 1280, 960], ['phone', 390, 900]]) {
    const pending = await context.newPage(); pending.setDefaultTimeout(5000); pending.setDefaultNavigationTimeout(5000); pending.on('pageerror', () => pageErrors++);
    await pending.setViewportSize({ width, height });
    phase = `pending-location-${label}`;
    await gate('arm', 'location'); await gate('arm', 'snapshot');
    await pending.goto(`${config.origin}/projects/${config.public_address}`);
    await gate('arrived', 'location');
    const resolving = await pendingGeometry(pending);
    if (config.proof_directory) await pending.screenshot({ path: `${config.proof_directory}/pending-location-${label}.png`, fullPage: true });
    await gate('release', 'location');
    phase = `pending-snapshot-${label}`;
    await gate('arrived', 'snapshot');
    const loading = await pendingGeometry(pending); sameShellGeometry(resolving, loading);
    need(Math.abs(resolving.hero.height - loading.hero.height) <= 1 && Math.abs(resolving.navigation.y - loading.navigation.y) <= 1);
    if (config.proof_directory) await pending.screenshot({ path: `${config.proof_directory}/pending-snapshot-${label}.png`, fullPage: true });
    await gate('release', 'snapshot');
    await pending.getByRole('heading', { name: 'Public showcase', exact: true }).waitFor();
    await expect(pending.locator('.public-project-skeleton')).toHaveCount(0);
    await pending.getByRole('heading', { name: 'Published roadmap card', exact: true }).waitFor();
    const loaded = await pending.locator('.public-project > .public-layout').evaluate(node => {
      const rect = element => { const { x, width } = element.getBoundingClientRect(); return { x, width }; };
      return { layout: rect(node), hero: rect(node.querySelector('.public-hero')), navigation: rect(node.querySelector('.public-navigation')) };
    });
    sameShellGeometry(loading, loaded);
    need(await pending.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await pending.close();
  }
  // A genuine failed initial current-snapshot response gets a retry control.
  // Retrying immediately returns to the skeleton while the real response waits.
  phase = 'initial-error';
  const retryPage = await context.newPage(); retryPage.setDefaultTimeout(5000); retryPage.setDefaultNavigationTimeout(5000); retryPage.on('pageerror', () => pageErrors++);
  await gate('arm', 'snapshot', true);
  await retryPage.goto(config.origin + '/public/public-smoke'); await gate('arrived', 'snapshot'); await gate('release', 'snapshot');
  await expect(retryPage.getByRole('alert')).toBeVisible();
  await expect(retryPage.getByRole('button', { name: 'Try again', exact: true })).toBeVisible();
  need(await retryPage.locator('.public-project-skeleton').count() === 0);
  phase = 'retry-pending';
  await gate('arm', 'snapshot'); await retryPage.getByRole('button', { name: 'Try again', exact: true }).click(); await gate('arrived', 'snapshot');
  await pendingGeometry(retryPage); need(await retryPage.getByRole('alert').count() === 0);
  await gate('release', 'snapshot'); phase = 'retry-loaded';
  await retryPage.getByRole('heading', { name: 'Public showcase', exact: true }).waitFor();
  need(await retryPage.getByRole('alert').count() === 0 && await retryPage.getByRole('button', { name: 'Try again', exact: true }).count() === 0);
  await expect(retryPage.locator('.public-project-skeleton')).toHaveCount(0); await retryPage.close();
  const pages = [];
  const page = await context.newPage(); pages.push(page);
  for (const page of pages) { page.setDefaultTimeout(5000); page.setDefaultNavigationTimeout(5000); page.on('pageerror', () => pageErrors++); }
  phase = 'desktop';
  await page.goto(config.origin + '/public/public-smoke');
  await page.getByRole('heading', { name: 'Public showcase', exact: true }).waitFor();
  const verify = async (page, rounds) => {
    const hostedLane = page.getByRole('region', {name:'Hosted review rounds',exact:true});
    await expect(hostedLane.getByRole('link', {name:/^Published Hosted round:/})).toHaveCount(Math.min(rounds,6));
    await expect(hostedLane.locator('.review-lane-heading')).toContainText(`${rounds} published ${rounds === 1 ? 'round' : 'rounds'}`);
    await expect(page.getByRole('region', {name:'CLI review rounds',exact:true}).getByRole('link', {name:/^Published CLI round:/})).toHaveCount(1);
    need(await page.getByRole('heading', { name: 'Public roadmap', exact: true }).count() === 1);
    need(await page.getByRole('heading', { name: 'Published roadmap card', exact: true }).count() === 1);
    need(await page.getByRole('heading', { name: 'Public builder lane', exact: true }).count() === 1);
    need(await page.getByRole('link', { name: 'Merged public work', exact: true }).count() === 1);
    need(await page.locator('.review-state-merged').getByText('Merged', { exact: true }).count() === 1);
    need(await page.getByLabel('Project credential', { exact: true }).count() === 0);
    need(!(await page.content()).includes('PRIVATE_') && !(await page.content()).includes(config.project) && !(await page.content()).includes(config.worker));
    need(await page.locator('literal').count() === 0);
    need(await page.locator('.brand .flame').evaluate(image => image.complete && image.naturalWidth > 0));
    await page.getByRole('navigation', { name: 'Public project sections' }).locator('a[href="#project-map"]').click();
    await expect.poll(() => page.evaluate(() => {
      const heading = document.querySelector('#project-map h2')?.getBoundingClientRect();
      const mast = document.querySelector('.mast')?.getBoundingClientRect();
      return !!heading && !!mast && heading.top >= mast.bottom;
    }), { timeout: 5000 }).toBe(true);
    need(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth && document.documentElement.style.getPropertyValue('--fc-accent_text') === '#fffffe'));
  };
  await verify(page, 1);
  need(await page.title() === 'Public showcase · FireController');
  need(await page.getByText('Powered by', { exact: false }).count() === 1);
  if (config.proof_directory) await page.screenshot({ path: config.proof_directory + '/desktop.png', fullPage: true });
  phase = 'private-quiet';
  const initialReads = publicReads;
  await call('PATCH', `/jobs/${privateJob.id}`, { expected_revision: 1, brief: 'PRIVATE_CHANGED_BRIEF_SENTINEL' });
  await page.waitForTimeout(150);
  const unchanged = await publicRead(publicPath);
  need(JSON.stringify(unchanged) === JSON.stringify(first) && publicReads === initialReads);
  await page.getByRole('button', { name: 'Browse published revisions', exact: true }).click();
  await page.getByRole('link', { name: 'Revision 1', exact: true }).waitFor();
  phase = 'live-summary';
  await page.evaluate(() => { window.publicSmokeIdentity = 'same-document'; });
  hosted.push(round('PRIVATE_HOSTED_RUN_TWO', 'hosted')); await observe(1);
  await verify(page, 2);
  await page.getByRole('link', { name: 'Revision 2', exact: true }).waitFor();
  need(await page.evaluate(() => window.publicSmokeIdentity) === 'same-document');
  const historyBeforeRefresh = historyReads;
  await page.getByRole('button', { name: 'Refresh published data', exact: true }).click();
  await expect.poll(() => historyReads, { timeout: 5000 }).toBe(historyBeforeRefresh + 1);
  phase = 'frozen-history';
  await page.getByRole('link', { name: 'Revision 1', exact: true }).click();
  await page.getByText('Revision 1 · frozen', { exact: true }).waitFor();
  await verify(page, 1);
  phase = 'phone';
  const phone = await context.newPage(); pages.push(phone); phone.setDefaultTimeout(5000); phone.setDefaultNavigationTimeout(5000); phone.on('pageerror', () => pageErrors++);
  await phone.setViewportSize({ width: 390, height: 900 });
  await phone.goto(config.origin + '/public/public-smoke');
  await phone.getByRole('heading', { name: 'Public showcase', exact: true }).waitFor(); await verify(phone, 2);
  hosted.push(round('PRIVATE_HOSTED_RUN_THREE', 'hosted')); await observe(2);
  await verify(phone, 3); await verify(page, 1);
  if (config.proof_directory) { await phone.screenshot({ path: config.proof_directory + '/phone.png', fullPage: true }); await page.screenshot({ path: config.proof_directory + '/history.png', fullPage: true }); }
  phase = 'public-window';
  // One ordinary bounded observation adds 100 eligible rounds. Preservation
  // retains the prior three Hosted rounds and omitted CLI round; only the
  // anonymous transfer window clips the resulting 103 Hosted summaries.
  await phone.evaluate(() => { window.publicSmokeIdentity = 'same-window-document'; });
  await observe(3, Array.from({ length: 100 }, (_, i) => round(`PRIVATE_HOSTED_WINDOW_${i}`, 'hosted')));
  await verify(phone, 100); await verify(page, 1);
  need(await phone.evaluate(() => window.publicSmokeIdentity) === 'same-window-document');
  const limited = await publicRead(publicPath), limitedReview = limited.content.reviews[0];
  need(limited.revision === 4 && limitedReview.hosted.length === 100 && limitedReview.hosted_has_more === true && limitedReview.cli.length === 1 && !limitedReview.cli_has_more);
  const hostedLane = phone.getByRole('region', { name: 'Hosted review rounds', exact: true });
  await expect(hostedLane.getByRole('link', {name:/^Published Hosted round:/})).toHaveCount(6);
  await expect(hostedLane.locator('.review-lane-heading')).toContainText('100 published rounds');
  need(await hostedLane.getByText(/^Round \d+$/).count() === 0);
  need(await phone.getByRole('region', { name: 'CLI review rounds', exact: true }).getByRole('link', {name:/^Published CLI round:/}).count() === 1);
  need(await hostedLane.getByText('More published Hosted rounds are outside this view.', { exact: false }).count() === 1);
  need(await hostedLane.getByText('these snapshots may not contain every round.', { exact: false }).count() === 1);
  if (config.proof_directory) await phone.screenshot({ path: config.proof_directory + '/phone-window.png', fullPage: true });
  phase = 'window-private-quiet';
  const limitedReads = publicReads;
  await call('PATCH', `/jobs/${privateJob.id}`, { expected_revision: 2, brief: 'PRIVATE_LIMITED_WINDOW_BRIEF_SENTINEL' });
  await phone.waitForTimeout(150);
  need(JSON.stringify(await publicRead(publicPath)) === JSON.stringify(limited) && publicReads === limitedReads);
  phase = 'window-history';
  const historyBeforeWindow = historyReads;
  await hostedLane.getByRole('link', { name: 'Browse retained published revisions', exact: true }).click();
  await expect.poll(() => historyReads, { timeout: 5000 }).toBe(historyBeforeWindow + 1);
  await phone.getByRole('link', { name: 'Revision 1', exact: true }).click();
  await phone.getByText('Revision 1 · frozen', { exact: true }).waitFor();
  await verify(phone, 1);
  need(JSON.stringify(await publicRead(`${publicPath}/history/1?generation=${first.generation}`)) === JSON.stringify(first));
  await phone.getByRole('link', { name: 'View the current publication', exact: true }).click();
  await verify(phone, 100);
  await expect(phone.getByText('Live published view', { exact: true })).toBeVisible();
  phase = 'unpublish';
  await call('POST', '/publication/unpublish', { expected_revision: 1 });
  // History navigation uses the unified address. Its terminal fallback is the
  // canonical inline sign-in view; legacy /public links use unavailable instead.
  need(new URL(phone.url()).pathname === '/projects/public-smoke');
  await phone.getByRole('heading', { name: 'Sign in to public-smoke', exact: true }).waitFor();
  await expect(phone.getByLabel('Project credential', { exact: true })).toBeVisible();
  need(await phone.locator('.public-project').count() === 0);
  phase = 'unpublish-retained';
  for (const path of [publicPath, `${publicPath}/history`, `${publicPath}/history/1?generation=${first.generation}`, `${publicPath}/events?cursor=${encodeURIComponent(first.cursor)}`]) { const response = await fetch(config.origin + path, { signal: AbortSignal.timeout(5000) }); need(response.status === 404); await response.body?.cancel(); }
  phase = 'unpublish-frozen';
  need(new URL(page.url()).pathname === '/projects/public-smoke/published/history/1');
  await page.reload(); await page.getByRole('heading', { name: 'Sign in to public-smoke', exact: true }).waitFor();
  need(await page.locator('.public-project').count() === 0);
  phase = 'unpublish-legacy';
  await phone.goto(config.origin + '/public/public-smoke');
  await phone.getByRole('heading', { name: 'Project unavailable', exact: true }).waitFor();
  need(await phone.locator('.public-project').count() === 0 && await phone.getByLabel('Project credential', { exact: true }).count() === 0);
  await page.goto(`${config.origin}/public/public-smoke/history/1?generation=${first.generation}`);
  await page.getByRole('heading', { name: 'Project unavailable', exact: true }).waitFor();
  need(await page.locator('.public-project').count() === 0);
  phase = 'privacy-final'; diagnostics = { page_errors: pageErrors, private_requests: privateRequests, page_writes: pageWrites, external_requests: external };
  need(pageErrors === 0 && privateRequests === 0 && pageWrites === 0 && external === 0);
  await context.close(); await browser.close(); browser = undefined; await server.close(); server = undefined;
  console.log(JSON.stringify({ state: 'PASS', fixture_calls: writes, public_requests: publicReads, private_requests: privateRequests, page_writes: pageWrites, page_errors: pageErrors, external_requests: external, desktop: 1280, phone: 390, pending_checks: pendingChecks, retry_recovered: true, unpublish_fallbacks: true, window_rounds: 100, window_has_more: limitedReview.hosted_has_more, window_private_quiet: true, window_history: true }));
} catch {
  console.log(JSON.stringify({ state: 'FAIL', phase, ...diagnostics })); process.exitCode = 1;
} finally {
  if (browser) await browser.close().catch(() => {});
  if (server) await server.close().catch(() => {});
}
