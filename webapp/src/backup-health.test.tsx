import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { BackupHealthView, dismissBackupHealth } from './backup-health';
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
  it('dismisses outside interaction and Escape without closing on inside clicks', () => {
    class TestNode {}
    vi.stubGlobal('Node', TestNode);
    const document = new EventTarget();
    const focus = vi.fn();
    const summary = new TestNode(), content = new TestNode(), outside = new TestNode();
    const details = Object.assign(new TestNode(), {
      ownerDocument: document,
      open: false,
      contains: (node: TestNode) => [details, summary, content].includes(node),
      querySelector: () => ({ focus }),
    });
    const cleanup = dismissBackupHealth(details as unknown as HTMLDetailsElement);
    const dispatch = (type: string, target: TestNode, key?: string) => {
      const event = new Event(type, { cancelable: true });
      Object.defineProperties(event, { target: { value: target }, key: { value: key } });
      document.dispatchEvent(event);
      return event;
    };
    try {
      details.open = true;
      dispatch('pointerdown', content); dispatch('click', content);
      expect(details.open).toBe(true);
      dispatch('pointerdown', summary); dispatch('click', summary);
      expect(details.open).toBe(true); // Native summary click retains responsibility for toggling.
      dispatch('pointerdown', outside);
      expect(details.open).toBe(false);
      expect(focus).not.toHaveBeenCalled();
      details.open = true;
      dispatch('click', outside); // Keyboard/synthetic clicks also dismiss.
      expect(details.open).toBe(false);
      details.open = true;
      expect(dispatch('keydown', content, 'Enter').defaultPrevented).toBe(false);
      expect(details.open).toBe(true);
      expect(dispatch('keydown', content, 'Escape').defaultPrevented).toBe(true);
      expect(details.open).toBe(false);
      expect(focus).toHaveBeenCalledOnce();
      expect(dispatch('keydown', outside, 'Escape').defaultPrevented).toBe(false);
      cleanup();
      details.open = true;
      dispatch('pointerdown', outside); dispatch('click', outside); dispatch('keydown', outside, 'Escape');
      expect(details.open).toBe(true);
      expect(focus).toHaveBeenCalledOnce();
    } finally { cleanup(); }
  });
  it('gives the popup a semantic heading distinct from copy labels', () => {
    const html = renderToStaticMarkup(<BackupHealthView health={health} now={now} />);
    expect(html).toContain('<h3 class="backup-health-heading">Backup status</h3>');
    expect(html).toContain('<strong>Local</strong>');
  });
});
afterEach(() => vi.unstubAllGlobals());
