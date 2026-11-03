import { describe, expect, it } from 'vitest';
import { retainedRead } from './retained-read';

describe('retained scoped reads', () => {
  it('keeps the previous verified snapshot paired with its data until replacement', () => {
    const old = { snapshot_id: 'old', items: ['old-run'] };
    const next = { snapshot_id: 'next', items: ['new-run'] };
    const previous = { scope: 'connection/project/pr/channel', value: old };
    expect(retainedRead(previous.scope, undefined, previous)).toBe(old);
    expect(retainedRead(previous.scope, next, previous)).toBe(next);
  });
  it('does not borrow another connection, project, PR, channel or disabled history', () => {
    const previous = { scope: 'connection/project/pr/channel', value: { private: 'old' } };
    for (const scope of ['other/project/pr/channel', 'connection/other/pr/channel', 'connection/project/other/channel', 'connection/project/pr/other']) {
      expect(retainedRead(scope, undefined, previous)).toBeUndefined();
    }
    expect(retainedRead(previous.scope, previous.value, previous, false)).toBeUndefined();
  });
});
