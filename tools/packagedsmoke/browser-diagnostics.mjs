import fs from 'node:fs/promises';
import path from 'node:path';

export function classifyBrowserFailure(error) {
  if (typeof error?.matcherResult?.name === 'string' || error?.name === 'AssertionError' || error?.message === 'assertion') return 'assertion';
  if (error?.name === 'TimeoutError' || /timeout|timed out|exceeded.*limit/i.test(error?.message || '')) return 'timeout';
  return 'driver';
}

// Callers supply only fixed local counters and selected DOM counts/booleans.
// Never retain a caught error, matcher result, page text, URL or cookie here.
export async function retainBrowserFailure({ directory, filename, snapshot, collect, screenshot }) {
  if (!['review-failure.json', 'delivery-failure.json', 'reload-failure.json', 'failure.json'].includes(filename)) throw new Error('diagnostic_filename');
  const persist = async () => {
    if (!directory) return;
    await fs.mkdir(directory, { recursive: true, mode: 0o700 });
    await fs.writeFile(path.join(directory, filename), JSON.stringify(snapshot), { mode: 0o600 });
  };
  // Keep the original checkpoint even if the page is closed or the driver dies
  // during optional diagnostics. Filesystem failures remain genuine failures.
  await persist();
  if (collect) {
    try { await collect(snapshot); }
    catch { snapshot.dom_diagnostics_unavailable = true; }
    await persist();
  }
  if (directory && screenshot) {
    try { await screenshot(path.join(directory, filename.replace(/\.json$/u, '.png'))); }
    catch { snapshot.screenshot_unavailable = true; await persist(); }
  }
  return snapshot;
}
