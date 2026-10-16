package reviewruntime

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

// These are trusted synthetic references, not 200 fabricated provider files.
// The recovery guard must stop before it would need to create the next artifact.
func fillHostedRecoveryObservations(t *testing.T, f *cliEvidenceFixture, a reviewexec.Attempt) {
	t.Helper()
	observations, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	for i := len(observations); i < 200; i++ {
		checksum := fmt.Sprintf("%064x", i+1)
		_, err = f.engine.RecordHostedObservation(t.Context(), f.project, a.ID, reviewexec.HostedObservationInput{
			CaptureID: "review_capture_" + checksum[:32], CaptureSHA256: checksum, CaptureBytes: 1024,
			StartedAt: at, FinishedAt: at, ObservedAt: at, ExpectedAnchor: *a.Anchor, Intent: a.Intent,
		})
		if err != nil {
			t.Fatal("fill owned reference journal", i, err)
		}
	}
	if recoveryRuntimeCount(t, f, "review_hosted_observations") != 200 {
		t.Fatal("capacity fixture did not retain exactly 200 references")
	}
}

func hostedCapacityArtifacts(t *testing.T, captures *HostedCaptureStore) []string {
	t.Helper()
	entries, err := os.ReadDir(captures.root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestRecoverHostedObservationCapacityStopsBeforeProviderAndArtifact(t *testing.T) {
	for _, state := range []string{"posting", "active"} {
		t.Run(state, func(t *testing.T) {
			f, captures, a, reader := recoveryRuntimeFixture(t, state)
			fillHostedRecoveryObservations(t, f, a)
			reader.failure = errors.New("provider must not be reached at capacity")
			beforeFiles := hostedCapacityArtifacts(t, captures)
			beforeTransitions := recoveryRuntimeCount(t, f, "review_execution_transitions")
			got, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || got.State != "uncertain" || got.Reason != "hosted_observation_capacity" || got.EvidenceID != "" || len(reader.calls) != 0 {
				t.Fatal("capacity attempted observation or lost safe outcome", got, err, reader.calls)
			}
			var journalState, reason string
			if err := f.store.DB().QueryRow(`SELECT a.state,o.reason FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE a.id=?`, a.ID).Scan(&journalState, &reason); err != nil || journalState != "uncertain" || reason != got.Reason {
				t.Fatal("capacity outcome was not durably settled", journalState, reason, err)
			}
			if recoveryRuntimeCount(t, f, "review_execution_transitions") != beforeTransitions+1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 || recoveryRuntimeCount(t, f, "review_hosted_observations") != 200 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || !reflect.DeepEqual(hostedCapacityArtifacts(t, captures), beforeFiles) {
				t.Fatal("capacity released reservation, created artifact/evidence, or changed references")
			}
			beforeReplay := cliAttemptState(t, f.store, a.ID)
			events := recoveryRuntimeCount(t, f, "events")
			again, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || again != got || len(reader.calls) != 0 || recoveryRuntimeCount(t, f, "events") != events || cliAttemptState(t, f.store, a.ID) != beforeReplay || !reflect.DeepEqual(hostedCapacityArtifacts(t, captures), beforeFiles) {
				t.Fatal("uncertain capacity replay created work/effects", again, err)
			}
		})
	}
}

func TestLiveHostedObservationCapacityLeavesSettlementToSupervisor(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "active")
	fillHostedRecoveryObservations(t, f, a)
	reader.failure = errors.New("provider must not be reached at capacity")
	beforeJournal := cliAttemptState(t, f.store, a.ID)
	beforeEvents := recoveryRuntimeCount(t, f, "events")
	beforeFiles := hostedCapacityArtifacts(t, captures)
	got, err := observeHostedAttempt(t.Context(), reader, captures, f.engine, f.project, a.ID, "runtime-capability", reader.record.PostingActor, reader.record.ProviderActor, false)
	if err != nil || got.State != "uncertain" || got.Reason != "hosted_observation_capacity" || len(reader.calls) != 0 || cliAttemptState(t, f.store, a.ID) != beforeJournal || recoveryRuntimeCount(t, f, "events") != beforeEvents || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_hosted_observations") != 200 || !reflect.DeepEqual(hostedCapacityArtifacts(t, captures), beforeFiles) {
		t.Fatal("live observer settled or performed provider/artifact work at capacity", got, err)
	}
}

func TestRecoverHostedRetainedFinalEvidenceSettlesOfflineAtObservationCapacity(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "active")
	recoveryRuntimeMetadata(t, f, a)
	retained, err := RetainHosted(t.Context(), captures, f.engine, a, reader.record)
	if err != nil || !retained.Complete || retained.Receipt.ID == "" {
		t.Fatal("final capture setup", retained, err)
	}
	fillHostedRecoveryObservations(t, f, a)
	beforeFiles := hostedCapacityArtifacts(t, captures)
	if len(beforeFiles) != 1 {
		t.Fatal("fixture must own one real final capture", beforeFiles)
	}
	reader.failure = errors.New("provider unavailable after final capture")
	got, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || got.State != "completed" || got.Reason != "hosted_evidence_recovered" || got.EvidenceID != retained.Receipt.ID || len(reader.calls) != 0 {
		t.Fatal("full reference journal blocked offline final evidence recovery", got, err, reader.calls)
	}
	if recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_hosted_observations") != 200 || !reflect.DeepEqual(hostedCapacityArtifacts(t, captures), beforeFiles) {
		t.Fatal("offline completion recollected or lost final evidence/reservation")
	}
	var sourceID string
	if err := f.store.DB().QueryRow(`SELECT r.source_run_id FROM review_workspace w JOIN review_runs r ON r.project_id=w.project_id AND r.snapshot_id=w.snapshot_id WHERE w.project_id=?`, f.project).Scan(&sourceID); err != nil || sourceID != retained.Receipt.ID {
		t.Fatal("offline recovery failed to publish retained source", sourceID, err)
	}
	adjudication, err := f.engine.ReadReviewAdjudication(t.Context(), f.scope(t, f.writer, false), retained.Receipt.ID)
	if err != nil || adjudication.Finalized {
		t.Fatal("execution completion invented source finalization", err)
	}
	beforeReplay := cliAttemptState(t, f.store, a.ID)
	events := recoveryRuntimeCount(t, f, "events")
	again, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || again.State != "completed" || again.Reason != "already_completed" || again.EvidenceID != retained.Receipt.ID || len(reader.calls) != 0 || cliAttemptState(t, f.store, a.ID) != beforeReplay || recoveryRuntimeCount(t, f, "events") != events {
		t.Fatal("completed offline replay performed new work", again, err)
	}
}
