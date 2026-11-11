package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Sanitized structural fixture from the public additional-only submission:
// https://github.com/benhook1013/FireController/pull/76#pullrequestreview-5849632157
// https://github.com/benhook1013/FireController/pull/76#issuecomment-6779242468
// Repository, branch, hashes, run/invocation IDs, paths, coordinates and finding
// prose are synthetic. Provider envelope, count 77, icons and layout are retained.
const submittedAdditionalBody = "<!-- coderabbit-ai-generated:v1 -->\n" +
	"\n" +
	"\n" +
	"\n" +
	"<details>\n" +
	"<summary><picture><source media=\"(prefers-color-scheme: dark)\" srcset=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/message-square-off-dark.svg\"><img src=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/message-square-off.svg\" width=\"16\" height=\"16\" align=\"absmiddle\" alt=\"\"></picture> Additional comments: 1</summary><blockquote>\n" +
	"<dl>\n" +
	"<dd>\n" +
	"<br>\n" +
	"\n" +
	"<details>\n" +
	"<summary>docs/example.md-24-24: 1</summary><blockquote>\n" +
	"<dl>\n" +
	"<dd>\n" +
	"<br>\n" +
	"\n" +
	"`24-24`: **<picture><img src=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/neutral-16.svg\" width=\"16\" height=\"16\" align=\"absmiddle\" alt=\"\"></picture> Trivial** · **Maintainability & Code Quality** · **Low value**\n" +
	"\n" +
	"<picture><img src=\"https://storage.googleapis.com/coderabbit_public_assets/pre-merge-checks-warning.svg\" width=\"20\" height=\"20\" align=\"absmiddle\" alt=\"\"></picture> **Unverified finding**\n" +
	"Verification ran but could not confirm this finding. It is shown for review, not as a verified issue.\n" +
	"\n" +
	"**Consider the optional example wording.**\n" +
	"\n" +
	"The synthetic example text may be clearer with an optional wording change. This observation remains unverified.\n" +
	"\n" +
	"_Source: Linters/SAST tools_\n" +
	"\n" +
	"</dd>\n" +
	"</dl>\n" +
	"</blockquote></details>\n" +
	"\n" +
	"</dd>\n" +
	"</dl>\n" +
	"</blockquote></details>\n" +
	"\n" +
	"<h6>\n" +
	"<details>\n" +
	"<summary><picture><img src=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/info.svg\" width=\"12\" height=\"12\" align=\"absmiddle\" alt=\"\"></picture> Review info</summary>\n" +
	"<dl>\n" +
	"<dd>\n" +
	"<br>\n" +
	"\n" +
	"<details>\n" +
	"<summary><picture><source media=\"(prefers-color-scheme: dark)\" srcset=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/cog-dark.svg\"><img src=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/cog.svg\" width=\"16\" height=\"16\" align=\"absmiddle\" alt=\"\"></picture> Run configuration</summary>\n" +
	"<dl>\n" +
	"<dd>\n" +
	"<br>\n" +
	"\n" +
	"- **Source mode**: Auto\n" +
	"- **Repository**: [example/project](<https://github.com/example/project>)\n" +
	"- **Branch**: [feature/example](<https://github.com/example/project/tree/feature%2Fexample>)\n" +
	"- **File**: [.coderabbit.yaml](<https://github.com/example/project/blob/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/.coderabbit.yaml>)\n" +
	"- **Commit**: [aaaaaaa](<https://github.com/example/project/commit/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa>)\n" +
	"- **Layers**: Repository YAML\n" +
	"- **Review profile**: ASSERTIVE\n" +
	"- **Plan**: Advanced\n" +
	"- **Run ID**: `33333333-3333-4333-8333-333333333333`\n" +
	"\n" +
	"</dd>\n" +
	"</dl>\n" +
	"</details>\n" +
	"\n" +
	"<details>\n" +
	"<summary><picture><source media=\"(prefers-color-scheme: dark)\" srcset=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/git-commit-horizontal-dark.svg\"><img src=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/git-commit-horizontal.svg\" width=\"16\" height=\"16\" align=\"absmiddle\" alt=\"\"></picture> Commits</summary>\n" +
	"<dl>\n" +
	"<dd>\n" +
	"<br>\n" +
	"\n" +
	"Reviewing files that changed from the base of the PR and between bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb and aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.\n" +
	"\n" +
	"</dd>\n" +
	"</dl>\n" +
	"</details>\n" +
	"\n" +
	"<details>\n" +
	"<summary><picture><source media=\"(prefers-color-scheme: dark)\" srcset=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/file-plus-corner-dark.svg\"><img src=\"https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/file-plus-corner.svg\" width=\"16\" height=\"16\" align=\"absmiddle\" alt=\"\"></picture> Files selected for processing: 77</summary>\n" +
	"<dl>\n" +
	"<dd>\n" +
	"<br>\n" +
	"\n" +
	"* `source/selected01.go`\n" +
	"* `source/selected02.go`\n" +
	"* `source/selected03.go`\n" +
	"* `source/selected04.go`\n" +
	"* `source/selected05.go`\n" +
	"* `source/selected06.go`\n" +
	"* `source/selected07.go`\n" +
	"* `source/selected08.go`\n" +
	"* `source/selected09.go`\n" +
	"* `docs/example.md`\n" +
	"* `source/selected10.go`\n" +
	"* `source/selected11.go`\n" +
	"* `source/selected12.go`\n" +
	"* `source/selected13.go`\n" +
	"* `source/selected14.go`\n" +
	"* `source/selected15.go`\n" +
	"* `source/selected16.go`\n" +
	"* `source/selected17.go`\n" +
	"* `source/selected18.go`\n" +
	"* `source/selected19.go`\n" +
	"* `source/selected20.go`\n" +
	"* `source/selected21.go`\n" +
	"* `source/selected22.go`\n" +
	"* `source/selected23.go`\n" +
	"* `source/selected24.go`\n" +
	"* `source/selected25.go`\n" +
	"* `source/selected26.go`\n" +
	"* `source/selected27.go`\n" +
	"* `source/selected28.go`\n" +
	"* `source/selected29.go`\n" +
	"* `source/selected30.go`\n" +
	"* `source/selected31.go`\n" +
	"* `source/selected32.go`\n" +
	"* `source/selected33.go`\n" +
	"* `source/selected34.go`\n" +
	"* `source/selected35.go`\n" +
	"* `source/selected36.go`\n" +
	"* `source/selected37.go`\n" +
	"* `source/selected38.go`\n" +
	"* `source/selected39.go`\n" +
	"* `source/selected40.go`\n" +
	"* `source/selected41.go`\n" +
	"* `source/selected42.go`\n" +
	"* `source/selected43.go`\n" +
	"* `source/selected44.go`\n" +
	"* `source/selected45.go`\n" +
	"* `source/selected46.go`\n" +
	"* `source/selected47.go`\n" +
	"* `source/selected48.go`\n" +
	"* `source/selected49.go`\n" +
	"* `source/selected50.go`\n" +
	"* `source/selected51.go`\n" +
	"* `source/selected52.go`\n" +
	"* `source/selected53.go`\n" +
	"* `source/selected54.go`\n" +
	"* `source/selected55.go`\n" +
	"* `source/selected56.go`\n" +
	"* `source/selected57.go`\n" +
	"* `source/selected58.go`\n" +
	"* `source/selected59.go`\n" +
	"* `source/selected60.go`\n" +
	"* `source/selected61.go`\n" +
	"* `source/selected62.go`\n" +
	"* `source/selected63.go`\n" +
	"* `source/selected64.go`\n" +
	"* `source/selected65.go`\n" +
	"* `source/selected66.go`\n" +
	"* `source/selected67.go`\n" +
	"* `source/selected68.go`\n" +
	"* `source/selected69.go`\n" +
	"* `source/selected70.go`\n" +
	"* `source/selected71.go`\n" +
	"* `source/selected72.go`\n" +
	"* `source/selected73.go`\n" +
	"* `source/selected74.go`\n" +
	"* `source/selected75.go`\n" +
	"* `source/selected76.go`\n" +
	"\n" +
	"</dd>\n" +
	"</dl>\n" +
	"</details>\n" +
	"\n" +
	"**Included review availability:** This review used your included allowance. Your plan provides up to 1 included review per hour; 0 remain after this review.\n" +
	"\n" +
	"</dd>\n" +
	"</dl>\n" +
	"</details>\n" +
	"</h6>\n" +
	"\n" +
	"<!-- This is an auto-generated comment by CodeRabbit for review status -->"

const submittedAdditionalAck = "<!-- coderabbit-ai-generated:v1 -->\n" +
	"\n" +
	"<!-- This is an auto-generated reply by CodeRabbit -->\n" +
	"<!-- CodeRabbit review command invocation: v2:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa -->\n" +
	"<details>\n" +
	"<summary>Action performed</summary>\n" +
	"<dl>\n" +
	"<dd>\n" +
	"<br>\n" +
	"\n" +
	"Full review finished.\n" +
	"\n" +
	"</dd>\n" +
	"</dl>\n" +
	"</details>"

func submittedAdditionalFixture(t *testing.T) hfFixture {
	t.Helper()
	f := submittedTestFixture(t)
	f.snapshot.Reviews[0].Body = submittedAdditionalBody
	f.snapshot.ReviewComments = []ReviewComment{}
	f.snapshot.IssueComments[1].Body = submittedAdditionalAck
	return f
}

func TestHostedSubmittedAdditionalOnlyPreservesPositiveInventory(t *testing.T) {
	for _, crlf := range []bool{false, true} {
		name := "LF"
		if crlf {
			name = "CRLF"
		}
		t.Run(name, func(t *testing.T) {
			f := submittedAdditionalFixture(t)
			if crlf {
				f.snapshot.Reviews[0].Body = strings.ReplaceAll(f.snapshot.Reviews[0].Body, "\n", "\r\n")
			}
			before, _ := json.Marshal(f.snapshot)
			got := f.extract(t)
			p := got.Publication
			if !got.Complete || got.Reason != "finding_inventory_complete" || p.State != "attributed" || p.SubmittedReview == nil || p.SubmittedReview.ProviderRunID != submittedTestRun || p.SubmittedReview.Acknowledgement != f.snapshot.IssueComments[1] || p.PublicationID != "" || p.ProviderAttemptID != "" || p.StandingZero != nil || len(p.Batches) != 1 || p.Batches[0].ID != 401 || len(p.Roots) != 0 || !p.FinishedAt.Equal(f.snapshot.IssueComments[1].UpdatedAt) || len(got.Findings) != 1 {
				t.Fatalf("additional submission lost positive inventory or real provenance: %+v", got)
			}
			finding := got.Findings[0]
			if finding.Kind != "additional" || finding.ReviewID != 401 || finding.CommentID != 0 || finding.Severity != "info" || finding.Path != "docs/example.md" || finding.StartLine == nil || *finding.StartLine != 24 || finding.EndLine == nil || *finding.EndLine != 24 || finding.Title != "Consider the optional example wording." || !strings.Contains(finding.Detail, "Unverified finding") || !strings.Contains(finding.Detail, "This observation remains unverified.") || !strings.HasPrefix(finding.SourceKey, "hosted-review:401:additional:") {
				t.Fatal("additional finding lost its original identity, location or qualification", finding)
			}
			if !strings.Contains(p.Batches[0].Body, finding.Detail) || p.Batches[0].Body != f.snapshot.Reviews[0].Body {
				t.Fatal("review source was rewritten")
			}
			after, _ := json.Marshal(f.snapshot)
			if string(before) != string(after) {
				t.Fatal("source snapshot changed")
			}
			if again := f.extract(t); len(again.Findings) != 1 || again.Findings[0].SourceKey != finding.SourceKey {
				t.Fatal("exact replay changed source identity")
			}
			if base, ok := ResolveHostedComparison(p, f.intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("c", 40)); !ok || base != strings.Repeat("b", 40) {
				t.Fatal("exact comparison was lost", base, ok)
			}
			if _, ok := ResolveHostedComparison(p, f.intent.HeadSHA, strings.Repeat("c", 40), strings.Repeat("d", 40)); ok {
				t.Fatal("unrelated comparison credited")
			}
		})
	}
}

func TestHostedSubmittedAdditionalOnlyRejectsUnprovenBody(t *testing.T) {
	replace := func(old, next string) func(string) string {
		return func(body string) string { return strings.Replace(body, old, next, 1) }
	}
	cases := map[string]func(string) string{
		"generated envelope missing":  replace("<!-- coderabbit-ai-generated:v1 -->", ""),
		"unknown generated envelope":  replace("coderabbit-ai-generated:v1", "coderabbit-ai-generated:v2"),
		"zero category":               replace("Additional comments: 1", "Additional comments: 0"),
		"category count mismatch":     replace("Additional comments: 1", "Additional comments: 2"),
		"category overflow":           replace("Additional comments: 1", "Additional comments: 9999999999999999999999999"),
		"unknown category":            replace("Additional comments: 1", "Other comments: 1"),
		"group count mismatch":        replace("docs/example.md-24-24: 1", "docs/example.md-24-24: 2"),
		"group range mismatch":        replace("docs/example.md-24-24: 1", "docs/example.md-25-25: 1"),
		"zero location":               replace("`24-24`:", "`0-24`:"),
		"reversed location":           replace("`24-24`:", "`25-24`:"),
		"path not selected":           replace("<summary>docs/example.md-24-24: 1", "<summary>docs/absent.md-24-24: 1"),
		"unsafe path":                 replace("<summary>docs/example.md-24-24: 1", "<summary>../example.md-24-24: 1"),
		"missing title":               replace("**Consider the optional example wording.**", "Consider the optional example wording."),
		"unbalanced wrappers":         replace("</dd>\n</dl>", "</dl>\n</dd>"),
		"duplicate bare break":        replace("<dd>\n<br>", "<dd>\n<br>\n<br>"),
		"misplaced break":             replace("_Source: Linters/SAST tools_", "_Source: Linters/SAST tools_\n<br>"),
		"missing h6":                  replace("<h6>\n", ""),
		"unknown h6":                  replace("<h6>\n", "<h5>\n"),
		"missing status footer":       replace("<!-- This is an auto-generated comment by CodeRabbit for review status -->", ""),
		"unknown trailer":             func(body string) string { return body + "\nUnknown findings: 1" },
		"unknown prefix":              func(body string) string { return "Unknown results\n" + body },
		"quoted envelope":             func(body string) string { return "> " + strings.ReplaceAll(body, "\n", "\n> ") },
		"fenced envelope":             func(body string) string { return "```\n" + body + "\n```" },
		"additional block in prompt":  replace("<details>\n", "<details>\n<summary>🤖 Prompt to fix review comments</summary>\n<details>\n"),
		"positive actionable claim":   replace("<!-- coderabbit-ai-generated:v1 -->", "<!-- coderabbit-ai-generated:v1 -->\n**Actionable comments posted: 1**"),
		"zero actionable claim":       replace("<!-- coderabbit-ai-generated:v1 -->", "<!-- coderabbit-ai-generated:v1 -->\n**Actionable comments posted: 0**"),
		"invalid actionable claim":    replace("<!-- coderabbit-ai-generated:v1 -->", "<!-- coderabbit-ai-generated:v1 -->\n**Actionable comments posted: unknown**"),
		"additional actionable claim": replace("_Source: Linters/SAST tools_", "**Actionable comments posted: 1**\n_Source: Linters/SAST tools_"),
		"empty additional": func(body string) string {
			start := strings.Index(body, "<details>")
			end := strings.Index(body, "<h6>")
			return body[:start] + "<details>\n<summary>🔇 Additional comments: 1</summary><blockquote>\n<dl>\n<dd>\n</dd>\n</dl>\n</blockquote></details>\n\n" + body[end:]
		},
		"duplicate category": func(body string) string {
			start := strings.Index(body, "<details>")
			end := strings.Index(body, "<h6>")
			return body[:end] + body[start:end] + body[end:]
		},
		"mixed outside category":        replace("<h6>", hfOutsideCategory(1, hfOutside(9, "docs/example.md", "7", "Unproven mixed result."))+"\n<h6>"),
		"mixed duplicate category":      replace("<h6>", hfDuplicateCategory(1, hfDuplicateGroup("docs/example.md", 1, hfDuplicateItem(9, "7-7", "Duplicate observation.")))+"\n<h6>"),
		"missing run":                   replace("- **Run ID**: `"+submittedTestRun+"`", ""),
		"malformed run":                 replace(submittedTestRun, "unknown"),
		"duplicate run":                 replace("- **Run ID**: `"+submittedTestRun+"`", "- **Run ID**: `"+submittedTestRun+"`\n- **Run ID**: `"+submittedTestRun+"`"),
		"quoted run":                    replace("- **Run ID**:", "> - **Run ID**:"),
		"fenced run":                    replace("- **Run ID**: `"+submittedTestRun+"`", "```\n- **Run ID**: `"+submittedTestRun+"`\n```"),
		"missing comparison":            replace(hostedScopeDeclaration(strings.Repeat("b", 40), strings.Repeat("a", 40)), "No comparison declared."),
		"partial comparison":            replace("from the base of the PR", "in selected files"),
		"wrong comparison head":         replace("and "+strings.Repeat("a", 40)+".", "and "+strings.Repeat("c", 40)+"."),
		"wrong configuration commit":    replace("/commit/"+strings.Repeat("a", 40), "/commit/"+strings.Repeat("c", 40)),
		"wrong configuration file head": replace("/blob/"+strings.Repeat("a", 40), "/blob/"+strings.Repeat("c", 40)),
		"selected count mismatch":       replace("processing: 77", "processing: 76"),
		"duplicate selected path":       replace("* `source/selected02.go`", "* `source/selected01.go`"),
		"zero selected":                 replace("processing: 77", "processing: 0"),
		"unknown metadata":              replace("- **Layers**:", "- **Unknown field**:"),
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := submittedAdditionalFixture(t)
			f.snapshot.Reviews[0].Body = mutate(f.snapshot.Reviews[0].Body)
			if f.snapshot.Reviews[0].Body == submittedAdditionalBody {
				t.Fatal("mutation did not alter fixture")
			}
			got := f.extract(t)
			if got.Complete || got.Publication.State == "attributed" || got.Publication.SubmittedReview != nil || len(got.Findings) != 0 || !got.Publication.FinishedAt.IsZero() {
				t.Fatalf("unproven body credited: state=%s reason=%s complete=%t findings=%d", got.Publication.State, got.Reason, got.Complete, len(got.Findings))
			}
		})
	}
}

func TestHostedSubmittedAdditionalOnlyKeepsAttributionBoundaries(t *testing.T) {
	cases := map[string]func(*hfFixture){
		"incomplete source":        func(f *hfFixture) { f.snapshot.Complete = false },
		"unknown root collection":  func(f *hfFixture) { f.snapshot.ReviewComments = nil },
		"unknown issue collection": func(f *hfFixture) { f.snapshot.IssueComments = nil },
		"wrong provider actor":     func(f *hfFixture) { f.snapshot.Reviews[0].Author.ID++ },
		"wrong provider kind":      func(f *hfFixture) { f.snapshot.Reviews[0].Author.Kind = "User" },
		"wrong head":               func(f *hfFixture) { f.snapshot.Reviews[0].HeadSHA = strings.Repeat("d", 40) },
		"pending review": func(f *hfFixture) {
			f.snapshot.Reviews[0].State = "pending"
			f.snapshot.Reviews[0].SubmittedAt = time.Time{}
		},
		"equal trigger time": func(f *hfFixture) { f.snapshot.Reviews[0].SubmittedAt = f.snapshot.IssueComments[0].CreatedAt },
		"before trigger": func(f *hfFixture) {
			f.snapshot.Reviews[0].SubmittedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Second)
		},
		"competing review": func(f *hfFixture) {
			other := f.snapshot.Reviews[0]
			other.ID++
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		},
		"competing foreign head review": func(f *hfFixture) {
			other := f.snapshot.Reviews[0]
			other.ID++
			other.HeadSHA = strings.Repeat("d", 40)
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		},
		"reused historical run": func(f *hfFixture) {
			other := f.snapshot.Reviews[0]
			other.ID++
			other.SubmittedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Hour)
			f.snapshot.Reviews = append(f.snapshot.Reviews, other)
		},
		"missing acknowledgement": func(f *hfFixture) { f.snapshot.IssueComments = f.snapshot.IssueComments[:1] },
		"foreign acknowledgement": func(f *hfFixture) { f.snapshot.IssueComments[1].Author.ID++ },
		"unknown acknowledgement": func(f *hfFixture) { f.snapshot.IssueComments[1].Body = "Full review finished." },
		"acknowledgement before trigger": func(f *hfFixture) {
			f.snapshot.IssueComments[1].CreatedAt = f.snapshot.IssueComments[0].CreatedAt.Add(-time.Second)
		},
		"acknowledgement at submission": func(f *hfFixture) { f.snapshot.IssueComments[1].UpdatedAt = f.snapshot.Reviews[0].SubmittedAt },
		"acknowledgement before submission": func(f *hfFixture) {
			f.snapshot.IssueComments[1].UpdatedAt = f.snapshot.Reviews[0].SubmittedAt.Add(-time.Second)
		},
		"duplicate acknowledgement": func(f *hfFixture) {
			other := f.snapshot.IssueComments[1]
			other.ID++
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, other)
		},
		"unknown provider reply": func(f *hfFixture) {
			other := f.snapshot.IssueComments[1]
			other.ID++
			other.Body = "Unknown output"
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, other)
		},
		"competing trigger": func(f *hfFixture) {
			other := f.snapshot.IssueComments[0]
			other.ID = 303
			other.Body = "@coderabbitai full review"
			other.CreatedAt = other.CreatedAt.Add(1500 * time.Millisecond)
			other.UpdatedAt = other.CreatedAt
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, other)
		},
		"attached root": func(f *hfFixture) {
			other := submittedTestFixture(t)
			f.snapshot.ReviewComments = other.snapshot.ReviewComments[:1]
		},
		"orphan root": func(f *hfFixture) {
			other := submittedTestFixture(t)
			other.snapshot.ReviewComments[0].ReviewID = 999
			f.snapshot.ReviewComments = other.snapshot.ReviewComments[:1]
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := submittedAdditionalFixture(t)
			mutate(&f)
			got := f.extract(t)
			if got.Complete || got.Publication.State == "attributed" || got.Publication.SubmittedReview != nil || len(got.Findings) != 0 || !got.Publication.FinishedAt.IsZero() {
				t.Fatalf("attribution boundary bypassed: state=%s reason=%s complete=%t findings=%d", got.Publication.State, got.Reason, got.Complete, len(got.Findings))
			}
		})
	}
}

func TestHostedSubmittedAdditionalUnknownSeverityRemainsUnknown(t *testing.T) {
	f := submittedAdditionalFixture(t)
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Trivial**", "Unknown**", 1)
	got := f.extract(t)
	if !got.Complete || len(got.Findings) != 1 || got.Findings[0].Severity != "unknown" || !strings.Contains(got.Findings[0].Detail, "Unknown**") || got.Publication.StandingZero != nil {
		t.Fatal("unknown severity became a clean result or invented a severity", got.Reason)
	}
}

func TestHostedSubmittedAdditionalOnlyCountsEveryItem(t *testing.T) {
	f := submittedAdditionalFixture(t)
	start := strings.Index(f.snapshot.Reviews[0].Body, "`24-24`:")
	end := strings.Index(f.snapshot.Reviews[0].Body[start:], "\n</dd>") + start
	item := f.snapshot.Reviews[0].Body[start:end]
	second := strings.ReplaceAll(item, "`24-24`:", "`30-30`:")
	second = strings.Replace(second, "Consider the optional example wording.", "Consider a second optional example.", 1)
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, item, item+"\n"+second, 1)
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Additional comments: 1", "Additional comments: 2", 1)
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "docs/example.md-24-24: 1", "docs/example.md-24-30: 2", 1)
	got := f.extract(t)
	if !got.Complete || len(got.Findings) != 2 || got.Findings[0].SourceKey == got.Findings[1].SourceKey || got.Findings[1].Title != "Consider a second optional example." {
		t.Fatal("counted additional inventory lost an item", got.Reason, len(got.Findings))
	}
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, second, item, 1)
	got = f.extract(t)
	if got.Complete || got.Publication.State == "attributed" || len(got.Findings) != 0 {
		t.Fatal("duplicated additional item supplied a count", got.Reason)
	}
}
