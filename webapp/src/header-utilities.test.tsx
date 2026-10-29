import { describe, expect, it } from 'vitest';
import { MemoryRouter } from 'react-router';
import { renderToStaticMarkup } from 'react-dom/server';
import { ProjectHeaderUtilities, ProjectIdentityHero } from './App';

describe('project header utilities', () => {
  it('exposes one attributed status with compact explicit refresh and sign-out controls', () => {
    const html = renderToStaticMarkup(<ProjectHeaderUtilities status="live" refreshing={false} refresh={() => {}} disconnect={() => {}} />);
    expect(html).toContain('role="group" aria-label="Project utilities"');
    expect(html.match(/role="status"/gu)).toHaveLength(1);
    expect(html).toContain('aria-label="Project updates: connected"');
    expect(html).toContain('>Connected</span>');
    expect(html).toContain('aria-label="Refresh visible data"');
    expect(html).toContain('aria-busy="false"');
    expect(html).toContain('>Refresh</button>');
    expect(html).toContain('>Sign out</button>');
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
      expect(html).toContain('>Sign out</button>');
      expect(html).not.toContain('may be stale');
    }
  });
  it('leaves malformed repository labels as literal text without links', () => {
    for (const repository of ['owner/../other', 'owner/..', '../repository', 'owner/repository/extra', 'owner/repo?token=1', 'https://evil.example/repo', 'owner/repo\n', 'owner\\repo', 'owner/%2e%2e']) {
      const html = renderToStaticMarkup(<ProjectIdentityHero name="Project" repository={repository} />);
      expect(html).not.toContain('href=');
    }
  });
  it('contracts the hero to literal project identity without internal identifiers or duplicate utilities', () => {
    const html = renderToStaticMarkup(<ProjectIdentityHero name="Project <literal>" repository="owner/repository" />);
    expect(html).toContain('<h1>Project &lt;literal&gt;</h1>');
    expect(html).toContain('href="https://github.com/owner/repository" target="_blank" rel="noreferrer noopener">owner/repository</a>');
    expect(html).not.toMatch(/class="identity"|Project utilities|role="status"|<button|Work, with context|Browse jobs and read worker conversations/u);
  });
  it('uses explicit project introduction copy and a project-scoped overview link', () => {
    const html = renderToStaticMarkup(<MemoryRouter><ProjectIdentityHero name="Project" repository="owner/repo" category="Category <literal>" introduction="Project description <literal>" overviewHref="/projects/example/project" /></MemoryRouter>);
    expect(html).toContain('Category &lt;literal&gt;');
    expect(html).toContain('Project description &lt;literal&gt;');
    expect(html).toContain('href="/projects/example/project"');
    expect(html).toContain('Project overview ↗');
    const empty = renderToStaticMarkup(<ProjectIdentityHero name="Project" repository="owner/repo" />);
    expect(empty).not.toContain('project-introduction-category');
    expect(empty).not.toContain('project-introduction-text');
  });

});
