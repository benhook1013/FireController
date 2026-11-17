import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { CompactReviewFinding } from './review-finding-display';
import { WorkspaceContext, type Session } from './workspace';
import type { ReviewFinding } from './types';

const finding: ReviewFinding = {
  source_finding_id: 'synthetic-finding', title: 'Preserve original bytes',
  detail: '**Preserve original bytes**\n\nDuplicate keys disappear in the round trip.\n\n<details><summary>Prompt for AI Agents</summary>UNTRUSTED_PROMPT</details>\n\n```go\nLONG_CODE_EXAMPLE\n```',
  severity: 'unknown', disposition: 'accepted', decision_reason: 'Verified the decoder receives altered input. Preserve the original bytes.',
  actor: 'synthetic-actor', decided_at: '2026-10-24T00:00:00Z', fix_sha: 'a'.repeat(40), proof_note: 'Focused decoder checks passed.', resolved_at: '2026-10-24T01:00:00Z',
};

function initial(html: string): string { return html.slice(0, html.indexOf('<details')); }
function renderFinding(value: ReviewFinding, repository = 'example/project'): string {
  const session = { project: { repository } } as Session;
  return renderToStaticMarkup(<WorkspaceContext.Provider value={{ session, key: ['synthetic'], home: '/projects/synthetic', ready: true, fail: () => {}, selectWorker: () => {} }}><CompactReviewFinding finding={value} /></WorkspaceContext.Provider>);
}

describe('compact review finding', () => {
  it('puts title, display severity, disposition, rationale and concise decision ahead of closed disclosures', () => {
    const html = renderFinding({ ...finding, display_severity: 'low' });
    expect(initial(html)).toContain('review-severity-low');
    expect(initial(html)).toContain('Minor');
    expect(initial(html)).toContain('Preserve original bytes');
    expect(initial(html)).toContain('Accepted');
    expect(initial(html)).toContain('Duplicate keys disappear');
    expect(initial(html)).toContain('Verified the decoder receives altered input.');
    expect(initial(html)).not.toMatch(/UNTRUSTED_PROMPT|LONG_CODE_EXAMPLE|synthetic-actor|Focused decoder|aaaaaaaa/u);
    expect(html).toContain('<summary>Full finding detail</summary>');
    expect(html).toContain('<summary>Decision and fix details</summary>');
    expect(html).toContain('<summary>Original source fields</summary>');
    expect(html).not.toMatch(/<details[^>]*\bopen(?:=|[ >])/u);
    expect(html).toContain('UNTRUSTED_PROMPT');
    expect(html).toContain('LONG_CODE_EXAMPLE');
    expect(html).toContain('Focused decoder checks passed.');
    expect(html).toContain('dateTime="2026-10-24T00:00:00Z"');
    expect(html).toContain('Resolved: <time dateTime="2026-10-24T01:00:00Z"');
    expect(html).toContain(`href="https://github.com/example/project/commit/${'a'.repeat(40)}"`);
    expect(html).toContain('&quot;severity&quot;: &quot;unknown&quot;');
    expect(html).toContain('&lt;details&gt;&lt;summary&gt;Prompt for AI Agents');
  });

  it('keeps long rationale and decisions reachable without overwhelming the initial card', () => {
    const detail = 'Readable rationale. '.repeat(150) + 'RATIONALE_END';
    const decision = 'Verified behavior. '.repeat(70) + 'DECISION_END';
    const html = renderFinding({ ...finding, detail, decision_reason: decision });
    expect(initial(html)).toContain('…');
    expect(initial(html)).not.toContain('RATIONALE_END');
    expect(initial(html)).not.toContain('DECISION_END');
    expect(html).toContain('Full finding detail · continued');
    expect(html).toContain('RATIONALE_END');
    expect(html).toContain('DECISION_END');
  });

  it('falls back to raw severity and keeps route, fix and source metadata in disclosures', () => {
    const html = renderFinding({ ...finding, severity: 'high', disposition: 'routed', route: { id: 'synthetic-route', target_pull_number: 23, revision: 1, state: 'active', disposition: 'accepted', reason: 'Receiving change owns this fix.', actor: 'receiver', decided_at: '2026-10-24T02:00:00Z', fix_sha: 'b'.repeat(40), proof_note: 'Receiving proof.', fixed_at: '2026-10-24T03:00:00Z' } });
    expect(initial(html)).toContain('review-severity-high');
    expect(initial(html)).toContain('Major');
    expect(initial(html)).toContain('Routed');
    expect(initial(html)).not.toContain('Receiving');
    expect(html).toContain('Receiving PR #23');
    expect(html).toContain('href="https://github.com/example/project/pull/23"');
    expect(html).toContain('Receiving change owns this fix.');
    expect(html).toContain('b'.repeat(40));
    expect(html).toContain('Receiving proof.');
    expect(html).toContain('dateTime="2026-10-24T02:00:00Z"');
    expect(html).toContain('Resolved: <time dateTime="2026-10-24T03:00:00Z"');
    expect(html).toContain('synthetic-route');
  });

  it('never activates provider HTML or fetches images, including in the original-source disclosure', () => {
    const value = { ...finding, title: '<script>title</script>', detail: '<picture><source srcset="https://example.invalid/badge"><img src="https://example.invalid/x"></picture>\n\nRationale with `<img src="example">`.\n\n<script>alert(1)</script>\n\n![alt](https://example.invalid/other)', decision_reason: '[Unsafe](javascript:alert%281%29) <img src="https://example.invalid/decision">' };
    const html = renderFinding(value);
    expect(html).not.toMatch(/<script|<img|<picture|<source|href="javascript:|src="https:/u);
    expect(html).toContain('&lt;script&gt;title&lt;/script&gt;');
    expect(html).toContain('Rationale with');
    expect(html).toContain('Image: alt');
    expect(html).toContain('Original source fields');
  });

  it('cleans a repeated provider headline and does not invent GitHub links for an invalid repository', () => {
    const value = { ...finding, title: '**Preserve `json.RawMessage`**', detail: '<blockquote>**Preserve `json.RawMessage`**\n\nKeep original bytes.</blockquote>' };
    const html = renderFinding(value, 'https://untrusted.invalid/example');
    expect(initial(html)).toContain('<h4 class="review-finding-compact-title">**Preserve `json.RawMessage`**</h4>');
    expect(initial(html)).toContain('Keep original bytes.');
    expect(initial(html).match(/Preserve `json.RawMessage`/g)).toHaveLength(1);
    expect(html).not.toContain('href="https://github.com/');
    expect(html).toContain('a'.repeat(40));
  });
});
