import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { BackupHealthView } from './backup-health';
import { backupHeadline, validBackupHealth, type BackupHealth } from './backup-health-model';
const now = Date.parse('2026-10-23T10:00:00Z');
const health: BackupHealth = { version:1, checked_at:'2026-10-23T10:00:00Z', local:{state:'healthy',last_success_at:'2026-10-23T09:30:00Z'}, offsite:[{label:'OneDrive',state:'healthy',last_success_at:'2026-10-23T09:00:00Z'}] };
describe('private recorded backup status', () => {
  it('distinguishes working local and offsite snapshots without claiming a fresh verification', () => {
    expect(validBackupHealth(health)).toBe(true);
    const html=renderToStaticMarkup(<BackupHealthView health={health} now={now} />);
    expect(html).toContain('Backups current'); expect(html).toContain('OneDrive'); expect(html).toContain('Snapshot'); expect(html).not.toContain('verified just now');
    expect(backupHeadline({...health,offsite:[]},false,now).label).toBe('Local backups current');
  });
  it('never displays a stale or failed report as healthy', () => {
    expect(backupHeadline(health,true,now).tone).toBe('unknown');
    expect(backupHeadline(health,false,now+121000).tone).toBe('unknown');
    for (const state of ['failed','overdue'] as const) expect(backupHeadline({...health,offsite:[{label:'S3',state}]},false,now).label).toBe('Backups need attention');
    expect(backupHeadline({...health,local:{state:'unconfigured'},offsite:[]},false,now).label).toBe('Backups not configured');
    const html=renderToStaticMarkup(<BackupHealthView health={health} unavailable now={now} />);
    expect(html).not.toContain('OneDrive'); expect(html).toContain('could not be checked');
  });
  it('rejects unexpected private payloads, invalid states and unbounded reports', () => {
    for (const bad of [{...health,path:'/private'}, {...health,local:{state:'healthy',destination:'/private'}}, {...health,offsite:[{label:'x',state:'success'}]}, {...health,offsite:[{label:'x\nsecret',state:'healthy'}]}, {...health,offsite:Array(9).fill(health.offsite[0])}, {...health,checked_at:'bad'}]) expect(validBackupHealth(bad)).toBe(false);
  });
});
