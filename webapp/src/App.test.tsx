import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { CapturedComparisonRefusal } from './App';

describe('captured comparison refusal presentation', () => {
  it('explains the repository binding refusal without suggesting a refresh', () => {
    const html = renderToStaticMarkup(<CapturedComparisonRefusal code="review_held" />);
    expect(html).toContain('role="alert"');
    expect(html).toContain('project repository no longer matches this captured comparison');
    expect(html).toContain('administrator to verify the repository binding');
    expect(html).not.toMatch(/refresh|sufficient|merge.ready|PRIVATE_|<script>/iu);
  });
  it('explains a retained-history capacity refusal without suggesting a refresh', () => {
    const html = renderToStaticMarkup(<CapturedComparisonRefusal code="review_capacity" />);
    expect(html).toContain('role="alert"');
    expect(html).toContain('supported review-history capacity');
    expect(html).toContain('operator to address the retained history limit');
    expect(html).not.toMatch(/refresh|sufficient|merge.ready|PRIVATE_|<script>/iu);
  });
});
