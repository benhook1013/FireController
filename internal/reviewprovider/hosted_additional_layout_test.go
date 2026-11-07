package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
)

func currentAdditionalLayoutFixture(t *testing.T) (hfFixture, string, string) {
	t.Helper()
	f, item := additionalOnlyFixture(t)
	base := strings.Repeat("b", 40)
	info := "<h6>\n<details>\n<summary>" + standingTestPicture(false) + " Review info</summary>\n<dl>\n<dd>\n<br>\n<details>\n<summary>📥 Commits</summary>\n<dl>\n<dd>\n<br>\nReviewing files that changed from the base of the PR and between " + base + " and " + f.intent.HeadSHA + ".\n</dd>\n</dl>\n</details>\n</dd>\n</dl>\n</details>\n</h6>\n"
	f.snapshot.Reviews[0].Body = strings.ReplaceAll(f.snapshot.Reviews[0].Body, "<dd>\n", "<dd>\n<br>\n")
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "<!-- coderabbit-review-publication", info+"<!-- coderabbit-review-publication", 1)
	return f, strings.ReplaceAll(item, "<dd>\n", "<dd>\n<br>\n"), base
}

func TestHostedAdditionalLayoutWithoutPromptPreservesSourceAndScope(t *testing.T) {
	f, item, base := currentAdditionalLayoutFixture(t)
	before, _ := json.Marshal(f.snapshot)
	got := hfWant(t, f, true, 1)
	if got.Findings[0].Detail != item || !strings.Contains(got.Findings[0].Detail, "Unverified finding") {
		t.Fatal("presentation changed original unverified finding detail")
	}
	if scope, ok := ResolveHostedComparison(got.Publication, f.intent.HeadSHA, base, base); !ok || scope != base {
		t.Fatal("standalone Review info heading lost exact comparison", scope, ok)
	}
	after, _ := json.Marshal(f.snapshot)
	if string(before) != string(after) || got.Publication.Batches[0].Body != f.snapshot.Reviews[0].Body {
		t.Fatal("decoder rewrote immutable provider source")
	}
	if again := hfWant(t, f, true, 1); again.Findings[0].SourceKey != got.Findings[0].SourceKey {
		t.Fatal("exact replay changed source identity")
	}
	if _, ok := ResolveHostedComparison(got.Publication, f.intent.HeadSHA, strings.Repeat("c", 40), strings.Repeat("c", 40)); ok {
		t.Fatal("presentation bypassed comparison binding")
	}
}

func TestHostedAdditionalLayoutRetainsRefusalBoundaries(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"missing heading close": func(s string) string { return strings.Replace(s, "</h6>", "", 1) },
		"heading attributes":    func(s string) string { return strings.Replace(s, "<h6>", `<h6 class="metadata">`, 1) },
		"unknown heading":       func(s string) string { return strings.Replace(s, " Review info</summary>", " Other info</summary>", 1) },
		"unknown break":         func(s string) string { return strings.Replace(s, "<br>", `<br data-extra="finding">`, 1) },
		"break outside layout":  func(s string) string { return "<br>\n" + s },
		"repeated leading break": func(s string) string {
			return strings.Replace(s, "<br>\n", "<br>\n\n<br>\n", 1)
		},
		"middle of finding": func(s string) string {
			return strings.Replace(s, "Independent rationale.", "Independent rationale.\n<br>", 1)
		},
		"break before definition": func(s string) string {
			return strings.Replace(s, "<dl>\n<dd>", "<dl>\n<br>\n<dd>", 1)
		},
		"break after nested body": func(s string) string {
			return strings.Replace(s, "</dd>\n</dl>\n</details>", "</dd>\n</dl>\n</details>\n<br>", 1)
		},
		"unbalanced layout": func(s string) string { return strings.Replace(s, "</dd>\n</dl>", "</dl>\n</dd>", 1) },
		"count mismatch": func(s string) string {
			return strings.Replace(s, "Additional comments: 1", "Additional comments: 2", 1)
		},
		"extra heading content": func(s string) string { return strings.Replace(s, "</h6>", "## Extra finding\n</h6>", 1) },
		"visible count":         func(s string) string { return "**Actionable comments posted: unknown**\n" + s },
	} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := currentAdditionalLayoutFixture(t)
			f.snapshot.Reviews[0].Body = mutate(f.snapshot.Reviews[0].Body)
			if got := f.extract(t); got.Complete {
				t.Fatal("unsupported presentation supplied completed inventory")
			}
		})
	}
}

func TestHostedAdditionalLayoutLeadingBreakVisibility(t *testing.T) {
	wrap := func(body string) string { return "<dl>\n<dd>\n" + body + "\n</dd>\n</dl>" }
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"no break", wrap("Body."), true},
		{"one leading break", wrap("<br>\nBody."), true},
		{"blank lines", wrap("\n<br>\nBody."), true},
		{"hidden example before break", wrap("```html\n</dd>\n<br>\n```\n<br>\nBody."), true},
		{"hidden comment before break", wrap("<!-- <br> -->\n<br>\nBody."), true},
		{"quoted example before break", wrap("> </dd>\n> <br>\n<br>\nBody."), true},
		{"one per nested definition", wrap("<br>\n<details>\n<summary>Example</summary>\n" + wrap("<br>\nBody.") + "\n</details>"), true},
		{"one per sibling definition", "<dl>\n<dd>\n<br>\nFirst.\n</dd>\n<dd>\n<br>\nSecond.\n</dd>\n</dl>", true},
		{"outside definition", "<br>", false},
		{"before definition", "<dl>\n<br>\n<dd>\nBody.\n</dd>\n</dl>", false},
		{"after definition", "<dl>\n<dd>\nBody.\n</dd>\n<br>\n</dl>", false},
		{"repeated break", wrap("<br>\n\n<br>\nBody."), false},
		{"middle of body", wrap("Body.\n<br>"), false},
		{"hidden definition cannot restart body", wrap("Body.\n<!-- <dd> -->\n<br>"), false},
		{"quoted definition cannot restart body", wrap("Body.\n> <dd>\n<br>"), false},
		{"hidden break cannot reset leading break", wrap("<br>\n```html\n<dd>\n```\n<br>"), false},
		{"different details depth", wrap("<details>\n<br>\n</details>"), false},
		{"closed details cannot restart body", wrap("<details>\n<summary>Example</summary>\n</details>\n<br>"), false},
		{"different HTML quote depth", wrap("<blockquote>\n<br>\n</blockquote>"), false},
		{"closed HTML quote cannot restart body", wrap("<blockquote>\nExample.\n</blockquote>\n<br>"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, sound := findingLines(tc.body)
			if !sound {
				t.Fatal("fixture is not balanced")
			}
			before := append([]findingLine(nil), lines...)
			projected, ok := findingAdditionalLayout(lines, 0, len(lines)-1)
			if ok != tc.want {
				t.Fatalf("layout accepted = %t, want %t", ok, tc.want)
			}
			for i, original := range before {
				if lines[i] != original {
					t.Fatal("projection mutated original source lines")
				}
				if !ok {
					continue
				}
				line := projected[i]
				if !original.hidden && original.quote == 0 && original.text == "<br>" && !line.hidden {
					t.Fatal("qualified leading break was not projected")
				}
				if (original.hidden || original.quote != 0) && line != original {
					t.Fatal("projection changed hidden or quoted examples")
				}
				line.hidden = original.hidden
				if line != original {
					t.Fatal("projection changed source text or offsets")
				}
			}
		})
	}
}
