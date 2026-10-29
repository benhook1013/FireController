package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func rateLimitedCommandReply() string {
	return "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>⚠️ Action not completed</summary>\n<dl>\n<dd>\n\nReview rate limited.\n\n</dd>\n</dl>\n</details>"
}

func TestHostedRateLimitedReplyIsTerminalWithoutReviewCredit(t *testing.T) {
	for _, mode := range []string{"current", "crlf", "ci_bot_edit", "bot_command", "human_edit", "wrong_actor", "old", "duplicate", "extra_reply", "quoted", "fenced", "partial", "later_command", "pending_review", "finished_review", "unknown_markup", "missing_invocation"} {
		t.Run(mode, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.Reviews, s.ReviewComments = []HostedReview{}, []ReviewComment{}
			s.IssueComments = []IssueComment{s.IssueComments[0], s.IssueComments[2]}
			s.IssueComments[1].Body = rateLimitedCommandReply()
			trigger := s.IssueComments[0].CreatedAt
			switch mode {
			case "crlf":
				s.IssueComments[1].Body = strings.ReplaceAll(s.IssueComments[1].Body, "\n", "\r\n")
			case "ci_bot_edit", "bot_command", "human_edit":
				author := Author{ID: 9002, Login: "ci-summary[bot]", Kind: "Bot"}
				body := "Updated CI summary"
				if mode == "bot_command" {
					body = "@coderabbitai full review"
				} else if mode == "human_edit" {
					author = poster
				}
				s.IssueComments = append(s.IssueComments, IssueComment{ID: 9002, Author: author, Body: body, CreatedAt: trigger.Add(-time.Minute), UpdatedAt: s.IssueComments[1].UpdatedAt})
			case "wrong_actor":
				s.IssueComments[1].Author = poster
			case "old":
				s.IssueComments[1].CreatedAt = trigger.Add(-time.Minute)
			case "duplicate", "extra_reply":
				other := s.IssueComments[1]
				other.ID++
				if mode == "extra_reply" {
					other.Body = "Still reviewing."
				}
				s.IssueComments = append(s.IssueComments, other)
			case "quoted":
				s.IssueComments[1].Body = "> " + strings.ReplaceAll(s.IssueComments[1].Body, "\n", "\n> ")
			case "fenced":
				s.IssueComments[1].Body = "```html\n" + s.IssueComments[1].Body + "\n```"
			case "partial":
				s.Complete = false
			case "later_command":
				at := trigger.Add(time.Nanosecond)
				s.IssueComments = append(s.IssueComments, IssueComment{ID: 9001, Author: poster, Body: "@coderabbitai full review", CreatedAt: at, UpdatedAt: at})
			case "pending_review":
				s.Reviews = []HostedReview{{ID: 9001, Author: bot, HeadSHA: intent.HeadSHA, State: "pending"}}
			case "finished_review":
				s.Reviews = []HostedReview{{ID: 9001, Author: bot, HeadSHA: intent.HeadSHA, State: "commented", SubmittedAt: s.IssueComments[1].CreatedAt, Body: "Unknown inventory"}}
			case "unknown_markup":
				s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "<dd>", "<div>", 1)
			case "missing_invocation":
				s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "v2:", "v3:", 1)
			}
			before := append([]IssueComment(nil), s.IssueComments...)
			got, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			wantLimited := mode == "current" || mode == "crlf"
			if err != nil || (got.Publication.State == "rate_limited") != wantLimited || got.Complete || len(got.Findings) != 0 || !got.Publication.FinishedAt.IsZero() || !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatalf("rejection attribution or no-credit boundary: %+v %v", got, err)
			}
			if wantLimited && got.Reason != "hosted_review_rate_limited" {
				t.Fatal("missing terminal rejection reason", got.Reason)
			}
		})
	}
}

func TestHostedRateLimitedReplyUsesQualifiedBotRefreshBaseline(t *testing.T) {
	s, intent, poster, bot, earliest := standingTestFixture(t)
	s.Reviews, s.ReviewComments = []HostedReview{}, []ReviewComment{}
	s.IssueComments = []IssueComment{s.IssueComments[0], s.IssueComments[2]}
	s.IssueComments[1].Body = rateLimitedCommandReply()
	s, baseline := botRefreshFixture(s)
	for _, qualified := range []bool{true, false} {
		if !qualified {
			baseline.IssueComments[0].Author.Kind = "User"
		}
		got, err := ExtractHostedFindingsWithBaseline(s, baseline, intent, poster, bot, earliest)
		if err != nil || (got.Publication.State == "rate_limited") != qualified || got.Complete || len(got.Findings) != 0 || !got.Publication.FinishedAt.IsZero() {
			t.Fatal("baseline rejection attribution or no-credit boundary", got, err)
		}
	}
}
