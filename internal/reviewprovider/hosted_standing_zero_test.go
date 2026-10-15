package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const standingTestOldRun = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func standingTestBody(head, run string) string {
	return "Historical section with another Run ID and finding text.\n" + standingStart + "\n\nNo actionable comments were generated in the recent review. 🎉\n\n<details>\n<summary><strong>ℹ️ Recent review info</strong></summary>\n<dl>\n<dd>\n<details>\n<summary><strong>⚙️ Run configuration</strong></summary>\n<dl>\n<dd>\n- **Configuration used**: Repository: example/project/.coderabbit.yaml\n- **Review profile**: ASSERTIVE\n- **Plan**: Advanced\n- **Run ID**: `" + run + "`\n</dd>\n</dl>\n</details>\n<details>\n<summary><strong>📥 Commits</strong></summary>\n<dl>\n<dd>\n" + hostedScopeDeclaration(strings.Repeat("b", 40), head) + "\n</dd>\n</dl>\n</details>\n<details>\n<summary><strong>⛔ Files ignored due to path filters: 1</strong></summary>\n<dl>\n<dd>\n* `generated.js` is excluded by `!generated.js`\n</dd>\n</dl>\n</details>\n<details>\n<summary><strong>📒 Files selected for processing: 1</strong></summary>\n<dl>\n<dd>\n* `source.go`\n</dd>\n</dl>\n</details>\n</dd>\n</dl>\n</details>\n" + "**Included review availability:** This review used your included allowance. Your plan provides up to 1 included review per hour; 0 remain after this review.\n" + standingEnd + "\nHistorical unrelated declaration: " + strings.Repeat("d", 40)
}

func standingTestFixture(t *testing.T) (HostedPublicationSnapshot, *HostedPublicationBaseline, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	intent, poster, earliest, observed, trigger := triggerFixture(t)
	bot := Author{ID: 999, Kind: "Bot", Login: "coderabbitai[bot]"}
	summary := IssueComment{ID: 100, Author: bot, Body: standingTestBody(intent.HeadSHA, publicationTestID), CreatedAt: earliest.Add(-time.Hour), UpdatedAt: trigger.CreatedAt.Add(4 * time.Second)}
	finish := IssueComment{ID: 101, Author: bot, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("a", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\n\nFull review finished.\n\n</details>", CreatedAt: trigger.CreatedAt.Add(time.Second), UpdatedAt: trigger.CreatedAt.Add(5 * time.Second)}
	old := summary
	old.Body = strings.ReplaceAll(old.Body, publicationTestID, standingTestOldRun)
	old.UpdatedAt = trigger.CreatedAt.Add(-time.Second)
	baseline := &HostedPublicationBaseline{Snapshot: HostedPublicationSnapshot{IssueComments: []IssueComment{trigger, old}, Reviews: []HostedReview{}, ReviewComments: []ReviewComment{}, Complete: true, ObservedAt: trigger.CreatedAt.Add(2 * time.Second)}, StartedAt: trigger.CreatedAt.Add(time.Second)}
	s := HostedPublicationSnapshot{IssueComments: []IssueComment{trigger, summary, finish}, Reviews: []HostedReview{}, ReviewComments: []ReviewComment{}, Complete: true, ObservedAt: observed}
	return s, baseline, intent, poster, bot, earliest
}

func TestHostedStandingZeroAttributedRecentScopeAndNoInventedReview(t *testing.T) {
	s, b, intent, poster, bot, earliest := standingTestFixture(t)
	before := append([]IssueComment(nil), s.IssueComments...)
	p, err := ResolveHostedPublicationWithBaseline(s, intent, poster, bot, earliest, b)
	if err != nil || p.State != "attributed" || p.StandingZero == nil {
		t.Fatalf("publication %s/%s: %v", p.State, p.Reason, err)
	}
	if p.StandingZero.CommentID != 100 || p.StandingZero.CompletionCommentID != 101 || p.StandingZero.RunID != publicationTestID || p.PublicationID != "" || p.ProviderAttemptID != "" || len(p.Batches) != 0 || len(p.Roots) != 0 || !p.FinishedAt.Equal(s.IssueComments[2].UpdatedAt) {
		t.Fatal("fabricated/lost source identity")
	}
	base, ok := ResolveHostedComparison(p, intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("e", 40))
	if !ok || base != strings.Repeat("b", 40) {
		t.Fatal("recent comparison unavailable")
	}
	if _, ok = ResolveHostedComparison(p, intent.HeadSHA, strings.Repeat("e", 40), strings.Repeat("f", 40)); ok {
		t.Fatal("foreign base credited")
	}
	inventory, err := ExtractHostedFindingsWithBaseline(s, intent, poster, bot, earliest, b)
	if err != nil || !inventory.Complete || inventory.Findings == nil || len(inventory.Findings) != 0 {
		t.Fatalf("inventory %+v %v", inventory, err)
	}
	if !reflect.DeepEqual(before, s.IssueComments) {
		t.Fatal("source mutated")
	}
	// The old API cannot silently manufacture the missing earlier observation.
	inventory, err = ExtractHostedFindings(s, intent, poster, bot, earliest)
	if err != nil || inventory.Complete || inventory.Reason != "standing_zero_baseline_missing" {
		t.Fatalf("missing baseline %+v %v", inventory, err)
	}
	for _, finishBody := range []string{"Full review finished.", "<!-- This is an auto-generated reply by CodeRabbit -->\n<details>\n<summary>✅ Action performed</summary>\nFull review finished.\n</details>"} {
		s.IssueComments[2].Body = finishBody
		inventory, err = ExtractHostedFindingsWithBaseline(s, intent, poster, bot, earliest, b)
		if err != nil || !inventory.Complete {
			t.Fatalf("observed finish grammar rejected: %v %s", err, inventory.Reason)
		}
	}
}

func TestHostedStandingZeroWindowAndCompleteInventoryGuards(t *testing.T) {
	tests := map[string]func(*HostedPublicationSnapshot){
		"partial":         func(s *HostedPublicationSnapshot) { s.Complete = false },
		"nil reviews":     func(s *HostedPublicationSnapshot) { s.Reviews = nil },
		"nil inline":      func(s *HostedPublicationSnapshot) { s.ReviewComments = nil },
		"missing trigger": func(s *HostedPublicationSnapshot) { s.IssueComments = s.IssueComments[1:] },
		"edited trigger": func(s *HostedPublicationSnapshot) {
			s.IssueComments[0].UpdatedAt = s.IssueComments[0].UpdatedAt.Add(time.Second)
		},
		"no finish":            func(s *HostedPublicationSnapshot) { s.IssueComments = s.IssueComments[:2] },
		"foreign finish":       func(s *HostedPublicationSnapshot) { s.IssueComments[2].Author.ID++ },
		"foreign standing":     func(s *HostedPublicationSnapshot) { s.IssueComments[1].Author.Kind = "User" },
		"finish same second":   func(s *HostedPublicationSnapshot) { s.IssueComments[2].CreatedAt = s.IssueComments[0].CreatedAt },
		"standing same second": func(s *HostedPublicationSnapshot) { s.IssueComments[1].UpdatedAt = s.IssueComments[0].CreatedAt },
		"edit after finish": func(s *HostedPublicationSnapshot) {
			s.IssueComments[1].UpdatedAt = s.IssueComments[2].UpdatedAt.Add(time.Second)
		},
		"duplicate summary": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[1]
			c.ID++
			c.ID += 10
			s.IssueComments = append(s.IssueComments, c)
		},
		"duplicate finish": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[2]
			c.ID += 10
			s.IssueComments = append(s.IssueComments, c)
		},
		"unknown appended finish": func(s *HostedPublicationSnapshot) { s.IssueComments[2].Body += "\nExtra reply prose" },
		"unknown invocation": func(s *HostedPublicationSnapshot) {
			s.IssueComments[2].Body = strings.ReplaceAll(s.IssueComments[2].Body, "v2:", "v3:")
		},
		"other bot output": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[2]
			c.ID += 10
			c.Body = "Unclassified provider output"
			s.IssueComments = append(s.IssueComments, c)
		},
		"edited old bot output": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[2]
			c.ID += 10
			c.CreatedAt = s.IssueComments[1].CreatedAt
			c.Body = "Previous finding edited"
			s.IssueComments = append(s.IssueComments, c)
		},
		"pending review with unknown time": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[2]
			s.Reviews = []HostedReview{{ID: 200, Author: c.Author, HeadSHA: strings.Repeat("c", 40), State: "pending"}}
		},
		"eligible review": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[2]
			s.Reviews = []HostedReview{{ID: 200, Author: c.Author, Body: "markerless", HeadSHA: strings.Repeat("c", 40), State: "commented", SubmittedAt: c.UpdatedAt}}
		},
		"old inline edited": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[2]
			s.ReviewComments = []ReviewComment{{ID: 200, ReviewID: 300, Author: c.Author, Body: "old root", Path: "source.go", HeadSHA: strings.Repeat("c", 40), OriginalHeadSHA: strings.Repeat("c", 40), CreatedAt: s.IssueComments[1].CreatedAt, UpdatedAt: c.UpdatedAt}}
		},
		"competing command": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[0]
			c.ID += 20
			c.CreatedAt = c.CreatedAt.Add(3 * time.Second)
			c.UpdatedAt = c.CreatedAt
			c.Body = "@coderabbitai full review"
			s.IssueComments = append(s.IssueComments, c)
		},
		"old foreign edit": func(s *HostedPublicationSnapshot) {
			c := s.IssueComments[0]
			c.ID += 20
			c.CreatedAt = c.CreatedAt.Add(-time.Hour)
			c.UpdatedAt = s.IssueComments[1].UpdatedAt
			c.Body = "Removed command"
			s.IssueComments = append(s.IssueComments, c)
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s, b, i, p, a, e := standingTestFixture(t)
			change(&s)
			got, err := ExtractHostedFindingsWithBaseline(s, i, p, a, e, b)
			if err != nil || got.Complete || got.Publication.StandingZero != nil {
				t.Fatalf("credited %s/%s err%v", got.Publication.State, got.Reason, err)
			}
		})
	}
}

func TestHostedStandingZeroRecentGrammarCannotBorrowHistoricalOrNestedFacts(t *testing.T) {
	changes := map[string]func(string) string{
		"missing zero": func(s string) string {
			return strings.ReplaceAll(s, "No actionable comments were generated in the recent review. 🎉", "")
		},
		"positive claim": func(s string) string {
			return strings.ReplaceAll(s, standingEnd, "2 actionable comments posted\n"+standingEnd)
		},
		"omissions": func(s string) string {
			return strings.ReplaceAll(s, standingEnd, "Files not reviewed: 1\n"+standingEnd)
		},
		"unknown category": func(s string) string {
			return strings.ReplaceAll(s, "Files selected for processing: 1", "Files not processed: 1")
		},
		"duplicate run": func(s string) string {
			return strings.ReplaceAll(s, "- **Run ID**:", "- **Run ID**: `"+publicationTestID+"`\n- **Run ID**:")
		},
		"uppercase run": func(s string) string {
			return strings.ReplaceAll(s, publicationTestID, strings.ToUpper(publicationTestID))
		},
		"missing run": func(s string) string { return strings.ReplaceAll(s, "- **Run ID**: `"+publicationTestID+"`\n", "") },
		"wrong head":  func(s string) string { return strings.ReplaceAll(s, strings.Repeat("c", 40), strings.Repeat("d", 40)) },
		"missing comparison": func(s string) string {
			return strings.ReplaceAll(s, hostedScopeDeclaration(strings.Repeat("b", 40), strings.Repeat("c", 40)), "")
		},
		"nested proposal":    func(s string) string { return strings.ReplaceAll(s, "📥 Commits", "Proposed wording") },
		"duplicate sections": func(s string) string { return s + "\n" + s },
		"quoted":             func(s string) string { return "> " + strings.ReplaceAll(s, "\n", "\n> ") },
		"fenced":             func(s string) string { return "```\n" + s + "\n```" },
		"pre":                func(s string) string { return "<pre>\n" + s + "\n</pre>" },
		"blockquote":         func(s string) string { return "<blockquote>\n" + s + "\n</blockquote>" },
		"hidden":             func(s string) string { return "<!-- example\n" + s + "\n-->" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, b, i, p, a, e := standingTestFixture(t)
			s.IssueComments[1].Body = change(s.IssueComments[1].Body)
			got, err := ExtractHostedFindingsWithBaseline(s, i, p, a, e, b)
			if err != nil || got.Complete {
				t.Fatalf("unsupported source credited %s %v", got.Reason, err)
			}
		})
	}
}

func TestHostedStandingZeroBaselineClosesStaleSameHeadCounterexample(t *testing.T) {
	changes := map[string]func(*HostedPublicationBaseline){
		"same old run": func(b *HostedPublicationBaseline) {
			b.Snapshot.IssueComments[1].Body = strings.ReplaceAll(b.Snapshot.IssueComments[1].Body, standingTestOldRun, publicationTestID)
		},
		"missing collection":    func(b *HostedPublicationBaseline) { b.Snapshot.Reviews = nil },
		"partial":               func(b *HostedPublicationBaseline) { b.Snapshot.Complete = false },
		"pretrigger start":      func(b *HostedPublicationBaseline) { b.StartedAt = b.Snapshot.IssueComments[0].CreatedAt },
		"observed before start": func(b *HostedPublicationBaseline) { b.StartedAt = b.Snapshot.ObservedAt.Add(time.Second) },
		"not earlier than edit": func(b *HostedPublicationBaseline) {
			b.Snapshot.ObservedAt = b.Snapshot.IssueComments[0].CreatedAt.Add(4 * time.Second)
		},
		"wrong trigger": func(b *HostedPublicationBaseline) { b.Snapshot.IssueComments[0].Body += " altered" },
		"wrong actor":   func(b *HostedPublicationBaseline) { b.Snapshot.IssueComments[0].Author.ID++ },
		"malformed prior recent": func(b *HostedPublicationBaseline) {
			b.Snapshot.IssueComments[1].Body = strings.ReplaceAll(b.Snapshot.IssueComments[1].Body, standingEnd, "")
		},
		"duplicate prior run": func(b *HostedPublicationBaseline) {
			b.Snapshot.IssueComments[1].Body = strings.ReplaceAll(b.Snapshot.IssueComments[1].Body, "- **Run ID**:", "- **Run ID**: `"+standingTestOldRun+"`\n- **Run ID**:")
		},
		"prior finish": func(b *HostedPublicationBaseline) {
			c := b.Snapshot.IssueComments[0]
			c.ID = 150
			c.Author = b.Snapshot.IssueComments[1].Author
			c.Body = "Full review finished."
			c.CreatedAt = b.StartedAt
			c.UpdatedAt = c.CreatedAt
			b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, c)
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, b, i, p, a, e := standingTestFixture(t)
			change(b)
			got, err := ExtractHostedFindingsWithBaseline(s, i, p, a, e, b)
			if err != nil || got.Complete {
				t.Fatalf("bad baseline credited %s %v", got.Reason, err)
			}
		})
	}
	for _, absent := range []bool{false, true} {
		s, b, i, p, a, e := standingTestFixture(t)
		if absent {
			b.Snapshot.IssueComments = b.Snapshot.IssueComments[:1]
		} else {
			b.Snapshot.IssueComments[1].Body = "Existing standing comment without a recent section"
		}
		got, err := ExtractHostedFindingsWithBaseline(s, i, p, a, e, b)
		if err != nil || !got.Complete {
			t.Fatalf("proven prior absence failed %s %v", got.Reason, err)
		}
	}
}

func TestHostedStandingZeroAvailabilityIsOnlyBoundedDecoration(t *testing.T) {
	for _, counts := range []string{"1 included review per hour; 0", "6 included reviews per hour; 5", "999999 included reviews per hour; 999999"} {
		s, b, i, p, a, e := standingTestFixture(t)
		s.IssueComments[1].Body = strings.ReplaceAll(s.IssueComments[1].Body, "1 included review per hour; 0", counts)
		got, err := ExtractHostedFindingsWithBaseline(s, i, p, a, e, b)
		if err != nil || !got.Complete {
			t.Fatalf("decoration rejected %s %v", got.Reason, err)
		}
	}
	for _, change := range []func(string) string{
		func(s string) string {
			return strings.ReplaceAll(s, "0 remain after this review.", "0 remain after this review. Unknown appended assertion.")
		},
		func(s string) string {
			return strings.ReplaceAll(s, "1 included review per hour", "1000000 included reviews per hour")
		},
		func(s string) string {
			return strings.ReplaceAll(s, "This review used your included allowance.", "This review used a different allowance.")
		},
		func(s string) string {
			return strings.ReplaceAll(s, "- **Plan**: Advanced", "- **Plan**: Advanced; 2 actionable findings")
		},
	} {
		s, b, i, p, a, e := standingTestFixture(t)
		s.IssueComments[1].Body = change(s.IssueComments[1].Body)
		got, err := ExtractHostedFindingsWithBaseline(s, i, p, a, e, b)
		if err != nil || got.Complete {
			t.Fatalf("unknown prose credited %s %v", got.Reason, err)
		}
	}
}

func TestHostedStandingZeroBaselineImmutableCommentAndVisibility(t *testing.T) {
	for _, change := range []func(*HostedPublicationBaseline){
		func(b *HostedPublicationBaseline) { b.Snapshot.IssueComments[1].Author.ID++ },
		func(b *HostedPublicationBaseline) {
			b.Snapshot.IssueComments[1].CreatedAt = b.Snapshot.IssueComments[1].CreatedAt.Add(time.Second)
		},
		func(b *HostedPublicationBaseline) {
			b.Snapshot.IssueComments[1].Body = strings.ReplaceAll(b.Snapshot.IssueComments[1].Body, standingStart, strings.ToUpper(standingStart))
		},
		func(b *HostedPublicationBaseline) {
			c := b.Snapshot.IssueComments[0]
			c.ID += 20
			c.Body = "@coderabbitai full review"
			c.CreatedAt = b.StartedAt
			c.UpdatedAt = c.CreatedAt
			b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, c)
		},
	} {
		s, b, i, p, a, e := standingTestFixture(t)
		change(b)
		got, err := ExtractHostedFindingsWithBaseline(s, i, p, a, e, b)
		if err != nil || got.Complete {
			t.Fatalf("mutable/ambiguous baseline credited %s %v", got.Reason, err)
		}
	}
	for _, wrap := range []func(string) string{
		func(s string) string { return "<details>\n<summary>Example</summary>\n" + s + "\n</details>" },
		func(s string) string { return "FC_STANDING_START\n<!-- hidden\n" + s + "\n-->\nFC_STANDING_END" },
	} {
		s, b, i, p, a, e := standingTestFixture(t)
		s.IssueComments[1].Body = wrap(s.IssueComments[1].Body)
		got, err := ExtractHostedFindingsWithBaseline(s, i, p, a, e, b)
		if err != nil || got.Complete {
			t.Fatalf("hidden section credited %s %v", got.Reason, err)
		}
	}
}
