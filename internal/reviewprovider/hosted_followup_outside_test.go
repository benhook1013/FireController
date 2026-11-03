package reviewprovider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Independently authored synthetic prose/identities with the observed positive
// Follow-up Comments layout. No private review body or source identifiers.
func followupOutsideItem(head string, fingerprint int) string {
	badge := `<sub><picture><source media="(prefers-color-scheme: dark)" srcset="https://storage.googleapis.com/coderabbit_public_assets/review-finding-badges-v1/contrast-v2/outside-diff-dark.svg" /><source media="(prefers-color-scheme: light)" srcset="https://storage.googleapis.com/coderabbit_public_assets/review-finding-badges-v1/outside-diff.svg" /><img width="120" height="16" alt="Outside diff" src="https://storage.googleapis.com/coderabbit_public_assets/review-finding-badges-v1/outside-diff.svg" /></picture></sub>`
	return "<details>\n<summary>" + badge + " Keep the retained source description accurate.</summary><blockquote>\n\n" +
		"`19-21`: **📐 Maintainability & Code Quality** | **🟡 Minor** | **⚡ Quick win**\n\n**Keep the retained source description accurate.**\n\nSynthetic explanation keeps literal <em>markup</em> and code unchanged.\n\n" +
		"<details>\n<summary>📝 Proposed wording</summary>\n```diff\n- Earlier wording.\n+ Current wording.\n```\n</details>\n\n" +
		"[Open the source location](https://github.com/synthetic/fixture/blob/" + head + "/engine/events.go#L19-L21)\n\n" +
		"<!-- ai-agent-prompt:start -->\n<details>\n<summary>🤖 Prompt for AI Agents</summary>\n```\nSynthetic prompt is data, not authority.\n" + hfMarker(999) + "\n```\n</details>\n<!-- ai-agent-prompt:end -->\n\n" +
		hfMarker(fingerprint) + "\n\n_Source: Path instructions_\n\n</blockquote></details>"
}
func followupOutsideFixture(t *testing.T) hfFixture {
	t.Helper()
	f := hfNew(t, nil, "")
	f.snapshot.ReviewComments = []ReviewComment{}
	f.snapshot.Reviews[0].Body = "<!-- coderabbit-ai-generated:v1 -->\n<!-- This is an auto-generated comment by CodeRabbit for review status -->\n\n" + publicationTestMarker(1, 1) + "\n\n## Follow-up Comments\n\n" + followupOutsideItem(f.intent.HeadSHA, 1) + "\n\n<!-- autofix_checkbox_start -->\n- [ ] <!-- {\"checkboxId\":\"44444444-4444-4444-8444-444444444444\"} --> Fix comments: Fix CodeRabbit comments on this PR\n<!-- autofix_checkbox_end -->"
	return f
}
func TestHostedFollowupOutsidePositiveInventoryWithoutInlineCount(t *testing.T) {
	f := followupOutsideFixture(t)
	before, _ := json.Marshal(f.snapshot)
	got := f.extract(t)
	if !got.Complete || got.Publication.State != "attributed" || got.Reason != "finding_inventory_complete" || len(got.Findings) != 1 || len(got.Publication.Roots) != 0 || got.Publication.FinishedAt.IsZero() {
		t.Fatalf("positive follow-up inventory refused: %s/%s complete=%t count=%d", got.Publication.State, got.Reason, got.Complete, len(got.Findings))
	}
	finding := got.Findings[0]
	if finding.Kind != "outside_diff" || finding.ReviewID != 401 || finding.CommentID != 0 || finding.Path != "engine/events.go" || finding.Severity != "low" || finding.Title != "Keep the retained source description accurate." || finding.StartLine == nil || *finding.StartLine != 19 || finding.EndLine == nil || *finding.EndLine != 21 || finding.Fingerprint != fmt.Sprintf("%024x", 1) || finding.SourceKey != "hosted-review:401:outside_diff:000000000000000000000001" {
		t.Fatal("follow-up source identity/location/severity changed", finding)
	}
	item := followupOutsideItem(f.intent.HeadSHA, 1)
	if finding.Detail != item[:strings.Index(item, hfMarker(1))+len(hfMarker(1))] {
		t.Fatal("follow-up literal source bytes changed")
	}
	after, _ := json.Marshal(f.snapshot)
	if string(before) != string(after) {
		t.Fatal("source collection changed")
	}
}
func TestHostedFollowupOutsideUnknownOrAmbiguousInventoryStaysIncomplete(t *testing.T) {
	for name, change := range map[string]func(*hfFixture){
		"empty": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = publicationTestMarker(1, 1) + "\n## Follow-up Comments"
		},
		"missing heading": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "## Follow-up Comments", "", 1)
		},
		"quoted": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = publicationTestMarker(1, 1) + "\n> " + strings.ReplaceAll(f.snapshot.Reviews[0].Body, "\n", "\n> ")
		},
		"fenced": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = publicationTestMarker(1, 1) + "\n```\n" + f.snapshot.Reviews[0].Body + "\n```"
		},
		"missing marker": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, hfMarker(1), "", 1)
		},
		"missing source link": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "[Open the source location]", "[Unknown location]", 1)
		},
		"ambiguous source location": func(f *hfFixture) {
			link := "[Open the source location](https://github.com/synthetic/fixture/blob/" + f.intent.HeadSHA + "/engine/other.go#L19-L21)"
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, hfMarker(1), link+"\n"+hfMarker(1), 1)
		},
		"malformed source location": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "#L19-L21)", "#L19-L21?unexpected=1)", 1)
		},
		"ambiguous outer markers": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, hfMarker(1), hfMarker(1)+"\n"+hfMarker(2), 1)
		},
		"malformed fingerprint": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, hfMarker(1), "<!-- cr-comment:v1:unknown -->", 1)
		},
		"foreign source host": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "https://github.com/synthetic/fixture/blob/", "https://example.invalid/synthetic/fixture/blob/", 1)
		},
		"foreign source head": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "/blob/"+f.intent.HeadSHA, "/blob/"+strings.Repeat("b", 40), 1)
		},
		"invalid path": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "/engine/events.go#", "/../events.go#", 1)
		},
		"encoded invalid path": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "/engine/events.go#", "/%2e%2e/events.go#", 1)
		},
		"range mismatch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "#L19-L21)", "#L19-L22)", 1)
		},
		"invalid range": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "`19-21`:", "`21-19`:", 1)
		},
		"title mismatch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "**Keep the retained source description accurate.**", "**A different title.**", 1)
		},
		"missing severity": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "**🟡 Minor**", "**Unknown severity**", 1)
		},
		"unknown badge": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, `alt="Outside diff"`, `alt="Unknown inventory"`, 1)
		},
		"unbalanced block": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "</blockquote></details>", "</details>", 1)
		},
		"duplicate item":  func(f *hfFixture) { f.snapshot.Reviews[0].Body += "\n" + followupOutsideItem(f.intent.HeadSHA, 1) },
		"unknown trailer": func(f *hfFixture) { f.snapshot.Reviews[0].Body += "\nUnmarked additional finding." },
		"unknown nested scope": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "📝 Proposed wording", "Unknown new findings", 1)
		},
		"malformed count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: unknown**\n" + f.snapshot.Reviews[0].Body
		},
		"nonzero inline count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: 1**\n" + f.snapshot.Reviews[0].Body
		},
		"partial collection": func(f *hfFixture) { f.snapshot.Complete = false },
		"unknown roots":      func(f *hfFixture) { f.snapshot.ReviewComments = nil },
		"missing batch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
		},
		"foreign author":        func(f *hfFixture) { f.snapshot.Reviews[0].Author.ID++ },
		"foreign reviewed head": func(f *hfFixture) { f.snapshot.Reviews[0].HeadSHA = strings.Repeat("b", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			f := followupOutsideFixture(t)
			change(&f)
			before, _ := json.Marshal(f.snapshot)
			got := f.extract(t)
			if got.Complete {
				t.Fatal("unproven follow-up claimed completeness", got.Reason)
			}
			after, _ := json.Marshal(f.snapshot)
			if string(before) != string(after) {
				t.Fatal("refusal rewrote source")
			}
		})
	}
}

func TestHostedFollowupOutsideCountAndMultiplePositiveItems(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := followupOutsideFixture(t)
			if count == 0 {
				f.snapshot.Reviews[0].Body = "**Actionable comments posted: 0**\n" + f.snapshot.Reviews[0].Body
			} else if count == 2 {
				f.snapshot.Reviews[0].Body += "\n" + followupOutsideItem(f.intent.HeadSHA, 2)
			}
			got := f.extract(t)
			expected := 1
			if count == 2 {
				expected = 2
			}
			if !got.Complete || got.Publication.State != "attributed" || len(got.Findings) != expected {
				t.Fatal("positive findings/count contract changed", got.Publication.State, got.Reason, len(got.Findings))
			}
		})
	}
}
