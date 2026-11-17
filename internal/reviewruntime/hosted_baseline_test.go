package reviewruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedDriverFullBaselineRetainedBeforeAdmission(t *testing.T) {
	f := newHostedDriverFixture(t)
	f.provider.delay = 1
	op, a, c := f.startHosted(t)
	calls := 0
	f.provider.baselineHook = func(ctx context.Context, snapshot *reviewprovider.HostedPublicationSnapshot) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second || time.Until(deadline) <= 0 {
			t.Fatal("full phase has no scoped15s budget")
		}
		if hostedDriverJournal(t, f, a.ID) != "preparing" || f.provider.queries != 1 || f.provider.reviews != 0 || f.provider.histories != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_preflight WHERE attempt_id=?`, a.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=? AND reason='admitted'`, a.ID) != 0 {
			t.Fatal("full baseline did not follow retained available query before admission")
		}
		// Preserve substantive prior provider bodies as well as proven empty lists.
		at := time.Now().UTC()
		snapshot.Reviews = []reviewprovider.HostedReview{{ID: 600, HeadSHA: f.headSHA, State: "commented", Body: "Previous provider Run ID " + runtimeStandingRun, Author: f.provider.bot, SubmittedAt: at}}
		snapshot.ReviewComments = []reviewprovider.ReviewComment{{ID: 601, ReviewID: 600, HeadSHA: f.headSHA, OriginalHeadSHA: f.headSHA, Path: "reviewed.txt", Body: "Prior provider detail", Author: f.provider.bot, CreatedAt: at, UpdatedAt: at}}
		snapshot.ObservedAt = at
		return nil
	}
	prepared, err := f.hosted.Prepare(t.Context(), op, c, a)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || len(journal) != 3 || calls != 1 || f.provider.baselines != 1 || f.provider.observations != 0 {
		t.Fatal("full baseline repeated/missing", len(journal), calls, err)
	}
	query, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, observation := range journal {
		in := observation.Input
		source, err := f.hostedCaptures.Read(t.Context(), a.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes})
		if err != nil || in.Kind != "allowance_query" || in.Intent != query.Intent || in.ExpectedAnchor != query.Anchor || !source.Snapshot.Complete {
			t.Fatal("baseline source binding", err)
		}
		if i < 2 {
			if source.Snapshot.Reviews != nil || source.Snapshot.ReviewComments != nil {
				t.Fatal("query-only source pretended it fetched reviews")
			}
		} else if source.Snapshot.IssueComments == nil || len(source.Snapshot.Reviews) != 1 || len(source.Snapshot.ReviewComments) != 1 || source.Snapshot.Reviews[0].Body != "Previous provider Run ID "+runtimeStandingRun || !hostedBaselineQueryPresent(source.Snapshot, query) {
			t.Fatal("full source arrays or prior provider bodies lost")
		}
	}
	allowanceBefore, err := f.engine.ReadHostedAllowance(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_hosted_preflight WHERE attempt_id=?`, a.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_preflight' AND resource_id=?`, a.ID) != 1 {
		t.Fatal("full history replaced or duplicated allowance action")
	}
	a, err = f.engine.Admit(t.Context(), f.project, a.ID, a.Revision, prepared.Anchor, prepared.Intent)
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := f.engine.ReadHostedPostingBoundary(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	finish, err := time.Parse(time.RFC3339Nano, journal[2].Input.FinishedAt)
	if err != nil || finish.After(boundary) {
		t.Fatal("full baseline was fetched after admission", finish, boundary, err)
	}
	allowanceAfter, err := f.engine.ReadHostedAllowance(t.Context(), f.project, a.ID)
	if err != nil || !reflect.DeepEqual(allowanceBefore, allowanceAfter) || f.provider.queries != 1 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("baseline created spend/final evidence or changed allowance", err)
	}
}

func TestHostedDriverFullBaselineFailureHoldsBeforeReviewSpend(t *testing.T) {
	for _, mode := range []string{"provider_failure", "cancelled", "incomplete", "nil_reviews", "nil_inline", "nil_issues", "missing_query", "edited_query"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			op, a, c := f.startHosted(t)
			f.provider.baselineHook = func(_ context.Context, s *reviewprovider.HostedPublicationSnapshot) error {
				switch mode {
				case "provider_failure":
					return errors.New("synthetic full-history failure")
				case "cancelled":
					return context.Canceled
				case "incomplete":
					s.Complete = false
				case "nil_reviews":
					s.Reviews = nil
				case "nil_inline":
					s.ReviewComments = nil
				case "nil_issues":
					s.IssueComments = nil
				case "missing_query":
					s.IssueComments = s.IssueComments[1:]
				case "edited_query":
					s.IssueComments[0].Body += " changed"
				}
				return nil
			}
			prepared, err := f.hosted.Prepare(t.Context(), op, c, a)
			if err == nil || !reflect.DeepEqual(prepared, reviewexec.Preparation{}) || hostedDriverJournal(t, f, a.ID) != "preparing" || f.provider.baselines != 1 || f.provider.observations != 0 || f.provider.queries != 1 || f.provider.reviews != 0 {
				t.Fatal("incomplete baseline permitted preparation/spend", prepared, err)
			}
			journal, readErr := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
			if readErr != nil || len(journal) != 1 || journal[0].Input.Kind != "allowance_query" || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_preflight WHERE attempt_id=?`, a.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=? AND reason='admitted'`, a.ID) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("failure lost query diagnostics or admitted/finalized", journal, readErr)
			}
			f.hosted.mu.Lock()
			_, exists := f.hosted.prepared[a.ID]
			f.hosted.mu.Unlock()
			if exists {
				t.Fatal("failed full baseline left a dispatchable prepared permit")
			}
		})
	}
}
