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
  it('shows CLI quota waiting without presenting the server check as a reset ETA', () => {
    vi.useFakeTimers();vi.setSystemTime('2026-10-30T12:00:00Z');
    const cliProgress: ReviewRoundProgress = {...progress,channel:'cli'};
    expect(validRoundProgress(cliProgress,'cli')).toBe(true);
    expect(operationWaitLabel(wait,Date.now(),'cli')).toBe('Quota wait');
    const html=renderToStaticMarkup(<ReviewOperationWaitBadge wait={wait} channel="cli"/>);
    expect(html).toContain('Quota wait');
    expect(html).toContain('next automatic quota check');
    expect(html).toContain('not a provider reset or promised start');
    expect(html).not.toContain('Cooldown');
    const counts=renderToStaticMarkup(<ReviewProgressView value={cliProgress}/>);
    expect(counts).toContain('Up to 3 more reviews');
    expect(counts).not.toContain('awaiting review result');
    expect(operationWaitLabel({...wait,paused:true},Date.now(),'cli')).toBe('Paused');
    vi.setSystemTime('2026-10-30T12:30:01Z');
    expect(operationWaitLabel(wait,Date.now(),'cli')).toBe('Quota wait');
  });
  it('requires boolean paused and attributable canonical deadline pairs', () => {
    expect(validOperationWait(wait,'hosted')).toBe(true);
    expect(validOperationWait({paused:true},'hosted')).toBe(true);
    for (const bad of [null,{}, {...wait,paused:null},{...wait,retry_not_before:'tomorrow'},{...wait,retry_not_before:null},{...wait,baseline_attempt_id:'other'}, {...wait,private_capture:'secret'}, {paused:false,retry_not_before:wait.retry_not_before}]) expect(validOperationWait(bad,'hosted')).toBe(false);
    expect(validOperationWait(wait,'cli')).toBe(true);
    expect(validOperationWait(wait,'other')).toBe(false);
  });
});

describe('retained CLI full-capacity estimate',()=>{
  const allowance={kind:'cli_exhausted' as const,observed_at:'2026-10-30T12:00:00Z',remaining:0,limit:3,full_capacity_in_ms:3600000,rolling_window_ms:86400000};
  it('shows the provider estimate and server recheck as distinct facts, including elapsed and zero estimates',()=>{
    vi.useFakeTimers();vi.setSystemTime('2026-10-30T12:15:00Z');
    const value={...wait,cli_allowance:allowance};
    expect(validOperationWait(value,'cli')).toBe(true);
    const html=renderToStaticMarkup(<ReviewOperationWaitBadge wait={value} channel="cli"/>);
    expect(html).toContain('Provider full-capacity estimate:');expect(html).toContain('Next quota recheck:');
    expect(html.match(/class="badge /gu)).toHaveLength(1);
    expect(html).toContain('aria-describedby=');
    expect(html).toContain('tabindex="0"');
    expect(html).toContain('hidden="" role="tooltip"');
    expect(html).toMatch(/<span[^>]+review-quota-label[^>]+>Quota wait<\/span>/u);
    expect(html).not.toContain('<button');
    expect(html).toContain('>in 45m</time>');expect(html).toContain('>in 15m</time>');
    expect(html).toContain('dateTime="2026-10-30T13:00:00.000Z"');
    expect(html).toContain('does not establish the next available slot, refill pattern or review start');
    expect(html).not.toContain('(past estimate)');
    vi.setSystemTime('2026-10-30T13:15:00Z');
    expect(renderToStaticMarkup(<ReviewOperationWaitBadge wait={value} channel="cli"/>)).toContain('(past estimate)');
    const zero={...value,cli_allowance:{...allowance,full_capacity_in_ms:0}};
    expect(validOperationWait(zero,'cli')).toBe(true);
    expect(renderToStaticMarkup(<ReviewOperationWaitBadge wait={zero} channel="cli"/>)).toContain('(past estimate)');
    expect(renderToStaticMarkup(<ReviewOperationWaitBadge wait={{...value,paused:true}} channel="cli"/>)).toContain('Retained recheck:');
    expect(renderToStaticMarkup(<ReviewOperationWaitBadge wait={wait} channel="cli"/>)).not.toContain('Provider full-capacity estimate:');
  });
  it('requires exact attributable bounded allowance fields and never accepts this on Hosted',()=>{
    expect(validOperationWait({...wait,cli_allowance:allowance},'hosted')).toBe(false);
    expect(validOperationWait({paused:false,cli_allowance:allowance},'cli')).toBe(false);
    for(const change of [{observed_at:'2026-02-30T12:00:00Z'},{observed_at:'bad'},{remaining:1},{limit:0},{limit:10001},{full_capacity_in_ms:undefined},{full_capacity_in_ms:-1},{full_capacity_in_ms:86400001},{full_capacity_in_ms:0.5},{rolling_window_ms:0},{kind:'other'},{extra:'private'}]) expect(validOperationWait({...wait,cli_allowance:{...allowance,...change}},'cli')).toBe(false);
  });
});


describe('accepted Hosted queue priority', () => {
  it('shows Behind without a visible countdown and retains the next request countdown', () => {
    const queued = {...wait, queue_ahead_pull_number:4};
    const html = renderToStaticMarkup(<ReviewOperationWaitBadge wait={queued}/>);
    expect(html).toContain('Behind #4');
    expect(html).not.toContain('Cooldown');
    expect(html).not.toContain('earliest retry');
    expect(operationWaitLabel(wait, Date.parse('2026-10-30T12:00:00Z'))).toBe('Cooldown · 30m');
    expect(operationWaitLabel({...queued, paused:true})).toBe('Paused');
  });
  it('rejects malformed, paused and cross-channel priority hints', () => {
    expect(validOperationWait({...wait, queue_ahead_pull_number:4}, 'hosted')).toBe(true);
    for (const value of [0,-1,1.5,2147483648,'4',null]) expect(validOperationWait({...wait, queue_ahead_pull_number:value}, 'hosted')).toBe(false);
    expect(validOperationWait({...wait, queue_ahead_pull_number:4}, 'cli')).toBe(false);
    expect(validOperationWait({...wait, queue_ahead_pull_number:4,paused:true}, 'hosted')).toBe(false);
  });
});
