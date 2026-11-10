import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { ReviewOperationWaitBadge, operationWaitLabel, reviewWaitExpandedForKey, validOperationWait } from './review-operation-wait';
import { ReviewProgressView } from './review-progress-view';
import { validReviewProgress, validRoundProgress, type ReviewRoundProgress } from './review-progress';

const retry = '2099-11-10T12:10:00Z';
const attempt = `review_attempt_${'a'.repeat(32)}`;
const behind = { paused: false, queue_ahead_pull_number: 4, baseline_attempt_id: attempt, retry_not_before: retry };
const cooldown = { paused: false, baseline_attempt_id: attempt, retry_not_before: retry };
const configured: ReviewRoundProgress = {
  channel: 'hosted', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0,
  maximum: 0, maximum_unbounded: true, taper: 1, used: 0, dry: 0, tapered: false, pending: 1,
  pending_execution: 1, pending_adjudication: 0,
};
const unconfigured = (channel: 'hosted' | 'cli') => ({
  channel, configured: false, revision: 0, epoch: 0, closed: false, minimum: 0, maximum: 0,
  taper: 0, used: 0, dry: 0, tapered: false, pending: 0,
});

describe('operation wait presentation', () => {
  it('shows an earlier accepted Hosted request as Behind without exposing its retained timer', () => {
    expect(validOperationWait(behind, 'hosted')).toBe(true);
    expect(operationWaitLabel(behind, Date.parse('2026-11-10T12:00:00Z'), 'hosted')).toBe('Behind #4');
    const html = renderToStaticMarkup(<ReviewOperationWaitBadge wait={behind} />);
    expect(html).toContain('Behind #4');
    expect(html).toContain('Queued behind PR #4');
    expect(html).not.toContain(retry);
    expect(html).not.toMatch(/cooldown|retry|recheck|tooltip|aria-describedby/iu);
  });

  it('makes a front Hosted cooldown deadline available on focus and touch', () => {
    const html = renderToStaticMarkup(<ReviewOperationWaitBadge wait={cooldown} />);
    expect(html).toContain('role="button"');
    expect(html).toContain('aria-expanded="false"');
    expect(html).toContain('aria-controls=');
    expect(html).toContain('Cooldown ·');
    expect(html).toContain('aria-describedby=');
    expect(html).toContain('role="tooltip"');
    expect(html).toContain('Earliest retry');
    expect(html).toContain(retry);
  });

  it('rejects queue priority on CLI, paused priority and unbound deadlines', () => {
    expect(validOperationWait(behind, 'cli')).toBe(false);
    expect(validOperationWait({ ...behind, paused: true }, 'hosted')).toBe(false);
    expect(validOperationWait({ paused: false, retry_not_before: retry }, 'hosted')).toBe(false);
  });

  it('keeps the wait details keyboard state on Enter, Space and Escape', () => {
    for (const key of ['Enter', ' ']) {
      expect(reviewWaitExpandedForKey(key, false)).toBe(true);
      expect(reviewWaitExpandedForKey(key, true)).toBe(false);
    }
    expect(reviewWaitExpandedForKey('Escape', true)).toBe(false);
    expect(reviewWaitExpandedForKey('ArrowDown', false)).toBeUndefined();
  });
});

describe('current progress decoder and presentation', () => {
  it('validates current channel shape and accepted Hosted queue priority', () => {
    const hosted = { ...configured, wait: behind };
    const progress = { project_id: 'project_one', evaluated_at: '2026-11-10T12:00:00Z', items: [{ pull_number: 7, channels: [hosted, unconfigured('cli')] }] };
    expect(validRoundProgress(hosted, 'hosted')).toBe(true);
    expect(validReviewProgress(progress, 'project_one', [7])).toBe(true);
    expect(validReviewProgress(progress, 'project_one', [8])).toBe(false);
    expect(validRoundProgress({ ...hosted, wait: { ...behind, queue_ahead_pull_number: 0 } }, 'hosted')).toBe(false);
  });

  it('preserves queued Hosted waits when round controls are unconfigured', () => {
    const unconfiguredHosted = { ...unconfigured('hosted'), pending_execution: 0, pending_adjudication: 0 };
    const behindWait = { ...unconfiguredHosted, wait: behind };
    const cooldownWait = { ...unconfiguredHosted, wait: cooldown };
    expect(validRoundProgress(behindWait, 'hosted')).toBe(true);
    expect(validRoundProgress(cooldownWait, 'hosted')).toBe(true);
    const behindHTML = renderToStaticMarkup(<ReviewProgressView channel="hosted" progress={behindWait} />);
    expect(behindHTML).toContain('Behind #4');
    expect(behindHTML).not.toContain(retry);
    const cooldownHTML = renderToStaticMarkup(<ReviewProgressView channel="hosted" progress={cooldownWait} />);
    expect(cooldownHTML).toContain('Cooldown ·');
    expect(cooldownHTML).toContain(retry);
    expect(cooldownHTML).not.toContain('No maximum');
  });

  it('renders lane-level allowance, wait and execution/adjudication stages when available', () => {
    const html = renderToStaticMarkup(<ReviewProgressView channel="hosted" progress={{ ...configured, wait: behind }} />);
    expect(html).toContain('Behind #4');
    expect(html).toContain('No maximum');
    expect(html).toContain('1 in execution · 0 awaiting adjudication');
    expect(renderToStaticMarkup(<ReviewProgressView channel="cli" progress={unconfigured('cli')} />)).toBe('');
  });
});
