package reviewprovider

import (
	"fmt"
	"reflect"
	"slices"
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

func TestHostedStandingZeroCRLFAcknowledgement(t *testing.T) {
	for _, summaryCRLF := range []bool{false, true} {
		t.Run(fmt.Sprintf("summary_CRLF_%t", summaryCRLF), func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.IssueComments[2].Body = strings.ReplaceAll(s.IssueComments[2].Body, "\n", "\r\n")
			if summaryCRLF {
				s.IssueComments[1].Body = strings.ReplaceAll(s.IssueComments[1].Body, "\n", "\r\n")
			}
			inventory, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || !inventory.Complete || inventory.Findings == nil || len(inventory.Findings) != 0 || inventory.Publication.StandingZero == nil || inventory.Publication.StandingZero.ProviderRunID != standingTestRun {
				t.Fatal("CRLF acknowledgement lost attributed empty inventory", inventory, err)
			}
			s.IssueComments[2].Body += "\r\nOne unresolved finding remains."
			inventory, err = ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || inventory.Complete || inventory.Publication.State == "attributed" {
				t.Fatal("CRLF normalization accepted an unknown acknowledgement trailer", inventory, err)
			}
		})
	}
}

func TestHostedStandingZeroModerateRiskStillRequiresCompleteInventory(t *testing.T) {
	for _, mode := range []string{"moderate", "unknown_label", "wrong_coverage_head", "missing_ack"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "🔵 Low", "🟡 Moderate", 1)
			switch mode {
			case "unknown_label":
				s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "🟡 Moderate", "Unclassified", 1)
			case "wrong_coverage_head":
				s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, `"coveredCommitId":"`+intent.HeadSHA, `"coveredCommitId":"`+strings.Repeat("d", 40), 1)
			case "missing_ack":
				s.IssueComments = s.IssueComments[:2]
			}
			before := append([]IssueComment(nil), s.IssueComments...)
			inventory, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "moderate" {
				if !inventory.Complete || inventory.Publication.StandingZero == nil || len(inventory.Findings) != 0 {
					t.Fatal("risk rating prevented otherwise proven zero inventory", inventory.Complete, inventory.Reason)
				}
			} else if inventory.Complete || inventory.Publication.StandingZero != nil {
				t.Fatal("risk label support bypassed inventory proof", inventory.Complete, inventory.Reason)
			}
			if !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatal("source comments changed")
			}
		})
	}
}

func TestHostedStandingZeroOptionalNoReviewableSubset(t *testing.T) {
	block := func(count, paths string) string {
		return "<details>\n<summary><strong>💤 Files with no reviewable changes: " + count + "</strong></summary>\n<dl>\n<dd>\n" + paths + "\n</dd>\n</dl>\n</details>"
	}
	for _, mode := range []string{"one", "all", "zero", "omitted_ignored", "unknown_path", "ignored_path", "duplicate", "count_mismatch", "zero_with_path", "overflow", "leading_zero", "more_than_selected", "backticks", "html", "link", "markup_selected_path", "emphasis_selected_path", "unsafe_path", "extra_prose", "wrong_envelope", "duplicate_block", "after_availability"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			body := strings.Replace(s.IssueComments[1].Body, "🔵 Low", "🟡 Moderate", 1)
			section := block("1", "* pkg/alpha.go")
			switch mode {
			case "all":
				section = block("2", "* pkg/alpha.go\n* docs/guide.md")
			case "zero":
				section = block("0", "")
			case "omitted_ignored":
				body = strings.Replace(body, standingTestIgnoredBlock(t, body), "", 1)
			case "unknown_path":
				section = block("1", "* pkg/unknown.go")
			case "ignored_path":
				section = block("1", "* build/generated.js")
			case "duplicate":
				section = block("2", "* pkg/alpha.go\n* pkg/alpha.go")
			case "count_mismatch":
				section = block("2", "* pkg/alpha.go")
			case "zero_with_path":
				section = block("0", "* pkg/alpha.go")
			case "overflow":
				section = block("10001", "* pkg/alpha.go")
			case "leading_zero":
				section = block("01", "* pkg/alpha.go")
			case "more_than_selected":
				section = block("3", "* pkg/alpha.go")
			case "backticks":
				section = block("1", "* `pkg/alpha.go`")
			case "html":
				section = block("1", "* <code>pkg/alpha.go</code>")
			case "link":
				section = block("1", "* [pkg/alpha.go](pkg/alpha.go)")
			case "markup_selected_path":
				body = strings.Replace(body, "`pkg/alpha.go`", "`[pkg/alpha.go](target)`", 1)
				section = block("1", "* [pkg/alpha.go](target)")
			case "emphasis_selected_path":
				body = strings.Replace(body, "`pkg/alpha.go`", "`**pkg/alpha.go**`", 1)
				section = block("1", "* **pkg/alpha.go**")
			case "unsafe_path":
				section = block("1", "* ../pkg/alpha.go")
			case "extra_prose":
				section = block("1", "* pkg/alpha.go\nUnknown review state.")
			case "wrong_envelope":
				section = strings.Replace(section, "<dd>", "<div>", 1)
			case "duplicate_block":
				section += "\n" + section
			}
			availability := "**Included review availability:**"
			if mode == "after_availability" {
				start := strings.Index(body, availability)
				end := start + strings.Index(body[start:], "\n")
				body = body[:end] + "\n" + section + body[end:]
			} else {
				body = strings.Replace(body, availability, section+"\n"+availability, 1)
			}
			s.IssueComments[1].Body = body
			before := append([]IssueComment(nil), s.IssueComments...)
			inventory, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			want := mode == "one" || mode == "all" || mode == "zero" || mode == "omitted_ignored"
			if err != nil || inventory.Complete != want || (inventory.Publication.StandingZero != nil) != want {
				t.Fatal("optional subset changed inventory proof", inventory.Complete, inventory.Reason, err)
			}
			if want && (inventory.Publication.StandingZero.SelectedFiles != 2 || len(inventory.Findings) != 0) {
				t.Fatal("subset changed selected-for-processing count or findings")
			}
			if !reflect.DeepEqual(s.IssueComments, before) {
				t.Fatal("optional metadata mutated original source")
			}
		})
	}
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

func TestHostedStandingZeroSummaryMayFinishAfterCommandAcknowledgement(t *testing.T) {
	s, intent, poster, bot, earliest := standingTestFixture(t)
	s.IssueComments[1].UpdatedAt = s.IssueComments[2].UpdatedAt.Add(time.Second)
	s.ObservedAt = s.IssueComments[1].UpdatedAt.Add(time.Second)
	before := append([]IssueComment(nil), s.IssueComments...)
	got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
	if err != nil || !got.Complete || got.Publication.StandingZero == nil || !got.Publication.FinishedAt.Equal(s.IssueComments[1].UpdatedAt) || !reflect.DeepEqual(before, s.IssueComments) {
		t.Fatal("proven final inventory requires an arbitrary provider edit order", got, err)
	}
	// A later trigger still closes attribution even when the delayed summary
	// proves a clean run on this head. It cannot be borrowed across requests.
	other := s.IssueComments[0]
	other.ID += 1000
	other.CreatedAt, other.UpdatedAt = s.IssueComments[2].UpdatedAt.Add(time.Millisecond), s.IssueComments[2].UpdatedAt.Add(time.Millisecond)
	other.Body = "@coderabbitai full review"
	s.IssueComments = append(s.IssueComments, other)
	got, err = ExtractHostedFindings(s, intent, poster, bot, earliest)
	if err != nil || got.Complete || got.Publication.StandingZero != nil {
		t.Fatal("late summary crossed a later request window", got, err)
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

func standingTestIgnoredBlock(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<details>\n<summary><strong>⛔ Files ignored due to path filters:")
	if start < 0 {
		t.Fatal("ignored fixture block missing")
	}
	end := strings.Index(body[start:], "</details>")
	if end < 0 {
		t.Fatal("ignored fixture block unclosed")
	}
	return body[start : start+end+len("</details>\n")]
}

func TestHostedStandingZeroObservedMinimalAndOmittedIgnored(t *testing.T) {
	for _, label := range []string{"🔵 Low", "⚪ Minimal"} {
		for _, ignored := range []string{"listed", "empty", "omitted"} {
			t.Run(label+"/"+ignored, func(t *testing.T) {
				s, intent, poster, bot, earliest := standingTestFixture(t)
				body := strings.Replace(s.IssueComments[1].Body, "🔵 Low", label, 1)
				block := standingTestIgnoredBlock(t, body)
				wantIgnored := int64(1)
				switch ignored {
				case "empty":
					body = strings.Replace(body, block, strings.ReplaceAll(strings.Replace(block, "path filters: 1", "path filters: 0", 1), "* `build/generated.js` is excluded by `!build/**`\n", ""), 1)
					wantIgnored = 0
				case "omitted":
					body = strings.Replace(body, block, "", 1)
					wantIgnored = 0
				}
				s.IssueComments[1].Body = body
				parsed, ok := parseStandingZero(body)
				if !ok || parsed.IgnoredFiles != wantIgnored || parsed.SelectedFiles != 2 {
					t.Fatalf("observed shape rejected: parsed=%v ignored=%d selected=%d", ok, parsed.IgnoredFiles, parsed.SelectedFiles)
				}
				inventory, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
				if err != nil || !inventory.Complete || inventory.Findings == nil || len(inventory.Findings) != 0 || inventory.Publication.StandingZero == nil {
					t.Fatalf("zero inventory unavailable: %s %v", inventory.Reason, err)
				}
				base, proven := ResolveHostedComparison(inventory.Publication, intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("d", 40))
				if !proven || base != strings.Repeat("b", 40) {
					t.Fatal("exact scope lost")
				}
			})
		}
	}
}

func TestHostedStandingZeroOptionalIgnoredNeverHidesMalformedInventory(t *testing.T) {
	changes := map[string]func(string) string{
		"malformed ignored header":   func(b string) string { return strings.Replace(b, "path filters: 1", "path filters: unknown", 1) },
		"leading zero ignored count": func(b string) string { return strings.Replace(b, "path filters: 1", "path filters: 01", 1) },
		"negative ignored count":     func(b string) string { return strings.Replace(b, "path filters: 1", "path filters: -1", 1) },
		"ignored overflow":           func(b string) string { return strings.Replace(b, "path filters: 1", "path filters: 10001", 1) },
		"extra ignored path": func(b string) string {
			return strings.Replace(b, "* `build/generated.js` is excluded by `!build/**`", "* `build/generated.js` is excluded by `!build/**`\n* `build/other.js` is excluded by `!build/**`", 1)
		},
		"ignored count zero with path": func(b string) string { return strings.Replace(b, "path filters: 1", "path filters: 0", 1) },
		"unknown ignored category": func(b string) string {
			return strings.Replace(b, "Files ignored due to path filters", "Files otherwise skipped", 1)
		},
		"duplicate ignored block": func(b string) string {
			block := standingTestIgnoredBlock(t, b)
			return strings.Replace(b, block, block+block, 1)
		},
		"orphan ignored path": func(b string) string {
			block := standingTestIgnoredBlock(t, b)
			return strings.Replace(b, block, "* `build/generated.js` is excluded by `!build/**`\n", 1)
		},
		"missing selected after omission": func(b string) string {
			b = strings.Replace(b, standingTestIgnoredBlock(t, b), "", 1)
			return strings.Replace(b, "Files selected for processing: 2", "Unexpected files: 2", 1)
		},
		"selected zero after omission": func(b string) string {
			b = strings.Replace(b, standingTestIgnoredBlock(t, b), "", 1)
			return strings.Replace(b, "processing: 2", "processing: 0", 1)
		},
		"selected count after omission": func(b string) string {
			b = strings.Replace(b, standingTestIgnoredBlock(t, b), "", 1)
			return strings.Replace(b, "processing: 2", "processing: 3", 1)
		},
		"selected unsafe after omission": func(b string) string {
			b = strings.Replace(b, standingTestIgnoredBlock(t, b), "", 1)
			return strings.Replace(b, "pkg/alpha.go", "../alpha.go", 1)
		},
		"selected duplicate after omission": func(b string) string {
			b = strings.Replace(b, standingTestIgnoredBlock(t, b), "", 1)
			return strings.Replace(b, "docs/guide.md", "pkg/alpha.go", 1)
		},
		"unknown risk label": func(b string) string { return strings.Replace(b, "🔵 Low", "Unclassified", 1) },
		"mismatched minimal prefix": func(b string) string {
			b = strings.Replace(b, "🔵 Low", "⚪ Minimal", 1)
			return strings.Replace(b, "<code>ccccc</code>", "<code>ddddd</code>", 1)
		},
		"duplicate coverage minimal": func(b string) string {
			b = strings.Replace(b, "🔵 Low", "⚪ Minimal", 1)
			line := ""
			for _, candidate := range strings.Split(b, "\n") {
				if standingCoverage.MatchString(candidate) {
					line = candidate
					break
				}
			}
			if line == "" {
				t.Fatal("coverage fixture missing")
			}
			return strings.Replace(b, line, line+"\n"+line, 1)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			before := s.IssueComments[1].Body
			s.IssueComments[1].Body = change(before)
			if before == s.IssueComments[1].Body {
				t.Fatal("fixture unchanged")
			}
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Publication.State == "attributed" {
				t.Fatalf("malformed optional inventory credited: %s %v", got.Reason, err)
			}
		})
	}
}

// This historical sentence is the independently verified trigger for the
// substring false positive. All source identities and surrounding proof are
// independently authored by standingTestFixture, not native capture bytes.
const standingHistoricalCheckpointTest = "The latest reported Hosted review checkpoint lists 3 accepted findings: 2 minor and 1 trivial, with 0 major and 0 routed. Finding details and current review findings are not available."

func standingTestHistoricalCheckpoint(body string) string {
	return strings.Replace(body, "<summary><strong>📝 Summary</strong></summary>\n<dl><dd>Independent synthetic change description.</dd></dl>", "<summary><strong>Summary</strong></summary>\n<dl>\n<dd>\n<br>\n<!-- This is an auto-generated comment: release notes by coderabbit.ai -->\n## Validation\n\n"+standingHistoricalCheckpointTest+"\n<!-- end of auto-generated comment: release notes by coderabbit.ai -->\nIndependent synthetic change description.\n</dd>\n</dl>", 1)
}

func TestHostedStandingZeroHistoricalCheckpointNarrative(t *testing.T) {
	s, intent, poster, bot, earliest := standingTestFixture(t)
	s.IssueComments[1].Body = standingTestHistoricalCheckpoint(s.IssueComments[1].Body)
	before := append([]IssueComment(nil), s.IssueComments...)
	got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
	if err != nil || !got.Complete || got.Findings == nil || len(got.Findings) != 0 || got.Publication.StandingZero == nil || got.Publication.Reason != "standing_zero_inventory_attributed" {
		t.Fatalf("historical release-note prose prevented explicit current zero inventory: complete=%v reason=%s err=%v", got.Complete, got.Reason, err)
	}
	z := got.Publication.StandingZero
	if z.ProviderRunID != standingTestRun || z.HeadSHA != intent.HeadSHA || z.BaseSHA != strings.Repeat("b", 40) || z.Summary != s.IssueComments[1] || z.Acknowledgement != s.IssueComments[2] || !reflect.DeepEqual(before, s.IssueComments) {
		t.Fatal("narrative classification changed identity or raw source")
	}
}

func TestHostedStandingZeroHistoricalCheckpointBoundaries(t *testing.T) {
	const releaseStart = "<!-- This is an auto-generated comment: release notes by coderabbit.ai -->"
	const releaseEnd = "<!-- end of auto-generated comment: release notes by coderabbit.ai -->"
	changes := map[string]func(string) string{
		"no walkthrough start": func(b string) string { return strings.Replace(b, "<!-- walkthrough_start -->", "", 1) },
		"no walkthrough end":   func(b string) string { return strings.Replace(b, "<!-- walkthrough_end -->", "", 1) },
		"duplicate walkthrough start": func(b string) string {
			return strings.Replace(b, "<!-- walkthrough_start -->", "<!-- walkthrough_start -->\n<!-- walkthrough_start -->", 1)
		},
		"duplicate walkthrough end": func(b string) string {
			return strings.Replace(b, "<!-- walkthrough_end -->", "<!-- walkthrough_end -->\n<!-- walkthrough_end -->", 1)
		},
		"nested walkthrough": func(b string) string {
			return strings.Replace(b, releaseStart, "<!-- walkthrough_start -->\n"+releaseStart, 1)
		},
		"wrong summary label": func(b string) string {
			return strings.Replace(b, "<summary><strong>Summary</strong></summary>", "<summary><strong>Current findings</strong></summary>", 1)
		},
		"nested summary": func(b string) string {
			return strings.Replace(strings.Replace(b, "<!-- walkthrough_start -->", "<section>\n<!-- walkthrough_start -->", 1), "<!-- walkthrough_end -->", "<!-- walkthrough_end -->\n</section>", 1)
		},
		"hidden summary": func(b string) string {
			return strings.Replace(b, "<!-- walkthrough_start -->\n<details>", "<!-- walkthrough_start -->\n<details hidden>", 1)
		},
		"wrong release wrapper": func(b string) string {
			return strings.Replace(strings.Replace(b, "<dd>\n<br>\n"+releaseStart, "<div>\n<br>\n"+releaseStart, 1), "Independent synthetic change description.\n</dd>", "Independent synthetic change description.\n</div>", 1)
		},
		"missing release start":   func(b string) string { return strings.Replace(b, releaseStart, "", 1) },
		"missing release end":     func(b string) string { return strings.Replace(b, releaseEnd, "", 1) },
		"duplicate release start": func(b string) string { return strings.Replace(b, releaseStart, releaseStart+"\n"+releaseStart, 1) },
		"duplicate release end":   func(b string) string { return strings.Replace(b, releaseEnd, releaseEnd+"\n"+releaseEnd, 1) },
		"second release region": func(b string) string {
			return strings.Replace(b, releaseEnd, releaseEnd+"\n"+releaseStart+"\nOther notes.\n"+releaseEnd, 1)
		},
		"altered release start": func(b string) string {
			return strings.Replace(b, releaseStart, "<!-- This is an auto-generated comment: Release notes by coderabbit.ai -->", 1)
		},
		"trailing release start": func(b string) string { return strings.Replace(b, releaseStart, releaseStart+" extra", 1) },
		"indented release start": func(b string) string { return strings.Replace(b, releaseStart, "    "+releaseStart, 1) },
		"quoted release start":   func(b string) string { return strings.Replace(b, releaseStart, "> "+releaseStart, 1) },
		"fenced release start":   func(b string) string { return strings.Replace(b, releaseStart, "```\n"+releaseStart+"\n```", 1) },
		"commented release start": func(b string) string {
			return strings.Replace(b, releaseStart, "<!-- example\n"+releaseStart+"\n-->", 1)
		},
		"release end after summary": func(b string) string {
			return strings.Replace(strings.Replace(b, releaseEnd, "", 1), "<!-- walkthrough_end -->", releaseEnd+"\n<!-- walkthrough_end -->", 1)
		},
		"repeated presentation break": func(b string) string {
			return strings.Replace(b, "<br>\n"+releaseStart, "<br>\n<br>\n"+releaseStart, 1)
		},
		"claim before release": func(b string) string {
			return strings.Replace(strings.Replace(b, standingHistoricalCheckpointTest, "", 1), releaseStart, standingHistoricalCheckpointTest+"\n"+releaseStart, 1)
		},
		"claim after release": func(b string) string {
			return strings.Replace(strings.Replace(b, standingHistoricalCheckpointTest, "", 1), releaseEnd, releaseEnd+"\n"+standingHistoricalCheckpointTest, 1)
		},
		"claim at root": func(b string) string {
			return strings.Replace(b, standingHistoricalCheckpointTest, "", 1) + "\n" + standingHistoricalCheckpointTest
		},
		"claim in recent": func(b string) string {
			return strings.Replace(strings.Replace(b, standingHistoricalCheckpointTest, "", 1), standingRecentEnd, standingHistoricalCheckpointTest+"\n"+standingRecentEnd, 1)
		},
		"claim in risk": func(b string) string {
			return strings.Replace(strings.Replace(b, standingHistoricalCheckpointTest, "", 1), "<!-- final_review_risk_end -->", standingHistoricalCheckpointTest+"\n<!-- final_review_risk_end -->", 1)
		},
		"nested claim": func(b string) string {
			return strings.Replace(b, standingHistoricalCheckpointTest, "<section>\n"+standingHistoricalCheckpointTest+"\n</section>", 1)
		},
		"quoted claim": func(b string) string {
			return strings.Replace(b, standingHistoricalCheckpointTest, "> "+standingHistoricalCheckpointTest, 1)
		},
		"bold claim": func(b string) string {
			return strings.Replace(b, standingHistoricalCheckpointTest, "**"+standingHistoricalCheckpointTest+"**", 1)
		},
		"duplicate claim": func(b string) string {
			return strings.Replace(b, standingHistoricalCheckpointTest, standingHistoricalCheckpointTest+"\n"+standingHistoricalCheckpointTest, 1)
		},
		"duplicate entire walkthrough": func(b string) string {
			start := strings.Index(b, "<!-- walkthrough_start -->")
			end := strings.Index(b, "<!-- walkthrough_end -->") + len("<!-- walkthrough_end -->")
			return b + "\n" + b[start:end]
		},
	}
	for name, replacement := range map[string]string{
		"current checkpoint":                    strings.Replace(standingHistoricalCheckpointTest, "latest reported Hosted review checkpoint", "current Hosted review", 1),
		"missing reported qualifier":            strings.Replace(standingHistoricalCheckpointTest, "reported ", "", 1),
		"missing current unavailable qualifier": strings.Replace(standingHistoricalCheckpointTest, " Finding details and current review findings are not available.", "", 1),
		"altered unavailable qualifier":         strings.Replace(standingHistoricalCheckpointTest, "not available", "available", 1),
		"truncated qualifier":                   strings.TrimSuffix(standingHistoricalCheckpointTest, "available."),
		"unknown prefix":                        "Current review inventory: " + standingHistoricalCheckpointTest,
		"unknown suffix":                        standingHistoricalCheckpointTest + " Current findings: 2",
		"count mismatch":                        strings.Replace(standingHistoricalCheckpointTest, "3 accepted", "4 accepted", 1),
		"major count mismatch":                  strings.Replace(standingHistoricalCheckpointTest, "0 major", "1 major", 1),
		"positive routed":                       strings.Replace(standingHistoricalCheckpointTest, "0 routed", "1 routed", 1),
		"leading zero":                          strings.Replace(standingHistoricalCheckpointTest, "3 accepted", "03 accepted", 1),
		"negative count":                        strings.Replace(standingHistoricalCheckpointTest, "3 accepted", "-3 accepted", 1),
		"overflow count":                        strings.Replace(standingHistoricalCheckpointTest, "3 accepted", "18446744073709551616 accepted", 1),
		"bounded total exceeded":                strings.Replace(strings.Replace(standingHistoricalCheckpointTest, "3 accepted", "101 accepted", 1), "2 minor", "100 minor", 1),
	} {
		changes[name] = func(b string) string { return strings.Replace(b, standingHistoricalCheckpointTest, replacement, 1) }
	}
	for _, claim := range []string{
		"findings: 1", "comments: 2", "Current findings: 1", "Current review findings: 0",
		"The current review has findings: 2.", "**Actionable comments posted: 1**", "**Actionable comments posted: 0**",
		"<details>\n<summary>Unknown findings (1)</summary>\n</details>", "<summary>Duplicate comments (1)</summary>",
		hfMarker(1), hfOutsideCategory(1, hfOutside(1, "pkg/alpha.go", "1", "Synthetic issue.")),
	} {
		for _, where := range []string{"release", "after walkthrough", "risk", "root"} {
			changes[where+"/"+claim] = func(b string) string {
				switch where {
				case "release":
					return strings.Replace(b, releaseEnd, claim+"\n"+releaseEnd, 1)
				case "after walkthrough":
					return strings.Replace(b, "<!-- walkthrough_end -->", "<!-- walkthrough_end -->\n"+claim, 1)
				case "risk":
					return strings.Replace(b, "<!-- final_review_risk_end -->", claim+"\n<!-- final_review_risk_end -->", 1)
				default:
					return b + "\n" + claim
				}
			}
		}
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			body := standingTestHistoricalCheckpoint(s.IssueComments[1].Body)
			s.IssueComments[1].Body = change(body)
			if s.IssueComments[1].Body == body {
				t.Fatal("negative fixture did not change source")
			}
			before := append([]IssueComment(nil), s.IssueComments...)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Publication.StandingZero != nil || got.Publication.State == "attributed" {
				t.Fatalf("unsupported narrative or contradictory current claim became clean: complete=%v reason=%s err=%v", got.Complete, got.Reason, err)
			}
			if !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatal("negative classification rewrote source")
			}
		})
	}
}

func TestHostedStandingZeroHistoricalCheckpointCountAndPresentation(t *testing.T) {
	for _, mode := range []string{"zero counts", "major count", "maximum count", "no break", "CRLF", "separate status"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			body := standingTestHistoricalCheckpoint(s.IssueComments[1].Body)
			switch mode {
			case "zero counts":
				body = strings.Replace(strings.Replace(strings.Replace(body, "3 accepted", "0 accepted", 1), "2 minor", "0 minor", 1), "1 trivial", "0 trivial", 1)
			case "major count":
				body = strings.Replace(strings.Replace(body, "3 accepted", "4 accepted", 1), "0 major", "1 major", 1)
			case "maximum count":
				body = strings.Replace(strings.Replace(body, "3 accepted", "100 accepted", 1), "2 minor", "99 minor", 1)
			case "no break":
				body = strings.Replace(body, "<br>\n", "", 1)
			case "CRLF":
				body = strings.ReplaceAll(body, "\n", "\r\n")
			case "separate status":
				body = strings.Replace(body, "No actionable comments were generated in the recent review. 🎉\n", "", 1)
				body = standingStatusStart + "\nNo actionable comments were generated in the recent review.\n" + standingStatusEnd + "\n" + body
			}
			s.IssueComments[1].Body = body
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || !got.Complete || got.Publication.StandingZero == nil || len(got.Findings) != 0 {
				t.Fatalf("bounded historical variation lost current proof: complete=%v reason=%s err=%v", got.Complete, got.Reason, err)
			}
		})
	}
}

func TestHostedStandingZeroHistoricalCheckpointPreservesBaselineProof(t *testing.T) {
	changes := map[string]func(*HostedPublicationSnapshot, *HostedPublicationSnapshot){
		"valid original root annotation": func(_, _ *HostedPublicationSnapshot) {},
		"missing baseline":               func(_, b *HostedPublicationSnapshot) { *b = HostedPublicationSnapshot{} },
		"incomplete baseline":            func(_, b *HostedPublicationSnapshot) { b.Complete = false },
		"changed root body":              func(s, _ *HostedPublicationSnapshot) { s.ReviewComments[0].Body += "\nNew finding." },
		"missing original root":          func(_, b *HostedPublicationSnapshot) { b.ReviewComments = []ReviewComment{} },
		"changed root actor":             func(_, b *HostedPublicationSnapshot) { b.ReviewComments[0].Author.ID++ },
		"changed review body":            func(s, _ *HostedPublicationSnapshot) { s.Reviews[0].Body += "\nChanged inventory." },
		"missing ack":                    func(s, _ *HostedPublicationSnapshot) { s.IssueComments = s.IssueComments[:2] },
		"foreign ack actor":              func(s, _ *HostedPublicationSnapshot) { s.IssueComments[2].Author.ID++ },
		"foreign summary actor":          func(s, _ *HostedPublicationSnapshot) { s.IssueComments[1].Author.ID++ },
		"late summary":                   func(s, _ *HostedPublicationSnapshot) { s.IssueComments[1].UpdatedAt = s.ObservedAt.Add(time.Second) },
		"incomplete capture":             func(s, _ *HostedPublicationSnapshot) { s.Complete = false },
		"missing source head": func(s, _ *HostedPublicationSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, " and "+strings.Repeat("c", 40)+".", " and "+strings.Repeat("d", 40)+".", 1)
		},
		"missing run": func(s, _ *HostedPublicationSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, standingTestRun, "not-a-run", 1)
		},
		"reused historical run": func(s, _ *HostedPublicationSnapshot) {
			old := s.IssueComments[1]
			old.ID += 100
			old.UpdatedAt = old.CreatedAt
			s.IssueComments = append(s.IssueComments, old)
		},
		"unknown submitted inventory": func(s, _ *HostedPublicationSnapshot) {
			review := s.Reviews[0]
			review.ID += 100
			review.HeadSHA = strings.Repeat("c", 40)
			review.SubmittedAt = s.IssueComments[1].UpdatedAt
			review.Body = "Unknown current review inventory."
			s.Reviews = append(s.Reviews, review)
		},
		"positive submitted inventory": func(s, _ *HostedPublicationSnapshot) {
			review := s.Reviews[0]
			review.ID += 100
			review.HeadSHA = strings.Repeat("c", 40)
			review.SubmittedAt = s.IssueComments[1].UpdatedAt
			review.Body = "**Actionable comments posted: 1**"
			s.Reviews = append(s.Reviews, review)
		},

		"duplicate recent": func(s, _ *HostedPublicationSnapshot) { s.IssueComments[1].Body += "\n" + s.IssueComments[1].Body },
		"duplicate risk": func(s, _ *HostedPublicationSnapshot) {
			body := s.IssueComments[1].Body
			s.IssueComments[1].Body += "\n" + body[strings.LastIndex(body, "<details>\n<summary><strong>Merge Risk:"):]
		},
		"duplicate status": func(s, _ *HostedPublicationSnapshot) {
			s.IssueComments[1].Body += "\n" + standingStatusStart + "\nNo actionable comments were generated in the recent review.\n" + standingStatusEnd
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, b, intent, poster, bot, earliest := annotationBaselineFixture(t)
			s.IssueComments[1].Body = standingTestHistoricalCheckpoint(s.IssueComments[1].Body)
			change(&s, &b)
			before, beforeBase := append([]IssueComment(nil), s.IssueComments...), slices.Clone(b.ReviewComments)
			strict, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || strict.Complete {
				t.Fatal("historical prose bypassed original-root baseline requirement", strict.Reason, err)
			}
			got, err := ExtractHostedFindingsWithBaseline(s, b, intent, poster, bot, earliest)
			want := name == "valid original root annotation"
			if err != nil || got.Complete != want || (got.Publication.StandingZero != nil) != want {
				t.Fatalf("historical prose changed baseline proof: complete=%v reason=%s err=%v", got.Complete, got.Reason, err)
			}
			if !reflect.DeepEqual(before, s.IssueComments) || !reflect.DeepEqual(beforeBase, b.ReviewComments) {
				t.Fatal("baseline proof rewrote original captures")
			}
		})
	}
}

func TestHostedStandingZeroHistoricalCheckpointEnvelopeContinuity(t *testing.T) {
	const releaseStart = "<!-- This is an auto-generated comment: release notes by coderabbit.ai -->"
	const releaseEnd = "<!-- end of auto-generated comment: release notes by coderabbit.ai -->"
	for name, change := range map[string]func(string) string{
		"reviewer sibling dd immediately after release start": func(b string) string {
			return strings.Replace(b, releaseStart, releaseStart+"\n</dd><dd>", 1)
		},
		"reviewer differently labelled root immediately after release start": func(b string) string {
			return strings.Replace(b, releaseStart, releaseStart+"\n</dd></dl></details><details><summary><strong>Current review findings</strong></summary><dl><dd>", 1)
		},
		"reviewer unrelated root immediately after release end": func(b string) string {
			return strings.Replace(b, releaseEnd, releaseEnd+"\n</dd></dl></details>\n<details>\n<summary><strong>Unrelated section</strong></summary>\n<dl>\n<dd>", 1)
		},

		"sibling dd before claim": func(b string) string {
			return strings.Replace(b, standingHistoricalCheckpointTest, "</dd><dd>\n"+standingHistoricalCheckpointTest, 1)
		},
		"sibling dd before release end": func(b string) string {
			return strings.Replace(b, releaseEnd, "</dd><dd>\n"+releaseEnd, 1)
		},
		"sibling dl before claim": func(b string) string {
			return strings.Replace(b, standingHistoricalCheckpointTest, "</dd></dl><dl><dd>\n"+standingHistoricalCheckpointTest, 1)
		},
		"sibling Summary before claim": func(b string) string {
			return strings.Replace(b, standingHistoricalCheckpointTest, "</dd></dl></details><details><summary><strong>Summary</strong></summary><dl><dd>\n"+standingHistoricalCheckpointTest, 1)
		},
		"sibling dd after release end": func(b string) string {
			return strings.Replace(b, releaseEnd, releaseEnd+"\n</dd><dd>", 1)
		},
		"sibling dl after release end": func(b string) string {
			return strings.Replace(b, releaseEnd, releaseEnd+"\n</dd></dl><dl><dd>", 1)
		},
		"sibling Summary after release end": func(b string) string {
			return strings.Replace(b, releaseEnd, releaseEnd+"\n</dd></dl></details><details><summary><strong>Summary</strong></summary><dl><dd>", 1)
		},
		"closed Summary then root prose and new Summary": func(b string) string {
			return strings.Replace(b, releaseEnd, releaseEnd+"\n</dd></dl></details>\nProse outside the owning Summary.\n<details>\n<summary><strong>Summary</strong></summary>\n<dl>\n<dd>", 1)
		},
		"closed Summary then new labelled root": func(b string) string {
			return strings.Replace(b, releaseEnd, releaseEnd+"\n</dd></dl></details>\n<details>\n<summary><strong>Different result</strong></summary>\n<dl>\n<dd>", 1)
		},
		"sibling dd before release start": func(b string) string {
			return strings.Replace(b, releaseStart, "</dd><dd>\n"+releaseStart, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.IssueComments[1].Body = change(standingTestHistoricalCheckpoint(s.IssueComments[1].Body))
			before := slices.Clone(s.IssueComments)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Complete || got.Publication.StandingZero != nil || got.Publication.State == "attributed" {
				t.Fatalf("a historical exemption crossed its owning Summary/body: complete=%v reason=%s err=%v", got.Complete, got.Reason, err)
			}
			if !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatal("continuity rejection changed source")
			}
		})
	}
}
