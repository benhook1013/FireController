package reviewprovider

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func secondsAllowanceReply(count int) string {
	unit := "seconds"
	if count == 1 {
		unit = "second"
	}
	return strings.Replace(allowanceReply(37), "37 minutes", fmt.Sprintf("%d %s", count, unit), 1)
}

func TestHostedAllowanceSecondsCooldownExactDeadlineAndExistingMinutes(t *testing.T) {
	for _, tc := range []struct {
		count int
		unit  time.Duration
	}{{1, time.Second}, {2, time.Second}, {28, time.Second}, {59, time.Second}, {60, time.Second}, {86399, time.Second}, {1, time.Minute}, {37, time.Minute}, {1439, time.Minute}} {
		for _, envelope := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-%s/envelope=%t", tc.count, tc.unit, envelope), func(t *testing.T) {
				s, intent, poster, bot, earliest := allowanceFixture(t)
				body := allowanceReply(tc.count)
				if tc.unit == time.Second {
					body = secondsAllowanceReply(tc.count)
				}
				if envelope {
					body = hostedAllowanceGeneratedEnvelope + body
				}
				s.IssueComments[1].Body = body
				before := append([]IssueComment(nil), s.IssueComments...)
				got := resolveAllowance(t, s, intent, poster, bot, earliest)
				want := s.IssueComments[1].CreatedAt.Add(time.Duration(tc.count+1) * tc.unit)
				if got.State != "cooldown_observed" || got.Reason != "provider_cooldown_observed" || got.QueryCommentID != 91 || got.ResponseCommentID != 92 || got.QueryCreatedAt != s.IssueComments[0].CreatedAt || got.ResponseCreatedAt != s.IssueComments[1].CreatedAt || got.RetryNotBefore != want || !ValidHostedAllowanceOutcome(got) || !reflect.DeepEqual(before, s.IssueComments) {
					t.Fatalf("exact attributed cooldown or retained source changed: %+v", got)
				}
			})
		}
	}
}

func TestHostedAllowanceSecondsMalformedOrOverlongDurationRemainsUnproven(t *testing.T) {
	for name, body := range map[string]string{
		"zero": secondsAllowanceReply(0), "negative": secondsAllowanceReply(-1), "24h plus margin": secondsAllowanceReply(86400), "large": secondsAllowanceReply(999999999),
		"wrong singular":    strings.Replace(secondsAllowanceReply(2), "2 seconds", "2 second", 1),
		"wrong plural":      strings.Replace(secondsAllowanceReply(1), "1 second", "1 seconds", 1),
		"leading zero":      strings.Replace(secondsAllowanceReply(28), "28 seconds", "028 seconds", 1),
		"decimal":           strings.Replace(secondsAllowanceReply(28), "28 seconds", "28.5 seconds", 1),
		"upper case":        strings.Replace(secondsAllowanceReply(28), "seconds", "Seconds", 1),
		"abbreviation":      strings.Replace(secondsAllowanceReply(28), "seconds", "secs", 1),
		"whitespace":        strings.Replace(secondsAllowanceReply(28), "28 seconds", "28  seconds", 1),
		"mixed units":       strings.Replace(secondsAllowanceReply(28), "28 seconds", "1 minute 28 seconds", 1),
		"extra prose":       secondsAllowanceReply(28) + "\nAdditional text.",
		"repeated envelope": hostedAllowanceGeneratedEnvelope + hostedAllowanceGeneratedEnvelope + secondsAllowanceReply(28),
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			s.IssueComments[1].Body = body
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State != "unproven" || got.ResponseCommentID != 0 || !got.RetryNotBefore.IsZero() {
				t.Fatalf("unsupported reply acquired cooldown proof: %+v", got)
			}
		})
	}
}

func TestHostedAllowanceSecondsDoesNotRelaxSourceAttribution(t *testing.T) {
	for name, mutate := range map[string]func(*HostedAllowanceSnapshot){
		"partial": func(s *HostedAllowanceSnapshot) { s.Complete = false },
		"edited query": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[0].UpdatedAt = s.IssueComments[0].CreatedAt.Add(time.Second)
		},
		"edited reply": func(s *HostedAllowanceSnapshot) {
			s.IssueComments[1].UpdatedAt = s.IssueComments[1].CreatedAt.Add(time.Second)
		},
		"foreign reply":      func(s *HostedAllowanceSnapshot) { s.IssueComments[1].Author.ID++ },
		"duplicate identity": func(s *HostedAllowanceSnapshot) { s.IssueComments = append(s.IssueComments, s.IssueComments[1]) },
		"duplicate reply": func(s *HostedAllowanceSnapshot) {
			c := s.IssueComments[1]
			c.ID++
			s.IssueComments = append(s.IssueComments, c)
		},
		"competing command": func(s *HostedAllowanceSnapshot) {
			c := s.IssueComments[0]
			c.ID = 93
			c.Body = "@coderabbitai review"
			c.CreatedAt = c.CreatedAt.Add(time.Second)
			c.UpdatedAt = c.CreatedAt
			s.IssueComments = append(s.IssueComments, c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := allowanceFixture(t)
			s.IssueComments[1].Body = secondsAllowanceReply(28)
			mutate(&s)
			before := append([]IssueComment(nil), s.IssueComments...)
			got := resolveAllowance(t, s, intent, poster, bot, earliest)
			if got.State == "cooldown_observed" || got.State == "available_observed" || got.ResponseCommentID != 0 || !got.RetryNotBefore.IsZero() || !reflect.DeepEqual(before, s.IssueComments) {
				t.Fatalf("unattributed source acquired permission facts: %+v", got)
			}
		})
	}
}

func TestHostedAllowanceOutcomeFiniteSecondPrecisionBounds(t *testing.T) {
	response := time.Date(2026, 10, 30, 11, 5, 56, 0, time.UTC)
	for _, tc := range []struct {
		delay time.Duration
		valid bool
	}{{time.Second, false}, {2 * time.Second, true}, {29 * time.Second, true}, {2 * time.Minute, true}, {24 * time.Hour, true}, {24*time.Hour + time.Second, false}, {2500 * time.Millisecond, false}, {0, false}, {-time.Second, false}} {
		o := HostedAllowanceObservation{State: "cooldown_observed", Reason: "provider_cooldown_observed", ResponseCreatedAt: response, RetryNotBefore: response.Add(tc.delay)}
		if ValidHostedAllowanceOutcome(o) != tc.valid {
			t.Fatalf("invalid retained outcome duration acceptance: %s", tc.delay)
		}
	}
	available := HostedAllowanceObservation{State: "available_observed", Reason: "provider_available_observed"}
	if !ValidHostedAllowanceOutcome(available) {
		t.Fatal("positive outcome changed")
	}
	available.RetryNotBefore = response
	if ValidHostedAllowanceOutcome(available) {
		t.Fatal("positive observation acquired cooldown time")
	}
}
