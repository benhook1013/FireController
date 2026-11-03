package reviewruntime

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"testing"
	"time"
)

func TestHostedSchedulerKeepsAmbiguousOriginalTriggerPendingWithoutLongBackoff(t *testing.T) {
	f, a, p := schedulerUncertainFixture(t)
	f.provider.partial = true
	query, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	lastReason := ""
	p.alterSnapshot = func(s *reviewprovider.HostedPublicationSnapshot) {
		var posted time.Time
		for _, c := range s.IssueComments {
			if c.ID == 200 {
				posted = c.CreatedAt
			}
		}
		s.IssueComments = append(s.IssueComments, reviewprovider.IssueComment{ID: 900, Body: "Earlier note edited while this review is pending.", Author: reviewprovider.Author{ID: 777, Login: "other-user", Kind: "User"}, CreatedAt: posted.Add(-time.Second), UpdatedAt: posted.Add(time.Nanosecond)})
		extracted, err := reviewprovider.ExtractHostedFindings(*s, reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: a.Anchor.CandidateSHA}, query.PostingActor, query.ProviderActor, posted)
		if err != nil {
			t.Error(err)
		}
		lastReason = extracted.Reason
	}
	gate := newSchedulerGate(f.hosted)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if i > 0 {
			gate.resume(t)
		}
		gate.interval(t, 30*time.Second)
		if lastReason != "attribution_window_closed" || hostedDriverJournal(t, f, a.ID) != "active" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
			t.Fatal("pending source was accepted or not reproduced", lastReason)
		}
	}
	p.repositoryError = &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 180, Message: "Synthetic provider limit"}
	gate.resume(t)
	gate.interval(t, 180*time.Second)
	p.repositoryError = nil
	gate.resume(t)
	gate.interval(t, 30*time.Second)
	if hostedDriverJournal(t, f, a.ID) != "active" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || f.provider.queries != 1 || f.provider.reviews != 1 {
		t.Fatal("ambiguous inventory gained completion credit or triggered another action")
	}
	cancel()
	schedulerDone(t, done, false)
}
