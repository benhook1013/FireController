import { expect, it, vi } from 'vitest';

it('inherits badge geometry and typography instead of maintaining a second pill design', async () => {
  const fs = await vi.importActual<{ readFileSync(path: URL, encoding: 'utf8'): string }>('node:fs');
  const styles = fs.readFileSync(new URL('./styles.css', import.meta.url), 'utf8');
  const review = fs.readFileSync(new URL('./review-queue.css', import.meta.url), 'utf8');
  const shared = styles.split('.badge{')[1]?.split('}')[0];
  expect(shared).toContain('border-radius:20px');
  expect(shared).toContain('text-transform:uppercase');
  const overrides = [...review.matchAll(/([^{}]+)\{([^{}]*)\}/gu)].filter(match => match[1].includes('.review-attempt-state'));
  expect(overrides.length).toBeGreaterThan(0);
  for (const [, , declarations] of overrides) {
    expect(declarations).not.toMatch(/(?:^|;)(?:padding(?:-[a-z]+)?|font(?:-[a-z]+)?|line-height|letter-spacing|text-transform|border-radius|height):/u);
  }
});


it('removes badge chrome only inside the three-line status while preserving outer badges', async () => {
  const fs = await vi.importActual<{ readFileSync(path: URL, encoding: 'utf8'): string }>('node:fs');
  const review = fs.readFileSync(new URL('./review-queue.css', import.meta.url), 'utf8');
  const embedded = review.split('.review-pill-status .badge{')[1]?.split('}')[0];
  for (const value of ['box-shadow:none', 'border:0', 'background:none', 'font:inherit', 'border-radius:0', 'padding:0', 'letter-spacing:normal', 'text-transform:none']) expect(embedded).toContain(value);
});


it('keeps embedded status badges wrapping despite the later channel badge nowrap rule', async () => {
  const fs = await vi.importActual<{ readFileSync(path: URL, encoding: 'utf8'): string }>('node:fs');
  const review = fs.readFileSync(new URL('./review-queue.css', import.meta.url), 'utf8');
  const states = fs.readFileSync(new URL('./review-states.css', import.meta.url), 'utf8');
  // The two-class embedded selector must override the shared one-class rule,
  // independent of stylesheet load order. Outer channel badges keep nowrap.
  const embedded = review.split('.review-pill-status .badge{')[1]?.split('}')[0];
  expect(states.split('.review-channel-position{')[1]?.split('}')[0]).toContain('white-space:nowrap');
  for (const value of ['max-width:100%', 'white-space:normal', 'overflow-wrap:anywhere']) expect(embedded).toContain(value);
  expect(embedded).not.toMatch(/(?:overflow(?:-[xy])?:(?:hidden|clip)|text-overflow:ellipsis)/u);
});
