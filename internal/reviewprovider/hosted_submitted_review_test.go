package reviewprovider

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const submittedTestRun = "33333333-3333-4333-8333-333333333333"

// Independently authored structural fixture: no live provider IDs, paths or text.
func submittedTestBody(base, head string) string {
	return `<!-- coderabbit-ai-generated:v1 -->

**Actionable comments posted: 2**

---

<!-- autofix_checkbox_start -->
- [ ] <!-- {"checkboxId":"44444444-4444-4444-8444-444444444444"} --> 🪄 Fix comments: Fix CodeRabbit comments on this PR
<!-- autofix_checkbox_end -->

<details>
<summary>🤖 Prompt to fix review comments</summary>

` + "```\nSynthetic instructions are retained as data.\n- **Run ID**: `99999999-9999-4999-8999-999999999999`\n**Actionable comments posted: 999**\n```" + `

</details>

---

<details>
<summary>ℹ️ Review info</summary>

<details>
<summary>⚙️ Run configuration</summary>

- **Configuration used**: Repository: synthetic/repository/.coderabbit.yaml
- **Review profile**: ASSERTIVE
- **Plan**: Advanced
- **Run ID**: ` + "`" + submittedTestRun + "`" + `

</details>

<details>
<summary>📥 Commits</summary>

` + hostedScopeDeclaration(base, head) + `

</details>

<details>
<summary>📒 Files selected for processing: 2</summary>

* ` + "`engine/work.go`\n* `engine/queue.go`" + `

</details>

**Included review availability:** This review used your included allowance. Your plan provides up to 1 included review per hour; 0 remain after this review.

</details>

<!-- This is an auto-generated comment by CodeRabbit for review status -->`
}

func submittedTestFixture(t *testing.T) hfFixture {
	t.Helper()
	root := func(category, severity, title string, marker int) string {
		return "<!-- coderabbit-ai-generated:v1 -->\n\n**" + category + "** | **" + severity + "** | **⚡ Quick win**\n\n**" + title + "**\n\nIndependent synthetic rationale.\n\n" + hfMarker(marker) + "\n\n<!-- This is an auto-generated comment by CodeRabbit -->"
	}
	f := hfNew(t, []string{root("🔒 Security & Privacy", "🟡 Minor", "Validate the retained input.", 1), root("🗄️ Data Integrity & Integration", "🔵 Trivial", "Preserve the response identity.", 2)}, "")
	trigger := f.snapshot.IssueComments[0].CreatedAt
	f.snapshot.Reviews[0].Body = submittedTestBody(strings.Repeat("b", 40), f.intent.HeadSHA)
	f.snapshot.Reviews[0].SubmittedAt = trigger.Add(2 * time.Second)
	f.snapshot.ReviewComments[1].Path = "engine/queue.go"
	line := int64(7)
	for i := range f.snapshot.ReviewComments {
		f.snapshot.ReviewComments[i].Line = &line
		f.snapshot.ReviewComments[i].OriginalLine = &line
	}
	f.snapshot.IssueComments = append(f.snapshot.IssueComments, IssueComment{ID: 302, Author: f.bot, Body: "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("a", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\n\nFull review finished.\n\n</details>", CreatedAt: trigger.Add(time.Second), UpdatedAt: trigger.Add(3 * time.Second)})
	f.snapshot.ObservedAt = trigger.Add(4 * time.Second)
	return f
}

func TestHostedSubmittedReviewPositiveInventoryAndRealMetadata(t *testing.T) {
	f := submittedTestFixture(t)
	before := f.snapshot.Reviews[0].Body
	got := f.extract(t)
	p := got.Publication
	if !got.Complete || got.Reason != "finding_inventory_complete" || len(got.Findings) != 2 || p.State != "attributed" || p.SubmittedReview == nil || p.SubmittedReview.ProviderRunID != submittedTestRun || p.SubmittedReview.Acknowledgement.ID != 302 || p.PublicationID != "" || p.ProviderAttemptID != "" || p.StandingZero != nil || len(p.Batches) != 1 || p.Batches[0].ID != 401 || len(p.Roots) != 2 || p.FinishedAt != f.snapshot.IssueComments[1].UpdatedAt {
		t.Fatalf("submitted review lost real provenance/inventory: %+v", got)
	}
	if p.Batches[0].Body != before || f.snapshot.Reviews[0].Body != before || p.SubmittedReview.Acknowledgement != f.snapshot.IssueComments[1] {
		t.Fatal("resolver rewrote retained review/ACK")
	}
	for i, want := range []struct{ title, severity string }{{"Validate the retained input.", "low"}, {"Preserve the response identity.", "info"}} {
		finding := got.Findings[i]
		rootBody := f.snapshot.ReviewComments[i].Body
		marker := hfMarker(i + 1)
		wantDetail := rootBody[:strings.Index(rootBody, marker)+len(marker)]
		if finding.Kind != "inline" || finding.Title != want.title || finding.Severity != want.severity || finding.CommentID != int64(501+i) || finding.ReviewID != 401 || finding.SourceKey != fmt.Sprintf("hosted-comment:%d:%024x", 501+i, i+1) || finding.Detail != wantDetail || p.Roots[i].Body != rootBody {
			t.Fatal("actual-shaped inline fields were not retained", finding)
		}
	}
	if base, ok := ResolveHostedComparison(p, f.intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("c", 40)); !ok || base != strings.Repeat("b", 40) {
		t.Fatal("explicit comparison lost", base, ok)
	}
	*p.Roots[0].Line = 99
	if *f.snapshot.ReviewComments[0].Line != 7 {
		t.Fatal("root location aliases caller memory")
	}
}

func TestHostedSubmittedReviewUnsafeEvidenceNeverCredits(t *testing.T) {
	runLine := "- **Run ID**: `" + submittedTestRun + "`"
	for _, tc := range []struct {
		name   string
		mutate func(*hfFixture)
	}{
		{"incomplete", func(f *hfFixture) { f.snapshot.Complete = false }},
		{"missing-generated-envelope", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.TrimPrefix(f.snapshot.Reviews[0].Body, "<!-- coderabbit-ai-generated:v1 -->\n")
		}},
		{"quoted-prompt-header", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "<summary>🤖 Prompt to fix review comments</summary>", "> <summary>🤖 Prompt to fix review comments</summary>", 1)
		}},
		{"unknown-envelope", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "coderabbit-ai-generated:v1", "coderabbit-ai-generated:v2", 1)
		}},
		{"missing-count", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "**Actionable comments posted: 2**", "", 1)
		}},
		{"quoted-count", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "**Actionable comments posted: 2**", "> **Actionable comments posted: 2**", 1)
		}},
		{"fenced-count", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "**Actionable comments posted: 2**", "```\n**Actionable comments posted: 2**\n```", 1)
		}},
		{"zero-count", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "**Actionable comments posted: 2**", "**Actionable comments posted: 0**", 1)
		}},
		{"contradictory-count", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "**Actionable comments posted: 2**", "**Actionable comments posted: 2**\n**Actionable comments posted: 1**", 1)
		}},
		{"missing-run", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, runLine, "", 1)
		}},
		{"quoted-run", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, runLine, "> "+runLine, 1)
		}},
		{"fenced-run", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, runLine, "```\n"+runLine+"\n```", 1)
		}},
		{"pre-run", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, runLine, "<pre>\n"+runLine+"\n</pre>", 1)
		}},
		{"run-outside-metadata", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, runLine, "", 1) + "\n" + runLine
		}},
		{"unknown-config-scope", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "⚙️ Run configuration", "Unknown metadata", 1)
		}},
		{"wrong-actor", func(f *hfFixture) { f.snapshot.Reviews[0].Author.ID++ }},
		{"wrong-actor-kind", func(f *hfFixture) { f.snapshot.Reviews[0].Author.Kind = "User" }},
		{"wrong-head", func(f *hfFixture) { f.snapshot.Reviews[0].HeadSHA = strings.Repeat("d", 40) }},
		{"wrong-comparison-head", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, f.intent.HeadSHA, strings.Repeat("d", 40), 1)
		}},
		{"missing-comparison", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, hostedScopeDeclaration(strings.Repeat("b", 40), f.intent.HeadSHA), "", 1)
		}},
		{"review-at-trigger", func(f *hfFixture) { f.snapshot.Reviews[0].SubmittedAt = f.snapshot.IssueComments[0].CreatedAt }},
		{"review-before-trigger", func(f *hfFixture) {
			f.snapshot.Reviews[0].SubmittedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Second)
		}},
		{"multiple-eligible", func(f *hfFixture) {
			other := f.snapshot.Reviews[0]
			other.ID++
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		}},
		{"foreign-head-publication", func(f *hfFixture) {
			other := f.snapshot.Reviews[0]
			other.ID++
			other.HeadSHA = strings.Repeat("d", 40)
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		}},
		{"pending-other-head", func(f *hfFixture) {
			other := f.snapshot.Reviews[0]
			other.ID++
			other.HeadSHA = strings.Repeat("d", 40)
			other.State = "pending"
			other.SubmittedAt = time.Time{}
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		}},
		{"missing-ack", func(f *hfFixture) { f.snapshot.IssueComments = f.snapshot.IssueComments[:1] }},
		{"wrong-ack-actor", func(f *hfFixture) { f.snapshot.IssueComments[1].Author.ID++ }},
		{"ack-before-review", func(f *hfFixture) { f.snapshot.IssueComments[1].UpdatedAt = f.snapshot.IssueComments[1].CreatedAt }},
		{"ack-at-review-submission", func(f *hfFixture) { f.snapshot.IssueComments[1].UpdatedAt = f.snapshot.Reviews[0].SubmittedAt }},
		{"ack-created-before-trigger", func(f *hfFixture) {
			f.snapshot.IssueComments[1].CreatedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Second)
		}},
		{"duplicate-ack", func(f *hfFixture) {
			ack := f.snapshot.IssueComments[1]
			ack.ID++
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, ack)
		}},
		{"unknown-fresh-provider-reply", func(f *hfFixture) {
			ack := f.snapshot.IssueComments[1]
			ack.ID++
			ack.Body = "Unknown review result"
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, ack)
		}},
		{"competing-command", func(f *hfFixture) {
			c := f.snapshot.IssueComments[0]
			c.ID = 303
			c.Body = "@coderabbitai full review"
			c.CreatedAt = c.CreatedAt.Add(1500 * time.Millisecond)
			c.UpdatedAt = c.CreatedAt
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, c)
		}},
		{"command-before-ack", func(f *hfFixture) {
			c := f.snapshot.IssueComments[0]
			c.ID = 303
			c.Body = "@coderabbitai review"
			c.CreatedAt = c.CreatedAt.Add(2500 * time.Millisecond)
			c.UpdatedAt = c.CreatedAt
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, c)
		}},
		{"unknown-reply-edited-after-closing-command", func(f *hfFixture) {
			c := f.snapshot.IssueComments[0]
			c.ID, c.Body = 303, "@coderabbitai review"
			c.CreatedAt = c.CreatedAt.Add(3500 * time.Millisecond)
			c.UpdatedAt = c.CreatedAt
			unknown := f.snapshot.IssueComments[1]
			unknown.ID, unknown.Body = 304, "Unknown fresh publication"
			unknown.UpdatedAt = f.snapshot.ObservedAt
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, c, unknown)
		}},
		{"missing-root", func(f *hfFixture) { f.snapshot.ReviewComments = f.snapshot.ReviewComments[:1] }},
		{"declared-root-count-mismatch", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "**Actionable comments posted: 2**", "**Actionable comments posted: 1**", 1)
		}},
		{"root-wrong-actor", func(f *hfFixture) { f.snapshot.ReviewComments[0].Author.ID++ }},
		{"root-wrong-original-head", func(f *hfFixture) { f.snapshot.ReviewComments[0].OriginalHeadSHA = strings.Repeat("d", 40) }},
		{"root-before-trigger", func(f *hfFixture) {
			f.snapshot.ReviewComments[0].CreatedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Second)
		}},
		{"root-after-submission", func(f *hfFixture) {
			f.snapshot.ReviewComments[0].CreatedAt = f.snapshot.IssueComments[1].UpdatedAt
			f.snapshot.ReviewComments[0].UpdatedAt = f.snapshot.IssueComments[1].UpdatedAt
		}},
		{"root-edited-after-submission", func(f *hfFixture) { f.snapshot.ReviewComments[0].UpdatedAt = f.snapshot.IssueComments[1].UpdatedAt }},
		{"empty-root", func(f *hfFixture) { f.snapshot.ReviewComments[0].Body = "" }},
		{"unattached-root", func(f *hfFixture) {
			c := f.snapshot.ReviewComments[0]
			c.ID = 503
			c.ReviewID = 999
			f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, c)
		}},
		{"unselected-root-path", func(f *hfFixture) { f.snapshot.ReviewComments[0].Path = "engine/unknown.go" }},
		{"wrong-selected-count", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Files selected for processing: 2", "Files selected for processing: 3", 1)
		}},
		{"duplicate-selected-path", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "* `engine/queue.go`", "* `engine/work.go`", 1)
		}},
		{"unsafe-selected-path", func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "* `engine/queue.go`", "* `../unsafe.go`", 1)
		}},
		{"unknown-trailer", func(f *hfFixture) { f.snapshot.Reviews[0].Body += "\nUnknown inventory" }},
		{"malformed-explicit-marker", func(f *hfFixture) { f.snapshot.Reviews[0].Body += "\n<!-- coderabbit-review-publication malformed -->" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := submittedTestFixture(t)
			tc.mutate(&f)
			got := f.extract(t)
			if got.Complete || got.Publication.SubmittedReview != nil {
				t.Fatalf("unsafe evidence credited: %+v", got)
			}
		})
	}
}

func TestHostedSubmittedReviewRunReuseAndSeparateComparison(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprint("reuse-", reuse), func(t *testing.T) {
			f := submittedTestFixture(t)
			old := f.snapshot.Reviews[0]
			old.ID++
			old.SubmittedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Second)
			if !reuse {
				old.Body = strings.Replace(old.Body, submittedTestRun, "55555555-5555-4555-8555-555555555555", 1)
				old.Body = strings.Replace(old.Body, "99999999-9999-4999-8999-999999999999", submittedTestRun, 1)
			}
			f.snapshot.Reviews = append(f.snapshot.Reviews, old)
			got := f.extract(t)
			if got.Complete == reuse || (got.Publication.SubmittedReview != nil) == reuse {
				t.Fatal("run reuse/example distinction lost", reuse, got.Reason)
			}
		})
	}
	t.Run("visible-reuse-in-unknown-old-envelope", func(t *testing.T) {
		f := submittedTestFixture(t)
		old := f.snapshot.Reviews[0]
		old.ID++
		old.SubmittedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Second)
		old.Body = "Unknown older envelope\n- **Run ID**: `" + submittedTestRun + "`"
		f.snapshot.Reviews = append(f.snapshot.Reviews, old)
		if got := f.extract(t); got.Complete || got.Publication.SubmittedReview != nil {
			t.Fatal("visible ambiguous run reuse credited", got.Reason)
		}
	})
	for _, older := range []bool{false, true} {
		t.Run(fmt.Sprint("standing-copy-older-", older), func(t *testing.T) {
			f := submittedTestFixture(t)
			standing := f.snapshot.IssueComments[1]
			standing.ID = 303
			standing.CreatedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Hour)
			standing.Body = strings.Replace(standingTestBody(f.intent.HeadSHA, strings.Repeat("b", 40)), standingTestRun, submittedTestRun, 1)
			if parsed, ok := parseStandingZero(standing.Body); !ok || parsed.ProviderRunID != submittedTestRun {
				t.Fatal("fixture did not establish supported standing run")
			}
			if older {
				standing.UpdatedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Second)
			}
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, standing)
			got := f.extract(t)
			if got.Complete == older || (got.Publication.SubmittedReview != nil) == older {
				t.Fatal("old run reuse/current standing distinction lost", older, got.Reason)
			}
		})
	}
	f := submittedTestFixture(t)
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, strings.Repeat("b", 40), strings.Repeat("d", 40), 1)
	got := f.extract(t)
	if !got.Complete || got.Publication.SubmittedReview == nil {
		t.Fatal("syntactic inventory lost", got.Reason)
	}
	if _, ok := ResolveHostedComparison(got.Publication, f.intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("c", 40)); ok {
		t.Fatal("submitted inventory bypassed comparison authority")
	}
}

func TestHostedSubmittedReviewPreservesExistingExplicitMarkers(t *testing.T) {
	f := submittedTestFixture(t)
	f.snapshot.Reviews[0].Body += "\n" + publicationTestMarker(1, 1)
	got := f.extract(t)
	if !got.Complete || got.Publication.SubmittedReview != nil || got.Publication.PublicationID != publicationTestID || got.Publication.ProviderAttemptID != publicationAttemptID {
		t.Fatal("explicit format changed", got.Reason)
	}
}
