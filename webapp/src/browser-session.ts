// This per-tab record contains only public binding metadata. Session authority
// is retained in a nonce-specific HttpOnly cookie, never in this record.
export const browserSessionKey = 'firecontroller.browser-session.v1';
export type BrowserSessionBinding = {
  version: 1;
  projectID: string;
  projectHandle?: string;
  instanceID: string;
  sessionID: string;
  expiresAt: string;
  selectedworkerID: string;
};
type StorageAccess = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;
export function sessionIdentity(value: unknown): value is string { return typeof value === 'string' && /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/u.test(value); }
export function sessionSelector(value: unknown): value is string { return typeof value === 'string' && /^[a-f0-9]{32}$/u.test(value); }
export function sessionExpiry(value: unknown): value is string {
  return typeof value === 'string' && value.length <= 64 && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/u.test(value) && Number.isFinite(Date.parse(value)) && new Date(value).toISOString().slice(0, 19) === value.slice(0, 19);
}
export function newSessionID(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, '0')).join('');
}
function storage(): StorageAccess | undefined { try { return globalThis.sessionStorage; } catch { return undefined; } }
function validBinding(value: unknown): value is BrowserSessionBinding {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const v = value as Record<string, unknown>;
  const fields = ['version', 'projectID', 'instanceID', 'sessionID', 'expiresAt', 'selectedworkerID'];
  return Object.keys(v).every(key => fields.includes(key) || key === 'projectHandle') && (v.projectHandle === undefined || typeof v.projectHandle === 'string' && /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/u.test(v.projectHandle)) && fields.every(key => Object.hasOwn(v, key)) && v.version === 1 && sessionIdentity(v.projectID) && sessionIdentity(v.instanceID) && sessionSelector(v.sessionID) && sessionExpiry(v.expiresAt) && (v.selectedworkerID === '' || sessionIdentity(v.selectedworkerID));
}
export function clearBrowserSession(target = storage()): void { try { target?.removeItem(browserSessionKey); } catch { /* storage may be disabled */ } }
export function saveBrowserSession(binding: BrowserSessionBinding, target = storage()): boolean {
  if (!validBinding(binding) || !target) { clearBrowserSession(target); return false; }
  try { target.setItem(browserSessionKey, JSON.stringify(binding)); return true; } catch { clearBrowserSession(target); return false; }
}

// Only expiry can change on renewal. Keep the currently selected worker and
// refuse another tab/project/instance without touching its stored metadata.
export function renewedBrowserSessionBinding(binding: BrowserSessionBinding, renewed: { session_id: string; instance_id: string; project_id: string; expires_at: string }): BrowserSessionBinding | null {
  if (!validBinding(binding) || renewed.session_id !== binding.sessionID || renewed.instance_id !== binding.instanceID || renewed.project_id !== binding.projectID || !sessionExpiry(renewed.expires_at)) return null;
  return { ...binding, expiresAt: renewed.expires_at };
}
export function loadBrowserSession(projectID: string | undefined, discarded?: (binding: BrowserSessionBinding) => void, target = storage()): BrowserSessionBinding | null {
  let value: unknown;
  try {
    const raw = target?.getItem(browserSessionKey);
    if (!raw) return null;
    if (raw.length > 2048) { clearBrowserSession(target); return null; }
    value = JSON.parse(raw) as unknown;
  } catch { clearBrowserSession(target); return null; }
  if (!validBinding(value)) { clearBrowserSession(target); return null; }
  // The recorded expiry can lag a successful Set-Cookie whose body was lost.
  // Only the current-session GET can establish whether the cookie is valid.
  if (value.projectID !== projectID && (value.projectHandle === undefined || value.projectHandle !== projectID)) {
    clearBrowserSession(target); discarded?.(value); return null;
  }
  return value;
}
export function terminalBrowserSession(error: { code: string; status: number }): boolean {
  return error.status === 401 || error.status === 403 || ['unauthorized', 'forbidden', 'instance_mismatch'].includes(error.code);
}
