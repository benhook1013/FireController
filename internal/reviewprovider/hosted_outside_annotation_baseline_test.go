package reviewprovider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func outsideAnnotationBaselineFixture(t *testing.T) (HostedPublicationSnapshot, HostedPublicationSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	s, baseline, intent, poster, bot, earliest := annotationBaselineFixture(t)
	outside := outsideOnlyFixture(t, true).snapshot.Reviews[0]
	outside.Author, outside.HeadSHA, outside.SubmittedAt = bot, intent.HeadSHA, s.IssueComments[0].CreatedAt.Add(2*time.Second)
	s.Reviews = append(s.Reviews, outside)
	s.IssueComments = s.IssueComments[:1]
	s, botBaseline := botRefreshFixture(s)
	baseline.IssueComments = botBaseline.IssueComments
	return s, baseline, intent, poster, bot, earliest
}

func TestHostedOutsideOnlyProvenOldAnnotations(t *testing.T) {
	for _, mode := range []string{"complete", "unknown_baseline", "new_root", "changed_root", "changed_review", "wrong_footer_head", "late_annotation", "human_edit", "hidden_command", "later_command", "new_inline_root", "mixed_count", "wrong_outside_count", "no_outside", "missing_marker"} {
		t.Run(mode, func(t *testing.T) {
			s, baseline, intent, poster, bot, earliest := outsideAnnotationBaselineFixture(t)
			trigger := s.IssueComments[0].CreatedAt
			switch mode {
			case "unknown_baseline":
				baseline.Complete = false
			case "new_root":
				s.ReviewComments[0].CreatedAt = trigger.Add(time.Second)
			case "changed_root":
				s.ReviewComments[0].Body += "\nChanged source."
			case "changed_review":
				s.Reviews[0].Body += "Changed review."
			case "wrong_footer_head":
				s.ReviewComments[0].Body = strings.Replace(s.ReviewComments[0].Body, intent.HeadSHA[:7], strings.Repeat("d", 7), 1)
			case "late_annotation":
				s.ReviewComments[0].UpdatedAt = trigger.Add(2500 * time.Millisecond)
			case "human_edit":
				s.IssueComments[1].Author.Kind = "User"
				baseline.IssueComments[0].Author.Kind = "User"
			case "hidden_command":
				s.IssueComments[1].Body += "<!-- coderabbit -->"
			case "later_command":
				at := trigger.Add(time.Millisecond)
				s.IssueComments = append(s.IssueComments, IssueComment{ID: 999, Author: poster, CreatedAt: at, UpdatedAt: at, Body: "@coderabbitai full review"})
			case "new_inline_root":
				r := s.ReviewComments[0]
				r.ID = 803
				r.ReviewID = 401
				r.HeadSHA = intent.HeadSHA
				r.OriginalHeadSHA = intent.HeadSHA
				r.CreatedAt = trigger.Add(time.Second)
				r.UpdatedAt = r.CreatedAt
				s.ReviewComments = append(s.ReviewComments, r)
			case "mixed_count":
				s.Reviews[1].Body = "**Actionable comments posted: 1**\n\n" + s.Reviews[1].Body
			case "wrong_outside_count":
				s.Reviews[1].Body = strings.Replace(s.Reviews[1].Body, "comments (1)", "comments (2)", 1)
			case "no_outside":
				s.Reviews[1].Body = publicationTestMarker(1, 1)
			case "missing_marker":
				s.Reviews[1].Body = strings.Replace(s.Reviews[1].Body, hfMarker(1), "", 1)
			}
			before, _ := json.Marshal(s)
			oldBefore, _ := json.Marshal(baseline)
			got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
			if err != nil || got.Complete != (mode == "complete") {
				t.Fatal("unproven annotation changed attribution", got.Reason, got.Complete, err)
			}
			if mode == "complete" && (len(got.Findings) != 1 || got.Findings[0].Kind != "outside_diff" || got.Findings[0].ReviewID != 401 || got.Findings[0].CommentID != 0 || got.Findings[0].Severity != "low" || len(got.Publication.Roots) != 0 || len(got.Publication.Batches) != 1 || got.Publication.Trigger.CommentID != s.IssueComments[0].ID) {
				t.Fatal("old roots counted or original identities lost", got)
			}
			after, _ := json.Marshal(s)
			oldAfter, _ := json.Marshal(baseline)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(oldBefore, oldAfter) {
				t.Fatal("raw source or baseline mutated")
			}
		})
	}
}

func TestHostedQualifiedBotProjectionKeepsInventoryRefusal(t *testing.T) {
	s, baseline, intent, poster, bot, earliest := outsideAnnotationBaselineFixture(t)
	baseline.ReviewComments = []ReviewComment{}
	got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
	if err != nil || got.Complete || got.Publication.State != "ambiguous" || got.Reason != "invalid_actionable_count" || len(got.Findings) != 0 {
		t.Fatal("real parser refusal hidden by qualified bot edit", got, err)
	}
}
