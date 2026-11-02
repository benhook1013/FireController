// Deployment checks carry only the same public asset names as index.html.
export const browserBuildCheckEvent = 'firecontroller:browser-build-check';
export function requestBrowserBuildCheck(): void {
  if (typeof window !== 'undefined') window.dispatchEvent(new Event(browserBuildCheckEvent));
}
const assetPattern = /^\/assets\/[A-Za-z0-9_-]+-[A-Za-z0-9_-]{8}\.(js|css)$/u;
export function browserAssetIdentity(assets: unknown): string | undefined {
  if (!Array.isArray(assets) || assets.length < 2 || assets.length > 8 || !assets.every(asset => typeof asset === 'string' && asset.length <= 160 && assetPattern.test(asset)) || new Set(assets).size !== assets.length || assets.filter(asset => asset.endsWith('.js')).length !== 1 || !assets.some(asset => asset.endsWith('.css'))) return undefined;
  return [...assets].sort().join('\n');
}
export function loadedBrowserIdentity(root: Document): string | undefined {
  const paths = [...root.querySelectorAll('script[type="module"][src],link[rel="stylesheet"][href]')].map(node => node.getAttribute(node.tagName.toLowerCase() === 'script' ? 'src' : 'href'));
  return browserAssetIdentity(paths);
}
export async function readBrowserIdentity(signal: AbortSignal): Promise<string> {
  const response = await fetch('/browser-build.json', { credentials: 'omit', redirect: 'error', cache: 'no-store', headers: { Accept: 'application/json' }, signal });
  if (!response.ok || response.headers.get('content-type')?.split(';')[0].trim().toLowerCase() !== 'application/json' || !response.body) { await response.body?.cancel(); throw new Error('Build unavailable'); }
  const reader = response.body.getReader();
  let raw = '', bytes = 0;
  const decoder = new TextDecoder('utf-8', { fatal: true });
  try {
    while (true) {
      const chunk = await reader.read();
      if (chunk.done) break;
      bytes += chunk.value.byteLength;
      if (bytes > 2048 || signal.aborted) throw new Error('Invalid build');
      raw += decoder.decode(chunk.value, { stream: true });
    }
    raw += decoder.decode();
  } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
  const value: unknown = JSON.parse(raw);
  if (!value || typeof value !== 'object' || Object.keys(value).sort().join(',') !== 'assets,version' || !('version' in value) || value.version !== 1 || !('assets' in value)) throw new Error('Invalid build');
  const identity = browserAssetIdentity(value.assets);
  if (!identity) throw new Error('Invalid build');
  return identity;
}

const attemptKey = 'firecontroller.browser-update.v1';
export type BrowserUpdateOptions = {
  loaded: string | undefined; read(signal: AbortSignal): Promise<string>; safe(): boolean;
  reload(): void; available(value: boolean): void; storage: Pick<Storage, 'getItem' | 'setItem'>;
};
// Events schedule bounded checks; no timer repeatedly checks for deployments.
export class BrowserBuildUpdate {
  private lastCheck = -Infinity;
  private scheduled?: ReturnType<typeof setTimeout>;
  private active?: AbortController;
  private stopped = false;
  private candidate?: string;
  private reloading = false;
  private followup = false;
  constructor(private readonly options: BrowserUpdateOptions) {}
  check = (): void => {
    if (this.stopped || !this.options.loaded || this.reloading || this.scheduled) return;
    if (this.active) { this.followup = true; return; }
    const delay = Math.max(0, 5000 - (Date.now() - this.lastCheck));
    this.scheduled = setTimeout(() => { this.scheduled = undefined; void this.run(); }, delay);
  };
  reconsider = (): void => { if (this.candidate && this.options.safe()) this.check(); };
  private async run(): Promise<void> {
    this.lastCheck = Date.now();
    const controller = new AbortController(); this.active = controller;
    const deadline = setTimeout(() => controller.abort(), 5000);
    try {
      const identity = await this.options.read(controller.signal);
      if (this.stopped || controller.signal.aborted) return;
      this.candidate = identity === this.options.loaded ? undefined : identity;
      this.options.available(Boolean(this.candidate));
      if (!this.candidate || !this.options.safe()) return;
      // Remember the target before navigating. A stale index or rollback can
      // never make the same tab repeatedly reload into this same target.
      try {
        const raw = this.options.storage.getItem(attemptKey);
        const saved = raw && JSON.parse(raw) as { targets?: unknown; at?: unknown } | null;
        const targets = saved && Array.isArray(saved.targets) && saved.targets.length <= 8 && saved.targets.every(target => typeof target === 'string' && target.length <= 2048) ? saved.targets as string[] : [];
        if (targets.includes(identity) || saved && typeof saved.at === 'number' && Date.now() - saved.at < 60_000) return;
        this.options.storage.setItem(attemptKey, JSON.stringify({ targets: [...targets, identity].slice(-8), at: Date.now() }));
      } catch { return; } // Without tab storage, leave the explicit control.
      this.reloading = true;
      this.options.reload();
    } catch { /* Rollout failures leave the current app and session usable. */ }
    finally { clearTimeout(deadline); if (this.active === controller) this.active = undefined; if (this.followup) { this.followup = false; this.check(); } }
  }
  update = (): void => {
    // Manual control obeys the identical draft/write guard. It can recover a
    // stale-index rollout once, but never silently discard local work.
    if (!this.candidate || !this.options.safe() || this.reloading) return;
    this.reloading = true;
    this.options.reload();
  };
  stop(): void { this.stopped = true; clearTimeout(this.scheduled); this.active?.abort(); }
}
