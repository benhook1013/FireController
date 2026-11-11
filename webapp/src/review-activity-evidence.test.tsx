import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { validActivityEvidence, validRoundProgress, type ReviewActivityEvidence, type ReviewRoundProgress } from './review-progress';
import { reviewAttemptStatus } from './review-attempt-status';
import { ReviewAttemptBadge } from './review-attempt-badge';
import type { ReviewAttemptSummary } from './types';

const id = `review_attempt_${'a'.repeat(32)}`, head = 'b'.repeat(40), observed = '2026-11-11T09:00:15Z';
const cli: ReviewActivityEvidence = { attempt_id: id, head_sha: head, kind: 'cli_process_spawned', observed_at: observed, process_started_at: observed };
const hosted: ReviewActivityEvidence = { attempt_id: id, head_sha: head, kind: 'hosted_processing_observed', observed_at: observed, comment_updated_at: '2026-11-11T09:00:10Z', source_comment_id: 123, provider_run_id: '11111111-2222-4333-8444-555555555555', trigger_id: '42' };
const attempt: ReviewAttemptSummary = { source_attempt_id: id, head_sha: head, channel: 'cli', state: 'active', started_at: '2026-11-11T09:00:00Z', finished_at: '', duration_seconds: null, source_run_id: '', trigger_id: '', provider_review_id: '' };
const progress: ReviewRoundProgress = { channel: 'cli', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 5, taper: 2, used: 0, dry: 0, tapered: false, pending: 1, pending_execution: 1, pending_adjudication: 0 };

describe('current native activity evidence', () => {
  it('accepts only complete channel-specific safe fields', () => {
    expect(validActivityEvidence(cli, 'cli')).toBe(true);
    expect(validActivityEvidence(hosted, 'hosted')).toBe(true);
    expect(validRoundProgress({ ...progress, activity_evidence: cli }, 'cli')).toBe(true);
    expect(validActivityEvidence(cli, 'hosted')).toBe(false);
    expect(validActivityEvidence(hosted, 'cli')).toBe(false);
    for (const change of [{ attempt_id: 'foreign' }, { head_sha: 'unknown' }, { observed_at: '' }, { observed_at: 'not a date' }, { kind: 'running' }, { raw_body: 'private' }, { process_started_at: '2026-11-11T09:00:16Z' }]) {
      expect(validActivityEvidence({ ...cli, ...change }, 'cli')).toBe(false);
    }
    for (const change of [{ comment_updated_at: '' }, { comment_updated_at: '2026-11-11T09:00:16Z' }, { source_comment_id: 0 }, { source_comment_id: 1.5 }, { provider_run_id: 'unknown' }, { trigger_id: '' }, { trigger_id: '42\n' }, { process_started_at: observed }]) {
      expect(validActivityEvidence({ ...hosted, ...change }, 'hosted')).toBe(false);
    }
    for (const field of Object.keys(hosted)) {
      const missing = { ...hosted } as Record<string, unknown>; delete missing[field];
      expect(validActivityEvidence(missing, 'hosted')).toBe(false);
    }
  });
  it('shows the actual CLI spawn separately from preparation', () => {
    const status = reviewAttemptStatus(attempt, { ...progress, activity_evidence: cli })!;
    expect(status.activity?.evidence).toEqual(cli);
    expect(status.description).toContain(`CLI process started ${observed}`);
    expect(status.description).not.toContain('Actual CLI process start time is not available');
    const html = renderToStaticMarkup(<ReviewAttemptBadge channel="cli" status={status} />);
    expect(html).toContain('CLI process started:');
    expect(html).toContain(`dateTime="${observed}"`);
    expect(html).toContain(`dateTime="${attempt.started_at}"`);
    expect(html).toContain('does not establish when CodeRabbit’s remote analysis started');
  });
  it('trusts matched Hosted processing while labeling the first observation honestly', () => {
    const status = reviewAttemptStatus({ ...attempt, channel: 'hosted', trigger_id: '42' }, { ...progress, channel: 'hosted', activity_evidence: hosted })!;
    expect(status.activity?.evidence).toEqual(hosted);
    expect(status.description).toContain(`CodeRabbit processing observed ${observed}`);
    expect(status.description).toContain('It had started by the observation time');
    expect(status.description).not.toContain('Actual provider start time is not available');
    const html = renderToStaticMarkup(<ReviewAttemptBadge channel="hosted" status={status} />);
    expect(html).toContain('CodeRabbit processing observed:');
    expect(html).toContain('Provider comment updated:');
    expect(html).toContain('not an exact provider start time');
    expect(html).not.toContain('Provider started:');
  });
  it('does not attach evidence from a different attempt, channel, head, or trigger', () => {
    for (const change of [{ source_attempt_id: `review_attempt_${'c'.repeat(32)}` }, { head_sha: 'c'.repeat(40) }, { channel: 'hosted' as const }, { started_at: '2026-11-11T10:00:00Z' }]) {
      expect(reviewAttemptStatus({ ...attempt, ...change }, { ...progress, activity_evidence: cli })?.activity?.evidence).toBeUndefined();
    }
    expect(reviewAttemptStatus({ ...attempt, channel: 'hosted', trigger_id: '43' }, { ...progress, channel: 'hosted', activity_evidence: hosted })?.activity?.evidence).toBeUndefined();
    for (const state of ['preparing', 'prepared', 'posting'] as const) expect(reviewAttemptStatus({ ...attempt, state }, { ...progress, activity_evidence: cli })?.activity?.evidence).toBeUndefined();
    expect(reviewAttemptStatus(attempt, { ...progress, activity_evidence: cli }, undefined, true)?.activity).toBeUndefined();
    expect(reviewAttemptStatus(attempt, { ...progress, activity_evidence: cli, wait: { paused: true } })?.activity).toBeUndefined();
  });
});
