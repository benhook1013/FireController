package reviewprovider

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const standingTestRun = "11111111-2222-4333-8444-555555555555"

// Independently authored source grammar: fake source identities, paths and
// configuration values. No private provider comment is included in this fixture.
func standingTestBody(head, base string) string {
	return "<!-- This is an auto-generated comment: summarize by coderabbit.ai -->\n" + standingRecentStart + `

No actionable comments were generated in the recent review. 🎉

<details>
<summary><strong>ℹ️ Recent review info</strong></summary>
<dl>
<dd>
<details>
<summary><strong>⚙️ Run configuration</strong></summary>
<dl>
<dd>
- **Configuration used**: Repository: synthetic/project/.coderabbit.yaml
- **Review profile**: ASSERTIVE
- **Plan**: Advanced
- **Run ID**: ` + "`" + standingTestRun + "`" + `
</dd>
</dl>
</details>
<details>
<summary><strong>📥 Commits</strong></summary>
<dl>
<dd>
` + hostedScopeDeclaration(base, head) + `
</dd>
</dl>
</details>
<details>
<summary><strong>⛔ Files ignored due to path filters: 1</strong></summary>
<dl>
<dd>
* ` + "`build/generated.js` is excluded by `!build/**`" + `
</dd>
</dl>
</details>
<details>
<summary><strong>📒 Files selected for processing: 2</strong></summary>
<dl>
<dd>
* ` + "`pkg/alpha.go`\n* `docs/guide.md`" + `
</dd>
</dl>
</details>
**Included review availability:** This review used your included allowance. Your plan provides up to 1 included review per hour; 0 remain after this review.
</dd>
</dl>
</details>
` + standingRecentEnd + `
<!-- walkthrough_start -->
<details>
<summary><strong>📝 Summary</strong></summary>
<dl><dd>Independent synthetic change description.</dd></dl>
</details>
<!-- walkthrough_end -->
<details>
<summary><strong>Merge Risk:</strong> <strong>🔵 Low</strong> · up to <code>` + head[:5] + `</code></summary>
<dl>
<dd>
<!-- final_review_risk_start -->
<!-- final_review_risk_label
Synthetic risk description, not inventory proof.
-->
<!-- final_review_risk_coverage:{"sourceCommitId":"` + head + `","coveredCommitId":"` + head + `","kind":"reviewed"} -->
<!-- final_review_risk_end -->
</dd>
</dl>
</details>`
}

func standingTestAck() string {
	return "<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\n\nFull review finished.\n\n</details>"
}

func standingTestFixture(t *testing.T) (HostedPublicationSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	s, intent, poster, bot, earliest := publicationFixture(t)
	s.Reviews, s.ReviewComments = []HostedReview{}, []ReviewComment{}
	triggerAt := s.IssueComments[0].CreatedAt
	s.ObservedAt = triggerAt.Add(4 * time.Second)
	s.IssueComments = append(s.IssueComments,
		IssueComment{ID: 601, Author: bot, CreatedAt: triggerAt.Add(-time.Hour), UpdatedAt: triggerAt.Add(2 * time.Second), Body: standingTestBody(intent.HeadSHA, strings.Repeat("b", 40))},
		IssueComment{ID: 602, Author: bot, CreatedAt: triggerAt.Add(time.Second), UpdatedAt: triggerAt.Add(3 * time.Second), Body: standingTestAck()},
	)
	return s, intent, poster, bot, earliest
}

func TestHostedStandingZeroAttributionInventoryAndComparison(t *testing.T) {
	s, intent, poster, bot, earliest := standingTestFixture(t)
	before := append([]IssueComment(nil), s.IssueComments...)
	p, err := ResolveHostedPublication(s, intent, poster, bot, earliest)
	if err != nil || p.State != "attributed" || p.Reason != "standing_zero_inventory_attributed" || p.StandingZero == nil || len(p.Batches) != 0 || len(p.Roots) != 0 || len(p.EligibleReviews) != 0 || p.PublicationID != "" || p.ProviderAttemptID != "" || !p.FinishedAt.Equal(s.IssueComments[2].UpdatedAt) {
		t.Fatal("standing zero proof not attributed independently of batch identity", p, err)
	}
	z := p.StandingZero
	if z.ProviderRunID != standingTestRun || z.HeadSHA != intent.HeadSHA || z.BaseSHA != strings.Repeat("b", 40) || z.SelectedFiles != 2 || z.IgnoredFiles != 1 || z.Summary != s.IssueComments[1] || z.Acknowledgement != s.IssueComments[2] {
		t.Fatal("standing source facts changed", z)
	}
	inventory, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
	if err != nil || !inventory.Complete || inventory.Findings == nil || len(inventory.Findings) != 0 || inventory.Publication.StandingZero == nil {
		t.Fatal("proven empty inventory lost", inventory, err)
	}
	for _, anchors := range [][2]string{{strings.Repeat("b", 40), strings.Repeat("d", 40)}, {strings.Repeat("d", 40), strings.Repeat("b", 40)}} {
		if base, ok := ResolveHostedComparison(p, intent.HeadSHA, anchors[0], anchors[1]); !ok || base != z.BaseSHA {
			t.Fatal("typed standing comparison lost", base, ok)
		}
	}
	if base, ok := ResolveHostedComparison(p, intent.HeadSHA, strings.Repeat("d", 40), strings.Repeat("e", 40)); ok || base != "" {
		t.Fatal("unrelated comparison granted scope")
	}
	p.StandingZero.HeadSHA = strings.Repeat("f", 40)
	if _, ok := ResolveHostedComparison(p, intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("d", 40)); ok {
		t.Fatal("caller-mutated typed facts trusted over raw evidence")
	}
	if !reflect.DeepEqual(before, s.IssueComments) {
		t.Fatal("source snapshot mutated")
	}
	again, err := ResolveHostedPublication(s, intent, poster, bot, earliest)
	if err != nil || again.StandingZero == nil || again.StandingZero.HeadSHA != intent.HeadSHA {
		t.Fatal("returned standing pointer aliases later result", again, err)
	}
}

func TestHostedStandingZeroMalformedOrIncompleteProofNeverCompletes(t *testing.T) {
	changes := map[string]func(string) string{
		"no zero claim": func(b string) string {
			return strings.ReplaceAll(b, "No actionable comments were generated in the recent review. 🎉", "Review finished.")
		},
		"selected count":           func(b string) string { return strings.Replace(b, "processing: 2", "processing: 3", 1) },
		"selected zero":            func(b string) string { return strings.Replace(b, "processing: 2", "processing: 0", 1) },
		"count leading zero":       func(b string) string { return strings.Replace(b, "processing: 2", "processing: 02", 1) },
		"count overflow":           func(b string) string { return strings.Replace(b, "processing: 2", "processing: 10001", 1) },
		"ignored count":            func(b string) string { return strings.Replace(b, "path filters: 1", "path filters: 2", 1) },
		"duplicate selected":       func(b string) string { return strings.Replace(b, "docs/guide.md", "pkg/alpha.go", 1) },
		"selected ignored overlap": func(b string) string { return strings.Replace(b, "build/generated.js", "pkg/alpha.go", 1) },
		"unsafe path":              func(b string) string { return strings.Replace(b, "pkg/alpha.go", "../alpha.go", 1) },
		"unknown category": func(b string) string {
			return strings.Replace(b, "📒 Files selected for processing", "Unexpected comments", 1)
		},
		"positive findings": func(b string) string {
			return strings.Replace(b, standingRecentEnd, "**Actionable comments posted: 1**\n"+standingRecentEnd, 1)
		},
		"outside diff": func(b string) string {
			return strings.Replace(b, standingRecentEnd, "**⚠️ Outside diff range comments (1)**\n"+standingRecentEnd, 1)
		},
		"duplicate comments": func(b string) string {
			return strings.Replace(b, standingRecentEnd, "<details><summary>♻️ Duplicate comments (1)</summary></details>\n"+standingRecentEnd, 1)
		},
		"missing run": func(b string) string { return strings.Replace(b, "- **Run ID**: `"+standingTestRun+"`", "", 1) },
		"duplicate run": func(b string) string {
			return strings.Replace(b, "- **Run ID**: `"+standingTestRun+"`", "- **Run ID**: `"+standingTestRun+"`\n- **Run ID**: `99999999-2222-4333-8444-555555555555`", 1)
		},
		"malformed run": func(b string) string { return strings.Replace(b, standingTestRun, "not-a-uuid", 1) },
		"wrong scope head": func(b string) string {
			return strings.Replace(b, " and "+strings.Repeat("c", 40)+".", " and "+strings.Repeat("d", 40)+".", 1)
		},
		"partial scope":             func(b string) string { return strings.Replace(b, "from the base of the PR", "in selected files", 1) },
		"unknown metadata ancestor": func(b string) string { return strings.Replace(b, "ℹ️ Recent review info", "Example", 1) },
		"missing coverage":          func(b string) string { return strings.Replace(b, "final_review_risk_coverage", "other_metadata", 1) },
		"wrong coverage kind":       func(b string) string { return strings.Replace(b, `"kind":"reviewed"`, `"kind":"skipped"`, 1) },
		"coverage head mismatch": func(b string) string {
			return strings.Replace(b, `"coveredCommitId":"`+strings.Repeat("c", 40), `"coveredCommitId":"`+strings.Repeat("d", 40), 1)
		},
		"duplicate recent": func(b string) string { return b + "\n" + b },
	}
	for name, edit := range changes {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			original := s.IssueComments[1].Body
			s.IssueComments[1].Body = edit(original)
			if s.IssueComments[1].Body == original {
				t.Fatal("fixture mutation did not change source")
			}
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Publication.State == "attributed" || got.Publication.StandingZero != nil {
				t.Fatal("malformed standing proof became zero inventory", got, err)
			}
		})
	}
}

func TestHostedStandingZeroVisibilityCannotBeLaundered(t *testing.T) {
	for _, wrap := range []struct{ before, after string }{
		{"```html\n", "\n```"}, {"~~~\n", "\n~~~"}, {"<!-- example\n", "\n-->"},
		{"<pre>\n", "\n</pre>"}, {"<blockquote>\n", "\n</blockquote>"},
		{"<details><summary>Example</summary>\n", "\n</details>"}, {"<div hidden>\n", "\n</div>"},
		{"> ", ""}, {"    ", ""},
	} {
		t.Run(fmt.Sprintf("%q", wrap.before), func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			body := s.IssueComments[1].Body
			if wrap.before == "> " || wrap.before == "    " {
				body = wrap.before + strings.ReplaceAll(body, "\n", "\n"+wrap.before)
			} else {
				body = wrap.before + body + wrap.after
			}
			s.IssueComments[1].Body = body
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Publication.State == "attributed" {
				t.Fatal("hidden standing example granted proof", got, err)
			}
		})
	}
}

func TestHostedStandingZeroWindowOriginAndCompetingInventory(t *testing.T) {
	changes := map[string]func(*HostedPublicationSnapshot, HostedTriggerIntent){
		"incomplete": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) { s.Complete = false },
		"summary old edit": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			s.IssueComments[1].UpdatedAt = s.IssueComments[0].CreatedAt
		},
		"ack at trigger": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			s.IssueComments[2].CreatedAt = s.IssueComments[0].CreatedAt
		},
		"summary after ack": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			s.IssueComments[1].UpdatedAt = s.IssueComments[2].UpdatedAt.Add(time.Second)
		},
		"future edit": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			s.IssueComments[1].UpdatedAt = s.ObservedAt.Add(time.Second)
		},
		"foreign summary id": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) { s.IssueComments[1].Author.ID++ },
		"foreign ack id":     func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) { s.IssueComments[2].Author.ID++ },
		"foreign kind":       func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) { s.IssueComments[1].Author.Kind = "User" },
		"duplicate summary": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			c := s.IssueComments[1]
			c.ID++
			c.ID += 10
			s.IssueComments = append(s.IssueComments, c)
		},
		"duplicate ack": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			c := s.IssueComments[2]
			c.ID += 10
			s.IssueComments = append(s.IssueComments, c)
		},
		"reused old run identity": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			c := s.IssueComments[1]
			c.ID += 10
			c.UpdatedAt = s.IssueComments[0].CreatedAt.Add(-time.Second)
			s.IssueComments = append(s.IssueComments, c)
		},
		"bare ack": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			s.IssueComments[2].Body = "Full review finished."
		},
		"ack unknown trailer": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			s.IssueComments[2].Body += "\nOne unresolved finding remains."
		},
		"ack quoted": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			s.IssueComments[2].Body = "> " + strings.ReplaceAll(s.IssueComments[2].Body, "\n", "\n> ")
		},
		"competing command": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			c := s.IssueComments[0]
			c.ID += 100
			c.Body = "@coderabbitai full review"
			c.CreatedAt = c.CreatedAt.Add(time.Second)
			c.UpdatedAt = c.CreatedAt
			s.IssueComments = append(s.IssueComments, c)
		},
		"eligible actionable review": func(s *HostedPublicationSnapshot, i HostedTriggerIntent) {
			s.Reviews = append(s.Reviews, HostedReview{ID: 701, Author: s.IssueComments[1].Author, HeadSHA: i.HeadSHA, State: "commented", SubmittedAt: s.IssueComments[1].UpdatedAt, Body: "**Actionable comments posted: 1**"})
		},
		"eligible unknown review": func(s *HostedPublicationSnapshot, i HostedTriggerIntent) {
			s.Reviews = append(s.Reviews, HostedReview{ID: 701, Author: s.IssueComments[1].Author, HeadSHA: i.HeadSHA, State: "commented", SubmittedAt: s.IssueComments[1].UpdatedAt, Body: "An unknown result format."})
		},
		"pending review": func(s *HostedPublicationSnapshot, i HostedTriggerIntent) {
			s.Reviews = append(s.Reviews, HostedReview{ID: 701, Author: s.IssueComments[1].Author, HeadSHA: i.HeadSHA, State: "pending", Body: ""})
		},
		"fresh inline": func(s *HostedPublicationSnapshot, i HostedTriggerIntent) {
			s.ReviewComments = append(s.ReviewComments, ReviewComment{ID: 801, ReviewID: 701, Author: s.IssueComments[1].Author, HeadSHA: i.HeadSHA, OriginalHeadSHA: i.HeadSHA, Path: "pkg/alpha.go", Body: hfInline(1), CreatedAt: s.IssueComments[1].UpdatedAt, UpdatedAt: s.IssueComments[1].UpdatedAt})
		},
		"unknown provider output": func(s *HostedPublicationSnapshot, _ HostedTriggerIntent) {
			c := s.IssueComments[2]
			c.ID += 10
			c.Body = "Unknown provider result."
			s.IssueComments = append(s.IssueComments, c)
		},
	}
	for name, edit := range changes {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			edit(&s, intent)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Publication.State == "attributed" || got.Publication.StandingZero != nil {
				t.Fatal("unproven causal window became zero inventory", got, err)
			}
		})
	}
}

func TestHostedStandingZeroBatchPriorityAndContradictions(t *testing.T) {
	for _, mode := range []string{"complete", "partial", "malformed", "contradictory"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			body := "**Actionable comments posted: 0**\n" + publicationTestMarker(1, 1)
			if mode == "partial" {
				body = "**Actionable comments posted: 0**\n" + publicationTestMarker(1, 2)
			}
			if mode == "malformed" {
				body = "**Actionable comments posted: 0**\n<!-- coderabbit-review-publication malformed -->"
			}
			s.Reviews = []HostedReview{{ID: 701, Author: bot, HeadSHA: intent.HeadSHA, State: "commented", SubmittedAt: s.IssueComments[1].UpdatedAt, Body: body}}
			if mode == "contradictory" {
				other := s.Reviews[0]
				other.ID++
				other.Body = strings.Replace(body, publicationTestID, "99999999-2222-4333-8444-555555555555", 1)
				s.Reviews = append(s.Reviews, other)
			}
			got, err := ResolveHostedPublication(s, intent, poster, bot, earliest)
			if err != nil || got.StandingZero != nil {
				t.Fatal("batch evidence fell through to standing zero", got, err)
			}
			if mode == "complete" {
				if got.State != "attributed" || len(got.Batches) != 1 {
					t.Fatal("existing batch priority lost", got)
				}
			} else if got.State == "attributed" {
				t.Fatal("partial/conflicting batch became clean", got)
			}
		})
	}
}

func TestHostedStandingZeroStrictSourceAndProofBounds(t *testing.T) {
	s, intent, poster, bot, earliest := standingTestFixture(t)
	s.IssueComments[1].Author.Login = "renamed-provider"
	s.IssueComments[2].Author.Login = "other-display-name"
	got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
	if err != nil || !got.Complete {
		t.Fatal("stable actor IDs depended on display login", got, err)
	}
	for _, extra := range []string{"\n**Actionable comments posted: 1**", "\n<details>\n<summary>Other findings (1)</summary>\n</details>", "\n" + hfOutsideCategory(1, hfOutside(1, "pkg/alpha.go", "1", "Synthetic issue.")), "\n" + hfMarker(1)} {
		candidate := s
		candidate.IssueComments = append([]IssueComment(nil), s.IssueComments...)
		candidate.IssueComments[1].Body += extra
		inv, err := ExtractHostedFindings(candidate, intent, poster, bot, earliest)
		if err != nil || inv.Complete || inv.Publication.State == "attributed" {
			t.Fatal("contradictory finding category vanished outside recent section", inv, err)
		}
	}
	for _, body := range []string{strings.Repeat("x", githubMaxText+1), s.IssueComments[1].Body + "\xff", s.IssueComments[1].Body + "\x00"} {
		candidate := s
		candidate.IssueComments = append([]IssueComment(nil), s.IssueComments...)
		candidate.IssueComments[1].Body = body
		inv, err := ExtractHostedFindings(candidate, intent, poster, bot, earliest)
		if err != nil || inv.Complete || inv.Publication.State == "attributed" {
			t.Fatal("out-of-contract source granted zero proof", inv, err)
		}
	}
}

func TestHostedStandingZeroCoverageRequiresCanonicalMergeRiskAncestry(t *testing.T) {
	changes := map[string]func(string) string{
		"unknown risk label": func(b string) string {
			return strings.Replace(b, "<strong>Merge Risk:</strong>", "<strong>Example:</strong>", 1)
		},
		"missing risk label": func(b string) string {
			lines := strings.Split(b, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "<summary><strong>Merge Risk:") {
					lines[i] = ""
				}
			}
			return strings.Join(lines, "\n")
		},
		"wrong risk prefix": func(b string) string { return strings.Replace(b, "<code>ccccc</code>", "<code>ddddd</code>", 1) },
		"hidden details": func(b string) string {
			return strings.Replace(b, "<details>\n<summary><strong>Merge Risk:", "<details hidden>\n<summary><strong>Merge Risk:", 1)
		},
		"unknown outer ancestor": func(b string) string {
			return strings.Replace(b, "<details>\n<summary><strong>Merge Risk:", "<section>\n<details>\n<summary><strong>Merge Risk:", 1) + "\n</section>"
		},
		"quoted example ancestor": func(b string) string {
			return strings.Replace(b, "<details>\n<summary><strong>Merge Risk:", "<blockquote>\n<details>\n<summary><strong>Merge Risk:", 1) + "\n</blockquote>"
		},
		"unknown metadata container": func(b string) string {
			at := strings.Index(b, "<summary><strong>Merge Risk:")
			return b[:at] + strings.ReplaceAll(strings.ReplaceAll(b[at:], "<dl>", "<section>"), "</dl>", "</section>")
		},
		"hidden dd": func(b string) string {
			at := strings.Index(b, "<summary><strong>Merge Risk:")
			return b[:at] + strings.Replace(b[at:], "<dd>", "<dd hidden>", 1)
		},
		"missing section start": func(b string) string { return strings.Replace(b, "<!-- final_review_risk_start -->", "", 1) },
		"missing section end":   func(b string) string { return strings.Replace(b, "<!-- final_review_risk_end -->", "", 1) },
		"unclosed wrapper":      func(b string) string { return strings.TrimSuffix(b, "</details>") },
		"duplicate section": func(b string) string {
			at := strings.LastIndex(b, "<details>\n<summary><strong>Merge Risk:")
			return b + "\n" + b[at:]
		},
	}
	for _, mode := range []string{"duplicate", "fenced", "pre", "comment", "nested example", "indented", "quoted"} {
		changes["coverage/"+mode] = func(b string) string {
			for _, line := range strings.Split(b, "\n") {
				if !strings.HasPrefix(line, "<!-- final_review_risk_coverage:") {
					continue
				}
				replacement := line
				switch mode {
				case "duplicate":
					replacement = line + "\n" + line
				case "fenced":
					replacement = "```html\n" + line + "\n```"
				case "pre":
					replacement = "<pre>\n" + line + "\n</pre>"
				case "comment":
					replacement = "<!-- example\n" + line + "\n-->"
				case "nested example":
					replacement = "<details>\n<summary>Example</summary>\n" + line + "\n</details>"
				case "indented":
					replacement = "    " + line
				case "quoted":
					replacement = "> " + line
				}
				return strings.Replace(b, line, replacement, 1)
			}
			panic("synthetic coverage marker missing")
		}
	}
	for name, edit := range changes {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			original := s.IssueComments[1].Body
			s.IssueComments[1].Body = edit(original)
			if s.IssueComments[1].Body == original {
				t.Fatal("fixture mutation did not change source")
			}
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Publication.StandingZero != nil || got.Publication.State == "attributed" {
				t.Fatal("unowned or hidden risk coverage granted zero proof", got, err)
			}
		})
	}
}
