import { afterEach, describe, expect, it, vi } from 'vitest';
import { Api } from './api';
import type { ProjectMapEntry } from './types';
const project = { id: 'project_one', name: 'Project', github_repository_id: 42, repository: 'synthetic/project' };
const entry: ProjectMapEntry = { id: 'entry_one', project_id: project.id, kind: 'section', title: 'Why this project', description: '## Direction\n\nFlexible **narrative** content.', state: '', where: '', next: '', trigger: '', phases: [], links: [], archived: false, revision: 1, created_at: '2026-10-18T00:00:00Z', updated_at: '2026-10-18T00:00:00Z' };
function json(value: unknown) { return new Response(JSON.stringify(value), { headers: { 'content-type': 'application/json' } }); }
async function client() {
  vi.stubGlobal('location', { origin: 'https://service.example' });
  const api = new Api('synthetic-private-token', project.id);
  vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(json({ instance_id: 'instance_one' })).mockResolvedValueOnce(json(project)));
  await api.connect(); return api;
}
afterEach(() => vi.unstubAllGlobals());
describe('ordered bounded Project editorial API', () => {
  it('reads full Markdown content once, preserving explicit order and scoped continuation', async () => {
    const api = await client();
    const page = { items: [{ ...entry, id: 'entry_z', position: 1 }, { ...entry, id: 'entry_a', position: 2 }], cursor: 'next_editorial', has_more: true };
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      expect(String(input)).toBe('https://service.example/api/v1/projects/project_one/map/editorial?limit=2&cursor=selected_cursor'); expect(init?.method).toBe('GET'); return json(page);
    });vi.stubGlobal('fetch', fetch);
    expect(await api.projectMapEditorial({ limit: 2, cursor: 'selected_cursor' })).toEqual(page); expect(fetch).toHaveBeenCalledTimes(1);api.close();
  });
  it('rejects foreign, archived, unordered, expanded and invalid generic sections', async () => {
    const api = await client();
    const invalid = [
      [{ ...entry, project_id: 'foreign_project' }], [{ ...entry, archived: true }], [entry, entry],
      [{ ...entry, position: 2 }, { ...entry, id: 'entry_two', position: 1 }],
      [{ ...entry, position: -1 }], [{ ...entry, position: null }], [{ ...entry, position: 1000001 }],
      [{ ...entry, trigger: 'A section is not a return point' }], [{ ...entry, phases: [{ id: 'phase', name: 'Unexpected', state: '', archived: false }] }],
      [{ ...entry, private_extra: 'not selected' }],
    ];
    for (const items of invalid) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ items, has_more: false })));
      await expect(api.projectMapEditorial()).rejects.toMatchObject({ code: 'invalid_response' });
    } api.close();
  });
  it('supports existing snapshots without position, and validates bounds before network access', async () => {
    const api = await client(); const fetch = vi.fn();vi.stubGlobal('fetch', fetch);
    for (const limit of [0, 11, 1.2]) await expect(api.projectMapEditorial({ limit })).rejects.toMatchObject({ code: 'invalid_request' });
    expect(fetch).not.toHaveBeenCalled();
    fetch.mockResolvedValue(json({ items: [entry], has_more: false }));expect((await api.projectMapEditorial()).items[0].position).toBeUndefined();api.close();
  });
});
