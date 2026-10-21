package reviewexec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Exercise the actual admitted provider snapshot ceiling, while retaining the
// ordinary background-write deadline and the observer's 100-run append budget.
func TestExternalHostedMaximumInventoryUsesOrdinaryWriteBudget(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	snapshot := externalSnapshot(10000, strings.Repeat("a", 40))
	snapshot.ObservedAt = snapshot.ObservedAt.Add(4 * time.Hour)
	r := externalReader(snapshot)
	discovered, err := reviewprovider.DiscoverExternalHosted(snapshot, r.TrustedHostedActor())
	if err != nil || len(discovered) != 10000 {
		t.Fatalf("fixture must be fully supported provider inventory: %d %v", len(discovered), err)
	}
	target, err := f.e.claimLifecycle(t.Context(), map[string]TargetReader{"test-capability": r}, s)
	if err != nil || target == nil {
		t.Fatal(target, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	started := time.Now()
	err = f.e.observeLifecycle(ctx, *target, s)
	t.Logf("complete source with %d groups: %s", len(discovered), time.Since(started))
	if err != nil {
		t.Fatalf("supported provider inventory stopped lifecycle publication: %v", err)
	}
	if got := externalArchiveCount(t, f); got != 100 {
		t.Fatalf("first bounded append = %d; want 100", got)
	}
}
