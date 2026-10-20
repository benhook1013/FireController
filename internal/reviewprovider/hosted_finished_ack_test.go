package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHostedFinishedAcknowledgementUnsupportedInventory(t *testing.T) {
	s, intent, poster, bot, earliest := standingTestFixture(t)
	s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "🔵 Low", "Unknown", 1)
	before := s.IssueComments[1].Body
	got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
	if err != nil || got.Complete || got.Reason != "hosted_finished_inventory_unproven" || got.Publication.State != "uncertain" || !got.Publication.FinishedAt.IsZero() || got.Publication.StandingZero != nil || len(got.Findings) != 0 || s.IssueComments[1].Body != before {
		t.Fatalf("finished acknowledgement concealed unsupported inventory or created credit: %+v %v", got, err)
	}
}

func TestHostedFinishedAcknowledgementDiagnosticGuards(t *testing.T) {
	for _, mode := range []string{"old", "at_trigger", "wrong_actor", "unknown_wrapper", "duplicate", "conflicting_reply", "partial", "edited_trigger", "wrong_head", "later_command", "late_edit", "pending_review"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.IssueComments[1].Body = "Unsupported standing inventory"
			trigger := s.IssueComments[0].CreatedAt
			switch mode {
			case "old":
				s.IssueComments[2].CreatedAt, s.IssueComments[2].UpdatedAt = trigger.Add(-time.Minute), trigger.Add(-time.Second)
			case "at_trigger":
				s.IssueComments[2].CreatedAt = trigger
			case "wrong_actor":
				s.IssueComments[2].Author = poster
			case "unknown_wrapper":
				s.IssueComments[2].Body += "\nUnknown trailer"
			case "duplicate", "conflicting_reply":
				other := s.IssueComments[2]
				other.ID++
				if mode == "conflicting_reply" {
					other.Body = "Provider is still working."
				}
				s.IssueComments = append(s.IssueComments, other)
			case "partial":
				s.Complete = false
			case "edited_trigger":
				s.IssueComments[0].UpdatedAt = trigger.Add(time.Second)
			case "wrong_head":
				intent.HeadSHA = strings.Repeat("d", 40)
			case "later_command", "late_edit":
				at := trigger.Add(2 * time.Second)
				s.IssueComments = append(s.IssueComments, IssueComment{ID: 901, Author: poster, Body: "@coderabbitai full review", CreatedAt: at, UpdatedAt: at})
				if mode == "late_edit" {
					s.IssueComments[2].CreatedAt = trigger.Add(time.Second)
				}
			case "pending_review":
				s.Reviews = append(s.Reviews, HostedReview{ID: 901, Author: bot, HeadSHA: intent.HeadSHA, State: "pending"})
			}
			before := append([]IssueComment(nil), s.IssueComments...)
			got, _ := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if got.Complete || got.Reason == "hosted_finished_inventory_unproven" || !got.Publication.FinishedAt.IsZero() || !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatalf("unsafe acknowledgement became diagnostic or changed raw source: %+v", got)
			}
		})
	}
}

// A command acknowledgement proves neither reviewed scope nor an empty finding
// inventory. In particular, an older zero-finding review cannot fill that gap.
func TestHostedFinishedAcknowledgementCannotCreateCleanCoverage(t *testing.T) {
	for _, oldReview := range []bool{false, true} {
		name := "without_publication"
		if oldReview {
			name = "with_older_clean_publication"
		}
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := publicationFixture(t)
			ack := IssueComment{
				ID: 3001, Author: bot,
				Body:      "<!-- This is an auto-generated reply by CodeRabbit -->\n<details>\n<summary>Action performed</summary>\n\nFull review finished.\n\n</details>",
				CreatedAt: s.IssueComments[0].CreatedAt.Add(time.Second),
				UpdatedAt: s.IssueComments[0].CreatedAt.Add(3 * time.Second),
			}
			s.IssueComments = append(s.IssueComments, ack)
			s.ReviewComments = nil
			if oldReview {
				s.Reviews[0].HeadSHA = strings.Repeat("b", 40)
				s.Reviews[0].SubmittedAt = s.IssueComments[0].CreatedAt.Add(-time.Minute)
				s.Reviews[0].Body = "**Actionable comments posted: 0**\n" + publicationTestMarker(1, 1)
			} else {
				s.Reviews = nil
			}
			got := resolvePublication(t, s, intent, poster, bot, earliest, "active", "publication_not_proven")
			if got.PublicationID != "" || !got.FinishedAt.IsZero() || len(got.Batches) != 0 || len(got.Roots) != 0 {
				t.Fatal("finished acknowledgement invented publication evidence")
			}
		})
	}
}
