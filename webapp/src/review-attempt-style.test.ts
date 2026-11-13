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
