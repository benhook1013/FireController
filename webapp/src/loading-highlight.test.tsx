import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router';
import { Jobs } from './App';
import { findingDisplayFingerprint, nextSemanticHighlight, pullDisplayFingerprint, runDisplayFingerprint } from './review-workspace';
import { DeliveryHeroSkeleton, MergeHeadSkeleton, PageSkeleton, PublicHistorySkeleton, PublicProjectSkeleton, WorkspaceSkeleton } from './loading';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewFinding, ReviewPullSummary, ReviewRunSummary, Worker } from './types';

const run: ReviewRunSummary = { source: 'import', id: 'snapshot_local_run_one', pull_number: 7, source_run_id: 'original_run', channel: 'cli', head_sha: 'old_head', reviewer: 'reviewer', scope: 'patch', coverage_limits: [], outcome: 'completed', attributable: true, complete: true, started_at: '2026-10-18T00:00:00Z', finished_at: '2026-10-18T00:01:00Z', found: 2, accepted: 1, routed: 0, finalized: true, fixed: 0 };
const pull: ReviewPullSummary = { number: 7, title: 'Project changes', state: 'open', head_sha: 'current_head', base_ref: 'main', base_sha: 'base_head', ci_state: 'pending', mergeability: 'mergeable', complete: true, run_count: 1, run_counts: { hosted: 0, cli: 1, subagent: 0, manual: 0 }, attempt_count: 1, recent_runs: [run], latest_attempts: [] };
const finding: ReviewFinding = { source_finding_id: 'original_finding', title: 'Finding', detail: 'Detail', severity: 'medium', disposition: 'accepted', decision_reason: 'Verified', actor: 'Builder', decided_at: '2026-10-18T00:00:00Z', fix_sha: '', proof_note: '', resolved_at: '' };

function jobsMarkup(inboxWorker: Worker | null) {
  const client = new QueryClient();
  const session = { worker: inboxWorker } as Session;
  try {
    return renderToStaticMarkup(<QueryClientProvider client={client}><MemoryRouter><WorkspaceContext.Provider value={{ session, key: ['fc', 'owned'], home: '/projects/owned', selectWorker: () => {}, fail: () => {}, ready: false }}><Jobs /></WorkspaceContext.Provider></MemoryRouter></QueryClientProvider>);
  } finally { client.clear(); }
}

describe('neutral stable loading', () => {
  it('exposes one accessible loading label and only decorative placeholders', () => {
    const html = renderToStaticMarkup(<PageSkeleton />);
    expect(html).toContain('role="status"><span class="visually-hidden">Loading content</span>');
    expect(html).toContain('aria-hidden="true"');
    expect(html).not.toContain('Loading…');
    expect(html).not.toContain('<button');
    expect(html.replace(/<span class="visually-hidden">[^<]*<\/span>/gu, '').replace(/<[^>]*>/gu, '')).toBe('');
  });
  it('uses the same two-sided Delivery structure and two quiet review cards while opening', () => {
    const hero = renderToStaticMarkup(<DeliveryHeroSkeleton />);
    expect(hero).toContain('class="delivery-hero skeleton-hero"');
    expect(hero).toContain('class="hero project-identity"');
    expect(hero).toContain('class="hero delivery-merge-head"');
    expect(hero.match(/class="review-lane skeleton-review-lane"/gu)).toHaveLength(2);
    expect(hero).toContain('data-channel="hosted"');
    expect(hero).toContain('data-channel="cli"');
    expect(hero).not.toMatch(/skeleton-connection|skeleton-row|<a|<button/gu);
    expect(hero.replace(/<[^>]*>/gu, '')).toBe('');
    const pending = renderToStaticMarkup(<MergeHeadSkeleton />);
    expect(pending).toContain('role="status"><span class="visually-hidden">Loading merge head</span>');
    expect(pending).toContain('aria-hidden="true"');
    expect(pending.replace(/<span class="visually-hidden">[^<]*<\/span>/gu, '').replace(/<[^>]*>/gu, '')).toBe('');
  });
  it('matches the page layout without exposing a previous project or forcing a sidebar', () => {
    const full = renderToStaticMarkup(<WorkspaceSkeleton />);
    expect(full).toContain('skeleton-hero');
    expect(full).toContain('role="status"><span class="visually-hidden">Opening project</span>');
    expect(full).toContain('project-main');
    expect(full).not.toContain('<aside');
    const inbox = renderToStaticMarkup(<WorkspaceSkeleton sidebar />);
    expect(inbox).toContain('workspace-grid');
    expect(inbox).toContain('<aside');
    expect(inbox.replace(/<span class="visually-hidden">[^<]*<\/span>/gu, '').replace(/<[^>]*>/gu, '')).toBe('');
  });
  it('reserves the public hero, navigation and prose without guessing project sections', () => {
    for (const embedded of [false, true]) {
      const html = renderToStaticMarkup(<PublicProjectSkeleton embedded={embedded} />);
      expect(html).toContain('class="public-layout" role="status"');
      expect(html).toContain('class="public-hero"');
      expect(html).toContain('class="public-hero-status"');
      expect(html).toContain('class="public-navigation"');
      expect(html.match(/role="status"/gu)).toHaveLength(1);
      expect(html).toContain('<span class="visually-hidden">Loading published project</span>');
      expect(html.includes('<main')).toBe(!embedded);
      expect(html.includes('<header')).toBe(!embedded);
      expect(html).not.toMatch(/<h[1-6]|<button|<a\b|<img|<section|<article|spinner|review-lane|delivery-merge-head/gu);
      expect(html.replace(/<span class="visually-hidden">[^<]*<\/span>/gu, '').replace(/<[^>]*>/gu, '')).toBe('');
    }
  });
  it('keeps a requested public revision-list placeholder compact and neutral', () => {
    const html = renderToStaticMarkup(<PublicHistorySkeleton />);
    expect(html).toContain('class="public-history-skeleton" role="status"');
    expect(html).toContain('<span class="visually-hidden">Loading published history</span>');
    expect(html.match(/class="skeleton-line/gu)).toHaveLength(1);
    expect(html.replace(/<span class="visually-hidden">[^<]*<\/span>/gu, '').replace(/<[^>]*>/gu, '')).toBe('');
    expect(html).not.toMatch(/<h[1-6]|<button|<a\b|<img|page-skeleton|spinner/gu);
  });
});

describe('page-local job filters', () => {
  it('defaults to Open jobs and All workers, independently of the inbox actor', () => {
    for (const worker of [null, { id: 'inbox_actor', name: 'Inbox actor', project_id: 'owned' }] as const) {
      const html = jobsMarkup(worker);
      expect(html).toContain('<option value="open" selected="">Open jobs</option>');
      expect(html).toContain('<option value="" selected="">All workers</option>');
      expect(html).not.toContain('Assigned to');
      expect(html).not.toContain('type="checkbox"');
      expect(html).not.toContain('Inbox actor');
      for (const state of ['active', 'blocked', 'parked', 'completed']) expect(html).toContain(`<option value="${state}">`);
    }
  });
});

describe('semantic review changes', () => {
  it('does not highlight initial hydration or equal values, but restarts for real changes', () => {
    const first = nextSemanticHighlight(undefined, pullDisplayFingerprint(pull));
    expect(first.version).toBe(0);
    expect(nextSemanticHighlight(first, pullDisplayFingerprint({ ...pull }))).toBe(first);
    const changed = nextSemanticHighlight(first, pullDisplayFingerprint({ ...pull, ci_state: 'success' }));
    expect(changed.version).toBe(1);
    expect(nextSemanticHighlight(changed, changed.fingerprint)).toBe(changed);
    expect(nextSemanticHighlight(changed, pullDisplayFingerprint({ ...pull, state: 'merged' })).version).toBe(2);
  });
  it('ignores snapshot metadata and transport IDs while retaining actual pill counts and status', () => {
    const sameDisplay = { ...pull, snapshot_id: 'new_snapshot', observed_at: 'later', recorded_at: 'later', recent_runs: [{ ...run, id: 'different_snapshot_local_id' }] };
    expect(pullDisplayFingerprint(sameDisplay)).toBe(pullDisplayFingerprint(pull));
    expect(runDisplayFingerprint({ ...run, id: 'new_id', source_run_id: 'same_original_binding' }, 'another_mismatching_head')).toBe(runDisplayFingerprint(run, pull.head_sha));
    for (const changed of [{ ...run, accepted: 0 }, { ...run, outcome: 'failed' as const }, { ...run, fixed: 1 }]) expect(runDisplayFingerprint(changed, pull.head_sha)).not.toBe(runDisplayFingerprint(run, pull.head_sha));
    expect(runDisplayFingerprint(run, run.head_sha)).not.toBe(runDisplayFingerprint(run, pull.head_sha));
  });
  it('includes displayed finding decisions, route state and fix proof, excluding route transport identity', () => {
    const route = { id: 'route_one', revision: 1, target_pull_number: 9, state: 'active' as const, disposition: 'accepted' as const, reason: 'Accepted', actor: 'Builder', decided_at: '', fix_sha: '', proof_note: '', fixed_at: '' };
    const routed = { ...finding, route };
    expect(findingDisplayFingerprint({ ...routed, route: { ...route, id: 'other', revision: 2 } })).toBe(findingDisplayFingerprint(routed));
    expect(findingDisplayFingerprint({ ...routed, route: { ...route, fix_sha: 'fixed_commit', proof_note: 'Verified' } })).not.toBe(findingDisplayFingerprint(routed));
    expect(findingDisplayFingerprint({ ...finding, disposition: 'rejected' })).not.toBe(findingDisplayFingerprint(finding));
  });
});
