import type { Presentation, PresentationSnapshot } from './types';

// One connection's project lifetime owns the revision, including worker changes.
// Metadata and explicit reads use the same acceptor so an older response cannot
// undo a newer accepted presentation while either transport is in flight.
export function createAppearanceAcceptor(project: string, current: () => boolean, publish: (value: Presentation) => void): (value: Presentation) => boolean {
  let revision = -1;
  return value => {
    if (!current() || value.project_id !== project || !Number.isSafeInteger(value.revision) || value.revision <= revision) return false;
    revision = value.revision;
    publish(value);
    return true;
  };
}

// Start both finite reads even when one fails synchronously, and retain busy
// ownership until both settle. The caller handles terminal failures only for
// its still-current session; this helper never replaces feeds or bindings.
export async function refreshProjectView(read: () => Promise<PresentationSnapshot>, refreshQueries: () => Promise<void>, current: () => boolean, accept: (value: Presentation) => void): Promise<unknown[]> {
  const results = await Promise.allSettled([
    Promise.resolve().then(read).then(snapshot => { if (current()) accept(snapshot.presentation); }),
    Promise.resolve().then(refreshQueries),
  ]);
  return results.filter((result): result is PromiseRejectedResult => result.status === 'rejected').map(result => result.reason as unknown);
}
