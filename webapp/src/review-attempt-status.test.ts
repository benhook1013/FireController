import { describe, expect, it } from 'vitest';
import { reviewAttemptStatus } from './review-attempt-status';
import type { ReviewAttemptSummary } from './types';
import type { ReviewRoundProgress } from './review-progress';

const attempt: ReviewAttemptSummary = { source_attempt_id: 'attempt', channel: 'cli', head_sha: 'a'.repeat(40), state: 'failed', reason: 'admission_changed', started_at: '2026-11-01T00:00:00Z', finished_at: '2026-11-01T00:00:20Z', duration_seconds: 20, source_run_id: '', trigger_id: '', provider_review_id: '' };
const progress: ReviewRoundProgress = { channel: 'cli', configured: true, revision: 1, epoch: 1, closed: false, minimum: 0, maximum: 7, taper: 2, used: 6, dry: 0, tapered: false, pending: 0, pending_execution: 0, pending_adjudication: 0 };

describe('attempt activity is separate from a PR review verdict', () => {
  it('shows unresolved process attention, retaining diagnostics without a Failed verdict', () => {
    const value = reviewAttemptStatus(attempt, progress, { text: 'Up next' });
    expect(value).toMatchObject({ text: 'Needs agent attention', kind: 'attention' });
    expect(value?.description).toContain('process issue, not a PR review verdict');
    expect(value?.description).toContain('No pending recovery is recorded');
    expect(value?.description).toContain('failed (admission_changed)');
    expect(progress.used).toBe(6); expect(attempt.reason).toBe('admission_changed');
  });
  it('does not infer retry from head movement, queue position, missing progress or combined pending counts', () => {
    expect(reviewAttemptStatus({ ...attempt, head_sha: 'b'.repeat(40) }, undefined, { text: 'Up next' })?.description).toContain('Current recovery status is unavailable');
    expect(reviewAttemptStatus(attempt, { ...progress, pending: 1, pending_execution: undefined, pending_adjudication: undefined })).toMatchObject({ text: 'Pending' });
  });
  it('uses explicit execution and adjudication stages instead of the older failure', () => {
    expect(reviewAttemptStatus(attempt, { ...progress, pending: 1, pending_execution: 1 })).toMatchObject({ text: 'Retry pending' });
    expect(reviewAttemptStatus(attempt, { ...progress, pending: 1, pending_adjudication: 1 })).toEqual({ text: 'Needs adjudication' });
    expect(reviewAttemptStatus(undefined, { ...progress, pending: 1, pending_execution: 1 })).toMatchObject({ text: 'Pending' });
  });
  it('defers to actual provider waits or pause without claiming a running retry', () => {
    for (const paused of [false, true]) expect(reviewAttemptStatus(attempt, { ...progress, pending: 1, pending_execution: 1, wait: { paused, baseline_attempt_id: `review_attempt_${'a'.repeat(32)}`, retry_not_before: '2026-11-02T00:00:00Z' } })).toBeUndefined();
  });
  it('distinguishes preparation from active execution without using the current head as a cancellation signal', () => {
    for (const state of ['preparing', 'prepared', 'posting', 'active'] as const) {
      expect(reviewAttemptStatus({ ...attempt, state, reason: undefined, finished_at: '' }, progress)).toEqual({ text: state === 'active' ? 'Reviewing' : 'Preparing' });
    }
    expect(reviewAttemptStatus({ ...attempt, state: 'active' }, progress)).toBeUndefined();
  });
  it('does not hide an uncertain provider outcome behind a new request, adjudication or closure', () => {
    for (const change of [{}, { pending: 1, pending_execution: 1 }, { pending: 1, pending_adjudication: 1 }, { closed: true }, { wait: { paused: false } }]) {
      expect(reviewAttemptStatus({ ...attempt, state: 'uncertain' }, { ...progress, ...change })).toMatchObject({ text: 'Needs agent attention', kind: 'attention' });
    }
  });
  it('explains a stopped dependency attempt without inventing an automatic retry', () => {
    const result = reviewAttemptStatus({ ...attempt, state: 'held', reason: 'predecessor_unfinished' }, progress);
    expect(result?.text).toBe('Dependency pending'); expect(result?.description).toContain('No retry is confirmed');
    expect(reviewAttemptStatus({ ...attempt, state: 'held', reason: 'predecessor_unfinished' }, { ...progress, pending: 1, pending_execution: 1 })?.text).toBe('Retry pending');
  });
  it('keeps merged/closed history quiet and closed grants separate from unfinished activity', () => {
    expect(reviewAttemptStatus(attempt, progress, { text: 'Up next' }, true)).toMatchObject({ text: 'Past attempt', kind: 'history' });
    expect(reviewAttemptStatus({ ...attempt, state: 'active', finished_at: '' }, progress, undefined, true)?.text).toBe('Past attempt');
    expect(reviewAttemptStatus(attempt, { ...progress, closed: true })).toEqual({ text: 'Closed' });
    expect(reviewAttemptStatus({ ...attempt, state: 'active', finished_at: '' }, { ...progress, closed: true })?.text).toBe('Reviewing');
    expect(reviewAttemptStatus({ ...attempt, state: 'completed' }, progress, undefined, true)).toBeUndefined();
    expect(reviewAttemptStatus({ ...attempt, state: 'uncertain' }, progress, undefined, true)?.description).toContain('provider outcome remains unconfirmed');
  });
  it('keeps cancelled and unknown states distinct and resumes normal position after completion', () => {
    expect(reviewAttemptStatus({ ...attempt, state: 'cancelled' }, progress)).toMatchObject({ text: 'Attempt cancelled', kind: 'history' });
    expect(reviewAttemptStatus({ ...attempt, state: 'unknown' }, progress)).toMatchObject({ text: 'Status unknown', kind: 'attention' });
    expect(reviewAttemptStatus({ ...attempt, state: 'completed' }, progress, { text: 'Up next' })).toEqual({ text: 'Up next' });
  });
});
