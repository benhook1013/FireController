package reviewprovider

import (
	"strings"
	"testing"
	"time"
)

func chatLimitReply(login string) string {
	return hostedAllowanceGeneratedEnvelope + "<!-- This is an auto-generated reply by CodeRabbit -->\n### Rate Limit Exceeded\n\n`@" + login + "` have exceeded the limit for the number of chat messages per hour. Please wait **44 minutes and 5 seconds** before sending another message."
}

func TestHostedChatCooldownExactAttributedNegative(t *testing.T) {
	s, i, p, b, e := allowanceFixture(t)
	s.IssueComments[1].Body = chatLimitReply(p.Login)
	got := resolveAllowance(t, s, i, p, b, e)
	want := s.IssueComments[1].CreatedAt.Add(44*time.Minute + 6*time.Second)
	if got.State != "cooldown_observed" || got.Reason != HostedChatCooldownReason || got.RetryNotBefore != want || !ValidHostedAllowanceOutcome(got) || got.QueryCommentID != 91 || got.ResponseCommentID != 92 {
		t.Fatalf("lost distinct bounded chat limit: %+v", got)
	}
	// Three independently observed durations resolve to the same provider reset;
	// the parser adds the same conservative second, never availability.
	for _, pair := range [][2]string{{"52", "17"}, {"52", "14"}, {"59", "59"}} {
		s.IssueComments[1].Body = strings.ReplaceAll(strings.ReplaceAll(chatLimitReply(p.Login), "44 minutes", pair[0]+" minutes"), "5 seconds", pair[1]+" seconds")
		got = resolveAllowance(t, s, i, p, b, e)
		if got.Reason != HostedChatCooldownReason || !ValidHostedAllowanceOutcome(got) {
			t.Fatal(got)
		}
	}
}

func TestHostedChatCooldownRejectsMalformedOrUnattributed(t *testing.T) {
	for name, mutate := range map[string]func(*HostedAllowanceSnapshot){
		"other addressee": func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Body = chatLimitReply("different-user") },
		"other author":    func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Author.ID++ },
		"edited reply": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].UpdatedAt = s.IssueComments[1].CreatedAt.Add(time.Second)
		},
		"edited query": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[0].UpdatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
		},
		"incomplete": func(s *HostedAllowanceSnapshot) { s.Complete = false },
		"missing envelope": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].Body = strings.TrimPrefix(s.IssueComments[1].Body, hostedAllowanceGeneratedEnvelope)
		},
		"repeated envelope": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].Body = hostedAllowanceGeneratedEnvelope + s.IssueComments[1].Body
		},
		"trailing text": func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Body += "\nReviews are available now." },
		"newline":       func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Body += "\n" },
		"quoted":        func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Body = "> " + s.IssueComments[1].Body },
		"zero": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "44 minutes", "0 minutes", 1)
		},
		"negative": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "44 minutes", "-44 minutes", 1)
		},
		"overflow": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "44 minutes", strings.Repeat("9", 100)+" minutes", 1)
		},
		"hour bound": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "44 minutes", "60 minutes", 1)
		},
		"second bound": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "5 seconds", "60 seconds", 1)
		},
		"leading zero": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].Body = strings.Replace(s.IssueComments[1].Body, "5 seconds", "05 seconds", 1)
		},
		"competing command": func(s *HostedAllowanceSnapshot) {
			c := s.IssueComments[0]
			c.ID = 93
			c.Body = "@coderabbitai full review"
			c.CreatedAt = c.CreatedAt.Add(time.Second)
			c.UpdatedAt = c.CreatedAt
			s.IssueComments = append(s.IssueComments, c)
		},
		"conflicting reply": func(s *HostedAllowanceSnapshot) {
			c := s.IssueComments[1]
			c.ID = 93
			c.Body = hostedAllowanceAvailable
			s.IssueComments = append(s.IssueComments, c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, i, p, b, e := allowanceFixture(t)
			s.IssueComments[1].Body = chatLimitReply(p.Login)
			mutate(&s)
			got := resolveAllowance(t, s, i, p, b, e)
			if got.State == "available_observed" || got.State == "cooldown_observed" || !got.RetryNotBefore.IsZero() {
				t.Fatalf("unsafe result: %+v", got)
			}
		})
	}
}
