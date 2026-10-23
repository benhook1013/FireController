package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func botRefreshFixture(snapshot HostedPublicationSnapshot) (HostedPublicationSnapshot, HostedPublicationSnapshot) {
	trigger := snapshot.IssueComments[0].CreatedAt
	old := IssueComment{ID: 901, Author: Author{ID: 902, Login: "synthetic-status[bot]", Kind: "Bot"}, CreatedAt: trigger.Add(-time.Hour), UpdatedAt: trigger.Add(-time.Minute), Body: "Build head=" + strings.Repeat("a", 40) + " run=123 attempt=1\nStatus ready."}
	baseline := HostedPublicationSnapshot{Complete: true, ObservedAt: trigger.Add(-time.Second), IssueComments: []IssueComment{old}, Reviews: []HostedReview{}, ReviewComments: []ReviewComment{}}
	current := old
	current.UpdatedAt = trigger.Add(time.Second)
	current.Body = "Build head=" + strings.Repeat("b", 40) + " run=456 attempt=2\nStatus ready."
	snapshot.IssueComments = append(snapshot.IssueComments, current)
	return snapshot, baseline
}

func TestHostedBaselineBotRefreshPreservesCompleteInventory(t *testing.T) {
	for _, positive := range []bool{false, true} {
		t.Run(map[bool]string{false: "standing_zero", true: "submitted_positive"}[positive], func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			if positive {
				f := submittedTestFixture(t)
				s, intent, poster, bot, earliest = f.snapshot, f.intent, f.poster, f.bot, f.earliest
			}
			want, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || !want.Complete {
				t.Fatal("fixture", want.Reason, err)
			}
			s, baseline := botRefreshFixture(s)
			before := append([]IssueComment(nil), s.IssueComments...)
			strict, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || strict.Complete || strict.Reason != "attribution_window_closed" {
				t.Fatal("strict guard changed", strict.Reason, err)
			}
			got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
			if err != nil || !got.Complete || !reflect.DeepEqual(got.Findings, want.Findings) || !reflect.DeepEqual(got.Publication.Roots, want.Publication.Roots) {
				t.Fatal("lost complete original inventory", got.Reason, err)
			}
			if !reflect.DeepEqual(s.IssueComments, before) {
				t.Fatal("raw source mutated")
			}
		})
	}
}

func TestHostedBaselineBotRefreshRefusesUnprovenEdits(t *testing.T) {
	for _, mode := range []string{"prose", "hidden_command", "removed_command", "human", "author", "created", "new_bot", "stale_baseline", "old_updated_after_baseline", "after_ack", "unknown_inventory", "short_word", "embedded_number", "unicode_number", "same_body"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s, baseline := botRefreshFixture(s)
			last := len(s.IssueComments) - 1
			current := &s.IssueComments[last]
			old := &baseline.IssueComments[0]
			switch mode {
			case "prose":
				current.Body += " changed"
			case "hidden_command":
				current.Body += "<!-- CoDeRaBbIt -->"
			case "removed_command":
				old.Body += "<!-- coderabbit -->"
			case "human":
				current.Author.Kind = "User"
				old.Author.Kind = "User"
			case "author":
				current.Author.ID++
			case "created":
				current.CreatedAt = current.CreatedAt.Add(time.Second)
			case "new_bot":
				baseline.IssueComments = []IssueComment{}
			case "stale_baseline":
				baseline.ObservedAt = s.IssueComments[0].CreatedAt
			case "old_updated_after_baseline":
				old.UpdatedAt = baseline.ObservedAt.Add(time.Nanosecond)
			case "after_ack":
				current.UpdatedAt = s.ObservedAt
			case "unknown_inventory":
				s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "No actionable comments were generated in the recent review.", "One unsupported finding remains.", 1)
			case "short_word":
				old.Body = "Status dead"
				current.Body = "Status beef"
			case "embedded_number":
				old.Body = "Status name123"
				current.Body = "Status name456"
			case "unicode_number":
				old.Body = "Status é123"
				current.Body = "Status é456"
			case "same_body":
				current.Body = old.Body
			}
			got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
			if err != nil {
				t.Fatal(err)
			}
			if got.Complete != (mode == "same_body") {
				t.Fatalf("unexpected complete=%v reason=%s", got.Complete, got.Reason)
			}
		})
	}
}

func TestHostedBotIdentifierProjectionBoundaries(t *testing.T) {
	for _, pair := range [][2]string{{"id=abcdef1 n=1", "id=fedcba2 n=2"}, {strings.Repeat("a", 40), strings.Repeat("b", 40)}, {strings.Repeat("a", 64), strings.Repeat("b", 64)}} {
		if botIdentifierProjection(pair[0]) != botIdentifierProjection(pair[1]) {
			t.Fatal("bounded identifier not recognized")
		}
	}
	for _, pair := range [][2]string{{"dead", "beef"}, {"abcdefa", "abcdefb"}, {"_123", "_456"}, {"x123", "x456"}, {"é123", "é456"}, {strings.Repeat("1", 65), strings.Repeat("2", 65)}, {"ready 1", "done 2"}} {
		if botIdentifierProjection(pair[0]) == botIdentifierProjection(pair[1]) {
			t.Fatal("prose/embedded/oversized token normalized")
		}
	}
}
