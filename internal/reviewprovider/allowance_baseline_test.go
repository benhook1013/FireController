package reviewprovider

import (
	"reflect"
	"testing"
	"time"
)

func allowanceBotReportFixture(t *testing.T) (HostedAllowanceSnapshot, HostedTriggerIntent, Author, Author, time.Time) {
	t.Helper()
	current, intent, poster, provider, earliest := allowanceFixture(t)
	query := current.IssueComments[0]
	report := IssueComment{ID: 94, Body: "Automated build report, revision two.", Author: Author{ID: 303, Login: "synthetic-build-bot", Kind: "Bot"}, CreatedAt: query.CreatedAt.Add(-time.Hour), UpdatedAt: query.CreatedAt.Add(2 * time.Second)}
	current.IssueComments = append(current.IssueComments, report)
	return current, intent, poster, provider, earliest
}

func TestHostedAllowanceBotRefreshUsesCanonicalCurrentPolicy(t *testing.T) {
	for _, state := range []string{"pending", "available", "cooldown"} {
		t.Run(state, func(t *testing.T) {
			current, intent, poster, provider, earliest := allowanceBotReportFixture(t)
			if state == "pending" {
				current.IssueComments = append(current.IssueComments[:1:1], current.IssueComments[2:]...)
			}
			if state == "available" {
				current.IssueComments[1].Body = hostedAllowanceAvailable
			}
			strict := resolveAllowance(t, current, intent, poster, provider, earliest)

			before := append([]IssueComment(nil), current.IssueComments...)
			got, err := ResolveHostedAllowance(current, intent, poster, provider, earliest)
			want := "allowance_not_proven"
			if state != "pending" {
				want = "provider_" + state + "_observed"
			}
			if err != nil || got.Reason != want || got.QueryCommentID != strict.QueryCommentID || got.QueryCreatedAt != strict.QueryCreatedAt {
				t.Fatalf("current Bot report changed attribution: %+v %v", got, err)
			}
			if state != "pending" && (got.ResponseCommentID != 92 || got.ResponseCreatedAt != current.IssueComments[1].CreatedAt) {
				t.Fatal("reply identity changed", got)
			}
			if state == "cooldown" && got.RetryNotBefore != current.IssueComments[1].CreatedAt.Add(38*time.Minute) {
				t.Fatal("rounding guard lost", got)
			}
			if !reflect.DeepEqual(current.IssueComments, before) {
				t.Fatal("resolver changed immutable inputs")
			}
		})
	}
}

func TestHostedAllowanceBotReportCannotMaskInvalidReplies(t *testing.T) {
	for _, mode := range []string{"unknown", "edited", "duplicate", "incomplete", "malformed current"} {
		t.Run(mode, func(t *testing.T) {
			current, intent, poster, provider, earliest := allowanceBotReportFixture(t)
			switch mode {
			case "unknown":
				current.IssueComments[1].Body = "Unknown provider answer"
			case "edited":
				current.IssueComments[1].UpdatedAt = current.IssueComments[1].UpdatedAt.Add(time.Second)
			case "duplicate":
				r := current.IssueComments[1]
				r.ID++
				current.IssueComments = append(current.IssueComments, r)
			case "incomplete":
				current.Complete = false
			case "malformed current":
				current.IssueComments[2].Body = "\x00"
			}
			got, err := ResolveHostedAllowance(current, intent, poster, provider, earliest)
			if err != nil || got.State == "available_observed" || got.State == "cooldown_observed" || got.ResponseCommentID != 0 || !got.RetryNotBefore.IsZero() {
				t.Fatalf("unsafe reply gained attribution: %+v %v", got, err)
			}
		})
	}
}
