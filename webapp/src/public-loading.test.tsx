import { Children, isValidElement, type ReactNode } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import { PublicProjectAddress, PublicProjectBinding, PublicProjectLoadState } from './public-site';

const generation = `public_generation_${'a'.repeat(32)}`;
const render = (view: ReactNode) => renderToStaticMarkup(<MemoryRouter>{view}</MemoryRouter>);

describe('public project loading states', () => {
  it('uses one neutral skeleton through both address resolution and snapshot reads', () => {
    const address = render(<PublicProjectAddress address="friendly-project" embedded fallback={<p>UNPUBLISHED_LOGIN</p>} />);
    const snapshot = render(<PublicProjectBinding slug="published-project" embedded />);
    const history = render(<PublicProjectBinding slug="published-project" revision={2} generation={generation} embedded />);
    expect(address).toBe(snapshot);
    expect(history).toBe(snapshot);
    expect(address).toContain('public-project-skeleton');
    expect(address).not.toMatch(/UNPUBLISHED_LOGIN|friendly-project|published-project|<h1|Try again|<img|<button/gu);
  });

  it('also keeps legacy standalone pending routes free of error actions', () => {
    const html = render(<PublicProjectBinding slug="legacy-published" />);
    expect(html).toContain('<header');
    expect(html).toContain('<main class="public-layout"');
    expect(html).not.toMatch(/<h1|Try again|Sign in|<img|role="alert"/gu);
  });

  it('shows retry and an optional sign-in destination only after an actual failure', () => {
    const retry = vi.fn(), notice = 'The published view could not be loaded. Try again.';
    const pending = render(<PublicProjectLoadState retry={retry} signIn="/projects/selected?signin=1" />);
    expect(pending).not.toContain('Try again');
    expect(pending).not.toContain('Sign in');
    const view = PublicProjectLoadState({ notice, retry, signIn: '/projects/selected?signin=1' });
    const html = render(view);
    expect(html).toContain(`<p role="alert">${notice}</p>`);
    expect(html).toContain('>Try again</button>');
    expect(html).toContain('href="/projects/selected?signin=1"');
    expect(html).not.toMatch(/public-project-skeleton|<h1|<img/gu);
    Children.forEach(view.props.children, child => {
      if (isValidElement<{ onClick?: () => void }>(child) && child.type === 'button') child.props.onClick?.();
    });
    expect(retry).toHaveBeenCalledOnce();
  });

  it('preserves explicit unpublished and invalid-publication fallbacks', () => {
    const fallback = <p>Sign in to the selected project</p>;
    expect(render(<PublicProjectAddress address="invalid/address" embedded fallback={fallback} />)).toContain('Sign in to the selected project');
    expect(render(<PublicProjectBinding slug="INVALID" fallback={fallback} />)).toContain('Sign in to the selected project');
    const unavailable = render(<PublicProjectBinding slug="INVALID" />);
    expect(unavailable).toContain('Project unavailable');
    expect(unavailable).not.toContain('public-project-skeleton');
  });
});
