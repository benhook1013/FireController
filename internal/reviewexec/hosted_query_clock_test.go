package reviewexec

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRetainedHostedQuerySurvivesClockRollback(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "intent", true: "confirmed"}[confirmed], func(t *testing.T) {
			f, a, anchor, poster, provider := hostedQueryFixture(t)
			q, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
			if err != nil || !created {
				t.Fatal(err)
			}
			if confirmed {
				q, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, hostedQueryComment(t, q))
				if err != nil {
					t.Fatal(err)
				}
			}
			// Model a previously valid durable record whose clock was one hour ahead.
			// The body remains canonical and its original ordering stays intact.
			at, _ := time.Parse(time.RFC3339Nano, q.EarliestPosting)
			q.EarliestPosting = at.Add(time.Hour).Format(time.RFC3339Nano)
			observed, _ := time.Parse(time.RFC3339Nano, q.Anchor.ObservedAt)
			q.Anchor.ObservedAt = observed.Add(time.Hour).Format(time.RFC3339Nano)
			if q.Comment != nil {
				q.Comment.CreatedAt = q.Comment.CreatedAt.Add(time.Hour)
				q.Comment.UpdatedAt = q.Comment.UpdatedAt.Add(time.Hour)
			}
			body, err := json.Marshal(q)
			if err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE id=?`, string(body), q.ID)
			before := hostedQueryState(t, f, a)
			read, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
			if err != nil || !reflect.DeepEqual(read, q) {
				t.Fatal("clock rollback invalidated retained query", err)
			}
			replay, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, q.Anchor, poster, provider)
			if err != nil || created || !reflect.DeepEqual(replay, q) {
				t.Fatal("retained intent replay lost or granted another post", err)
			}
			if confirmed {
				got, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, *q.Comment)
				if err != nil || !reflect.DeepEqual(got, q) {
					t.Fatal("retained confirmation replay lost", err)
				}
			}
			if hostedQueryState(t, f, a) != before {
				t.Fatal("retained read/replay changed journal")
			}
			cliAllowanceReopen(t, f)
			got, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
			if err != nil || !reflect.DeepEqual(got, q) {
				t.Fatal("reopened retained query lost", err)
			}
		})
	}
}

// A provider-owned timestamp can trail the local durable query intent. Both a
// direct POST result and the same result recovered from history must survive
// confirmation, retained reads and reopening without rewriting either clock.
func TestHostedQueryConfirmationProviderClockSkew(t *testing.T) {
	for _, mode := range []string{"direct", "recovered"} {
		for _, lag := range []time.Duration{0, time.Second, 5 * time.Second} {
			t.Run(mode+"/"+lag.String(), func(t *testing.T) {
				f, a, anchor, poster, provider := hostedQueryFixture(t)
				query, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
				if err != nil || !created {
					t.Fatal("query setup", err)
				}
				original := query
				comment := hostedQueryComment(t, query)
				comment.CreatedAt = comment.CreatedAt.Add(-lag)
				comment.UpdatedAt = comment.CreatedAt
				if mode == "recovered" {
					earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
					if err != nil {
						t.Fatal(err)
					}
					responseAt := earliest.Truncate(time.Second).Add(time.Nanosecond)
					response := reviewprovider.IssueComment{ID: 92, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\nYour [plan](https://docs.coderabbit.ai/management/plans#fair-usage-limits-policy) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). Reviews are available now.", Author: provider, CreatedAt: responseAt, UpdatedAt: responseAt}
					observed, err := reviewprovider.ResolveHostedAllowance(reviewprovider.HostedAllowanceSnapshot{IssueComments: []reviewprovider.IssueComment{comment, response}, Complete: true, ObservedAt: earliest.Add(time.Second)}, reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA}, poster, provider, earliest.Truncate(time.Second))
					if err != nil || observed.State != "available_observed" || observed.QueryCommentID != comment.ID || observed.QueryCreatedAt != comment.CreatedAt {
						t.Fatal("recovery lost original provider query", observed, err)
					}
				}
				confirmed, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, comment)
				if err != nil || confirmed.Revision != 2 || confirmed.Comment == nil || *confirmed.Comment != comment || confirmed.EarliestPosting != original.EarliestPosting || confirmed.Anchor != original.Anchor {
					t.Fatal("bounded provider skew rejected or rewritten", err)
				}
				before := hostedQueryState(t, f, a)
				for _, reopen := range []bool{false, true} {
					if reopen {
						cliAllowanceReopen(t, f)
					}
					read, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
					if err != nil || !reflect.DeepEqual(read, confirmed) {
						t.Fatal("retained skew lost on read/reopen", err)
					}
					replay, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, comment)
					if err != nil || !reflect.DeepEqual(replay, confirmed) {
						t.Fatal("confirmation replay lost original timestamps", err)
					}
					replay, created, err = f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
					if err != nil || created || !reflect.DeepEqual(replay, confirmed) || hostedQueryState(t, f, a) != before {
						t.Fatal("replay changed journal or granted another post", err)
					}
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_query_posted'`) != 1 {
					t.Fatal("confirmation duplicated event")
				}
			})
		}
	}
}

func TestHostedQueryConfirmationSkewKeepsOwnershipAndTimeBounds(t *testing.T) {
	for _, damage := range []string{"older-than-five-seconds", "wrong-actor", "wrong-body", "edited", "future"} {
		t.Run(damage, func(t *testing.T) {
			f, a, anchor, poster, provider := hostedQueryFixture(t)
			query, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
			if err != nil {
				t.Fatal(err)
			}
			comment := hostedQueryComment(t, query)
			comment.CreatedAt = comment.CreatedAt.Add(-5 * time.Second)
			comment.UpdatedAt = comment.CreatedAt
			switch damage {
			case "older-than-five-seconds":
				comment.CreatedAt = comment.CreatedAt.Add(-time.Nanosecond)
				comment.UpdatedAt = comment.CreatedAt
			case "wrong-actor":
				comment.Author.ID++
			case "wrong-body":
				comment.Body += " edited"
			case "edited":
				comment.UpdatedAt = comment.UpdatedAt.Add(time.Nanosecond)
			case "future":
				comment.CreatedAt = time.Now().UTC().Add(time.Minute)
				comment.UpdatedAt = comment.CreatedAt
			}
			before := hostedQueryState(t, f, a)
			_, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, comment)
			attemptCode(t, err, "review_conflict")
			if hostedQueryState(t, f, a) != before {
				t.Fatal("invalid confirmation changed journal")
			}
		})
	}
}
