import { describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { QueryClient, QueryClientProvider, QueryObserver } from '@tanstack/react-query';
import { WorkspaceContext, type Session } from './workspace';
import { BrowserActionMemory } from './browser-actions';
import { WorkerList } from './App';
import { RetirementBlockers, WorkerRetirementPanel, refreshRetirementViews } from './worker-retirement';
import type { WorkerRetirement } from './types';

const key = ['fc', 'session', 'instance_one', 'project_one', 'worker_actor'];
const home = '/projects/project_one';
const worker = { id: 'worker_one', project_id: 'project_one', name: 'Builder <literal>' };
const clear: WorkerRetirement = { worker_id: worker.id, retired: false, can_retire: true, blockers: { jobs: { count: 0, ids: [] }, notes: { count: 0, ids: [] }, messages: { count: 0, ids: [] } } };
function fixture(writes = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const api = { browserCapabilities: { allow_writes: writes, can_edit_presentation: false }, workerRetirement: vi.fn(), retireWorker: vi.fn(), acknowledgeWorkerMessage: vi.fn() };
  const session = { api, actions: new BrowserActionMemory(), controller: new AbortController(), worker: { id: 'worker_actor', project_id: worker.project_id, name: 'Caller' } } as unknown as Session;
  const render = (node: React.ReactNode) => renderToStaticMarkup(<MemoryRouter><QueryClientProvider client={client}><WorkspaceContext.Provider value={{ session, key, home, ready: true, selectWorker: vi.fn(), fail: vi.fn() }}>{node}</WorkspaceContext.Provider></QueryClientProvider></MemoryRouter>);
  return { client, api, render };
}
describe('worker retirement views', () => {
  it('shows every blocker count and exact actionable links without clearing anything', () => {
    const f = fixture();
    const blocked = { ...clear, can_retire: false, blockers: { jobs: { count: 1, ids: ['job_one'] }, notes: { count: 2, ids: ['note_one', 'note_two'] }, messages: { count: 1, ids: ['message_one'] } } };
    f.client.setQueryData([...key, 'worker-retirement', worker.id], blocked);
    try {
      const html = f.render(<WorkerRetirementPanel worker={worker} />);
      expect(html).toContain('1 unfinished jobs'); expect(html).toContain('2 pending notes'); expect(html).toContain('1 unacknowledged messages');
      expect(html).toContain(`href="${home}/jobs/job_one"`); expect(html).toContain(`href="${home}/notes/note_two"`); expect(html).toContain(`href="${home}/inbox?worker_id=worker_one"`);
      expect(html).toMatch(/<button[^>]*disabled=""[^>]*>Retire Builder &lt;literal&gt;<\/button>/u);
      expect(html).toContain('Project managers may explicitly acknowledge'); expect(html).toContain('Acknowledge message_one for this worker');
      expect(f.api.retireWorker).not.toHaveBeenCalled(); expect(f.api.acknowledgeWorkerMessage).not.toHaveBeenCalled();
    } finally { f.client.clear(); }
  });
  it('offers retirement only for current clear workers with actual write permission', () => {
    for (const writes of [true, false]) {
      const f = fixture(writes); f.client.setQueryData([...key, 'worker-retirement', worker.id], clear);
      try { const html = f.render(<WorkerRetirementPanel worker={worker} />); expect(html.includes('>Retire Builder &lt;literal&gt;</button>')).toBe(writes); expect(html).toContain('No unfinished jobs'); expect(f.api.retireWorker).not.toHaveBeenCalled(); }
      finally { f.client.clear(); }
    }
    const f = fixture(); f.client.setQueryData([...key, 'worker-retirement', worker.id], { ...clear, retired: true, can_retire: false });
    try { const html = f.render(<WorkerRetirementPanel worker={{ ...worker, retired: true }} />); expect(html).toContain('Retired. Existing history is retained.'); expect(html).not.toContain('>Retire Builder'); }
    finally { f.client.clear(); }
  });
  it('keeps retired workers readable in the archive without offering active worker selection', () => {
    const f = fixture();
    f.client.setQueryData([...key, 'workers', ''], { items: [worker], has_more: false });
    f.client.setQueryData([...key, 'workers', 'archive', ''], { items: [worker, { ...worker, id: 'worker_retired', name: 'Old worker', retired: true }], has_more: false });
    try {
      const active = f.render(<WorkerList compact />), archive = f.render(<WorkerList includeRetired />);
      expect(active).not.toContain('Old worker'); expect(active).toContain('Select worker Builder');
      expect(archive).toContain('Old worker · Retired'); expect(archive).toContain(`href="${home}/workers/worker_retired"`); expect(archive).not.toContain('Open Old worker inbox'); expect(archive).not.toContain('Review retirement for Old worker');
    } finally { f.client.clear(); }
  });
  it('refreshes active workers and lanes after success while keeping historical and foreign data', async () => {
    const client = new QueryClient(); const subscriptions: (() => void)[] = []; const reads = new Map<string, number>();
    const add = (prefix: string[], active = true, connection = key) => {
      const queryKey = [...connection, ...prefix], label = JSON.stringify(queryKey);
      client.setQueryData(queryKey, 'original'); reads.set(label, 0);
      if (active) { const observer = new QueryObserver(client, { queryKey, staleTime: Infinity, queryFn: async () => { reads.set(label, reads.get(label)! + 1); return 'fresh'; } }); subscriptions.push(observer.subscribe(() => {})); }
      return { reads: () => reads.get(label), state: () => client.getQueryState(queryKey) };
    };
    const workers = add(['workers', '']), archive = add(['workers', 'archive', '']), lanes = add(['delivery']), selected = add(['worker', worker.id]), preview = add(['worker-retirement', worker.id]);
    const history = add(['worker-primary-history', worker.id]), note = add(['note', 'note_one']), foreign = add(['workers', ''], true, ['fc', 'other_session']);
    try {
      await refreshRetirementViews(client, key, worker.id);
      for (const view of [workers, archive, lanes, selected, preview]) expect(view.reads()).toBe(1);
      for (const view of [history, note, foreign]) { expect(view.reads()).toBe(0); expect(view.state()?.data).toBe('original'); }
      expect(client.getMutationCache().getAll()).toHaveLength(0);
    } finally { subscriptions.forEach(close => close()); client.clear(); }
  });
  it('keeps large blocker counts explicit instead of pretending the first page is complete', () => {
    const f = fixture();
    try { const html = f.render(<RetirementBlockers value={{ ...clear, can_retire: false, blockers: { ...clear.blockers, jobs: { count: 25, ids: Array.from({ length: 20 }, (_, i) => `job_${i}`) } } }} home={home} />); expect(html).toContain('25 unfinished jobs'); expect(html).toContain('Showing 20 of 25.'); }
    finally { f.client.clear(); }
  });
});
