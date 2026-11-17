package reviewruntime

import (
	"testing"
	"time"
)

func TestRetainHostedLateStandingSummaryPreservesEvidenceAndCountsOnce(t *testing.T) {
	f, record := runtimeStandingFixture(t, nil, nil)
	record.Snapshot.IssueComments[1].UpdatedAt = record.Snapshot.IssueComments[2].UpdatedAt.Add(time.Second)
	record.Snapshot.ObservedAt = record.Snapshot.IssueComments[1].UpdatedAt.Add(time.Second)
	record.FinishedAt = record.Snapshot.ObservedAt
	first, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || !first.Complete {
		t.Fatal("late exact-run inventory was not retained", first, err)
	}
	second, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || second != first || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 1 {
		t.Fatal("same original observation was credited twice", second, err)
	}
}
