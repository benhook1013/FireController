import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { ProjectConnectionStatus, ProjectUpdateNotice } from './App';

describe('project update recovery control', () => {
  it('offers explicit retry only for a closed project update watch', () => {
    const closed = renderToStaticMarkup(<ProjectUpdateNotice status="closed" reconnect={() => {}} />);
    expect(closed).toContain('role="status"');
    expect(closed).toContain('class="notice"');
    expect(closed).toContain('type="button"');
    expect(closed).toContain('Reconnect project updates');
    expect(closed).toContain('may be stale');
    expect(renderToStaticMarkup(<ProjectUpdateNotice status="live" reconnect={() => {}} />)).toBe('');
  });
  it('uses the hero label for a live project without losing transport attribution', () => {
    const html = renderToStaticMarkup(<ProjectConnectionStatus status="live" connectedLabel="Connected project" />);
    expect(html).toContain('>Connected project</span>');
    expect(html).toContain('aria-label="Project updates: connected"');
  });
  it('inserts no project-data row during attachment or automatic recovery', () => {
    for (const status of ['connecting', 'reconnecting', 'live'] as const) {
      expect(renderToStaticMarkup(<ProjectUpdateNotice status={status} reconnect={() => {}} />)).toBe('');
      const indicator = renderToStaticMarkup(<ProjectConnectionStatus status={status} />);
      expect(indicator).toContain(`connection-${status}`);
      expect(indicator).toContain('role="status"');
      expect(indicator).not.toContain('<div');
      expect(indicator).not.toContain('may be stale');
      expect(indicator).not.toContain('notice');
    }
  });
});
