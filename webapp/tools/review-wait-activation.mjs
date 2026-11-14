import { test } from 'node:test';
import { randomUUID } from 'node:crypto';
import { writeFile, unlink } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react-swc';
import { chromium, expect } from '@playwright/test';

// Real DOM event sequences; no Controller connection, credentials or provider requests.
test('review wait supports click-only activation and one toggle per native activation', async () => {
  const root = fileURLToPath(new URL('../', import.meta.url));
  const name = `.review-wait-proof-${randomUUID()}`;
  const html = `${root}${name}.html`;
  const entry = `${root}${name}.tsx`;
  let server;
  let browser;
  try {
    await writeFile(entry, `import React from 'react';
import { createRoot } from 'react-dom/client';
import { ReviewOperationWaitBadge } from './src/review-operation-wait';
import './src/styles.css';
window.parentClicks = 0;
createRoot(document.getElementById('root')).render(<div onClick={() => window.parentClicks++}>
<ReviewOperationWaitBadge channel="cli" wait={{paused:false, baseline_attempt_id:'review_attempt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', retry_not_before:'2099-11-10T12:10:00Z'}} />
<button id="outside">Outside</button></div>);`);
    await writeFile(html, `<!doctype html><html><body><div id="root"></div><script type="module" src="/${name}.tsx"></script></body></html>`);
    server = await createServer({ root, configFile: false, plugins: [react()], server: { host: '127.0.0.1', port: 0 } });
    await server.listen();
    browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined });
    const context = await browser.newContext({ hasTouch: true });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.goto(`${server.resolvedUrls.local[0]}${name}.html`);
    const trigger = page.getByRole('button', { name: 'Quota wait', exact: true });
    const expanded = value => expect(trigger).toHaveAttribute('aria-expanded', String(value));
    await trigger.focus();
    await expanded(true);
    await page.keyboard.press('Escape');
    await expanded(false);
    // Assistive technology can emit click without pointerup, keydown or a new focus event.
    await trigger.evaluate(element => element.click());
    await expanded(true);
    await trigger.evaluate(element => element.click());
    await expanded(false);
    await page.keyboard.press('Enter');
    await expanded(true);
    await page.keyboard.press('Space');
    await expanded(false);
    await trigger.tap();
    await expanded(true);
    await trigger.tap();
    await expanded(false);
    await trigger.hover();
    await expanded(true);
    await page.keyboard.press('Escape');
    await expanded(false);
    await trigger.click();
    await expanded(true);
    await trigger.click();
    await expanded(false);
    expect(await page.evaluate(() => window.parentClicks)).toBe(0);
    await trigger.evaluate(element => element.click());
    await page.getByRole('region', { name: 'Review wait details' }).focus();
    await page.keyboard.press('Escape');
    await expanded(false);
    await expect(trigger).toBeFocused();
    await trigger.evaluate(element => element.click());
    await page.locator('#outside').focus();
    await expanded(false);
    expect(errors).toEqual([]);
    console.log(`Verified native activation sequences in Chromium ${browser.version()}`);
  } finally {
    await browser?.close();
    await server?.close();
    await Promise.all([html, entry].map(path => unlink(path).catch(error => { if (error.code !== 'ENOENT') throw error; })));
  }
});
