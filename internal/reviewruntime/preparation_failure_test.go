package reviewruntime

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type preparationFailingSource struct {
	reviewexec.TargetReader
	failure error
}

func (r preparationFailingSource) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	return reviewprovider.Repository{}, r.failure
}

type preparationFailingHosted struct {
	HostedProvider
	failure error
}

func (r preparationFailingHosted) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	return reviewprovider.Repository{}, r.failure
}

func TestNativePreparationFailureAtSourceBeforeProviderAction(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			failure := &reviewprovider.Error{Code: "provider_unauthorized", Message: "/private/path ghp_secret provider response"}
			var err error
			var f *cliDriverFixture
			if channel == "hosted" {
				hosted := newHostedDriverFixture(t)
				f = hosted.cliDriverFixture
				op, a, c := hosted.startHosted(t)
				hosted.hosted.provider = preparationFailingHosted{HostedProvider: hosted.provider, failure: failure}
				_, err = hosted.hosted.Prepare(t.Context(), op, c, a)
				if len(hosted.hosted.prepared) != 0 {
					t.Fatal("failed preparation leaked permit")
				}
				if hosted.provider.queries != 0 || hosted.provider.reviews != 0 {
					t.Fatal("failed source invoked Hosted allowance or review")
				}
			} else {
				f = newCLIDriverFixture(t)
				op, a, c := f.start(t, 2, 1, "source-failure")
				f.driver.reader = preparationFailingSource{TargetReader: f.reader, failure: failure}
				_, err = f.driver.Prepare(t.Context(), op, c, a)
				if len(f.driver.prepared) != 0 {
					t.Fatal("failed preparation leaked permit")
				}
			}
			if err == nil || err.Error() != "preparation_source_resolution_provider_unauthorized" || !errors.Is(err, failure) {
				t.Fatalf("missing safe source attribution: %v", err)
			}
			if f.fetches.Load() != 0 {
				t.Fatal("failed source invoked allowance, review or Git")
			}
			if _, err = os.Stat(f.log); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed source ran CLI", err)
			}
		})
	}
}

type transientCLISource struct {
	reviewexec.TargetReader
	pullCalls int
	failures  int
}

func (r *transientCLISource) Pull(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	r.pullCalls++
	if r.pullCalls <= r.failures {
		return reviewprovider.Pull{}, &reviewprovider.Error{Code: "provider_unavailable", Status: http.StatusServiceUnavailable}
	}
	return r.TargetReader.Pull(ctx, repository, number)
}

func TestCLIDriverRetriesTransientSourceBeforeWorkspace(t *testing.T) {
	f := newCLIDriverFixture(t)
	op, attempt, configuration := f.start(t, 2, 1, "transient-source")
	reader := &transientCLISource{TargetReader: f.reader, failures: 1}
	f.driver.reader = reader
	prepared, err := f.driver.Prepare(t.Context(), op, configuration, attempt)
	if err != nil || prepared.Anchor.CandidateSHA != op.HeadSHA {
		t.Fatal("original source did not recover", err)
	}
	if reader.pullCalls != 2 || f.reader.repositoryCalls != 1 || f.fetches.Load() != 1 {
		t.Fatal("source retry repeated successful reads or workspace preparation", reader.pullCalls, f.reader.repositoryCalls, f.fetches.Load())
	}
	body, err := os.ReadFile(f.log)
	if err != nil || strings.TrimSpace(string(body)) != "usage --agent" {
		t.Fatal("transient source retry repeated allowance or dispatched a review", err)
	}
}

func TestCLIPreparationRetriesAgedSourceWithoutAcceptingMovedComparison(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		moved    bool
	}{
		{name: "recovers", failures: 1},
		{name: "changed_comparison", failures: 1, moved: true},
		{name: "bounded", failures: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			op, _, configuration := f.start(t, 2, 1, "transient-final-source")
			plan, err := reviewexec.ResolveSource(t.Context(), f.reader, 901, configuration, operationRequest(op))
			if err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
				plan.ObservedAt = now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
				if tc.moved {
					moved := f.reader.pulls[2]
					moved.BaseSHA = f.headSHA
					f.reader.pulls[2] = moved
				}
				reader := &transientCLISource{TargetReader: f.reader, failures: tc.failures}
				refreshed, err := retryCLIPreparationSource(t.Context(), op.ID, "source_refresh", func() (reviewexec.SourcePlan, error) {
					return refreshAgedMaterializedSource(t.Context(), reader, 901, configuration, op, plan, now)
				})
				if reader.pullCalls != 2 || time.Since(now) != 5*time.Second {
					t.Fatal("unexpected source retry", reader.pullCalls, time.Since(now))
				}
				switch {
				case tc.moved:
					sourceHoldReason(t, err, "source_changed")
				case tc.failures == 2:
					if failure := reviewexec.PreparationFailureAt("source_refresh", err); failure == nil || failure.Error() != "preparation_source_refresh_provider_unavailable" {
						t.Fatal("retry lost the final refresh diagnostic", failure)
					}
				default:
					if err != nil || !samePreparedSource(refreshed, plan) || refreshed.ObservedAt == plan.ObservedAt {
						t.Fatal("retry failed to refresh the original comparison", err)
					}
				}
			})
		})
	}
}
