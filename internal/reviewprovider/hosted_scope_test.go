package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func hostedScopeDeclaration(start, end string) string {
	return "Reviewing files that changed from the base of the PR and between " + start + " and " + end + "."
}

func hostedScopePublication(bodies ...string) HostedPublication {
	p := HostedPublication{State: "attributed"}
	for i, body := range bodies {
		p.Batches = append(p.Batches, HostedReview{ID: int64(i + 1), Body: body})
	}
	return p
}

func TestHostedComparisonExplicitParentAndMergeBase(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, start := range []string{parent, merge} {
		for _, quotedStart := range []bool{false, true} {
			for _, quotedEnd := range []bool{false, true} {
				first, last := start, head
				if quotedStart {
					first = "`" + first + "`"
				}
				if quotedEnd {
					last = "`" + last + "`"
				}
				line := hostedScopeDeclaration(first, last)
				for _, body := range []string{line, "<details>\n<summary>ℹ️ Review info</summary>\n<details>\n<summary>📥 Commits</summary>\n" + line + "\n</details>\n</details>"} {
					p := hostedScopePublication(body)
					got, ok := ResolveHostedComparison(p, head, parent, merge)
					if !ok || got != start {
						t.Fatalf("comparison not established: %s %v", got, ok)
					}
				}
			}
		}
	}
}

func TestHostedComparisonAllDeclarationsMustAgree(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	good := hostedScopeDeclaration(parent, head)
	for _, p := range []HostedPublication{hostedScopePublication(good + "\n" + good), hostedScopePublication(good, "Other batch metadata.", good)} {
		if base, ok := ResolveHostedComparison(p, head, parent, merge); !ok || base != parent {
			t.Fatal("agreeing declarations rejected")
		}
	}
	for _, bad := range []string{hostedScopeDeclaration(merge, head), hostedScopeDeclaration(parent, merge), hostedScopeDeclaration(strings.Repeat("d", 40), head), "Reviewing files from an unknown base."} {
		for _, p := range []HostedPublication{hostedScopePublication(good + "\n" + bad), hostedScopePublication(good, bad)} {
			if base, ok := ResolveHostedComparison(p, head, parent, merge); ok || base != "" {
				t.Fatal("conflicting/unknown declaration accepted")
			}
		}
	}
}

func TestHostedComparisonExactSyntaxAndLowercaseInputs(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	line := hostedScopeDeclaration(parent, head)
	for _, malformed := range []string{
		strings.TrimSuffix(line, "."), "prefix " + line, line + " suffix", " " + line, "**" + line + "**",
		strings.Replace(line, parent, parent[:39], 1), strings.Replace(line, head, strings.ToUpper(head), 1),
		strings.Replace(line, parent, "`"+parent, 1), strings.Replace(line, parent, parent+"`", 1),
		strings.Replace(line, parent, "``"+parent+"``", 1), strings.Replace(line, "Reviewing", "reviewing", 1),
		strings.Replace(line, "base of the PR", "a selected base", 1),
	} {
		if base, ok := ResolveHostedComparison(hostedScopePublication(line+"\n"+malformed), head, parent, merge); ok || base != "" {
			t.Fatalf("malformed line accepted: %q", malformed)
		}
	}
	for _, bad := range []string{"", "bad", strings.ToUpper(head), strings.Repeat("a", 39), strings.Repeat("g", 40)} {
		for i := 0; i < 3; i++ {
			inputs := []string{head, parent, merge}
			inputs[i] = bad
			if base, ok := ResolveHostedComparison(hostedScopePublication(line), inputs[0], inputs[1], inputs[2]); ok || base != "" {
				t.Fatal("invalid input SHA accepted")
			}
		}
	}
}

func TestHostedComparisonHiddenAndPromptPayloadCannotEstablishScope(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	line := hostedScopeDeclaration(parent, head)
	wrappers := []struct{ before, after string }{
		{"> ", ""}, {"> > ", ""}, {"    ", ""}, {"```text\n", "\n```"}, {"~~~\n", "\n~~~"},
		{"<pre>\n", "\n</pre>"}, {"<blockquote>\n", "\n</blockquote>"}, {"<!-- example\n", "\n-->"},
		{"<details>\n<summary>🤖 Prompt for AI Agents</summary>\n", "\n</details>"},
		{"<details>\n<summary class=\"hint\"><em>🤖 Prompt for AI Agents</em></summary>\n", "\n</details>"},
		{"<details><summary>🤖 Prompt to fix review comments</summary>\n<details>\n<summary>Example</summary>\n", "\n</details>\n</details>"},
		{"<details>\n<summary>Other metadata</summary>\n</details><details><summary>🤖 Prompt for AI Agents</summary>\n", "\n</details>"},
	}
	for _, w := range wrappers {
		hidden := w.before + line + w.after
		if base, ok := ResolveHostedComparison(hostedScopePublication(hidden), head, parent, merge); ok || base != "" {
			t.Fatal("hidden/prompt-only declaration accepted")
		}
		hiddenConflict := w.before + hostedScopeDeclaration(merge, head) + w.after
		if base, ok := ResolveHostedComparison(hostedScopePublication(line+"\n"+hiddenConflict), head, parent, merge); !ok || base != parent {
			t.Fatal("hidden payload overrode source declaration")
		}
	}
}

func TestHostedComparisonBoundsAndMalformedPublicationNeverMutate(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	line := hostedScopeDeclaration(parent, head)
	for _, state := range []string{"", "active", "uncertain", "ambiguous", "completed"} {
		p := hostedScopePublication(line)
		p.State = state
		if base, ok := ResolveHostedComparison(p, head, parent, merge); ok || base != "" {
			t.Fatal("unattributed publication accepted")
		}
	}
	for _, p := range []HostedPublication{hostedScopePublication(), hostedScopePublication("status only"), hostedScopePublication(strings.Repeat("x", (64<<10)+1)), hostedScopePublication(line + "\xff"), hostedScopePublication(line + "\x00"), hostedScopePublication("<details>\n" + line), hostedScopePublication(line + "\n</details>")} {
		if base, ok := ResolveHostedComparison(p, head, parent, merge); ok || base != "" {
			t.Fatal("unsupported publication accepted")
		}
	}
	p := hostedScopePublication(line)
	for len(p.Batches) < 200 {
		p.Batches = append(p.Batches, HostedReview{Body: "metadata"})
	}
	before := append([]HostedReview(nil), p.Batches...)
	if base, ok := ResolveHostedComparison(p, head, parent, merge); !ok || base != parent {
		t.Fatal("bounded publication rejected")
	}
	if !reflect.DeepEqual(before, p.Batches) {
		t.Fatal("input modified")
	}
	p.Batches = append(p.Batches, HostedReview{Body: line})
	if base, ok := ResolveHostedComparison(p, head, parent, merge); ok || base != "" {
		t.Fatal("excess batches accepted")
	}
}

func TestHostedComparisonNestedExampleAndProposalScopesAreNotProviderFacts(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	line := hostedScopeDeclaration(parent, head)
	for _, label := range []string{"Example", "Proposed wording", "Suggested change", "Proposed fix", "📝 Committable suggestion", "🤖 Prompt for AI Agents"} {
		for _, decorated := range []string{label, "<em>" + label + "</em>", strings.ReplaceAll(label, " ", " <strong></strong>")} {
			wrapper := func(declaration string) string {
				return "<details>\n<summary>ℹ️ Review info</summary>\n<details>\n<summary class=\"metadata\">" + decorated + "</summary>\n" + declaration + "\n</details>\n</details>"
			}
			if base, ok := ResolveHostedComparison(hostedScopePublication(wrapper(line)), head, parent, merge); ok || base != "" {
				t.Fatalf("%s established source scope", label)
			}
			body := "<details>\n<summary>ℹ️ Review info</summary>\n" + line + "\n</details>\n" + wrapper(hostedScopeDeclaration(merge, head))
			if base, ok := ResolveHostedComparison(hostedScopePublication(body), head, parent, merge); !ok || base != parent {
				t.Fatalf("%s hid legitimate provider header", label)
			}
			body = wrapper(hostedScopeDeclaration(merge, head)) + "\n<details>\n<summary>ℹ️ Review info</summary>\n" + line + "\n</details>"
			if base, ok := ResolveHostedComparison(hostedScopePublication(body), head, parent, merge); !ok || base != parent {
				t.Fatalf("%s exclusion leaked into later provider metadata", label)
			}
		}
	}
}

func TestHostedComparisonCategoryClosersCannotPoisonMetadataScopes(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	header := hostedScopeDeclaration(parent, head)
	metadata := "<details>\n<summary>ℹ️ Review info</summary>\n<details>\n<summary>📥 Commits</summary>\n" + header + "\n</details>\n</details>"
	outside := hfOutsideCategory(1, hfOutside(2, "a/source.go", "3-4", "Synthetic full title."))
	duplicate := hfDuplicateCategory(1, hfDuplicateGroup("b/source.md", 1, hfDuplicateItem(3, "8-8", "Independent duplicate.")))
	for _, body := range []string{outside + "\n" + duplicate + "\n" + metadata, metadata + "\n" + outside + "\n" + duplicate, outside + "\n" + metadata + "\n" + duplicate} {
		if base, ok := ResolveHostedComparison(hostedScopePublication(body), head, parent, merge); !ok || base != parent {
			t.Fatal("category scopes hid/poisoned genuine metadata declaration")
		}
	}
	// Structural tokens in raw pre examples must not escape into the metadata
	// stack. Closing pre and details on the same line still closes both scopes.
	example := "<details><summary>Unknown illustration</summary><pre>\n<details>\n<!-- example\n" + hostedScopeDeclaration(merge, head) + "\n</pre></details>"
	if base, ok := ResolveHostedComparison(hostedScopePublication(example+"\n"+metadata), head, parent, merge); !ok || base != parent {
		t.Fatal("skipped pre example poisoned following metadata")
	}
}

func TestHostedComparisonOnlyRecognizedMetadataAncestorsCanProveScope(t *testing.T) {
	head, parent, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	header := hostedScopeDeclaration(parent, head)
	for _, label := range []string{"Illustration", "Unrecognized template", "<em>Illustration</em>", "Review info example", "Commits example"} {
		unknown := "<details><summary>ℹ️ Review info</summary><details><summary>" + label + "</summary>\n" + header + "\n</details></details>"
		if base, ok := ResolveHostedComparison(hostedScopePublication(unknown), head, parent, merge); ok || base != "" {
			t.Fatalf("unknown %s granted provider scope", label)
		}
		if base, ok := ResolveHostedComparison(hostedScopePublication(unknown+"\n"+header), head, parent, merge); !ok || base != parent {
			t.Fatal("unknown unrelated section blocked genuine top-level declaration")
		}
	}
	for _, wrapper := range []string{
		"<details>\n" + header + "\n</details>",
		"<details><summary>Illustration</summary><details><summary>📥 Commits</summary>\n" + header + "\n</details></details>",
		"<section><blockquote>\n" + header + "\n</blockquote></section>",
	} {
		if base, ok := ResolveHostedComparison(hostedScopePublication(wrapper), head, parent, merge); ok || base != "" {
			t.Fatal("unowned ancestor was laundered through metadata")
		}
	}
	conflict := hostedScopeDeclaration(merge, head)
	sameLine := "<details><summary>Illustration</summary><blockquote>\n" + conflict + "\n</blockquote></details><details><summary><em>ℹ️ Review info</em></summary><details><summary>📥 Commits</summary>\n" + header + "\n</details></details>"
	if base, ok := ResolveHostedComparison(hostedScopePublication(sameLine), head, parent, merge); !ok || base != parent {
		t.Fatal("same-line close/open scope order changed declaration authority")
	}
}
