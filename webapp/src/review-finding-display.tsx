import { EditorialMarkdown } from './editorial-markdown';
import { reviewProviderText, reviewTextExcerpt } from './review-provider-text';
import { useWorkspace } from './workspace';
import { githubRepositoryURL } from './github-links';
import { RelativeTime } from './relative-time';
import type { ReviewFinding } from './types';

function label(value: string): string {
  const text = value.replaceAll('_', ' ');
  return text.charAt(0).toUpperCase() + text.slice(1);
}

export function CompactReviewFinding({ finding }: { finding: ReviewFinding }) {
  const { session } = useWorkspace();
  const repository = githubRepositoryURL(session.project.repository);
  const text = reviewProviderText(finding.detail, finding.title);
  const rationale = reviewTextExcerpt(text.rationale, 600);
  const decision = reviewTextExcerpt(reviewProviderText(finding.decision_reason, '').rationale, 320);
  const severity = finding.display_severity ?? finding.severity;
  const severityLabel = { critical: 'Critical', high: 'Major', medium: 'Minor', low: 'Minor', info: 'Trivial', unknown: 'Severity unknown' }[severity];
  const route = finding.route;
  const commit = route?.fix_sha ?? finding.fix_sha;
  const proof = route?.proof_note ?? finding.proof_note;
  const resolved = route?.fixed_at ?? finding.resolved_at;
  return <li className="review-finding-compact">
    <div className="review-finding-compact-header">
      <span className={`badge review-severity-${severity}`}>{severityLabel}</span>
      <h4 className="review-finding-compact-title">{finding.title}</h4>
      <span className="badge neutral">{label(finding.disposition)}</span>
    </div>
    {rationale.text && <EditorialMarkdown compact className="review-finding-compact-rationale">{rationale.text}</EditorialMarkdown>}
    <div className="review-finding-compact-decision"><strong>Decision</strong><EditorialMarkdown compact>{decision.text || (finding.decision_reason.trim() ? 'Decision details recorded below.' : 'No decision reason recorded.')}</EditorialMarkdown></div>
    {!!finding.detail && <details className="review-finding-disclosure">
      <summary>Full finding detail{rationale.truncated ? ' · continued' : ''}</summary>
      <EditorialMarkdown compact>{text.full || 'No issue prose remains after provider presentation cleanup. Original source is available below.'}</EditorialMarkdown>
    </details>}
    <details className="review-finding-disclosure review-finding-technical">
      <summary>Decision and fix details</summary>
      <EditorialMarkdown compact>{finding.decision_reason || 'No decision reason recorded.'}</EditorialMarkdown>
      <dl><div><dt>Decision actor</dt><dd>{finding.actor || 'Unknown'}</dd></div><div><dt>Decision time</dt><dd>{finding.decided_at ? <RelativeTime value={finding.decided_at} /> : 'Unknown'}</dd></div></dl>
      {route && <section><h5>{repository ? <a href={`${repository}/pull/${route.target_pull_number}`} target="_blank" rel="noopener noreferrer">Receiving PR #{route.target_pull_number}</a> : `Receiving PR #${route.target_pull_number}`}</h5><p>{label(route.disposition)} · {label(route.state)}</p><EditorialMarkdown compact>{route.reason || 'Awaiting receiving decision.'}</EditorialMarkdown><dl><div><dt>Receiving actor</dt><dd>{route.actor || 'Unknown'}</dd></div><div><dt>Receiving decision time</dt><dd>{route.decided_at ? <RelativeTime value={route.decided_at} /> : 'Unknown'}</dd></div></dl></section>}
      <section><h5>{route ? 'Receiving fix proof' : 'Fix proof'}</h5>{commit ? (repository ? <a href={`${repository}/commit/${commit}`} target="_blank" rel="noopener noreferrer"><code>{commit}</code> ↗</a> : <code>{commit}</code>) : <p>{route?.disposition === 'rejected' ? 'Receiving finding rejected; no fix claimed.' : 'No fix commit recorded.'}</p>}<EditorialMarkdown compact>{proof || 'No proof note recorded.'}</EditorialMarkdown><p>{resolved ? <>Resolved: <RelativeTime value={resolved} /></> : 'Resolution time unknown'}</p></section>
    </details>
    <details className="review-finding-disclosure review-finding-raw">
      <summary>Original source fields</summary>
      <pre>{JSON.stringify(finding, null, 2)}</pre>
    </details>
  </li>;
}
