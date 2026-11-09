package reviewruntime

import (
	"errors"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Model a retained historical attempt while using real fresh observation time.
// The first incomplete transition is aged, never a live provider or production clock.
func agedIncompleteRuntime(t *testing.T) (*cliEvidenceFixture, *HostedCaptureStore, reviewexec.Attempt, *hostedRecoveryReader) {
	t.Helper()
	f, c, a, r, _ := agedIncompleteRuntimeSource(t)
	return f, c, a, r
}
func agedIncompleteRuntimeSource(t *testing.T, publicationOnly ...bool) (*cliEvidenceFixture, *HostedCaptureStore, reviewexec.Attempt, *hostedRecoveryReader, reviewprovider.HostedPublicationSnapshot) {
	t.Helper()
	f, captures, a, reader := recoveryRuntimeFixture(t, "active")
	original := reader.record.Snapshot
	admitted := time.Now().UTC().Add(-10 * time.Minute)
	reader.record.EarliestPosting = admitted
	for i := range reader.record.Snapshot.IssueComments {
		if reader.record.Snapshot.IssueComments[i].ID == 100 {
			reader.record.Snapshot.IssueComments[i].CreatedAt = admitted.Add(time.Second)
			reader.record.Snapshot.IssueComments[i].UpdatedAt = admitted.Add(time.Second)
		}
	}
	for revision, at := range map[int]time.Time{1: admitted.Add(-time.Second), 2: admitted, 3: admitted.Add(2 * time.Second)} {
		if _, err := f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=?`, at.Format(time.RFC3339Nano), a.ID, revision); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.DB().Exec(`UPDATE review_operations SET created_at=?,updated_at=? WHERE id=?`, admitted.Add(-time.Second).Format(time.RFC3339Nano), admitted.Add(2*time.Second).Format(time.RFC3339Nano), a.OperationID); err != nil {
		t.Fatal(err)
	}
	if len(publicationOnly) > 0 && publicationOnly[0] {
		reader.record.Snapshot.ReviewComments[0].Body = "Unsupported finding inventory in an attributable completed publication."
	} else {
		unsupportedFinishedSnapshot(&reader.record.Snapshot, reader.record.ProviderActor, 100)
	}
	for i := range reader.record.Snapshot.IssueComments {
		if reader.record.Snapshot.IssueComments[i].ID == 5002 {
			reader.record.Snapshot.IssueComments[i].CreatedAt = admitted.Add(time.Minute)
			reader.record.Snapshot.IssueComments[i].UpdatedAt = admitted.Add(time.Minute)
		}
	}
	result, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || result.Reason != "hosted_finished_inventory_unproven" || result.State != "uncertain" {
		t.Fatal(result, err)
	}
	bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
	if err != nil {
		t.Fatal(err)
	}
	a = bound.Attempt
	first := time.Now().UTC().Add(-reviewexec.HostedIncompleteGrace - time.Second).Format(time.RFC3339Nano)
	if _, err := f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=?`, first, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE review_operations SET updated_at=? WHERE id=?`, first, a.OperationID); err != nil {
		t.Fatal(err)
	}
	return f, captures, a, reader, original
}
func TestRecoverHostedFinishedIncompleteReleasesWithoutCreditOrRepost(t *testing.T) {
	f, captures, a, reader := agedIncompleteRuntime(t)
	original, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || len(original) != 1 {
		t.Fatal(original, err)
	}
	result, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || result.State != "failed" || result.Reason != reviewexec.HostedFinishedIncompleteReason || result.EvidenceID != "" || result.TriggerCommentID != 100 {
		t.Fatal(result, err)
	}
	if recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 {
		t.Fatal("incomplete release gained evidence or retained capacity")
	}
	refs, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || len(refs) != len(original) || refs[0] != original[0] {
		t.Fatal("release rewrote original source", refs, err)
	}
	if _, err := captures.Read(t.Context(), a.ID, CaptureReference{ID: refs[0].Input.CaptureID, SHA256: refs[0].Input.CaptureSHA256, Bytes: refs[0].Input.CaptureBytes}); err != nil {
		t.Fatal(err)
	}
	// A terminal replay cannot call the provider, synthesize evidence or post.
	calls := len(reader.calls)
	reader.failure = errors.New("provider unavailable")
	replay, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || replay.State != result.State || replay.Reason != result.Reason || len(reader.calls) != calls || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 {
		t.Fatal(replay, err, reader.calls)
	}
}
func TestRecoverHostedIncompleteAgeAloneDoesNotRelease(t *testing.T) {
	for _, kind := range []string{"provider outage", "partial collection", "missing acknowledgement", "changed trigger"} {
		t.Run(kind, func(t *testing.T) {
			f, captures, a, reader := agedIncompleteRuntime(t)
			switch kind {
			case "provider outage":
				reader.failure = errors.New("network unavailable")
			case "partial collection":
				reader.record.Snapshot.Complete = false
			case "missing acknowledgement":
				reader.record.Snapshot.IssueComments = reader.record.Snapshot.IssueComments[:len(reader.record.Snapshot.IssueComments)-1]
			case "changed trigger":
				reader.record.Snapshot.IssueComments[0].Body = "changed original trigger"
			}
			result, _ := recoverRuntime(t, f, captures, a, reader)
			bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
			if err != nil || bound.Attempt.State != "uncertain" || bound.Attempt.Revision != a.Revision || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 {
				t.Fatal("age alone released capacity", result, bound, err)
			}
		})
	}
}

func TestRecoverHostedCompleteInventoryWinsAfterGrace(t *testing.T) {
	f, captures, a, reader, complete := agedIncompleteRuntimeSource(t)
	reader.record.Snapshot.Reviews = complete.Reviews
	reader.record.Snapshot.ReviewComments = complete.ReviewComments
	// Publish after the incomplete observation. Use this same fixture
	// comparison: another Git fixture may create different commit identities.
	published := time.Now().UTC().Add(-time.Millisecond)
	for i := range reader.record.Snapshot.Reviews {
		reader.record.Snapshot.Reviews[i].SubmittedAt = published
	}
	for i := range reader.record.Snapshot.ReviewComments {
		reader.record.Snapshot.ReviewComments[i].CreatedAt = published
		reader.record.Snapshot.ReviewComments[i].UpdatedAt = published
	}
	result, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || result.State != "completed" || result.EvidenceID == "" || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
		t.Fatal("complete source lost to incomplete release", result, err)
	}
}

func TestRecoverHostedPublicationGraceNeedsFreshFinishedACK(t *testing.T) {
	f, captures, a, reader, _ := agedIncompleteRuntimeSource(t, true)
	got, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || got.State != "uncertain" || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 {
		t.Fatal("publication alone released capacity", got, err)
	}
	unsupportedFinishedSnapshot(&reader.record.Snapshot, reader.record.ProviderActor, 100)
	got, err = recoverRuntime(t, f, captures, a, reader)
	if err != nil || got.State != "failed" || got.Reason != reviewexec.HostedFinishedIncompleteReason || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
		t.Fatal("fresh stop proof restarted elapsed publication grace", got, err)
	}
}
