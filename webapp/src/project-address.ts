import type { Project } from './types';

export function projectAddress(project: Project): string { return project.handle ?? project.id; }
export function projectHome(project: Project): string { return `/projects/${encodeURIComponent(projectAddress(project))}`; }
export function projectMatches(project: Project, selector: string | undefined): boolean { return selector === project.id || project.handle !== undefined && selector === project.handle; }
export function canonicalProjectLocation(project: Project, location: { pathname: string; search: string; hash: string }, authenticatedSelector?: string): string {
  const prefix = /^\/projects\/([^/]+)(?=\/|$)/u.exec(location.pathname);
  // Only the completed connection may attest an extra initial address alias.
  // Do not retain arbitrary aliases in caches or weaken resumed ID binding.
  const selector = prefix && decodeURIComponent(prefix[1]);
  const suffix = prefix && (projectMatches(project, selector ?? undefined) || authenticatedSelector !== undefined && selector === authenticatedSelector) ? location.pathname.slice(prefix[0].length) : '';
  return `${projectHome(project)}${suffix}${location.search}${location.hash}`;
}
