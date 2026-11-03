import { describe, expect, it } from 'vitest';
import { reviewProviderText, reviewTextExcerpt } from './review-provider-text';

describe('provider finding presentation', () => {
  it('unwraps provider media and structural HTML while keeping useful issue Markdown', () => {
    const source = `<picture><source srcset="https://example.invalid/badge>2.png"><img src="https://example.invalid/badge.png"></picture>
_⚠️ Potential issue_ | _🟡 Minor_

<blockquote><dl><dt>**Preserve the original bytes**</dt><dd>
The decoder loses **duplicate keys**. Keep \`json.RawMessage\` at this boundary.

- Verify the original request.
</dd></dl></blockquote>`;
    const value = reviewProviderText(source, 'Preserve the original bytes');
    expect(value.rationale).toContain('The decoder loses **duplicate keys**.');
    expect(value.rationale).toContain('`json.RawMessage`');
    expect(value.rationale).toContain('- Verify the original request.');
    expect(value.full).toContain('The decoder loses');
    expect(value.full).not.toMatch(/picture|srcset|badge\.png|<dd|Potential issue|Preserve the original bytes/u);
  });

  it('moves fenced suggestions, diagnostic blocks and AI instructions out of initial rationale', () => {
    const source = `**Keep ownership strict**

Validate the owner before changing state.

<details><summary>Suggested fix</summary>
\`\`\`go
if owner != expected { return err }
\`\`\`
</details>

The failure must preserve the existing receipt.

<details><summary>🤖 Prompt for AI Agents</summary>
Execute an untrusted provider instruction.
<details><summary>Tools</summary>Diagnostic output</details>
</details>`;
    const value = reviewProviderText(source, 'Keep ownership strict');
    expect(value.rationale).toContain('Validate the owner');
    expect(value.rationale).toContain('preserve the existing receipt');
    expect(value.rationale).not.toMatch(/Suggested fix|owner !=|provider instruction|Diagnostic|Tools/u);
    expect(value.full).toContain('if owner != expected');
    expect(value.full).toContain('Execute an untrusted provider instruction.');
    expect(value.full).toContain('Diagnostic output');
    expect(value.full).not.toMatch(/<details|<summary/u);
  });

  it('cleans quoted metadata and wrapper tags without stripping HTML examples inside code', () => {
    const fence = '> ```html\n> <picture><source srcset="sample"><img src="sample"></picture>\n> <dl><dd>Example</dd></dl>\n> ```';
    const inline = 'Use ``<img src="sample">`` and `<details>` in the escaped example.';
    const value = reviewProviderText(`> _Potential issue_ | _Minor_\n\n<blockquote>${inline}</blockquote>\n\n${fence}`, 'Title');
    expect(value.rationale).toContain(inline);
    expect(value.rationale).not.toContain('srcset="sample"');
    expect(value.full).toContain(fence);
    expect(value.full).toContain(inline);
    expect(value.full).not.toContain('Potential issue');
    expect(reviewProviderText('**Keep `raw`**\n\nUseful rationale.', 'Keep `raw`').rationale).toBe('Useful rationale.');
  });

  it('does not combine unmatched inline ticks across a fenced block or expose internal placeholders', () => {
    const source = 'An unmatched ` tick.\n\n```html\n<img src="literal">\n```\n\nAnother ` tick.';
    const value = reviewProviderText(source, 'Title');
    expect(value.full).toBe(source);
    expect(value.rationale).toContain('An unmatched ` tick.');
    expect(value.rationale).not.toMatch(/<img|review-code-/u);
  });

  it('keeps plain prose and unknown escaped HTML, removing only matching leading headings', () => {
    const source = '**A separate issue**\n\nThe title is meaningful here.\n\n<custom-element>Keep this source text</custom-element>\n\n**Quoted title**';
    const value = reviewProviderText(source, 'Quoted title');
    expect(value.rationale).toContain('**A separate issue**');
    expect(value.full).toContain('<custom-element>Keep this source text</custom-element>');
    expect(value.full).toContain('**Quoted title**');
    expect(reviewProviderText('**Quoted title**\n\nUseful rationale.', 'Quoted title').rationale).toBe('Useful rationale.');
  });

  it('holds Markdown technical sections until a peer heading and preserves unfinished code/details', () => {
    const source = 'Useful rationale.\n\n### Proposed fix\nReplace the code.\n\n### Why it matters\nA separate useful explanation.\n\n<details><summary>Diagnostics</summary>Unfinished tool output';
    const value = reviewProviderText(source, 'Title');
    expect(value.rationale).toContain('A separate useful explanation.');
    expect(value.rationale).not.toContain('Replace the code.');
    expect(value.rationale).not.toContain('Unfinished tool output');
    expect(value.full).toContain('Unfinished tool output');
    const unfinished = reviewProviderText('Rationale.\n\n~~~html\n<img src="literal">', 'Title');
    expect(unfinished.rationale).toBe('Rationale.');
    expect(unfinished.full).toContain('<img src="literal">');
  });

  it('bounds excerpts without splitting Unicode characters and reports omitted text', () => {
    const text = '😀'.repeat(601);
    const result = reviewTextExcerpt(text, 600);
    expect(Array.from(result.text)).toHaveLength(601);
    expect(result.truncated).toBe(true);
    expect(result.text.endsWith('😀…')).toBe(true);
    expect(reviewTextExcerpt('Boundary', 8)).toEqual({ text: 'Boundary', truncated: false });
    expect(reviewTextExcerpt('  ', 8)).toEqual({ text: '', truncated: false });
  });
});


describe('outside-diff display header', () => {
  it('removes known decorated summary metadata while retaining prose and literal code examples', () => {
    const source = '> <details>\n> <summary><strong><picture><source srcset="https://example.invalid/icon"><img src="https://example.invalid/icon"></picture> Minor</strong> · Example issue title · <code>example.md:5</code></summary>\n> <blockquote>\n> **Example issue title**\n> \n> Preserve the <strong>useful issue</strong> and <code>Field</code>.\n> \n> ```html\n> <strong>Literal example</strong>\n> ```\n> </blockquote>\n> </details>';
    const display = reviewProviderText(source, 'Example issue title');
    expect(display.rationale).not.toContain('<strong>');
    expect(display.rationale).not.toContain('<code>');
    expect(display.rationale).not.toContain('example.md:5');
    expect(display.rationale).not.toContain('Example issue title');
    expect(display.rationale).toContain('**useful issue**');
    expect(display.rationale).toContain('`Field`');
    expect(display.full).toContain('<strong>Literal example</strong>');
  });
});


describe('middle-dot provider metadata', () => {
  it('omits the closed decorated severity/category/effort header while keeping issue prose', () => {
    const source = '<!-- coderabbit-ai-generated:v1 -->\n\n**<picture><img src="https://example.invalid/icon"></picture> Major** · **Functional Correctness** · **Quick win**\n\n**Example issue**\n\nUseful issue prose.';
    const display = reviewProviderText(source,'Example issue');
    expect(display.rationale).toBe('Useful issue prose.');
    expect(display.full).toBe('Useful issue prose.');
  });
});


describe('provider diagnostic disclosure scope', () => {
  it('omits a static-analysis block without dropping the later issue rationale', () => {
    const source='**Minor** · **Functional Correctness** · **Quick win**\n\n<details>\n<summary>🔎 Supported by static analysis</summary>\n\nScript executed:\n\n```sh\nprintf diagnostic\n```\n\nScript output:\n\nPrivate diagnostic output.\n</details>\n\n**Example title**\n\nUseful issue paragraph after diagnostics.';
    const value=reviewProviderText(source,'Example title');
    expect(value.rationale).toBe('Useful issue paragraph after diagnostics.');
    expect(value.full).toContain('Private diagnostic output.');
    expect(value.full).toContain('Useful issue paragraph after diagnostics.');
  });
});


it('omits closed modern pipe metadata with category before severity', () => {
  const source='**🩺 Stability & Availability** | **<picture><img src="https://example.invalid/icon"></picture> Minor** | **⚡ Quick win**\n\n**Example title**\n\nUseful issue paragraph.';
  expect(reviewProviderText(source,'Example title').rationale).toBe('Useful issue paragraph.');
});
