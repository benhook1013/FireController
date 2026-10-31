package reviewruntime

import (
	"context"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestRefreshMaterializedSourceRenewsOnlyFromVerifiedProviderReads(t *testing.T) {
	f := newHostedDriverFixture(t)
	operation, _, configuration := f.startHosted(t)
	plan, err := reviewexec.ResolveSource(t.Context(), f.provider, 901, configuration, operationRequest(operation))
	if err != nil {
		t.Fatal(err)
	}
	plan.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	oldReads := f.reader.repositoryCalls
	fresh, err := refreshMaterializedSource(t.Context(), f.provider, 901, configuration, operation, plan)
	observed, parseErr := time.Parse(time.RFC3339Nano, fresh.ObservedAt)
	if err != nil || parseErr != nil || time.Since(observed) > time.Second || !samePreparedSource(fresh, plan) || fresh.ObservedAt == plan.ObservedAt || f.reader.repositoryCalls != oldReads+1 {
		t.Fatal("aged facts were not genuinely reread", fresh, err, parseErr)
	}
	if f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal("source renewal sent a provider command")
	}
}

func TestHostedColdMaterializationRechecksMovedComparisonBeforeQuery(t *testing.T) {
	f := newHostedDriverFixture(t)
	operation, attempt, configuration := f.startHosted(t)
	verify := f.hosted.verify
	f.hosted.verify = func(ctx context.Context, workspace reviewworkspace.Workspace) error {
		if err := verify(ctx, workspace); err != nil {
			return err
		}
		// A real source move during preparation, without a wall-clock sleep.
		pull := f.reader.pulls[2]
		pull.BaseSHA = f.headSHA
		f.reader.pulls[2] = pull
		return nil
	}
	_, err := f.hosted.Prepare(t.Context(), operation, configuration, attempt)
	sourceHoldReason(t, err, "source_changed")
	if f.provider.queries != 0 || f.provider.reviews != 0 || f.reader.repositoryCalls != 2 {
		t.Fatal("changed source reached allowance/provider execution", f.provider.queries, f.provider.reviews, f.reader.repositoryCalls)
	}
}

func TestAgedCLISourceRefreshSkipsFreshAndRejectsMovedComparison(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, _, cfg := f.startHosted(t)
	plan, err := reviewexec.ResolveSource(t.Context(), f.provider, 901, cfg, operationRequest(op))
	if err != nil {
		t.Fatal(err)
	}
	before := f.reader.repositoryCalls
	fresh, err := refreshAgedMaterializedSource(t.Context(), f.provider, 901, cfg, op, plan, time.Now())
	if err != nil || fresh.ObservedAt != plan.ObservedAt || f.reader.repositoryCalls != before {
		t.Fatal("fresh source added reads", err)
	}
	plan.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	fresh, err = refreshAgedMaterializedSource(t.Context(), f.provider, 901, cfg, op, plan, time.Now())
	if err != nil || fresh.ObservedAt == plan.ObservedAt || f.reader.repositoryCalls != before+1 {
		t.Fatal("aged source was not verified", err)
	}
	moved := f.reader.pulls[2]
	moved.HeadSHA = f.baseSHA
	f.reader.pulls[2] = moved
	_, err = refreshAgedMaterializedSource(t.Context(), f.provider, 901, cfg, op, plan, time.Now())
	if err == nil {
		t.Fatal("moved source admitted")
	}
	if f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal("source check requested provider work")
	}
}
