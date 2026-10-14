import { realpath, readdir, readFile, stat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import GithubSlugger from 'github-slugger';
import { gfmFromMarkdown } from 'mdast-util-gfm';
import { fromMarkdown } from 'mdast-util-from-markdown';
import { gfm } from 'micromark-extension-gfm';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

function visit(node, callback) {
  for (const child of node.children ?? []) visit(child, callback);
  callback(node);
}

function markdownNodes(markdown) {
  return fromMarkdown(markdown, {
    extensions: [gfm()],
    mdastExtensions: [gfmFromMarkdown()],
  });
}

function markdownLinks(markdown) {
  const destinations = [];
  visit(markdownNodes(markdown), (node) => {
    if (node.type === 'link' || node.type === 'image' || node.type === 'definition') {
      if (typeof node.url === 'string') {
        destinations.push({ destination: node.url, line: node.position?.start.line ?? 1 });
      }
    }
  });
  return destinations;
}

function markdownDestinations(markdown) {
  return markdownLinks(markdown).map(({ destination }) => destination);
}

function headingText(node) {
  if (node.type === 'text' || node.type === 'inlineCode') return node.value;
  if (node.type === 'image') return node.alt ?? '';
  if (node.type === 'break') return ' ';
  if (node.type === 'html') return node.value.replace(/<[^>]*>/g, '');
  return (node.children ?? []).map(headingText).join('');
}

function headingAnchors(markdown) {
  const anchors = new Set();
  const slugger = new GithubSlugger();
  visit(markdownNodes(markdown), (node) => {
    if (node.type === 'heading') anchors.add(slugger.slug(headingText(node)));
  });
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

function insideRoot(root, target) {
  const relative = path.relative(root, target);
  return relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative);
}

async function validateLinks(root) {
  const failures = [];
  const absoluteRoot = await realpath(path.resolve(root));
  for (const sourcePath of await markdownFiles(absoluteRoot)) {
    const source = await readFile(sourcePath, 'utf8');
    for (const { destination, line } of markdownLinks(source)) {
      if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/i.test(destination)) continue;
      const hashAt = destination.indexOf('#');
      const queryAt = destination.indexOf('?');
      const cutAt = [hashAt, queryAt].filter((index) => index >= 0).reduce((a, b) => Math.min(a, b), destination.length);
      const rawTarget = destination.slice(0, cutAt);
      const fragment = hashAt < 0 ? '' : destination.slice(hashAt + 1).split('?')[0];
      let target;
      try {
        // Normalize raw separators before URL decoding; encoded backslashes
        // retain the path semantics of the host platform.
        target = decodeURIComponent(rawTarget.replace(/\\/g, '/'));
      } catch {
        failures.push(`${path.relative(absoluteRoot, sourcePath)}:${line}: malformed escaped path in ${destination}`);
        continue;
      }
      const resolved = path.resolve(path.dirname(sourcePath), target || path.basename(sourcePath));
      if (!insideRoot(absoluteRoot, resolved)) {
        failures.push(`${path.relative(absoluteRoot, sourcePath)}:${line}: local target escapes repository root: ${destination}`);
        continue;
      }
      let targetStat;
      try {
        targetStat = await stat(resolved);
      } catch {
        failures.push(`${path.relative(absoluteRoot, sourcePath)}:${line}: missing local target ${destination}`);
        continue;
      }
      try {
        const canonicalTarget = await realpath(resolved);
        if (!insideRoot(absoluteRoot, canonicalTarget)) {
          failures.push(`${path.relative(absoluteRoot, sourcePath)}:${line}: local target escapes repository root: ${destination}`);
          continue;
        }
      } catch {
        failures.push(`${path.relative(absoluteRoot, sourcePath)}:${line}: cannot resolve local target ${destination}`);
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
          failures.push(`${path.relative(absoluteRoot, sourcePath)}:${line}: missing heading #${fragment} in ${path.relative(absoluteRoot, resolved)}`);
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

export { headingAnchors, markdownDestinations, validateLinks };
