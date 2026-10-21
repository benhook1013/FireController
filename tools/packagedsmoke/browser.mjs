import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

// All inputs are synthetic, private files produced by browser_test.go. Neither
// Playwright diagnostics nor request/credential bytes are printed on failure.
let server, browser;
let phase = 'configuration';
let checkpoint = 'none';
let diagnostics = () => ({});
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
  page.setDefaultTimeout(5000);
  page.setDefaultNavigationTimeout(5000);
  const prefix = '/api/v1/projects/' + config.project;
  let sessionPosts = 0, sessionDeletes = 0, forbiddenWrites = 0, errors = 0, external = 0;
  let presentationReads = 0, metadataStreams = 0, inboxStreams = 0, injectedResponses = 0, injectedConsoleErrors = 0;
  let injectedRequest;
  diagnostics = () => ({ presentation_reads:presentationReads, metadata_streams:metadataStreams, inbox_streams:inboxStreams, injected_metadata_failures:injectedResponses, injected_console_errors:injectedConsoleErrors, errors, external });
  page.on('pageerror', () => errors++);
  page.on('console', value => {
    if (value.type() !== 'error') return;
    // Only this exact controlled 400 may produce Chromium's resource-error
    // console entry. Every other console error still fails the workload.
    if (injectedRequest && injectedConsoleErrors === 0 && value.location().url === injectedRequest.url()
      && /^Failed to load resource: the server responded with a status of 400 \(Bad Request\)$/u.test(value.text())) injectedConsoleErrors++;
    else errors++;
  });
  page.on('response', value => {
    if (value.status() < 400) return;
    if (value.request() === injectedRequest && value.status() === 400) injectedResponses++;
    else errors++;
  });
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.origin !== config.origin) { external++; return; }
    if (request.method() === 'GET') {
      if (url.pathname === prefix + '/presentation') presentationReads++;
      if (url.pathname === prefix + '/project-events/stream') metadataStreams++;
      if (url.pathname === prefix + '/message-events/stream') inboxStreams++;
      return;
    }
    if (url.pathname === prefix + '/browser-session' && request.method() === 'POST') sessionPosts++;
    else if (url.pathname === prefix + '/browser-session' && request.method() === 'DELETE') sessionDeletes++;
    else forbiddenWrites++;
  });
  await context.route('**/*', async route => {
    if (new URL(route.request().url()).origin !== config.origin) { await route.abort(); return; }
    if (!injectedRequest && route.request().method() === 'GET' && new URL(route.request().url()).pathname === prefix + '/project-events/stream') {
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
  await page.getByRole('button', {name:'Connect to project', exact:true}).click();
  await page.getByRole('heading', {name:'Packaged smoke', exact:true}).waitFor();
  await defaults();
  need((await appearance()).title === 'Packaged smoke · FireController');
  const defaultIndicators = await indicatorColors();
  need(defaultIndicators.connection === 'rgb(103, 118, 89)' && defaultIndicators.status === (defaultIndicators.online ? 'rgb(103, 128, 93)' : 'rgb(169, 164, 160)'));
  phase = 'metadata-failure';
  checkpoint = 'metadata-notice';
  const reconnect = page.getByRole('button', {name:'Reconnect project updates', exact:true});
  await reconnect.waitFor();
  checkpoint = 'metadata-initial-counts';
  need(metadataStreams === 1 && presentationReads === 1 && injectedResponses === 1);
  await page.getByRole('button', {name:'Open Builder inbox', exact:true}).click();
  checkpoint = 'metadata-inbox-ready';
  await page.getByText('Live inbox connected', {exact:true}).waitFor();
  need(metadataStreams === 1 && inboxStreams === 1);
  checkpoint = 'none';
  const bindingKey = 'firecontroller.browser-session.v1';
  const binding = await page.evaluate(key => sessionStorage.getItem(key), bindingKey);
  need(binding && !binding.includes(token) && JSON.parse(binding).selectedworkerID === config.worker);
  const sessionID = JSON.parse(binding).sessionID;
  const cookieName = 'fc-local-browser-' + sessionID;
  const cookie = (await context.cookies()).find(value => value.name === cookieName);
  need(cookie?.httpOnly && cookie.sameSite === 'Strict');

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
  let fixtureWrites = 0;
  const update = async (revision, brand, key) => {
    const response = await fetch(config.origin + prefix + '/presentation', {
      method:'PUT', headers: {'Authorization':'Bearer '+admin, 'X-FC-Instance':config.instance, 'X-FC-Worker':config.worker, 'Idempotency-Key':key, 'Content-Type':'application/json'},
      body:JSON.stringify({expected_revision:revision, presentation:{name:title, brand_name:brand, theme}}), signal:AbortSignal.timeout(5000),
    });
    need(response.status === 200);
    const receipt = await response.json();
    need(receipt.revision === revision + 1 && receipt.outcome === 'updated');
    fixtureWrites++;
  };
  await update(0, 'Smoke <literal>', 'packaged-browser-presentation');
  await page.getByRole('heading', {name:'Packaged smoke', exact:true}).waitFor();
  need(metadataStreams === 1 && presentationReads === 1);
  phase = 'metadata-reconnect';
  await reconnect.click();
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

  await expect(reconnect).toHaveCount(0);
  await page.getByText('Live inbox connected', {exact:true}).waitFor();
  need(metadataStreams === 2 && presentationReads === 2 && inboxStreams === 1 && sessionPosts === 1);
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === binding);
  await expect(page.getByRole('button', {name:'Open Builder inbox', exact:true})).toHaveAttribute('aria-pressed', 'true');
  phase = 'metadata-live-update';
  await update(1, 'Smoke live <literal>', 'packaged-browser-presentation-live');
  await expect.poll(async () => (await appearance()).brand, {timeout:5000}).toBe('Smoke live <literal>');
  need(metadataStreams === 2 && presentationReads === 3 && inboxStreams === 1 && sessionPosts === 1);

  phase = 'reload';
  await page.reload();
  await page.getByRole('heading', {name:title, exact:true}).waitFor();
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === binding);
  const resumed = (await context.cookies()).find(value => value.name === cookieName);
  need(resumed?.value === cookie.value && sessionPosts === 1);
  await expect.poll(async () => (await appearance()).accent, {timeout:5000}).toBe('#fffffe');
  await page.getByText('Live inbox connected', {exact:true}).waitFor();
  need((await appearance()).brand === 'Smoke live <literal>' && metadataStreams === 3 && presentationReads === 4 && inboxStreams === 2);

  phase = 'disconnect';
  await page.getByRole('button', {name:'Disconnect', exact:true}).click();
  await page.getByRole('button', {name:'Connect to project', exact:true}).waitFor();
  await expect.poll(async () => (await context.cookies()).some(value => value.name === cookieName), {timeout:5000}).toBe(false);
  await defaults();
  need((await appearance()).title === 'FireController');
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === null);
  need(sessionPosts === 1 && sessionDeletes === 1 && forbiddenWrites === 0 && errors === 0 && external === 0 && injectedResponses === 1);
  await context.close();
  await browser.close(); browser = undefined;
  await server.close(); server = undefined;
  console.log(JSON.stringify({state:'PASS', fixture_writes:fixtureWrites, session_posts:sessionPosts, session_deletes:sessionDeletes, metadata_reconnect:true, injected_metadata_failures:injectedResponses, injected_console_errors:injectedConsoleErrors, presentation_reads:presentationReads, metadata_streams:metadataStreams, inbox_streams:inboxStreams}));
} catch {
  console.log(JSON.stringify({state:'FAIL', phase, checkpoint, ...diagnostics()}));
  process.exitCode = 1;
} finally {
  if (browser) await browser.close().catch(() => {});
  if (server) await server.close().catch(() => {});
}
