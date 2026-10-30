import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { ReviewOperationWaitBadge, operationWaitLabel, validOperationWait, type ReviewOperationWait } from './review-operation-wait';
import { ReviewProgressView } from './review-progress-view';
import { validRoundProgress, type ReviewRoundProgress } from './review-progress';

const wait: ReviewOperationWait = { baseline_attempt_id: `review_attempt_${'a'.repeat(32)}`, retry_not_before: '2026-10-30T12:30:00Z', paused: false };
const progress: ReviewRoundProgress = {channel:'hosted',configured:true,revision:4,epoch:1,closed:false,minimum:2,maximum:4,taper:2,used:1,dry:0,tapered:false,pending:1,pending_execution:1,pending_adjudication:0,wait};
afterEach(() => vi.useRealTimers());

describe('retained automatic review waiting', () => {
  it('labels future and due deadlines without promising a provider start', () => {
    vi.useFakeTimers();vi.setSystemTime('2026-10-30T12:00:00Z');
    expect(operationWaitLabel(wait)).toBe('Cooldown · 30m');
    const future = renderToStaticMarkup(<ReviewOperationWaitBadge wait={wait}/>);
    expect(future).toContain('Cooldown · 30m');expect(future).toContain('earliest retry, not a promised review start');
    vi.setSystemTime('2026-10-30T12:30:01Z');expect(operationWaitLabel(wait)).toBe('Waiting');
    expect(operationWaitLabel({...wait,paused:true})).toBe('Paused');
    expect(renderToStaticMarkup(<ReviewOperationWaitBadge wait={{...wait,paused:true}}/>)).toContain('resume is explicit');
    expect(operationWaitLabel({paused:false})).toBe('Waiting');
  });
  it('preserves limits/counts without claiming that queued cooldown is a review in flight', () => {
    expect(validRoundProgress(progress,'hosted')).toBe(true);
    const html=renderToStaticMarkup(<ReviewProgressView value={progress}/>);
    expect(html).toContain('Up to 3 more reviews');expect(html).toContain('At least 1 more');expect(html).toContain('Taper 0/2');
    expect(html).not.toMatch(/in flight|awaiting review result|review pending/u);
    const adjudication=renderToStaticMarkup(<ReviewProgressView value={{...progress,pending:2,pending_adjudication:1}}/>);
    expect(adjudication).toContain('1 awaiting adjudication');
    const active=renderToStaticMarkup(<ReviewProgressView value={{...progress,wait:undefined}}/>);
    expect(active).toContain('1 awaiting review result');
  });
  it('requires boolean paused and attributable canonical deadline pairs', () => {
    expect(validOperationWait(wait,'hosted')).toBe(true);
    expect(validOperationWait({paused:true},'hosted')).toBe(true);
    for (const bad of [null,{}, {...wait,paused:null},{...wait,retry_not_before:'tomorrow'},{...wait,retry_not_before:null},{...wait,baseline_attempt_id:'other'}, {...wait,private_capture:'secret'}, {paused:false,retry_not_before:wait.retry_not_before}]) expect(validOperationWait(bad,'hosted')).toBe(false);
    expect(validOperationWait(wait,'cli')).toBe(false);
  });
});
