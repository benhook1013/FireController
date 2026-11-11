import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { ReviewAttemptBadge } from './review-attempt-badge';
import { reviewAttemptStatus } from './review-attempt-status';
import type { ReviewAttemptSummary } from './types';

const attempt: ReviewAttemptSummary = { source_attempt_id: 'attempt', channel: 'hosted', head_sha: 'a'.repeat(40), state: 'active', started_at: '2026-11-01T00:00:00Z', finished_at: '', duration_seconds: null, source_run_id: '', trigger_id: '42', provider_review_id: '' };

describe('accessible attempt activity details', () => {
  it('exposes channel-specific timing through a focusable badge and described tooltip', () => {
    const html = renderToStaticMarkup(<ReviewAttemptBadge channel="hosted" status={reviewAttemptStatus(attempt)!} />);
    expect(html).toContain('role="button" tabindex="0"');
    expect(html).toContain('aria-label="hosted execution: Reviewing"');
    expect(html).toContain('aria-expanded="false"');
    expect(html).toContain('hidden="" role="tooltip"');
    const id = html.match(/aria-describedby="([^"]+)"/u)![1];
    expect(html).toContain(`aria-controls="${id}"`);
    expect(html).toContain(`id="${id}"`);
    expect(html).toContain('dateTime="2026-11-01T00:00:00Z"');
    expect(html).toContain('Attempt began:');
    expect(html).toContain('trigger is recorded as sent');
    expect(html).toContain('not proof of reviewer start');
  });
  it('keeps unknown timing explicit and does not invent a time element', () => {
    const html = renderToStaticMarkup(<ReviewAttemptBadge channel="cli" status={reviewAttemptStatus({ ...attempt, channel: 'cli', started_at: '' })!} />);
    expect(html).toContain('Attempt start time is unknown');
    expect(html).not.toContain('<time');
    expect(html).toContain('Actual CLI process start time is not available');
  });
  it('leaves nonactivity labels and visible stop causes unchanged without new focus stops', () => {
    const status = reviewAttemptStatus({ ...attempt, state: 'failed', finished_at: '2026-11-01T00:01:00Z', reason: 'process_not_started' })!;
    const html = renderToStaticMarkup(<ReviewAttemptBadge channel="hosted" status={status} />);
    expect(html).toContain('Needs agent attention');
    expect(html).toContain('review-attempt-attention');
    expect(html).not.toMatch(/tabindex|role="button"|role="tooltip"/u);
  });
});
