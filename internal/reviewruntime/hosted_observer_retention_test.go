package reviewruntime

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHostedObserverRetentionFailurePreservesValidatedFreshness(t *testing.T) {
	for _, failure := range []string{"capture save", "observation journal"} {
		t.Run(failure, func(t *testing.T) {
			f, captures, a, reader := recoveryRuntimeFixture(t, "active")
			if recoveryRuntimeCount(t, f, "review_hosted_observations") != 0 {
				t.Fatal("fixture already retained a post-trigger source")
			}
			at := time.Now().UTC().Add(-time.Hour)
			first := f.engine.BeginHostedObserver("runtime-capability", f.project, a.OperationID, a.ID)
			f.engine.FinishHostedObserver(first, at, at, "publication_not_proven", nil)
			if failure == "capture save" {
				captures = &HostedCaptureStore{root: filepath.Join(t.TempDir(), "missing")}
			} else if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_observer_retention BEFORE INSERT ON review_hosted_observations BEGIN SELECT RAISE(ABORT, 'injected retention failure'); END`); err != nil {
				t.Fatal(err)
			}
			before := cliAttemptState(t, f.store, a.ID)
			events := recoveryRuntimeCount(t, f, "events")
			got, err := recoverRuntime(t, f, captures, a, reader)
			if err == nil || got.Reason != "observation_unavailable" || len(reader.calls) != 2 {
				t.Fatal("fixture did not fail retention after fresh collection", got, err, reader.calls)
			}
			status, err := f.engine.ReadOperationStatus(t.Context(), f.scope(t, f.writer, false), a.OperationID)
			if err != nil || status.Observer == nil {
				t.Fatal(status, err)
			}
			observer := status.Observer
			if observer.State != "retrying" || observer.Error != "observer_unavailable" || observer.LastSourceAt != at.Format(time.RFC3339Nano) || observer.LastCheckAt == "" || observer.LastCheckAt == at.Format(time.RFC3339Nano) {
				t.Fatal("failed retention advertised fresh validated source", observer)
			}
			if cliAttemptState(t, f.store, a.ID) != before || recoveryRuntimeCount(t, f, "events") != events || recoveryRuntimeCount(t, f, "review_hosted_observations") != 0 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 {
				t.Fatal("failed diagnostic pass changed execution, credit, or reservation")
			}
		})
	}
}
