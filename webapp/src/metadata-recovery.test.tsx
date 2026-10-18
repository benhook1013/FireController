import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { ProjectUpdateNotice } from './App';

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
  it('uses a neutral loading indicator during attachment and automatic recovery', () => {
    for (const status of ['connecting', 'reconnecting'] as const) {
      const html = renderToStaticMarkup(<ProjectUpdateNotice status={status} reconnect={() => {}} />);
      expect(html).toContain('class="loading-spinner"');
      expect(html).toContain(`role="status" aria-label="${status === 'connecting' ? 'Connecting' : 'Reconnecting'} project updates"`);
      expect(html).not.toContain('class="notice"');
      expect(html).not.toContain('disconnected');
      expect(html).not.toContain('may be stale');
      expect(html).not.toContain('<button');
    }
  });
});
