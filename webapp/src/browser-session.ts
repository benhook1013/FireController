// This per-tab record contains only public binding metadata. Session authority
// is retained in a nonce-specific HttpOnly cookie, never in this record.
export const browserSessionKey = 'firecontroller.browser-session.v1';
export const browserRecoveryKey = 'firecontroller.browser-recovery.v1';
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
function recoveryStorage(): StorageAccess | undefined { try { return globalThis.localStorage; } catch { return undefined; } }
function validBinding(value: unknown): value is BrowserSessionBinding {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const v = value as Record<string, unknown>;
  const fields = ['version', 'projectID', 'instanceID', 'sessionID', 'expiresAt', 'selectedworkerID'];
  return Object.keys(v).every(key => fields.includes(key) || key === 'projectHandle') && (v.projectHandle === undefined || typeof v.projectHandle === 'string' && /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/u.test(v.projectHandle)) && fields.every(key => Object.hasOwn(v, key)) && v.version === 1 && sessionIdentity(v.projectID) && sessionIdentity(v.instanceID) && sessionSelector(v.sessionID) && sessionExpiry(v.expiresAt) && (v.selectedworkerID === '' || sessionIdentity(v.selectedworkerID));
}
function recoveryBindings(target: StorageAccess | undefined): BrowserSessionBinding[] {
  try {
    const raw = target?.getItem(browserRecoveryKey);
    if (!raw || raw.length > 16384) return [];
    const value: unknown = JSON.parse(raw);
    return Array.isArray(value) && value.length <= 8 && value.every(validBinding) && new Set(value.map(binding => binding.projectID)).size === value.length ? value : [];
  } catch { return []; }
}
function writeRecovery(bindings: BrowserSessionBinding[], target: StorageAccess | undefined): void {
  try {
    if (bindings.length) target?.setItem(browserRecoveryKey, JSON.stringify(bindings));
    else target?.removeItem(browserRecoveryKey);
  } catch { /* Durable recovery is best effort; the current tab still works. */ }
}
function matchesProject(binding: BrowserSessionBinding, projectID: string | undefined): boolean {
  return binding.projectID === projectID || binding.projectHandle !== undefined && binding.projectHandle === projectID;
}
function forgetRecovery(binding: BrowserSessionBinding, target: StorageAccess | undefined): void {
  writeRecovery(recoveryBindings(target).filter(value => value.instanceID !== binding.instanceID || value.projectID !== binding.projectID || value.sessionID !== binding.sessionID), target);
}
function readTab(target: StorageAccess | undefined): BrowserSessionBinding | null {
  try {
    const raw = target?.getItem(browserSessionKey);
    if (!raw || raw.length > 2048) return null;
    const value: unknown = JSON.parse(raw);
    return validBinding(value) ? value : null;
  } catch { return null; }
}
// Inspect only metadata for the opening shell. Authentication still happens
// through the cookie-bound API before any private data can be rendered.
export function hasBrowserSession(projectID: string | undefined, target = storage(), recovery = recoveryStorage()): boolean {
  try { if (target?.getItem(browserSessionKey)) return true; } catch { /* Try durable metadata. */ }
  return recoveryBindings(recovery).some(binding => matchesProject(binding, projectID));
}
export function clearBrowserSession(target = storage(), recovery = recoveryStorage()): void {
  const binding = readTab(target);
  if (binding) forgetRecovery(binding, recovery);
  try { target?.removeItem(browserSessionKey); } catch { /* storage may be disabled */ }
}
export function saveBrowserSession(binding: BrowserSessionBinding, target = storage(), recovery = recoveryStorage()): boolean {
  if (!validBinding(binding) || !target) { clearBrowserSession(target, recovery); return false; }
  try { target.setItem(browserSessionKey, JSON.stringify(binding)); }
  catch { clearBrowserSession(target, recovery); return false; }
  // Tab metadata wins while it exists. The last verified binding per project
  // can recover a re-created tab without retaining credentials or private data.
  writeRecovery([...recoveryBindings(recovery).filter(value => value.projectID !== binding.projectID), binding].slice(-8), recovery);
  return true;
}

// Only expiry can change on renewal. Keep the currently selected worker and
// refuse another tab/project/instance without touching its stored metadata.
export function renewedBrowserSessionBinding(binding: BrowserSessionBinding, renewed: { session_id: string; instance_id: string; project_id: string; expires_at: string }): BrowserSessionBinding | null {
  if (!validBinding(binding) || renewed.session_id !== binding.sessionID || renewed.instance_id !== binding.instanceID || renewed.project_id !== binding.projectID || !sessionExpiry(renewed.expires_at)) return null;
  return { ...binding, expiresAt: renewed.expires_at };
}
export function loadBrowserSession(projectID: string | undefined, discarded?: (binding: BrowserSessionBinding) => void, target = storage(), recovery = recoveryStorage()): BrowserSessionBinding | null {
  const value = readTab(target);
  // The recorded expiry can lag a successful Set-Cookie whose body was lost.
  // Only the current-session GET can establish whether the cookie is valid.
  if (value && matchesProject(value, projectID)) return value;
  if (value) {
    clearBrowserSession(target, recovery); discarded?.(value);
  } else {
    try { target?.removeItem(browserSessionKey); } catch { /* storage may be disabled */ }
  }
  const candidates = recoveryBindings(recovery).filter(binding => matchesProject(binding, projectID));
  if (candidates.length !== 1) return null;
  const binding = candidates[0];
  // Rehydrate metadata only. Api.resume must still prove that its HttpOnly
  // cookie exists, is current and permits this exact project and instance.
  try { target?.setItem(browserSessionKey, JSON.stringify(binding)); } catch { /* connect reports unavailable tab storage */ }
  return binding;
}
export function terminalBrowserSession(error: { code: string; status: number }): boolean {
  return error.status === 401 || error.status === 403 || ['unauthorized', 'forbidden', 'instance_mismatch'].includes(error.code);
}
