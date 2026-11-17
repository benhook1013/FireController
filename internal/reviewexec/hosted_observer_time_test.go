package reviewexec

import (
	"testing"
	"time"
)

func TestHostedObserverInvalidSourceTimePreservesValidatedFreshness(t *testing.T) {
	for _, source := range []time.Time{time.Time{}, time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		t.Run(source.String(), func(t *testing.T) {
			e, o := observerFixture(t)
			at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			first := e.BeginHostedObserver("test-capability", o.ProjectID, o.ID, o.AttemptID)
			e.FinishHostedObserver(first, at, at, "publication_not_proven", nil)
			current := e.BeginHostedObserver("test-capability", o.ProjectID, o.ID, o.AttemptID)
			e.FinishHostedObserver(current, at.Add(time.Minute), source, "observation_unavailable", nil)
			got := e.observerStatus(o)
			if got.LastSourceAt != at.Format(time.RFC3339Nano) || got.LastCheckAt != at.Add(time.Minute).Format(time.RFC3339Nano) {
				t.Fatal("invalid source time erased valid freshness or hid completed check", got)
			}
		})
	}
}
