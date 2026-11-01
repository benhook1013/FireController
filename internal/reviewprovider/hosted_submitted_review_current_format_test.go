package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

// Independently authored, minimal structural fixture; no captured project data.
func submittedCurrentBody(head string) string {
	icon := `<picture><img src="https://example.invalid/icon.svg" width="16" height="16" align="absmiddle" alt=""></picture>`
	dark := `<picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.svg"><img src="https://example.invalid/icon.svg" width="16" height="16" align="absmiddle" alt=""></picture>`
	outside := hfOutsideCategory(1, hfOutside(3, "engine/queue.go", "12-15", "Preserve the outside finding."))
	outside = strings.Replace(outside, "Some comments are outside the diff and are listed below.", "Some comments are outside the diff and can’t be posted inline due to GitHub limitations.", 1)
	return `<!-- coderabbit-ai-generated:v1 -->

**Actionable comments posted: 1**

` + outside + `

---

<!-- autofix_checkbox_start -->
- [ ] <!-- {"checkboxId":"44444444-4444-4444-8444-444444444444"} --> Fix comments: Fix CodeRabbit comments on this PR
<!-- autofix_checkbox_end -->

<details>
<summary>` + dark + ` Prompt to fix review comments</summary>
<dl>
<dd>

` + "```\nSynthetic prompt is data, not inventory.\n- **Run ID**: `99999999-9999-4999-8999-999999999999`\n```" + `

</dd>
</dl>
</details>

---

<details>
<summary>` + icon + ` Review info</summary>
<dl>
<dd>

<details>
<summary>` + dark + ` Run configuration</summary>
<dl>
<dd>

- **Source mode**: Auto
- **Repository**: synthetic/repository
- **Branch**: feature
- **File**: [settings](https://github.com/synthetic/repository/blob/` + head + `/.coderabbit.yaml)
- **Commit**: [` + head[:7] + `](https://github.com/synthetic/repository/commit/` + head + `)
- **Layers**: Repository
- **Review profile**: ASSERTIVE
- **Plan**: Advanced
- **Run ID**: ` + "`" + submittedTestRun + "`" + `

</dd>
</dl>
</details>

<details>
<summary>` + dark + ` Commits</summary>
<dl>
<dd>

` + hostedScopeDeclaration(strings.Repeat("b", 40), head) + `

</dd>
</dl>
</details>

<details>
<summary>` + dark + ` Files selected for processing: 2</summary>
<dl>
<dd>

* ` + "`engine/work.go`\n* `engine/queue.go`" + `

</dd>
</dl>
</details>

<details>
<summary>` + dark + ` Files with no reviewable changes: 1</summary>
<dl>
<dd>

* engine/queue.go

</dd>
</dl>
</details>

**Included review availability:** This review used your included allowance. Your plan provides up to 1 included review per hour; 0 remain after this review.

</dd>
</dl>
</details>

<!-- This is an auto-generated comment by CodeRabbit for review status -->`
}

func submittedCurrentFixture(t *testing.T) hfFixture {
	t.Helper()
	f := submittedTestFixture(t)
	f.snapshot.ReviewComments = f.snapshot.ReviewComments[:1]
	f.snapshot.Reviews[0].Body = submittedCurrentBody(f.intent.HeadSHA)
	return f
}

func TestHostedSubmittedReviewCurrentFormatRetainsBothInventories(t *testing.T) {
	for _, mode := range []string{"current", "CRLF", "no caution", "no no-reviewable block", "decorated checkbox"} {
		t.Run(mode, func(t *testing.T) {
			f := submittedCurrentFixture(t)
			body := &f.snapshot.Reviews[0].Body
			want := 2
			switch mode {
			case "CRLF":
				*body = strings.ReplaceAll(*body, "\n", "\r\n")
			case "no caution":
				start, end := strings.Index(*body, "> [!CAUTION]"), strings.Index(*body, "\n---")
				*body = (*body)[:start] + (*body)[end:]
				want = 1
			case "no no-reviewable block":
				start := strings.LastIndex(*body, "<details>\n<summary>")
				end := start + strings.Index((*body)[start:], "</details>") + len("</details>")
				*body = (*body)[:start] + (*body)[end:]
			case "decorated checkbox":
				*body = strings.Replace(*body, " --> Fix comments:", " --> 🪄 Fix comments:", 1)
			}
			before := fixtureJSON(t, f)
			got := f.extract(t)
			p := got.Publication
			if !got.Complete || got.Reason != "finding_inventory_complete" || len(got.Findings) != want || p.State != "attributed" || len(p.Roots) != 1 || p.SubmittedReview == nil || p.SubmittedReview.ProviderRunID != submittedTestRun || p.SubmittedReview.Acknowledgement.ID != 302 || p.FinishedAt != f.snapshot.IssueComments[1].UpdatedAt || p.StandingZero != nil || p.PublicationID != "" || p.ProviderAttemptID != "" {
				t.Fatalf("original positive inventory lost: complete=%v reason=%s count=%d", got.Complete, got.Reason, len(got.Findings))
			}
			if !reflect.DeepEqual(before, fixtureJSON(t, f)) || p.Batches[0].Body != *body {
				t.Fatal("parser rewrote original source")
			}
			if got.Findings[0].Kind != "inline" || got.Findings[0].CommentID != 501 || got.Findings[0].ReviewID != 401 || got.Findings[0].Severity != "low" {
				t.Fatal("inline attribution changed")
			}
			if want == 2 && (got.Findings[1].Kind != "outside_diff" || got.Findings[1].ReviewID != 401 || got.Findings[1].CommentID != 0 || got.Findings[1].Path != "engine/queue.go" || got.Findings[1].Severity != "low" || !strings.Contains(*body, got.Findings[1].Detail)) {
				t.Fatal("outside finding or exact detail lost")
			}
			if base, ok := ResolveHostedComparison(p, f.intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("c", 40)); !ok || base != strings.Repeat("b", 40) {
				t.Fatal("original comparison changed")
			}
		})
	}
}

func TestHostedSubmittedReviewCurrentFormatRejectsUnknownOrIncomplete(t *testing.T) {
	for name, mutate := range map[string]func(*hfFixture){
		"truncated": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.TrimSuffix(f.snapshot.Reviews[0].Body, "<!-- This is an auto-generated comment by CodeRabbit for review status -->")
		},
		"missing wrapper close": func(f *hfFixture) {
			run := "- **Run ID**: `" + submittedTestRun + "`\n\n"
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, run+"</dd>", run, 1)
		},
		"unknown decoration": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "height=\"16\" align", "height=\"16\" unknown=\"yes\" align", 1)
		},
		"unknown source mode": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Source mode**: Auto", "Source mode**: Unknown", 1)
		},
		"wrong configuration head": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "/blob/"+f.intent.HeadSHA, "/blob/"+strings.Repeat("d", 40), 1)
		},
		"wrong comparison": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, hostedScopeDeclaration(strings.Repeat("b", 40), f.intent.HeadSHA), hostedScopeDeclaration(strings.Repeat("b", 40), strings.Repeat("d", 40)), 1)
		},
		"outside count mismatch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Outside diff range comments (1)", "Outside diff range comments (2)", 1)
		},
		"outside fingerprint reused": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, hfMarker(3), hfMarker(1), 1)
		},
		"unquoted caution": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "> [!CAUTION]", "[!CAUTION]", 1)
		},
		"selected count mismatch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Files selected for processing: 2", "Files selected for processing: 3", 1)
		},
		"unknown no-reviewable path": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "* engine/queue.go", "* engine/foreign.go", 1)
		},
		"duplicate no-reviewable path": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "* engine/queue.go", "* engine/queue.go\n* engine/queue.go", 1)
		},
		"no-reviewable count mismatch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Files with no reviewable changes: 1", "Files with no reviewable changes: 2", 1)
		},
		"unknown checkbox": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, " --> Fix comments:", " --> Unknown action:", 1)
		},
		"quoted real run": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "- **Run ID**: `"+submittedTestRun+"`", "> - **Run ID**: `"+submittedTestRun+"`", 1)
		},
		"incomplete source":   func(f *hfFixture) { f.snapshot.Complete = false },
		"wrong provider":      func(f *hfFixture) { f.snapshot.Reviews[0].Author.ID++ },
		"wrong head":          func(f *hfFixture) { f.snapshot.Reviews[0].HeadSHA = strings.Repeat("d", 40) },
		"missing inline root": func(f *hfFixture) { f.snapshot.ReviewComments = nil },
		"ambiguous review": func(f *hfFixture) {
			r := f.snapshot.Reviews[0]
			r.ID++
			f.snapshot.Reviews = append(f.snapshot.Reviews, r)
		},
		"wrong finished actor": func(f *hfFixture) { f.snapshot.IssueComments[1].Author.ID++ },
	} {
		t.Run(name, func(t *testing.T) {
			f := submittedCurrentFixture(t)
			mutate(&f)
			before := fixtureJSON(t, f)
			got := f.extract(t)
			if got.Complete || !reflect.DeepEqual(before, fixtureJSON(t, f)) {
				t.Fatal("unsupported facts gained complete inventory or source changed", got.Reason)
			}
		})
	}
}
