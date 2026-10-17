import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { ProjectUpdateNotice } from './App';

describe('project update recovery control', () => {
  it('offers explicit retry only for a closed project update watch', () => {
    const closed = renderToStaticMarkup(<ProjectUpdateNotice status="closed" reconnect={() => {}} />);
    expect(closed).toContain('role="status"');
    expect(closed).toContain('type="button"');
    expect(closed).toContain('Reconnect project updates');
    expect(closed).toContain('may be stale');
    for (const status of ['connecting', 'reconnecting', 'live'] as const) {
      expect(renderToStaticMarkup(<ProjectUpdateNotice status={status} reconnect={() => {}} />)).not.toContain('button');
    }
    expect(renderToStaticMarkup(<ProjectUpdateNotice status="live" reconnect={() => {}} />)).toBe('');
  });
});
