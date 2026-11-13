export type BackupState = 'healthy' | 'running' | 'failed' | 'overdue' | 'unconfigured' | 'unknown';
export interface BackupHealthRow { state: BackupState; last_success_at?: string; last_attempt_at?: string; reason?: string }
export interface BackupHealth { version: 1; checked_at: string; local: BackupHealthRow; offsite: Array<BackupHealthRow & { label: string }> }

function object(value: unknown): value is Record<string, unknown> { return value !== null && typeof value === 'object' && !Array.isArray(value); }
function stamp(value: unknown): value is string { return typeof value === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(value) && Number.isFinite(Date.parse(value)); }
const states = new Set(['healthy', 'running', 'failed', 'overdue', 'unconfigured', 'unknown']);
function row(value: unknown, offsite = false): boolean {
  return object(value) && typeof value.state === 'string' && states.has(value.state) && Object.keys(value).every(key => ['state','last_success_at','last_attempt_at','reason', ...(offsite ? ['label'] : [])].includes(key))
    && (value.last_success_at === undefined || stamp(value.last_success_at)) && (value.last_attempt_at === undefined || stamp(value.last_attempt_at))
    && (value.reason === undefined || typeof value.reason === 'string' && /^[a-z][a-z0-9_]{0,79}$/.test(value.reason))
    && (!offsite || typeof value.label === 'string' && value.label.trim() === value.label && value.label.length > 0 && new TextEncoder().encode(value.label).length <= 80 && !/[\p{C}]/u.test(value.label));
}
export function validBackupHealth(value: unknown): value is BackupHealth {
  return object(value) && Object.keys(value).length === 4 && value.version === 1 && stamp(value.checked_at) && row(value.local)
    && Array.isArray(value.offsite) && value.offsite.length <= 8 && value.offsite.every(item => row(item, true));
}
export function backupHeadline(health: BackupHealth | undefined, unavailable: boolean, now = Date.now()): { label: string; tone: string } {
  if (!health || unavailable || now - Date.parse(health.checked_at) > 120000 || Date.parse(health.checked_at) > now + 30000) return { label: 'Backup status unavailable', tone: 'unknown' };
  const rows = [health.local, ...health.offsite];
  if (rows.some(row => row.state === 'failed' || row.state === 'overdue')) return { label: 'Backups need attention', tone: 'failed' };
  if (rows.some(row => row.state === 'unknown')) return { label: 'Backup status incomplete', tone: 'unknown' };
  if (rows.some(row => row.state === 'running')) return { label: 'Backup in progress', tone: 'running' };
  if (health.local.state === 'unconfigured' && health.offsite.length === 0) return { label: 'Backups not configured', tone: 'unknown' };
  if (rows.some(row => row.state === 'unconfigured')) return { label: 'Backup setup incomplete', tone: 'unknown' };
  return { label: health.offsite.length ? 'Backups current' : 'Local backups current', tone: 'healthy' };
}
