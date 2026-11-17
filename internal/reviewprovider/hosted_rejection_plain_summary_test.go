package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHostedRateLimitedPlainSummaryHasNoCompletionCredit(t *testing.T) {
	// Retain the observed generated invocation and closed dl/dd envelope. Only
	// the provider's presentation heading differs from the decorated reply.
	plain := strings.Replace(rateLimitedCommandReply(), "⚠️ Action not completed", "Action not completed", 1)
	for name, body := range map[string]string{
		"observed definition list": plain,
		"decorated unchanged":      rateLimitedCommandReply(),
		"multiline CRLF":           strings.ReplaceAll(strings.Replace(plain, "<summary>Action not completed</summary>", "<summary>\nAction not completed\n</summary>", 1), "\n", "\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.Reviews, s.ReviewComments = []HostedReview{}, []ReviewComment{}
			s.IssueComments = []IssueComment{s.IssueComments[0], s.IssueComments[2]}
			s.IssueComments[1].Body = body
			before := append([]IssueComment(nil), s.IssueComments...)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Publication.State != "rate_limited" || got.Reason != "hosted_review_rate_limited" || got.Complete || len(got.Findings) != 0 || !got.Publication.FinishedAt.IsZero() || got.Publication.StandingZero != nil || got.Publication.SubmittedReview != nil || !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatal("negative reply lost terminal rejection or manufactured review credit", got, err)
			}
		})
	}
}

func TestHostedRateLimitedPlainSummaryRetainsAttributionAndEvidenceGuards(t *testing.T) {
	for _, mode := range []string{"wrong actor", "old", "duplicate", "other reply", "later command", "pending review", "submitted review", "fresh inline", "partial", "unknown heading", "unknown outcome", "unknown markup", "missing invocation", "fenced", "trailer"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.Reviews, s.ReviewComments = []HostedReview{}, []ReviewComment{}
			s.IssueComments = []IssueComment{s.IssueComments[0], s.IssueComments[2]}
			reply := &s.IssueComments[1]
			reply.Body = strings.Replace(rateLimitedCommandReply(), "⚠️ Action not completed", "Action not completed", 1)
			trigger := s.IssueComments[0].CreatedAt
			switch mode {
			case "wrong actor":
				reply.Author = poster
			case "old":
				reply.CreatedAt = trigger.Add(-time.Minute)
			case "duplicate", "other reply":
				other := *reply
				other.ID++
				if mode == "other reply" {
					other.Body = "Still reviewing."
				}
				s.IssueComments = append(s.IssueComments, other)
			case "later command":
				at := trigger.Add(time.Nanosecond)
				s.IssueComments = append(s.IssueComments, IssueComment{ID: 9001, Author: poster, Body: "@coderabbitai full review", CreatedAt: at, UpdatedAt: at})
			case "pending review":
				s.Reviews = []HostedReview{{ID: 9001, Author: bot, HeadSHA: intent.HeadSHA, State: "pending"}}
			case "submitted review":
				s.Reviews = []HostedReview{{ID: 9001, Author: bot, HeadSHA: intent.HeadSHA, State: "commented", SubmittedAt: reply.CreatedAt, Body: "Unknown inventory"}}
			case "fresh inline":
				// The rejection predicate must refuse contradictory fresh findings
				// independently of the outer snapshot validator.
				s.ReviewComments = []ReviewComment{{Author: bot, CreatedAt: reply.CreatedAt, UpdatedAt: reply.UpdatedAt}}
				if publicationRateLimited(s, HostedPublication{Trigger: HostedTriggerRecovery{CreatedAt: trigger}}, bot, time.Time{}) {
					t.Fatal("fresh inline evidence accepted as rejected execution")
				}
				return
			case "partial":
				s.Complete = false
			case "unknown heading":
				reply.Body = strings.Replace(reply.Body, "Action not completed", "Action pending", 1)
			case "unknown outcome":
				reply.Body = strings.Replace(reply.Body, "Review rate limited.", "Review failed.", 1)
			case "unknown markup":
				reply.Body = strings.Replace(reply.Body, "<dd>", "<div>", 1)
			case "missing invocation":
				reply.Body = strings.Replace(reply.Body, "v2:", "v3:", 1)
			case "fenced":
				reply.Body = "```html\n" + reply.Body + "\n```"
			case "trailer":
				reply.Body += "\nA second outcome."
			}
			before := append([]IssueComment(nil), s.IssueComments...)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || got.Publication.State == "rate_limited" || got.Complete || len(got.Findings) != 0 || !got.Publication.FinishedAt.IsZero() || !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatal("plain heading weakened attribution/evidence boundary", got, err)
			}
		})
	}
}
