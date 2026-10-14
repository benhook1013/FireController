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

test('checks both destinations in links wrapping badge images', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'badge.svg'), '<svg></svg>\n');
    await writeFile(path.join(root, 'index.md'), '[![License](badge.svg)](LICENSE)\n');

    const failures = await validateLinks(root);
    assert.equal(failures.length, 1);
    assert.match(failures[0], /missing local target LICENSE/);
  });
});

test('checks nested image links with escaped paths while ignoring code-span examples', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'badge image.svg'), '<svg></svg>\n');
    await writeFile(path.join(root, 'outer target.md'), '# Target\n');
    await writeFile(path.join(root, 'index.md'), [
      '[![Badge `![ignored](fake.svg)`](badge\\ image.svg)](outer\\ target.md)',
      '[![Missing image](missing\\ badge.svg)](missing\\ outer.md)',
      '',
    ].join('\n'));

    const failures = await validateLinks(root);
    assert.equal(failures.length, 2);
    assert.match(failures[0], /missing local target missing badge\.svg/);
    assert.match(failures[1], /missing local target missing outer\.md/);
    assert.ok(!failures.some((failure) => failure.includes('fake.svg')));
  });
});

test('reports missing reference-definition targets at the definition line', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'index.md'), [
      '[guide][setup]',
      '',
      '[setup]: missing.md',
      '',
    ].join('\n'));

    const failures = await validateLinks(root);
    assert.equal(failures.length, 1);
    assert.match(failures[0], /^index\.md:3: missing local target missing\.md$/);
  });
});

test('accepts local reference definitions with fragments and escaped paths', async () => {
  await withFixture(async (root) => {
    await mkdir(path.join(root, 'guide notes'));
    await writeFile(path.join(root, 'guide notes', 'setup.md'), '## Shared Heading\n');
    await writeFile(path.join(root, 'guide notes', 'escaped page.md'), '# Escaped\n');
    await writeFile(path.join(root, 'index.md'), [
      '[guide][setup]',
      '[setup]: <guide notes/setup.md#shared-heading> "the setup guide"',
      '[escaped][escaped]',
      "[escaped]: guide\\ notes/escaped\\ page.md 'escaped title'",
      '[external][site]',
      '[site]: https://example.test/guide "external guide"',
      '`[inline]: missing-inline.md`',
      '```md',
      '[fenced]: missing-fenced.md',
      '```',
      '',
    ].join('\n'));

    assert.deepEqual(await validateLinks(root), []);

    await writeFile(path.join(root, 'index.md'), [
      '[guide][setup]',
      '[setup]: <guide notes/setup.md#missing-heading> "the setup guide"',
      '',
    ].join('\n'));
    const badHeading = await validateLinks(root);
    assert.equal(badHeading.length, 1);
    assert.match(badHeading[0], /^index\.md:2: missing heading #missing-heading in guide notes\/setup\.md$/);

    await writeFile(path.join(root, 'index.md'), [
      '[escaped][escaped]',
      "[escaped]: missing\\ escaped.md 'escaped title'",
      '',
    ].join('\n'));
    const missingEscapedPath = await validateLinks(root);
    assert.equal(missingEscapedPath.length, 1);
    assert.match(missingEscapedPath[0], /^index\.md:2: missing local target missing escaped\.md$/);
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

test('ignores links inside matched inline code spans including multiline spans', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'present.md'), '# Present\n');
    await writeFile(path.join(root, 'index.md'), [
      '# Links',
      'Inline `[missing](missing-inline.md)` should be ignored.',
      'Long delimiter ```contains ``two ticks`` and `one` plus [missing](missing-long.md)``` should be ignored.',
      'Multiline ``starts here',
      '[missing](missing-multiline.md)',
      'ends here`` and [valid](present.md).',
      '',
    ].join('\n'));
    assert.deepEqual(await validateLinks(root), []);
  });
});

test('reports invalid links outside and after inline code spans with original line numbers', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'index.md'), [
      '# Links',
      'Inline `[hidden](inside.md)` then [broken](absent.md).',
      'Long code ```contains ``two ticks`` and `ticks` plus [hidden](inside-long.md)``` then [broken](after-long.md).',
      'Escaped opener \\` leaves [broken](after-escaped.md) visible.',
      '`unmatched opener leaves [broken](after-unmatched.md) visible.',
      'Multiline ``starts here',
      '[hidden](inside-multiline.md)',
      'ends here`` then [broken](after-multiline.md).',
      '',
    ].join('\n'));

    const failures = await validateLinks(root);
    assert.equal(failures.length, 5);
    assert.match(failures[0], /^index\.md:2: missing local target absent\.md$/);
    assert.match(failures[1], /^index\.md:3: missing local target after-long\.md$/);
    assert.match(failures[2], /^index\.md:4: missing local target after-escaped\.md$/);
    assert.match(failures[3], /^index\.md:5: missing local target after-unmatched\.md$/);
    assert.match(failures[4], /^index\.md:8: missing local target after-multiline\.md$/);
  });
});

test('ignores odd escaped link openers but validates even runs and escaped image markers', async () => {
  await withFixture(async (root) => {
    await writeFile(path.join(root, 'index.md'), [
      '# Links',
      '\\[odd](missing-odd.md)',
      '\\\\[even](missing-even.md)',
      '\\![escaped image marker](missing-image-link.md)',
      '',
    ].join('\n'));

    const failures = await validateLinks(root);
    assert.equal(failures.length, 2);
    assert.match(failures[0], /^index\.md:3: missing local target missing-even\.md$/);
    assert.match(failures[1], /^index\.md:4: missing local target missing-image-link\.md$/);
  });
});

test('normalizes unencoded backslash separators before percent decoding', async () => {
  await withFixture(async (root) => {
    await mkdir(path.join(root, 'nested'));
    await writeFile(path.join(root, 'nested', 'page.md'), '# Nested\n');
    await mkdir(path.join(root, 'rawconcat'));
    await writeFile(path.join(root, 'rawconcat', 'page.md'), '# Concatenated decoy\n');

    const encodedBackslashFile = path.sep === '\\'
      ? path.join(root, 'encoded', 'backslash.md')
      : path.join(root, 'encoded\\backslash.md');
    await mkdir(path.dirname(encodedBackslashFile), { recursive: true });
    await writeFile(encodedBackslashFile, '# Encoded backslash\n');

    const links = [
      '[valid separator](nested\\page.md)',
      '[encoded backslash](encoded%5Cbackslash.md)',
      '[wrong concatenated target](raw\\concat/page.md)',
    ];
    if (path.sep === '/') links.push('[raw separator differs from encoded backslash](encoded\\backslash.md)');
    await writeFile(path.join(root, 'index.md'), links.join('\n'));

    const failures = await validateLinks(root);
    assert.equal(failures.length, path.sep === '/' ? 2 : 1);
    assert.match(failures[0], /^index\.md:3: missing local target raw\\concat\/page\.md$/);
    if (path.sep === '/') {
      assert.match(failures[1], /^index\.md:4: missing local target encoded\\backslash\.md$/);
    }
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
