import { expect, it, vi } from 'vitest';

it('keeps diagnostic status corners consistent with the ordinary rounded pills', async () => {
  const fs = await vi.importActual<{ readFileSync(path: URL, encoding: 'utf8'): string }>('node:fs');
  const styles = fs.readFileSync(new URL('./styles.css', import.meta.url), 'utf8');
  const review = fs.readFileSync(new URL('./review-queue.css', import.meta.url), 'utf8');
  const radius = (css: string, selector: string) => css.split(selector + '{')[1]?.split('}')[0].match(/border-radius:([^;]+)/u)?.[1];
  expect(radius(styles, '.badge')).toBe('20px');
  expect(radius(review, '.review-attempt-state')).toBe(radius(styles, '.badge'));
});
