import { sessionExpiry } from './browser-session';

export interface WorkerContact { worker_id: string; attached: boolean; last_seen_at: string | null }
export interface WorkerPresence { project_id: string; evaluated_at: string; observed_since: string; complete: boolean; items: WorkerContact[] }
export function presenceWorkers(value: unknown): value is string[] {
  return Array.isArray(value) && value.length > 0 && value.length <= 100 && value.every(id => typeof id === 'string' && /^worker_[a-f0-9]{32}$/u.test(id)) && new Set(value).size === value.length;
}
// Contact is optional context; larger appended boards never trigger a fleet walk.
export function presenceSelection(workers: string[]): string[] { return [...workers].sort().slice(0,100); }
function object(value: unknown): value is Record<string, unknown> { return !!value && typeof value === 'object' && !Array.isArray(value); }
function fields(value: Record<string, unknown>, names: string[]) { return Object.keys(value).length === names.length && names.every(name => Object.hasOwn(value,name)); }
export function validWorkerPresence(value: unknown, project: string, workers: string[]): value is WorkerPresence {
  if (!object(value) || !fields(value,['project_id','evaluated_at','observed_since','complete','items']) || value.project_id !== project || !sessionExpiry(value.evaluated_at) || !sessionExpiry(value.observed_since) || typeof value.complete !== 'boolean' || !Array.isArray(value.items) || value.items.length !== workers.length) return false;
  return value.items.every((item,index) => object(item) && fields(item,['worker_id','attached','last_seen_at']) && item.worker_id === workers[index] && typeof item.attached === 'boolean' && (item.last_seen_at === null ? !item.attached : !!sessionExpiry(item.last_seen_at)));
}
