package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func allowanceRetryFixture(t *testing.T) (HostedAllowanceSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	s, intent, poster, bot, earliest := allowanceFixture(t)
	prior := intent
	prior.AttemptID = "review_attempt_" + strings.Repeat("d", 32)
	body, err := HostedAllowanceQueryBody(prior)
	if err != nil {
		t.Fatal(err)
	}
	at := earliest.Add(-5 * time.Minute)
	s.IssueComments = append([]IssueComment{{ID: 81, Body: body, Author: poster, CreatedAt: at, UpdatedAt: at}, {ID: 82, Body: allowanceReply(1), Author: bot, CreatedAt: at.Add(time.Second), UpdatedAt: at.Add(time.Second)}}, s.IssueComments...)
	s.IssueComments[3].Body = hostedAllowanceAvailable
	return s, intent, poster, bot, earliest
}

func TestHostedAllowanceRetryRetainsExactHistoricalAttemptQueries(t *testing.T) {
	s, intent, poster, bot, earliest := allowanceRetryFixture(t)
	before := append([]IssueComment(nil), s.IssueComments...)
	s.IssueComments[0].Author.Login = "renamed-poster"
	before[0].Author.Login = "renamed-poster"
	for _, comments := range [][]IssueComment{s.IssueComments, {s.IssueComments[3], s.IssueComments[0], s.IssueComments[2], s.IssueComments[1]}} {
		s.IssueComments = comments
		result := resolveAllowance(t, s, intent, poster, bot, earliest)
		if result.State != "available_observed" || result.QueryCommentID != 91 || result.ResponseCommentID != 92 || !result.RetryNotBefore.IsZero() {
			t.Fatalf("old cooldown query displaced fresh attribution: %+v", result)
		}
	}
	if !reflect.DeepEqual(before, []IssueComment{s.IssueComments[1], s.IssueComments[3], s.IssueComments[2], s.IssueComments[0]}) {
		t.Fatal("retry parser mutated original provider source")
	}
}

func TestHostedAllowanceRetryHistoricalExceptionRejectsConflictingFacts(t *testing.T) {
	for name, mutate := range map[string]func(*HostedAllowanceSnapshot, HostedTriggerIntent, time.Time){
		"foreign author": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, _ time.Time) { s.IssueComments[0].Author.ID++ },
		"foreign kind": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, _ time.Time) {
			s.IssueComments[0].Author.Kind = "Bot"
		},
		"edited": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, _ time.Time) {
			s.IssueComments[0].UpdatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
		},
		"malformed": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, _ time.Time) { s.IssueComments[0].Body += "\n" },
		"quoted": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, _ time.Time) {
			s.IssueComments[0].Body = "> " + s.IssueComments[0].Body
		},
		"wrong head": func(s *HostedAllowanceSnapshot, i HostedTriggerIntent, _ time.Time) {
			s.IssueComments[0].Body = strings.Replace(s.IssueComments[0].Body, i.HeadSHA, strings.Repeat("e", 40), 1)
		},
		"invalid attempt": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, _ time.Time) {
			s.IssueComments[0].Body = strings.Replace(s.IssueComments[0].Body, strings.Repeat("d", 32), "short", 1)
		},
		"current attempt": func(s *HostedAllowanceSnapshot, i HostedTriggerIntent, _ time.Time) {
			s.IssueComments[0].Body, _ = HostedAllowanceQueryBody(i)
		},
		"at current intent": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, e time.Time) {
			s.IssueComments[0].CreatedAt, s.IssueComments[0].UpdatedAt = e, e
		},
		"just after current intent": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, e time.Time) {
			s.IssueComments[0].CreatedAt, s.IssueComments[0].UpdatedAt = e.Add(time.Nanosecond), e.Add(time.Nanosecond)
		},
		"later command": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, e time.Time) {
			s.IssueComments[0].CreatedAt, s.IssueComments[0].UpdatedAt = e.Add(2*time.Second), e.Add(2*time.Second)
		},
		"duplicate identity": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, _ time.Time) {
			s.IssueComments = append(s.IssueComments, s.IssueComments[0])
		},
		"duplicate attempt": func(s *HostedAllowanceSnapshot, _ HostedTriggerIntent, _ time.Time) {
			other := s.IssueComments[0]
			other.ID += 100
			s.IssueComments = append(s.IssueComments, other)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceRetryFixture(t)
			mutate(&s, intent, earliest)
			result := resolveAllowance(t, s, intent, poster, bot, earliest)
			if result.State == "available_observed" || result.State == "cooldown_observed" || result.ResponseCommentID != 0 || !result.RetryNotBefore.IsZero() {
				t.Fatalf("conflicting historical marker gained allowance proof: %+v", result)
			}
		})
	}
}

func TestHostedAllowanceRetryHistoricalCommandStillClosesAttribution(t *testing.T) {
	s, intent, poster, bot, earliest := allowanceRetryFixture(t)
	// Provider skew can place the current query just before its local intent.
	// A different command after that query remains causally competing even when
	// its exact immutable marker predates the local intent.
	s.IssueComments[2].CreatedAt = earliest.Add(-2 * time.Second)
	s.IssueComments[2].UpdatedAt = s.IssueComments[2].CreatedAt
	s.IssueComments[0].CreatedAt = earliest.Add(-time.Second)
	s.IssueComments[0].UpdatedAt = s.IssueComments[0].CreatedAt
	result := resolveAllowance(t, s, intent, poster, bot, earliest)
	if result.State == "available_observed" || result.State == "cooldown_observed" || result.ResponseCommentID != 0 {
		t.Fatalf("historical marker bypassed command-window closure: %+v", result)
	}
}

func TestHostedAllowanceRetryExceptionDoesNotAdoptHistoricalReviewTriggers(t *testing.T) {
	intent, poster, earliest, observed, current := triggerFixture(t)
	prior := intent
	prior.AttemptID = "review_attempt_" + strings.Repeat("d", 32)
	body, err := HostedTriggerBody(prior)
	if err != nil {
		t.Fatal(err)
	}
	at := earliest.Add(-5 * time.Minute)
	old := IssueComment{ID: 81, Body: body, Author: poster, CreatedAt: at, UpdatedAt: at}
	result, err := RecoverHostedTrigger([]IssueComment{old, current}, true, intent, poster, earliest, observed)
	if err != nil || result.State != "ambiguous" || result.CommentID != 0 {
		t.Fatalf("allowance exception changed review-trigger recovery: %+v %v", result, err)
	}
}
