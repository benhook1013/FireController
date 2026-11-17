import { afterEach, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { BrowserActionMemory, MessageComposer } from './browser-actions';

const fixture = vi.hoisted(() => ({ states: [] as unknown[], session: {} as Record<string, unknown> }));
vi.mock('react', async original => ({ ...await original<typeof import('react')>(), useState: (initial: unknown) => [fixture.states.length ? fixture.states.shift() : typeof initial === 'function' ? initial() : initial, vi.fn()] }));
vi.mock('@tanstack/react-query', () => ({ useQueryClient: () => ({ invalidateQueries: vi.fn() }) }));
vi.mock('./workspace', () => ({ useWorkspace: () => ({ session: fixture.session, key: ['fixture'], home: '/projects/fixture', fail: vi.fn() }), signalFor: (_session: unknown, signal: AbortSignal) => signal }));
afterEach(() => { fixture.states = []; });
it('retains the update guard when an unsent message composer is collapsed without storing its content', () => {
  fixture.session = { api: { browserCapabilities: { allow_writes: true } }, worker: { id: 'worker' }, actions: new BrowserActionMemory(), controller: new AbortController() };
  fixture.states = [false, 'recipient', 'Private unsent draft', ''];
  const html = renderToStaticMarkup(<MessageComposer />);
  expect(html).toContain('data-browser-dirty="true"');
  expect(html).not.toContain('<form');
  expect(html).not.toContain('Private unsent draft');
});
it('leaves a pristine collapsed composer safe for automatic updates', () => {
  fixture.session = { api: { browserCapabilities: { allow_writes: true } }, worker: { id: 'worker' }, actions: new BrowserActionMemory(), controller: new AbortController() };
  expect(renderToStaticMarkup(<MessageComposer />)).toContain('data-browser-dirty="false"');
});

it('does not treat a recipient retained after a confirmed send as another unsent message', () => {
  fixture.session = { api: { browserCapabilities: { allow_writes: true } }, worker: { id: 'worker' }, actions: new BrowserActionMemory(), controller: new AbortController() };
  fixture.states = [false, 'recipient', '', 'conversation', 'recipient'];
  expect(renderToStaticMarkup(<MessageComposer />)).toContain('data-browser-dirty="false"');
});
