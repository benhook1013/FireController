package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func botRefreshFixture(snapshot HostedPublicationSnapshot) (HostedPublicationSnapshot, HostedPublicationSnapshot) {
	trigger := snapshot.IssueComments[0].CreatedAt
	old := IssueComment{ID: 901, Author: Author{ID: 902, Login: "synthetic-status[bot]", Kind: "Bot"}, CreatedAt: trigger.Add(-time.Hour), UpdatedAt: trigger.Add(-time.Minute), Body: "Build pending.\n| component | status |\n| runtime | pending |"}
	baseline := HostedPublicationSnapshot{Complete: true, ObservedAt: trigger.Add(-time.Second), IssueComments: []IssueComment{old}, Reviews: []HostedReview{}, ReviewComments: []ReviewComment{}}
	current := old
	current.UpdatedAt = trigger.Add(time.Second)
	current.Body = "Build completed successfully.\nUpdated report: runtime and UI passed; coverage increased."
	snapshot.IssueComments = append(snapshot.IssueComments, current)
	return snapshot, baseline
}

func batchedFiveBotFixture(t *testing.T) hfFixture {
	t.Helper()
	f := hfNew(t, []string{hfInline(1), hfInline(2), hfInline(3), hfInline(4), hfInline(5)}, "")
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
	second := f.snapshot.Reviews[0]
	second.ID = 402
	second.Body = "**Actionable comments posted: 2**\n\n<!-- coderabbit-review-publication v1 publication=11111111-1111-1111-1111-111111111111 attempt=22222222-2222-2222-2222-222222222222 batch=2/2 -->"
	f.snapshot.Reviews = append(f.snapshot.Reviews, second)
	for i := 3; i < 5; i++ {
		f.snapshot.ReviewComments[i].ReviewID = second.ID
	}
	return f
}

func TestHostedBaselineBotRefreshPreservesCompleteInventory(t *testing.T) {
	for _, kind := range []string{"standing_zero", "submitted_positive", "batched_five"} {
		for _, edit := range []string{"generated_report", "same_body", "after_ack", "multiple_reports", "plural_sources"} {
			t.Run(kind+"/"+edit, func(t *testing.T) {
				s, intent, poster, bot, earliest := standingTestFixture(t)
				if kind != "standing_zero" {
					f := submittedTestFixture(t)
					if kind == "batched_five" {
						f = batchedFiveBotFixture(t)
					}
					s, intent, poster, bot, earliest = f.snapshot, f.intent, f.poster, f.bot, f.earliest
				}
				if edit == "plural_sources" && kind != "standing_zero" {
					s.ReviewComments[0].Body += "\n\n_Sources: Coding guidelines, Learnings_"
				}
				want, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
				if err != nil || !want.Complete {
					t.Fatal("fixture", want.Reason, err)
				}
				if kind == "batched_five" && (len(want.Findings) != 5 || len(want.Publication.Batches) != 2) {
					t.Fatal("not five findings in two real batches")
				}
				s, baseline := botRefreshFixture(s)
				last := len(s.IssueComments) - 1
				switch edit {
				case "same_body":
					s.IssueComments[last].Body = baseline.IssueComments[0].Body
				case "after_ack":
					s.IssueComments[last].UpdatedAt = s.ObservedAt
				case "multiple_reports":
					old := baseline.IssueComments[0]
					old.ID += 10
					old.Author.ID += 10
					old.Author.Login = "synthetic-other[bot]"
					baseline.IssueComments = append(baseline.IssueComments, old)
					current := old
					current.Body = "A second entirely rewritten status report."
					current.UpdatedAt = s.ObservedAt
					s.IssueComments = append(s.IssueComments, current)
				}
				before := append([]IssueComment(nil), s.IssueComments...)
				strict, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
				if err != nil || strict.Complete || strict.Reason != "attribution_window_closed" {
					t.Fatal("strict guard changed", strict.Reason, err)
				}
				got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
				if err != nil || !got.Complete || !reflect.DeepEqual(got.Findings, want.Findings) || !reflect.DeepEqual(got.Publication.Roots, want.Publication.Roots) || !reflect.DeepEqual(got.Publication.Batches, want.Publication.Batches) || !got.Publication.FinishedAt.Equal(want.Publication.FinishedAt) {
					t.Fatal("lost complete original inventory", got.Reason, err)
				}
				if !reflect.DeepEqual(s.IssueComments, before) {
					t.Fatal("raw source mutated")
				}
			})
		}
	}
}

func TestHostedBaselineBotRefreshRefusesUnprovenEdits(t *testing.T) {
	for _, mode := range []string{"hidden_command", "removed_command", "fenced_command", "human", "human_same_body", "posting_bot", "author_id", "author_login", "author_kind", "comment_id", "created", "new_bot", "newly_created_bot", "stale_baseline", "old_updated_after_baseline", "incomplete_baseline", "missing_baseline_collection", "missing_current_collection", "future_update", "unknown_inventory", "later_human_command"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s, baseline := botRefreshFixture(s)
			last := len(s.IssueComments) - 1
			current := &s.IssueComments[last]
			old := &baseline.IssueComments[0]
			switch mode {
			case "hidden_command":
				current.Body += "<!-- CoDeRaBbIt -->"
			case "removed_command":
				old.Body += "<!-- coderabbit -->"
			case "fenced_command":
				current.Body += "\n```\n@CoDeRaBbItAi review\n```"
			case "human", "human_same_body":
				current.Author.Kind = "User"
				old.Author.Kind = "User"
				if mode == "human_same_body" {
					current.Body = old.Body
				}
			case "posting_bot":
				poster.Kind = "Bot"
				s.IssueComments[0].Author = poster
				current.Author = poster
				old.Author = poster
			case "author_id":
				current.Author.ID++
			case "author_login":
				current.Author.Login = "synthetic-renamed[bot]"
			case "author_kind":
				current.Author.Kind = "User"
			case "comment_id":
				current.ID++
			case "created":
				current.CreatedAt = current.CreatedAt.Add(time.Second)
			case "new_bot":
				baseline.IssueComments = []IssueComment{}
			case "newly_created_bot":
				current.CreatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
				current.UpdatedAt = s.ObservedAt
			case "stale_baseline":
				baseline.ObservedAt = s.IssueComments[0].CreatedAt
			case "old_updated_after_baseline":
				old.UpdatedAt = baseline.ObservedAt.Add(time.Nanosecond)
			case "incomplete_baseline":
				baseline.Complete = false
			case "missing_baseline_collection":
				baseline.Reviews = nil
			case "missing_current_collection":
				s.Reviews = nil
			case "future_update":
				current.UpdatedAt = s.ObservedAt.Add(time.Second)
			case "unknown_inventory":
				s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "No actionable comments were generated in the recent review.", "One unsupported finding remains.", 1)
			case "later_human_command":
				at := s.IssueComments[0].CreatedAt.Add(time.Second)
				s.IssueComments = append(s.IssueComments, IssueComment{ID: 990, Author: poster, CreatedAt: at, UpdatedAt: at, Body: "@coderabbitai full review"})
			}
			got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
			if err != nil {
				t.Fatal(err)
			}
			if got.Complete {
				t.Fatalf("unproven edit completed: %s", got.Reason)
			}
		})
	}
}
