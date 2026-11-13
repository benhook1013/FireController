import { afterEach, expect, it, vi } from 'vitest';
import { Api } from './api';
afterEach(() => vi.unstubAllGlobals());
it('uses one bounded scoped GET for backup status and rejects unexpected private fields', async () => {
  vi.stubGlobal('location', {origin:'https://service.example'});
  const project = {id:'project_one',name:'One',github_repository_id:1,repository:'owner/repo'};
  let value: unknown = {version:1,checked_at:'2026-10-23T10:00:00Z',local:{state:'healthy'},offsite:[]};
  const calls: Array<{url:URL;init?:RequestInit}> = [];
  vi.stubGlobal('fetch',vi.fn(async (input:RequestInfo|URL,init?:RequestInit) => {
    const url = new URL(String(input)); calls.push({url,init});
    return new Response(JSON.stringify(url.pathname.endsWith('/instance') ? {instance_id:'instance_one'} : url.pathname.endsWith('/project_one') ? project : value),{headers:{'content-type':'application/json'}});
  }));
  const api = new Api('fixture-token',project.id);
  try {
    await api.connect(); const before = calls.length;
    expect((await api.backupHealth()).local.state).toBe('healthy');
    expect(calls.length-before).toBe(1); const call=calls.at(-1)!;
    expect(call.url.pathname).toBe('/api/v1/projects/project_one/backup-health'); expect(call.init?.method).toBe('GET'); expect(call.url.search).toBe('');
    expect(new Headers(call.init?.headers).get('x-fc-instance')).toBe('instance_one');
    value={version:1,checked_at:'2026-10-23T10:00:00Z',local:{state:'healthy',destination:'/private/storage'},offsite:[]};
    await expect(api.backupHealth()).rejects.toMatchObject({code:'invalid_response'});
  } finally { api.close(); }
});
