// Rasterize the existing shared flame without changing its artwork.
// Run with the repository Playwright Chromium installed: node webapp/generate-home-icons.mjs
import { chromium } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';
const source = await readFile(new URL('./src/flame-ember.svg', import.meta.url), 'utf8');
const browser = await chromium.launch({ headless: true });
try {
  const page = await browser.newPage();
  for (const [size, name] of [[180, 'apple-touch-icon'], [192, 'icon-192'], [512, 'icon-512']]) {
    const png = await page.evaluate(async ({ source, size }) => {
      const svg = new Image();
      svg.src = 'data:image/svg+xml;base64,' + btoa(source);
      await svg.decode();
      const canvas = document.createElement('canvas');
      canvas.width = canvas.height = size;
      const context = canvas.getContext('2d');
      // An opaque background avoids black corners when platforms apply their own icon mask.
      context.fillStyle = '#a51f27';
      context.fillRect(0, 0, size, size);
      context.drawImage(svg, 0, 0, size, size);
      return canvas.toDataURL('image/png').split(',')[1];
    }, { source, size });
    await writeFile(new URL('./public/assets/' + name + '.png', import.meta.url), Buffer.from(png, 'base64'));
  }
} finally { await browser.close(); }
