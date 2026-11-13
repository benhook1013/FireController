import { chromium, expect } from '../../webapp/node_modules/@playwright/test/index.mjs';
import { readFile } from 'node:fs/promises';

// Focused CSS/navigation regression; synthetic geometry, no service or API calls.
// Run: node tools/packagedsmoke/queue-anchor-layout.mjs [chromium executable]
const browser = await chromium.launch({ headless: true, ...(process.argv[2] ? { executablePath: process.argv[2] } : {}) });
try {
  const page = await browser.newPage();
  const styles = await Promise.all(['styles.css', 'review-queue.css', 'delivery.css'].map(name => readFile(new URL(`../../webapp/src/${name}`, import.meta.url), 'utf8')));
  for (const width of [1440, 1024, 651, 650, 390, 320]) {
    await page.setViewportSize({ width, height: 900 });
    await page.setContent(`<style>${styles.join('\n')}</style>
      <a href="#head">Jump to queue head</a><div style="height:1000px"></div>
      <section class="delivery-reviews" id="queue"><ul class="review-queue">
        <li class="review-queue-row review-row-merged" style="height:400px">Preceding merged PR</li>
        <li class="review-queue-row" id="head" style="height:400px">Queue head</li>
      </ul></section><div style="height:1000px"></div>`);
    const clearance = width > 650 ? 260 : 160;
    // Hash links and the shared scrollIntoView path must agree, including repeats.
    await page.getByRole('link').click();
    await expect.poll(() => page.locator('#head').evaluate(node => Math.round(node.getBoundingClientRect().top))).toBe(clearance);
    for (let repeat = 0; repeat < 2; repeat++) {
      await page.evaluate(() => window.scrollTo(0, 0));
      await page.locator('#head').evaluate(node => node.scrollIntoView());
      await expect.poll(() => page.locator('#head').evaluate(node => Math.round(node.getBoundingClientRect().top))).toBe(clearance);
    }
    // Unrelated section jumps keep their original desktop/mobile clearance.
    await expect.poll(() => page.locator('#queue').evaluate(node => parseFloat(getComputedStyle(node).scrollMarginTop))).toBe(width > 650 ? 100 : 160);
    console.log(JSON.stringify({ width, queueRowTop: clearance, repeatedJumps: 'passed' }));
  }
} finally {
  await browser.close();
}
