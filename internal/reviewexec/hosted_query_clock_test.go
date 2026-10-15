package reviewexec

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
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
