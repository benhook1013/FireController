import type { Project, ReviewPullSummary } from './types';
import type { ReviewFronts } from './review-fronts';
import { githubRepositoryURL } from './github-links';

// Only the retained comparison and bounded thread facts are consumed here.
// The assessment's CI/policy verdict is not a browser merge authorization.
export interface ReviewSourceReadiness {
  unassigned_routes_pending?: number;
  project_id: string;
  refresh_id: string;
  source: 'observed_comparison';
  state: 'ready_as_observed'|'held'|'unconfigured';
  configuration_revision: number;
  source_observed_at: string;
  reasons: string[];
  target: {
    repository_id: number; repository_name: string; pull_number: number;
    head_sha: string; base_sha: string; base_ref: string;
    parent_pull_number: number; parent_sha: string; observed_at: string;
  };
  review_state: {
    state: 'complete'|'unproven'; reason: string; observed_at: string;
    pull_state: string; draft: boolean; mergeable: string; merge_state_status: string;
    review_decision: string|null;
    total_threads: number; unresolved_current: number; unresolved_outdated: number;
  };
}

export function githubThreadContext(value: ReviewSourceReadiness | null | undefined, project: Project, pull: ReviewPullSummary, snapshot: string, fronts: ReviewFronts | undefined): { count: number; url: string; title: string } | undefined {
  const repository = githubRepositoryURL(project.repository);
  if (!value || value.state === 'unconfigured' || !repository || !fronts?.configured || fronts.project_id !== project.id || fronts.snapshot_id !== snapshot || fronts.repository_id !== project.github_repository_id || !fronts.queue.includes(pull.number) || value.project_id !== project.id || value.configuration_revision !== fronts.configuration_revision || value.reasons.some(reason => reason === 'source_configuration_changed' || reason === 'source_comparison_changed')) return undefined;
  const { target, review_state: threads } = value;
  // Current fronts identify the selected workspace snapshot, not the unrelated
  // source-refresh ID. An old pinned history view cannot inherit today's facts.
  if (threads.state !== 'complete' || target.repository_id !== project.github_repository_id || target.repository_name !== project.repository || target.pull_number !== pull.number || !pull.head_sha || !pull.base_sha || !pull.base_ref || target.head_sha !== pull.head_sha || target.base_sha !== pull.base_sha || target.base_ref !== pull.base_ref) return undefined;
  const count = threads.unresolved_current + threads.unresolved_outdated;
  if (count === 0) return undefined;
  const parent = target.parent_pull_number ? `PR #${target.parent_pull_number} at ${target.parent_sha}` : target.parent_sha;
  return { count, url: `${repository}/pull/${pull.number}`, title: `All-provider GitHub threads as of ${threads.observed_at}. ${threads.unresolved_current} current and ${threads.unresolved_outdated} outdated unresolved; ${threads.total_threads} total threads. Observed comparison: head ${target.head_sha}; base ${target.base_ref} at ${target.base_sha}; effective parent ${parent}. Retained observation, not a live GitHub refresh.` };
}
