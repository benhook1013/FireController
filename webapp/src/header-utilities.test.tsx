import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { ProjectHeaderUtilities, ProjectIdentityHero } from './App';

describe('project header utilities', () => {
  it('exposes one attributed status with compact explicit refresh and disconnect controls', () => {
    const html = renderToStaticMarkup(<ProjectHeaderUtilities status="live" refreshing={false} refresh={() => {}} disconnect={() => {}} />);
    expect(html).toContain('role="group" aria-label="Project utilities"');
    expect(html.match(/role="status"/gu)).toHaveLength(1);
    expect(html).toContain('aria-label="Project updates: connected"');
    expect(html).toContain('>Connected</span>');
    expect(html).toContain('aria-label="Refresh visible data"');
    expect(html).toContain('aria-busy="false"');
    expect(html).toContain('>Refresh</button>');
    expect(html).toContain('>Disconnect</button>');
    expect(html).not.toContain('disabled');
    expect(html).not.toMatch(/connection-card|hero-actions|Project updates disconnected/u);
  });
  it('preserves attachment attribution and disables repeated refresh while it is active', () => {
    for (const status of ['connecting', 'reconnecting', 'closed'] as const) {
      const html = renderToStaticMarkup(<ProjectHeaderUtilities status={status} refreshing refresh={() => {}} disconnect={() => {}} />);
      expect(html).toContain(`connection-${status}`);
      expect(html.match(/role="status"/gu)).toHaveLength(1);
      expect(html).toContain('aria-busy="true" disabled=""');
      expect(html).toContain('>Refreshing…</button>');
      expect(html).toContain('>Disconnect</button>');
      expect(html).not.toContain('may be stale');
    }
  });
  it('contracts the hero to literal project identity without internal identifiers or duplicate utilities', () => {
    const html = renderToStaticMarkup(<ProjectIdentityHero name="Project <literal>" repository="owner/repository" />);
    expect(html).toContain('<h1>Project &lt;literal&gt;</h1>');
    expect(html).toContain('>owner/repository</p>');
    expect(html).not.toMatch(/class="identity"|Project utilities|role="status"|<button|Work, with context|Browse jobs and read worker conversations/u);
  });
});
