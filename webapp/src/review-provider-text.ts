// Presentation only: retained finding text is never rewritten. Code is protected
// before recognizing provider HTML, including code inside quoted wrappers.
interface ProtectedCode { token: string; source: string; fenced: boolean }

function protectCode(source: string) {
  let prefix = '\uE000review-code-';
  while (source.includes(prefix)) prefix += '-';
  const code: ProtectedCode[] = [];
  const protect = (value: string, fenced: boolean) => {
    const token = `${prefix}${code.length}\uE001`;
    code.push({ token, source: value, fenced });
    return token;
  };
  const lines = source.replace(/\r\n?/gu, '\n').split('\n');
  const blocks: string[] = [];
  for (let i = 0; i < lines.length; i++) {
    const opening = lines[i].match(/^[ \t]{0,3}(?:>[ \t]?)*(`{3,}|~{3,})/u);
    if (!opening) { blocks.push(lines[i]); continue; }
    const fence = opening[1];
    const closing = new RegExp(`^[ \\t]{0,3}(?:>[ \\t]?)*${fence[0]}{${fence.length},}[ \\t]*$`, 'u');
    const start = i;
    while (++i < lines.length && !closing.test(lines[i])) { /* Preserve an unfinished fence too. */ }
    blocks.push(protect(lines.slice(start, Math.min(i + 1, lines.length)).join('\n'), true));
  }
  const fenced = blocks.join('\n');
  // Backtick spans can cross lines. Match equal-length delimiters so a tag in a
  // double-backtick example cannot accidentally become presentation markup.
  const protectInline = (text: string) => {
    const spans = /`+/gu;
    let result = '', from = 0, opening: RegExpExecArray | null;
    while ((opening = spans.exec(text))) {
      let closing: RegExpExecArray | null;
      while ((closing = spans.exec(text)) && closing[0].length !== opening[0].length) { /* Find its matching delimiter. */ }
      if (!closing) break;
      result += text.slice(from, opening.index) + protect(text.slice(opening.index, spans.lastIndex), false);
      from = spans.lastIndex;
    }
    return result + text.slice(from);
  };
  // An inline span cannot cross a fenced block boundary.
  const text = fenced.split(new RegExp(`(${prefix}\\d+\uE001)`, 'gu'))
    .map(part => part.startsWith(prefix) ? part : protectInline(part)).join('');
  return {
    text,
    withoutFences(value: string) { for (const item of code) if (item.fenced) value = value.replaceAll(item.token, '\n'); return value; },
    restore(value: string) { for (const item of code) value = value.replaceAll(item.token, item.source); return value; },
  };
}

function headingText(value: string): string {
  return value.trim().replace(/^(?:>[ \t]*)+/u, '').replace(/^#{1,6}\s+/u, '')
    .replace(/<[^>]*>/gu, '').replace(/[*_`]/gu, '').replace(/^[^\p{L}\p{N}]+/u, '')
    .replace(/[\s:.]+$/u, '').replace(/\s+/gu, ' ').toLowerCase();
}

function technicalHeading(value: string): boolean {
  return /^(?:prompt for ai agents?|ai (?:agent )?prompt|suggested (?:change|fix|code)|code suggestion|committable suggestion|proposed (?:fix|refactor)|tools?|diagnostics?|analysis chain|verification (?:output|details)|script (?:output|executed)|supported by static analysis|learn more)$/u.test(headingText(value));
}

function omitTechnicalDetails(text: string): string {
  const tags = /<\/?details\b[^>]*>/giu;
  const stack: { start: number; body: number }[] = [];
  const omitted: { start: number; end: number }[] = [];
  let tag: RegExpExecArray | null;
  const close = (item: { start: number; body: number }, end: number) => {
    const summary = text.slice(item.body, end).match(/^\s*<summary\b[^>]*>([\s\S]*?)<\/summary\s*>/iu);
    if (summary && technicalHeading(summary[1])) omitted.push({ start: item.start, end });
  };
  while ((tag = tags.exec(text))) {
    if (/^<\//u.test(tag[0])) { const item = stack.pop(); if (item) close(item, tags.lastIndex); }
    else stack.push({ start: tag.index, body: tags.lastIndex });
  }
  for (const item of stack) close(item, text.length);
  omitted.sort((a, b) => a.start - b.start || b.end - a.end);
  let result = '', from = 0;
  for (const range of omitted) {
    if (range.start < from) continue;
    result += text.slice(from, range.start) + '\n\n';
    from = range.end;
  }
  return result + text.slice(from);
}

function unwrapProviderHTML(text: string): string {
  text = text.replace(/<!--[\s\S]*?(?:-->|$)/gu, '')
    .replace(/<(script|style)\b[^>]*>[\s\S]*?<\/\1\s*>/giu, '');
  text = text.replace(/<picture\b[^>]*>[\s\S]*?<\/picture\s*>/giu, '')
    .replace(/<summary\b[^>]*>([\s\S]*?)<\/summary\s*>/giu, (tag, body: string) =>
      /^\s*<strong>\s*(?:Critical|Major|Minor|Trivial)\s*<\/strong>\s*·/iu.test(body) ? '\n\n' : tag);
  // Attributes are consumed as quoted units, so a '>' inside an image attribute
  // cannot leave a provider URL behind. Unknown markup stays inert Markdown.
  return text.replace(/<\/?([a-z][\w-]*)(?:\s+(?:"[^"]*"|'[^']*'|[^'">])*)?\s*\/?>/giu, (tag, name: string) => {
    switch (name.toLowerCase()) {
      case 'picture': case 'source': case 'img': return '';
      case 'details': case 'summary': case 'blockquote': case 'dl': case 'dt': case 'dd': case 'p': return '\n\n';
      case 'br': return '\n';
      case 'strong': case 'b': return '**';
      case 'em': case 'i': return '_';
      case 'code': return '`';
      default: return tag;
    }
  });
}

function providerMetadata(line: string): boolean {
  const source = line.trim().replace(/^(?:>[ \t]*)+/u, '');
  const fields = source.split(/ (?:·|\|) /u);
  if (fields.length === 3 && fields.every(field => /^\*\*.+\*\*$/u.test(field))
    && fields.some(field => /^(?:critical|major|minor|trivial)$/u.test(headingText(field)))) return true;
  const legacyFields = headingText(line).split(/\s*\|\s*/u);
  return legacyFields.length >= 2 && legacyFields.length <= 4
    && /^(?:potential issue|nitpick|suggestion|refactor suggestion|security issue|bug)$/u.test(legacyFields[0])
    && legacyFields.slice(1).some(field => /^(?:[^\p{L}\p{N}]*)(?:critical|major|minor|trivial|high|medium|low|info|p[0-3])$/u.test(field));
}

function cleanedLines(text: string, title: string, compact: boolean, restore: (value: string) => string): string {
  let first = true, technicalLevel: number | undefined;
  const result: string[] = [];
  for (const line of unwrapProviderHTML(text).split('\n')) {
    if (providerMetadata(line)) continue;
    if (/^\s*(?:>\s*)+$/u.test(line)) continue;
    if (first && line.trim()) {
      first = false;
      if (headingText(restore(line)) === headingText(title)) continue;
    }
    const level = line.match(/^\s*(?:>\s*)*(#{1,6})\s/u)?.[1].length;
    if (compact && technicalHeading(line)) { technicalLevel = level ?? 7; continue; }
    if (technicalLevel !== undefined) {
      if (level !== undefined && level <= technicalLevel) technicalLevel = undefined;
      else continue;
    }
    result.push(line);
  }
  return result.join('\n').replace(/\n[ \t]*\n(?:[ \t]*\n)+/gu, '\n\n').trim();
}

export interface ReviewProviderText {
  rationale: string;
  full: string;
}

export function reviewProviderText(detail: string, title: string): ReviewProviderText {
  const protectedCode = protectCode(detail);
  return {
    rationale: protectedCode.restore(cleanedLines(protectedCode.withoutFences(omitTechnicalDetails(protectedCode.text)), title, true, protectedCode.restore)),
    full: protectedCode.restore(cleanedLines(protectedCode.text, title, false, protectedCode.restore)),
  };
}

export function reviewTextExcerpt(text: string, limit: number): { text: string; truncated: boolean } {
  const characters = Array.from(text.trim());
  if (characters.length <= limit) return { text: text.trim(), truncated: false };
  let excerpt = characters.slice(0, limit).join('');
  const boundary = Math.max(excerpt.lastIndexOf('\n'), excerpt.lastIndexOf(' '));
  if (boundary >= excerpt.length * 0.6) excerpt = excerpt.slice(0, boundary);
  return { text: excerpt.trimEnd() + '…', truncated: true };
}
