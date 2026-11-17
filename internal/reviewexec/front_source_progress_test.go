package reviewexec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestFrontSourceFailureReportsVerifiedCoverageWithoutClaimingFront(t *testing.T) {
	for _, stage := range []string{"repository", "prefix"} {
		t.Run(stage, func(t *testing.T) {
			f := frontFixture(t)
			reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
				if n == 4 {
					return reviewprovider.Pull{}, errRunSourceBudget
				}
				return selectionSnapshotReader{merged: true}.Pull(ctx, repo, n)
			}}
			count, next := 1, int64(4)
			if stage == "repository" {
				count, next = 0, 2
				reader.repository = func(context.Context, int64) (reviewprovider.Repository, error) {
					return reviewprovider.Repository{}, errRunSourceBudget
				}
			}
			receipt, _ := selectionEnqueue(t, f, "source-progress", "cli", nil)
			selectionNext(t, f, reader)
			calls := reader.calls.Load()
			got := frontChannel(t, frontRead(t, f), "cli")
			if got.State != "unknown" || got.Reason != "source_budget_exhausted" || got.PullNumber != 0 || got.SourceProgress == nil {
				t.Fatal("failed acquisition claimed a front or lost attribution", got)
			}
			if got.QueuedPriority == nil || got.QueuedPriority.PullNumber != next {
				t.Fatal("source failure erased local priority", got)
			}
			if *got.SourceProgress != (FrontSourceProgress{VerifiedPullCount: count, QueuePullCount: 2, NextUnverifiedPullNumber: next}) {
				t.Fatal("incorrect retained coverage", got.SourceProgress)
			}
			if reader.calls.Load() != calls || selectionRead(t, f, receipt.ID).OperationID != "" || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
				t.Fatal("diagnostic acquired source or dispatched work")
			}
			// A healthy channel has no failed-acquisition attribution. Diagnostic
			// coverage is historical and must never escape a changed binding fence.
			frontSelect(t, f, "healthy-hosted", "hosted", &refreshTestReader{})
			hosted := frontChannel(t, frontRead(t, f), "hosted")
			raw, err := json.Marshal(hosted)
			if err != nil || hosted.State != "ready" || hosted.SourceProgress != nil || strings.Contains(string(raw), "source_progress") {
				t.Fatal(hosted, err)
			}
			admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
			got = frontChannel(t, frontRead(t, f), "cli")
			if got.Reason != "repository_changed" || got.SourceProgress != nil || got.QueuedPriority != nil {
				t.Fatal("stale binding supplied diagnostic coverage", got)
			}
		})
	}
}
