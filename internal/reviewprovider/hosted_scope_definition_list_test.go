package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func hostedDefinitionListScope(declaration string) string {
	return "<details>\n<summary><strong>ℹ️ Review info</strong></summary>\n<dl>\n<dd>\n<details>\n<summary>📥 Commits</summary>\n<dl>\n<dd>\n" + declaration + "\n</dd>\n</dl>\n</details>\n</dd>\n</dl>\n</details>"
}

func TestHostedComparisonDefinitionListMetadata(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, start := range []string{parent, merge} {
		for _, body := range []string{hostedDefinitionListScope(hostedScopeDeclaration(start, head)), hostedDefinitionListScope(hostedScopeDeclaration("`"+start+"`", "`"+head+"`")), strings.ReplaceAll(hostedDefinitionListScope(hostedScopeDeclaration(start, head)), ">\n<", "><")} {
			publication := hostedScopePublication(body)
			before := append([]HostedReview(nil), publication.Batches...)
			got, ok := ResolveHostedComparison(publication, head, parent, merge)
			if !ok || got != start || !reflect.DeepEqual(before, publication.Batches) {
				t.Fatal("balanced metadata definition lists lost exact comparison or raw bodies", got, ok)
			}
		}
	}
}

func TestHostedComparisonDefinitionListsCannotGrantAuthority(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	line := hostedScopeDeclaration(parent, head)
	for name, wrap := range map[string]func(string) string{
		"top level": func(s string) string { return "<dl><dd>\n" + s + "\n</dd></dl>" },
		"prompt": func(s string) string {
			return "<details><summary>🤖 Prompt for AI Agents</summary>\n" + s + "\n</details>"
		},
		"example": func(s string) string { return "<details><summary>Example</summary>\n" + s + "\n</details>" },
		"unknown details": func(s string) string {
			return "<details><summary>Unrecognized metadata</summary>\n" + s + "\n</details>"
		},
		"unknown wrapper": func(s string) string { return "<section>\n" + s + "\n</section>" },
		"blockquote":      func(s string) string { return "<blockquote>\n" + s + "\n</blockquote>" },
		"pre":             func(s string) string { return "<pre>\n" + s + "\n</pre>" },
		"fence":           func(s string) string { return "```text\n" + s + "\n```" },
		"comment":         func(s string) string { return "<!-- example\n" + s + "\n-->" },
		"quote":           func(s string) string { return "> " + strings.ReplaceAll(s, "\n", "\n> ") },
	} {
		t.Run(name, func(t *testing.T) {
			if base, ok := ResolveHostedComparison(hostedScopePublication(wrap(hostedDefinitionListScope(line))), head, parent, merge); ok || base != "" {
				t.Fatal("definition list laundered a blocked context", base, ok)
			}
			hiddenConflict := wrap(hostedDefinitionListScope(hostedScopeDeclaration(merge, head)))
			if base, ok := ResolveHostedComparison(hostedScopePublication(line+"\n"+hiddenConflict), head, parent, merge); !ok || base != parent {
				t.Fatal("hidden definition list changed genuine comparison", base, ok)
			}
		})
	}
	// A definition list itself never turns an unlabelled/unknown child into metadata.
	for _, body := range []string{"<dl><dd>\n" + line + "\n</dd></dl>", "<details><summary>ℹ️ Review info</summary><dl><dd><details>\n" + line + "\n</details></dd></dl></details>", "<details><summary>ℹ️ Review info</summary><dl><dd><details><summary>Example</summary>\n" + line + "\n</details></dd></dl></details>"} {
		if base, ok := ResolveHostedComparison(hostedScopePublication(body), head, parent, merge); ok || base != "" {
			t.Fatal("unknown scope gained metadata authority", base, ok)
		}
	}
}

func TestHostedComparisonDefinitionListStructureAndDeclarationsRemainStrict(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	good := hostedScopeDeclaration(parent, head)
	for _, body := range []string{
		strings.Replace(hostedDefinitionListScope(good), "</dd>\n</dl>", "</dl>\n</dd>", 1),
		strings.Replace(hostedDefinitionListScope(good), "</dd>", "", 1),
		strings.Replace(hostedDefinitionListScope(good), "</dl>", "</dd>", 1),
		hostedDefinitionListScope(good) + "\n</dl>",
		hostedDefinitionListScope(good + "\n" + hostedScopeDeclaration(merge, head)),
		hostedDefinitionListScope(good + "\n" + hostedScopeDeclaration(parent, merge)),
		hostedDefinitionListScope(good + "\n" + hostedScopeDeclaration(strings.Repeat("f", 40), head)),
		hostedDefinitionListScope(good + "\n" + good + " trailing"),
	} {
		if base, ok := ResolveHostedComparison(hostedScopePublication(body), head, parent, merge); ok || base != "" {
			t.Fatal("definition lists weakened structural or declaration guards", base, ok)
		}
	}
	for _, publication := range []HostedPublication{hostedScopePublication(hostedDefinitionListScope(good + "\n" + good)), hostedScopePublication(hostedDefinitionListScope(good), hostedDefinitionListScope(good))} {
		if base, ok := ResolveHostedComparison(publication, head, parent, merge); !ok || base != parent {
			t.Fatal("agreeing definition-list declarations changed existing semantics", base, ok)
		}
	}
	if base, ok := ResolveHostedComparison(hostedScopePublication(hostedDefinitionListScope(good), hostedDefinitionListScope(hostedScopeDeclaration(merge, head))), head, parent, merge); ok || base != "" {
		t.Fatal("conflicting batches accepted", base, ok)
	}
}
