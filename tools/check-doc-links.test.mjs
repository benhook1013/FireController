import assert from 'node:assert/strict';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { validateLinks } from './check-doc-links.mjs';

async function withFixture(run) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'firecontroller-doc-links-'));
  try {
    await run(root);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

test('accepts local links, heading fragments, and percent-escaped paths', async () => {
  await withFixture(async (root) => {
    await mkdir(path.join(root, 'guide notes'));
    await writeFile(path.join(root, 'guide notes', 'setup.md'), '# First Steps\n\n## Shared Heading\n');
    await writeFile(path.join(root, 'index.md'), [
      '[setup](guide%20notes/setup.md)',
      '[section](guide%20notes/setup.md#shared-heading)',
      '[self](#local-title)',
      '# Local Title',
      '',
    ].join('\n'));
    assert.deepEqual(await validateLinks(root), []);
  });
});

test('reports missing local targets and heading fragments', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'index.md'), [
      '[missing file](absent.md)',
      '[missing heading](present.md#absent)',
    ].join('\n'));
    await writeFile(path.join(root, 'present.md'), '# Present\n');
    const failures = await validateLinks(root);
    assert.equal(failures.length, 2);
    assert.match(failures[0], /missing local target absent\.md/);
    assert.match(failures[1], /missing heading #absent/);
  });
});

test('ignores links inside fenced examples and external references', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'index.md'), [
      '```md',
      '[missing](missing.md#nope)',
      '~~~',
      '[also ignored](missing-too.md)',
      '~~~',
      '```',
      '[external](https://example.com/missing.md#not-local)',
      '[scheme](mailto:docs@example.com)',
      '',
    ].join('\n'));
    assert.deepEqual(await validateLinks(root), []);
  });
});

test('keeps shorter and info-string fence runs inside longer fenced examples', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'index.md'), [
      '````markdown',
      '[ignored before short run](missing-before.md)',
      '```',
      '[ignored after short run](missing-after.md)',
      '````js',
      '[ignored after info string](missing-info.md)',
      '````',
      '',
    ].join('\n'));
    assert.deepEqual(await validateLinks(root), []);
  });
});

test('preserves underscores in heading anchors', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'guide.md'), '## max_retries\n');
    await writeFile(path.join(root, 'index.md'), [
      '[valid underscore anchor](guide.md#max_retries)',
      '[invalid missing underscore](guide.md#maxretries)',
    ].join('\n'));
    const failures = await validateLinks(root);
    assert.equal(failures.length, 1);
    assert.match(failures[0], /missing heading #maxretries/);
  });
});

test('does not scan private or generated directories', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'index.md'), '# Public docs\n');
    for (const directory of ['.local', 'bin', 'dist']) {
      await mkdir(path.join(root, directory));
      await writeFile(path.join(root, directory, 'private.md'), '[missing](absent.md)\n');
    }
    assert.deepEqual(await validateLinks(root), []);
  });
});

test('rejects local targets that escape the repository root', async () => {
  await withFixture(async (fixtureRoot) => {
    const root = path.join(fixtureRoot, 'repo');
    await mkdir(path.join(root, 'nested'), { recursive: true });
    await writeFile(path.join(fixtureRoot, 'outside.md'), '# Outside file\n');
    await writeFile(path.join(root, 'nested', 'index.md'), [
      '[relative traversal](../../outside.md)',
      '[encoded traversal](%2e%2e/%2e%2e/outside.md)',
    ].join('\n'));
    const failures = await validateLinks(root);
    assert.equal(failures.length, 2);
    assert.match(failures[0], /escapes repository root/);
    assert.match(failures[1], /escapes repository root/);
  });
});
