import { realpath, readdir, readFile, stat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

function stripCodeFences(markdown) {
  let fence = null;
  return markdown
    .split('\n')
    .map((line) => {
      const match = line.match(/^ {0,3}(`{3,}|~{3,})(.*)$/);
      if (match && fence === null) {
        const marker = match[1];
        const info = match[2];
        if (marker[0] !== '`' || !info.includes('`')) {
          fence = { character: marker[0], length: marker.length };
        }
        return '';
      }
      if (match && fence !== null) {
        const [marker, info] = [match[1], match[2]];
        if (marker[0] === fence.character && marker.length >= fence.length && /^\s*$/.test(info)) {
          fence = null;
        }
        return '';
      }
      return fence === null ? line : '';
    })
    .join('\n');
}

function stripInlineCodeSpans(markdown) {
  const characters = Array.from(markdown);
  let index = 0;

  while (index < characters.length) {
    if (characters[index] !== '`') {
      index += 1;
      continue;
    }

    let precedingBackslashes = 0;
    for (let cursor = index - 1; cursor >= 0 && characters[cursor] === '\\'; cursor -= 1) {
      precedingBackslashes += 1;
    }
    if (precedingBackslashes % 2 === 1) {
      index += 1;
      continue;
    }

    let openingEnd = index + 1;
    while (characters[openingEnd] === '`') openingEnd += 1;
    const delimiterLength = openingEnd - index;
    let closingStart = openingEnd;

    while (closingStart < characters.length) {
      if (characters[closingStart] !== '`') {
        closingStart += 1;
        continue;
      }

      let closingEnd = closingStart + 1;
      while (characters[closingEnd] === '`') closingEnd += 1;
      if (closingEnd - closingStart === delimiterLength) break;
      closingStart = closingEnd;
    }

    if (closingStart >= characters.length) {
      // An unmatched delimiter is literal Markdown, so links after it remain visible.
      index = openingEnd;
      continue;
    }

    let closingEnd = closingStart + delimiterLength;
    for (let cursor = index; cursor < closingEnd; cursor += 1) {
      if (characters[cursor] !== '\n' && characters[cursor] !== '\r') characters[cursor] = ' ';
    }
    index = closingEnd;
  }

  return characters.join('');
}

function markdownDestinations(markdown) {
  const source = stripInlineCodeSpans(stripCodeFences(markdown));
  const destinations = [];
  const pattern = /!?(?:\[[^\]]*\])\(\s*(<[^>]*>|(?:\\.|[^\s()])+)(?:\s+[^)]*)?\)/g;
  for (const match of source.matchAll(pattern)) {
    const openingBracket = match.index + match[0].indexOf('[');
    let precedingBackslashes = 0;
    for (let cursor = openingBracket - 1; cursor >= 0 && source[cursor] === '\\'; cursor -= 1) {
      precedingBackslashes += 1;
    }
    if (precedingBackslashes % 2 === 1) continue;

    let destination = match[1];
    if (destination.startsWith('<') && destination.endsWith('>')) {
      destination = destination.slice(1, -1);
    }
    destination = destination.replace(/\\([\\()[\]<> ])/g, '$1');
    destinations.push(destination);
  }
  return destinations;
}

function slugHeading(heading) {
  return heading
    .trim()
    .toLowerCase()
    .replace(/[`*~]/g, '')
    .replace(/[^\p{L}\p{N}\s_-]/gu, '')
    .replace(/\s+/g, '-');
}

function headingAnchors(markdown) {
  const anchors = new Set();
  const counts = new Map();
  for (const line of stripCodeFences(markdown).split('\n')) {
    const match = line.match(/^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$/);
    if (!match) continue;
    const base = slugHeading(match[1]);
    const count = counts.get(base) ?? 0;
    counts.set(base, count + 1);
    anchors.add(count === 0 ? base : `${base}-${count}`);
  }
  return anchors;
}

async function markdownFiles(directory) {
  const files = [];
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    if (['.git', '.local', 'bin', 'dist', 'node_modules'].includes(entry.name)) continue;
    const fullPath = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...await markdownFiles(fullPath));
    else if (entry.isFile() && entry.name.toLowerCase().endsWith('.md')) files.push(fullPath);
  }
  return files;
}

async function validateLinks(root) {
  const failures = [];
  const absoluteRoot = await realpath(path.resolve(root));
  for (const sourcePath of await markdownFiles(absoluteRoot)) {
    const source = await readFile(sourcePath, 'utf8');
    const lines = stripInlineCodeSpans(stripCodeFences(source)).split('\n');
    let lineNumber = 0;
    for (const line of lines) {
      lineNumber += 1;
      for (const destination of markdownDestinations(line)) {
        if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/i.test(destination)) continue;
        const hashAt = destination.indexOf('#');
        const queryAt = destination.indexOf('?');
        const cutAt = [hashAt, queryAt].filter((index) => index >= 0).reduce((a, b) => Math.min(a, b), destination.length);
        const rawTarget = destination.slice(0, cutAt);
        const fragment = hashAt < 0 ? '' : destination.slice(hashAt + 1).split('?')[0];
        let target;
        try {
          target = decodeURIComponent(rawTarget.replace(/\\/g, '/'));
        } catch {
          failures.push(`${path.relative(root, sourcePath)}:${lineNumber}: malformed escaped path in ${destination}`);
          continue;
        }
        const resolved = path.resolve(path.dirname(sourcePath), target || path.basename(sourcePath));
        const relativeTarget = path.relative(absoluteRoot, resolved);
        if (relativeTarget === '..' || relativeTarget.startsWith(`..${path.sep}`) || path.isAbsolute(relativeTarget)) {
          failures.push(`${path.relative(absoluteRoot, sourcePath)}:${lineNumber}: local target escapes repository root: ${destination}`);
          continue;
        }
        let targetStat;
        try {
          targetStat = await stat(resolved);
        } catch {
          failures.push(`${path.relative(root, sourcePath)}:${lineNumber}: missing local target ${destination}`);
          continue;
        }
        try {
          const canonicalTarget = await realpath(resolved);
          const canonicalRelative = path.relative(absoluteRoot, canonicalTarget);
          if (canonicalRelative === '..' || canonicalRelative.startsWith(`..${path.sep}`) || path.isAbsolute(canonicalRelative)) {
            failures.push(`${path.relative(absoluteRoot, sourcePath)}:${lineNumber}: local target escapes repository root: ${destination}`);
            continue;
          }
        } catch {
          // The stat above already confirmed the target; report a canonicalization failure as a missing local target.
          failures.push(`${path.relative(absoluteRoot, sourcePath)}:${lineNumber}: cannot resolve local target ${destination}`);
          continue;
        }
        if (fragment && targetStat.isFile() && resolved.toLowerCase().endsWith('.md')) {
          const targetMarkdown = await readFile(resolved, 'utf8');
          const anchors = headingAnchors(targetMarkdown);
          let decodedFragment;
          try {
            decodedFragment = decodeURIComponent(fragment).toLowerCase();
          } catch {
            decodedFragment = fragment.toLowerCase();
          }
          if (!anchors.has(decodedFragment)) {
            failures.push(`${path.relative(root, sourcePath)}:${lineNumber}: missing heading #${fragment} in ${path.relative(root, resolved)}`);
          }
        }
      }
    }
  }
  return failures;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const failures = await validateLinks(repoRoot);
  if (failures.length > 0) {
    process.stderr.write(`${failures.join('\n')}\n`);
    process.exitCode = 1;
  } else {
    process.stdout.write('Local Markdown links and heading fragments are valid.\n');
  }
}

export { headingAnchors, markdownDestinations, stripCodeFences, validateLinks };
