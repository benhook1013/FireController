package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func sourceRefreshStatusFixture(t *testing.T, ci, merge string) *attemptFixture {
	t.Helper()
	f := attemptProjectionFixture(t, false)
	p := projectionPull(2)
	p.CIState, p.Mergeability = ci, merge
	p.Runs = []reviews.RunInput{{SourceRunID: "retained-status-run", Channel: "hosted", HeadSHA: p.HeadSHA, Scope: "retained comparison", Outcome: "completed", Attributable: true, Complete: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Findings: []reviews.Finding{{SourceFindingID: "retained-status-finding", Title: "Retained finding", Detail: "Retained decision evidence", Severity: "low", Disposition: "rejected"}}}}
	p.Attempts = []reviews.Attempt{{SourceAttemptID: "retained-status-attempt", Channel: "cli", HeadSHA: p.HeadSHA, State: "prepared"}}
	r := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "imported-history", ObservedAt: "2026-01-01T00:00:00Z", Queue: []reviews.PullInput{projectionPull(4), p}}
	if w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "status-seed", encoded(r)); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	return f
}

func sourceRefreshStatusQueue(t *testing.T, f *attemptFixture) reviews.QueueResponse {
	t.Helper()
	var q reviews.QueueResponse
	projectionRead(t, f, "/queue", &q)
	if len(q.Items) != 2 || q.Items[0].Number != 4 || q.Items[1].Number != 2 {
		t.Fatal("queue order changed", q.Items)
	}
	return q
}

func sourceRefreshStatusPull(number int64) reviewprovider.Pull {
	p := lifecyclePull(number)
	p.ChangedFiles = 1
	return p
}

func sourceRefreshStatusRun(t *testing.T, f *attemptFixture, request SourceRefreshRequest, reader TargetReader) SourceRefreshOperation {
	t.Helper()
	receipt := refreshEnqueue(t, f, "status-refresh", request)
	worked, err := f.e.refreshNext(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || !worked {
		t.Fatal("refresh failed", worked, err)
	}
	return refreshRead(t, f, receipt.ID)
}

func TestSourceRefreshQueueStatusReadinessKeepsUnknownCIAndRetainedEvidence(t *testing.T) {
	f := sourceRefreshStatusFixture(t, "unknown", "unknown")
	roundGrant(t, f, 2, "cli", 0, 0, 2, "status-rounds")
	rounds := roundRead(t, f, 2, "cli")
	before := sourceRefreshStatusQueue(t, f)
	history := projectionDigest(t, f, before.SnapshotID)
	reader := newReadinessTestReader()
	reader.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
		p := sourceRefreshStatusPull(2)
		p.Mergeability = "unknown" // The later complete readiness read has the answer.
		return p, nil
	}
	reader.ci = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		now := time.Now().UTC()
		return reviewprovider.CIObservation{Target: target, StartedAt: now, ObservedAt: now, State: "unproven", Reason: "ci_merge_identity_unproven", MergeState: "unproven", Candidate: &reviewprovider.CICandidate{State: "unproven", Reason: "ci_merge_identity_unproven", RepositoryID: target.RepositoryID, Repository: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, MergeState: "unproven", ObservedAt: now}}, nil
	}
	got := sourceRefreshStatusRun(t, f, readinessRequest(2, 1), reader)
	if got.State != "completed" || got.ReviewState == nil || got.ReviewState.Mergeable != "MERGEABLE" || got.CI == nil || got.CI.Reason != "ci_merge_identity_unproven" {
		t.Fatal("retained readiness changed", got)
	}
	after := sourceRefreshStatusQueue(t, f)
	p := after.Items[1]
	if p.Mergeability != "mergeable" || p.CIState != "unknown" {
		t.Fatal("independent status did not reach ordinary queue", p)
	}
	old := before.Items[1]
	if p.RunCount != old.RunCount || p.AttemptCount != old.AttemptCount || !reflect.DeepEqual(p.RunCounts, old.RunCounts) || !reflect.DeepEqual(p.RecentRuns, old.RecentRuns) || !reflect.DeepEqual(p.LatestAttempts, old.LatestAttempts) || projectionDigest(t, f, before.SnapshotID) != history || !reflect.DeepEqual(roundRead(t, f, 2, "cli"), rounds) {
		t.Fatal("status publication changed retained evidence, counts, history or round controls")
	}
	if reader.calls.Load() != 2 || reader.ciCalls.Load() != 1 || reader.reviewCalls.Load() != 1 {
		t.Fatal("publication added provider work")
	}
}

func TestSourceRefreshQueueStatusCIStates(t *testing.T) {
	for _, state := range []string{"satisfied", "failed", "pending", "not_required", "unproven"} {
		t.Run(state, func(t *testing.T) {
			f := sourceRefreshStatusFixture(t, "success", "unknown")
			reader := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
				return sourceRefreshStatusPull(2), nil
			}}, ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				if state == "unproven" {
					return *unavailableSourceCI(target, time.Now().UTC(), "ci_unproven"), nil
				}
				return sourceCITestObservation(target, state), nil
			}}
			got := sourceRefreshStatusRun(t, f, sourceCIRequest(), reader)
			want := map[string]string{"satisfied": "success", "failed": "failure", "pending": "pending", "not_required": "not_required", "unproven": "unknown"}[state]
			p := sourceRefreshStatusQueue(t, f).Items[1]
			if got.State != "completed" || got.CI == nil || got.CI.State != state || p.CIState != want || p.Mergeability != "mergeable" {
				t.Fatal("incorrect CI projection", got, p)
			}
		})
	}
}

func TestSourceRefreshQueueStatusSourceOnlyComparisonAndNullableMergeability(t *testing.T) {
	for _, change := range []string{"unchanged", "head", "base", "unknown"} {
		t.Run(change, func(t *testing.T) {
			f := sourceRefreshStatusFixture(t, "success", "conflicting")
			p := sourceRefreshStatusPull(2)
			request := refreshRequest()
			wantCI, wantMerge := "success", "mergeable"
			switch change {
			case "head":
				p.HeadSHA, request.HeadSHA, wantCI = strings.Repeat("c", 40), strings.Repeat("c", 40), "unknown"
			case "base":
				p.BaseSHA, wantCI = strings.Repeat("c", 40), "unknown"
			case "unknown":
				p.Mergeability, wantMerge = "unknown", "unknown"
			}
			reader := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return p, nil }}
			got := sourceRefreshStatusRun(t, f, request, reader)
			current := sourceRefreshStatusQueue(t, f).Items[1]
			if got.State != "completed" || got.CI != nil || got.ReviewState != nil || current.CIState != wantCI || current.Mergeability != wantMerge || current.HeadSHA != p.HeadSHA || current.BaseSHA != p.BaseSHA {
				t.Fatal("source-only status inherited unsupported credit", got, current)
			}
		})
	}
}

func TestSourceRefreshQueueStatusReadinessFallback(t *testing.T) {
	for _, outcome := range []string{"conflicting", "unknown", "unavailable", "wrong-comparison"} {
		t.Run(outcome, func(t *testing.T) {
			f := sourceRefreshStatusFixture(t, "unknown", "unknown")
			reader := newReadinessTestReader()
			reader.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
				return sourceRefreshStatusPull(2), nil
			}
			reader.review = func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.PullReviewObservation, error) {
				if outcome == "unavailable" {
					return reviewprovider.PullReviewObservation{}, errors.New("synthetic unavailable")
				}
				mergeable := strings.ToUpper(outcome)
				if outcome == "wrong-comparison" {
					target.HeadSHA, mergeable = strings.Repeat("c", 40), "CONFLICTING"
				}
				return reviewprovider.PullReviewObservation{Target: target, State: "complete", ObservedAt: time.Now().UTC(), PullState: "OPEN", Mergeable: mergeable, MergeStateStatus: "UNKNOWN", Threads: []reviewprovider.PullReviewThread{}}, nil
			}
			got := sourceRefreshStatusRun(t, f, readinessRequest(2, 1), reader)
			want := outcome
			if outcome == "unavailable" || outcome == "wrong-comparison" {
				want = "mergeable"
			}
			if p := sourceRefreshStatusQueue(t, f).Items[1]; got.State != "completed" || p.Mergeability != want {
				t.Fatal("readiness override or source fallback failed", got, p)
			}
		})
	}
}

func TestSourceRefreshQueueStatusRejectsStalePublication(t *testing.T) {
	for _, change := range []string{"metadata", "comparison", "configuration"} {
		t.Run(change, func(t *testing.T) {
			f := sourceRefreshStatusFixture(t, "unknown", "unknown")
			reader := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
				if change == "configuration" {
					// Change only the capability, preserving the selected display order.
					c := enrollmentConfiguration(1, []int64{4, 2}, []string{"cli", "hosted"})
					c.CapabilityID = "other-account"
					if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "status-capability-change", c); err != nil {
						t.Fatal(err)
					}
				} else {
					err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
						pins, err := reviews.PinSourceMetadata(ctx, tx, attemptProject, []int64{2})
						if err != nil {
							return err
						}
						p := sourceDisplayPull(SourcePlan{Pull: sourceRefreshStatusPull(2)})
						p.Title = "Concurrent source metadata"
						if change == "comparison" {
							p.HeadSHA = strings.Repeat("c", 40)
						}
						_, err = reviews.PublishSourceMetadata(ctx, tx, core.Scope{ProjectID: attemptProject, PrincipalID: "concurrent-source"}, 801, pins, []reviews.PullInput{p})
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				return sourceRefreshStatusPull(2), nil
			}}}
			got := sourceRefreshStatusRun(t, f, sourceCIRequest(), reader)
			p := sourceRefreshStatusQueue(t, f).Items[1]
			if p.CIState != "unknown" || p.Mergeability != "unknown" || change == "configuration" && (got.State != "held" || got.Reason != "source_configuration_changed") || change != "configuration" && (got.State != "completed" || p.Title != "Concurrent source metadata") || change == "comparison" && p.HeadSHA != strings.Repeat("c", 40) {
				t.Fatal("stale observation replaced concurrent state", got, p)
			}
		})
	}
}

func TestSourceRefreshQueueStatusRollsBackWithCompletion(t *testing.T) {
	f := sourceRefreshStatusFixture(t, "unknown", "unknown")
	before := sourceRefreshStatusQueue(t, f)
	admissionSQL(t, f, `CREATE TRIGGER reject_status_completion BEFORE UPDATE ON review_source_refreshes WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT, 'synthetic completion failure'); END`)
	refreshEnqueue(t, f, "atomic-status", sourceCIRequest())
	reader := &sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
		return sourceRefreshStatusPull(2), nil
	}}}
	if worked, err := f.e.refreshNext(t.Context(), map[string]TargetReader{"test-capability": reader}); !worked || err == nil {
		t.Fatal("completion failure not exercised", worked, err)
	}
	if after := sourceRefreshStatusQueue(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("queue status committed without its source observation", before, after)
	}
}
