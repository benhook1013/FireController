import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import fs from 'node:fs/promises';

// All inputs are synthetic, private files produced by browser_test.go. Neither
// Playwright diagnostics nor request/credential bytes are printed on failure.
let server, browser;
let phase = 'configuration';
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
  page.on('pageerror', () => errors++);
  page.on('console', value => { if (value.type() === 'error') errors++; });
  page.on('response', value => { if (value.status() >= 400) errors++; });
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.origin !== config.origin) { external++; return; }
    if (request.method() === 'GET') return;
    if (url.pathname === prefix + '/browser-session' && request.method() === 'POST') sessionPosts++;
    else if (url.pathname === prefix + '/browser-session' && request.method() === 'DELETE') sessionDeletes++;
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
  need(defaultIndicators.connection === 'rgb(103, 118, 89)' && defaultIndicators.status === 'rgb(169, 164, 160)');
  const bindingKey = 'firecontroller.browser-session.v1';
  const binding = await page.evaluate(key => sessionStorage.getItem(key), bindingKey);
  need(binding && !binding.includes(token));
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
  const response = await fetch(config.origin + prefix + '/presentation', {
    method:'PUT', headers: {'Authorization':'Bearer '+admin, 'X-FC-Instance':config.instance, 'X-FC-Worker':config.worker, 'Idempotency-Key':'packaged-browser-presentation', 'Content-Type':'application/json'},
    body:JSON.stringify({expected_revision:0, presentation:{name:title, brand_name:'Smoke <literal>', theme}}),
    signal:AbortSignal.timeout(5000),
  });
  need(response.status === 200);
  const receipt = await response.json();
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

  phase = 'reload';
  await page.reload();
  await page.getByRole('heading', {name:title, exact:true}).waitFor();
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === binding);
  const resumed = (await context.cookies()).find(value => value.name === cookieName);
  need(resumed?.value === cookie.value && sessionPosts === 1);
  await expect.poll(async () => (await appearance()).accent, {timeout:5000}).toBe('#fffffe');

  phase = 'disconnect';
  await page.getByRole('button', {name:'Disconnect', exact:true}).click();
  await page.getByRole('button', {name:'Connect to project', exact:true}).waitFor();
  await expect.poll(async () => (await context.cookies()).some(value => value.name === cookieName), {timeout:5000}).toBe(false);
  await defaults();
  need((await appearance()).title === 'FireController');
  need(await page.evaluate(key => sessionStorage.getItem(key), bindingKey) === null);
  need(sessionPosts === 1 && sessionDeletes === 1 && forbiddenWrites === 0 && errors === 0 && external === 0);
  await context.close();
  await browser.close(); browser = undefined;
  await server.close(); server = undefined;
  console.log(JSON.stringify({state:'PASS', fixture_writes:1, session_posts:sessionPosts, session_deletes:sessionDeletes}));
} catch {
  console.log(JSON.stringify({state:'FAIL', phase}));
  process.exitCode = 1;
} finally {
  if (browser) await browser.close().catch(() => {});
  if (server) await server.close().catch(() => {});
}
