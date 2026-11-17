package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func skippedCommandReply() string {
	return "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>Action not completed</summary>\n<dl>\n<dd>\n\nReview skipped.\n\n</dd>\n</dl>\n</details>"
}

func TestHostedSkippedCommandReplyStopsWithoutReviewCredit(t *testing.T) {
	for _, mode := range []string{"observed", "decorated", "crlf", "old seat summary", "wrong actor", "wrong bot ID", "old reply", "root only", "duplicate", "other reply", "later command", "pending review", "submitted review", "fresh finding", "partial", "quoted", "fenced", "unknown outcome", "unknown envelope", "missing invocation", "trailer"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, _ := standingTestFixture(t)
			s.Reviews, s.ReviewComments = []HostedReview{}, []ReviewComment{}
			s.IssueComments = []IssueComment{s.IssueComments[0], s.IssueComments[2]}
			triggerAt := time.Date(2026, 11, 2, 12, 49, 12, 0, time.UTC)
			earliest := triggerAt
			s.ObservedAt = triggerAt.Add(10 * time.Second)
			s.IssueComments[0].ID, s.IssueComments[0].CreatedAt, s.IssueComments[0].UpdatedAt = 6577552197, triggerAt, triggerAt
			reply := &s.IssueComments[1]
			reply.ID, reply.CreatedAt, reply.UpdatedAt, reply.Body = 6577553918, triggerAt.Add(6*time.Second), triggerAt.Add(6*time.Second), skippedCommandReply()
			switch mode {
			case "decorated":
				reply.Body = strings.Replace(reply.Body, "Action not completed", "⚠️ Action not completed", 1)
			case "crlf":
				reply.Body = strings.ReplaceAll(reply.Body, "\n", "\r\n")
			case "old seat summary", "root only":
				old := IssueComment{ID: 500, Author: bot, CreatedAt: triggerAt.Add(-time.Hour), UpdatedAt: triggerAt.Add(-time.Hour), Body: "The author does not have a CodeRabbit seat."}
				if mode == "root only" {
					s.IssueComments = s.IssueComments[:1]
				}
				s.IssueComments = append(s.IssueComments, old)
			case "wrong actor":
				reply.Author = poster
			case "wrong bot ID":
				reply.Author.ID++
			case "old reply":
				reply.CreatedAt = triggerAt.Add(-time.Minute)
			case "duplicate", "other reply":
				other := *reply
				other.ID++
				if mode == "other reply" {
					other.Body = "Still reviewing."
				}
				s.IssueComments = append(s.IssueComments, other)
			case "later command":
				at := triggerAt.Add(time.Second)
				s.IssueComments = append(s.IssueComments, IssueComment{ID: 9001, Author: poster, Body: "@coderabbitai full review", CreatedAt: at, UpdatedAt: at})
			case "pending review":
				s.Reviews = []HostedReview{{ID: 9001, Author: bot, HeadSHA: intent.HeadSHA, State: "pending"}}
			case "submitted review":
				s.Reviews = []HostedReview{{ID: 9001, Author: bot, HeadSHA: intent.HeadSHA, State: "commented", SubmittedAt: reply.CreatedAt, Body: "Unknown inventory"}}
			case "fresh finding":
				s.ReviewComments = []ReviewComment{{Author: bot, CreatedAt: reply.CreatedAt, UpdatedAt: reply.UpdatedAt}}
			case "partial":
				s.Complete = false
			case "quoted":
				reply.Body = "> " + strings.ReplaceAll(reply.Body, "\n", "\n> ")
			case "fenced":
				reply.Body = "```html\n" + reply.Body + "\n```"
			case "unknown outcome":
				reply.Body = strings.Replace(reply.Body, "Review skipped.", "Review complete.", 1)
			case "unknown envelope":
				reply.Body = strings.Replace(reply.Body, "<dd>", "<div>", 1)
			case "missing invocation":
				reply.Body = strings.Replace(reply.Body, "v2:", "v3:", 1)
			case "trailer":
				reply.Body += "\nA second outcome."
			}
			before := append([]IssueComment(nil), s.IssueComments...)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			wantSkipped := mode == "observed" || mode == "decorated" || mode == "crlf" || mode == "old seat summary"
			if err != nil || (got.Publication.State == "failed" && got.Reason == "hosted_review_skipped") != wantSkipped || got.Publication.State == "rate_limited" || got.Complete || len(got.Findings) != 0 || !got.Publication.FinishedAt.IsZero() || got.Publication.StandingZero != nil || got.Publication.SubmittedReview != nil || !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatal("skip attribution or no-credit boundary changed", got, err)
			}
		})
	}
}

func TestHostedHeadChangedWithHistoricalAnnotation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*HostedPublicationSnapshot, *HostedPublicationSnapshot)
		wantFailed bool
	}{
		{name: "exact reply and old annotation", wantFailed: true},
		{name: "exact reply without annotation", wantFailed: true, mutate: func(s, b *HostedPublicationSnapshot) {
			s.ReviewComments[0] = b.ReviewComments[0]
		}},
		{name: "unrecognized outcome", mutate: func(s, _ *HostedPublicationSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "Head commit changed.", "Head may change.", 1)
		}},
		{name: "unproven annotation", mutate: func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].Body += "\nNew finding."
		}},
		{name: "late annotation", mutate: func(s, _ *HostedPublicationSnapshot) {
			s.ReviewComments[0].UpdatedAt = s.IssueComments[1].UpdatedAt.Add(time.Second)
		}},
		{name: "new submitted review", mutate: func(s, _ *HostedPublicationSnapshot) {
			review := s.Reviews[0]
			review.ID++
			review.SubmittedAt = s.IssueComments[1].UpdatedAt
			s.Reviews = append(s.Reviews, review)
		}},
		{name: "new inline root", mutate: func(s, _ *HostedPublicationSnapshot) {
			root := s.ReviewComments[0]
			root.ID++
			root.CreatedAt = s.IssueComments[1].CreatedAt
			root.UpdatedAt = root.CreatedAt
			s.ReviewComments = append(s.ReviewComments, root)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, baseline, intent, poster, bot, earliest := annotationBaselineFixture(t)
			reply := s.IssueComments[len(s.IssueComments)-1]
			reply.Body = strings.Replace(skippedCommandReply(), "\n<dd>\n\nReview skipped.", "\n<dd>\n<br>\n\nHead commit changed.", 1)
			s.IssueComments = []IssueComment{s.IssueComments[0], reply}
			if tc.mutate != nil {
				tc.mutate(&s, &baseline)
			}
			got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
			if err != nil {
				t.Fatal(err)
			}
			failed := got.Publication.State == "failed" && got.Reason == "hosted_review_skipped"
			if failed != tc.wantFailed || got.Complete || len(got.Findings) != 0 {
				t.Fatalf("state=%s reason=%s complete=%v findings=%d, want terminal=%v", got.Publication.State, got.Reason, got.Complete, len(got.Findings), tc.wantFailed)
			}
			if failed && !got.Publication.FinishedAt.Equal(reply.UpdatedAt) {
				t.Fatal("missing refusal timestamp")
			}
		})
	}
}
