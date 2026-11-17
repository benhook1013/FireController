package reviewruntime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func retainedHostedRecoveryFixture(t *testing.T) (*runtimeAllowanceFixture, HostedCaptureRecord, CaptureReference, *hostedRecoveryReader) {
	t.Helper()
	f, record := runtimeStandingFixture(t, nil, nil)
	if err := reviews.EnsureSchema(t.Context(), f.f.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.f.engine, err = reviewexec.New(f.f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	recoveryRuntimeMetadata(t, f.f, f.a)
	// Keep the provider's leading presentation breaks in the original bytes.
	record.Snapshot.IssueComments[1].Body = strings.ReplaceAll(record.Snapshot.IssueComments[1].Body, "<dl>\n<dd>\n", "<dl>\n<dd>\n<br>\n")
	// Model a decoder that previously retained this source without recognizing
	// its complete inventory. No final evidence or completion credit exists yet.
	retained, _, err := retainHostedObservation(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || retained.Reason != "capture_retained" {
		t.Fatal("retain original undecoded source", retained, err)
	}
	f.a, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "uncertain", "hosted_finished_inventory_unproven", f.a.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	return f, record, retained.Capture, &hostedRecoveryReader{record: hostedRetainClone(t, record), repositoryID: record.RepositoryID}
}

func TestRecoverHostedLatestRetainedSourceKeepsOriginalTimeOfflineAndAfterRestart(t *testing.T) {
	for _, source := range []string{"unavailable", "later summary edit"} {
		t.Run(source, func(t *testing.T) {
			f, original, ref, reader := retainedHostedRecoveryFixture(t)
			if source == "unavailable" {
				reader.failure = errors.New("provider unavailable after original completion")
			} else {
				reader.record.Snapshot.IssueComments[1].UpdatedAt = original.Snapshot.IssueComments[1].UpdatedAt.Add(time.Hour)
				reader.record.Snapshot.IssueComments[1].Body += "\nLater unrelated presentation."
			}
			path := filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, ref))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
			if err != nil || got.State != "completed" || got.EvidenceID == "" || got.TriggerCommentID != 100 || len(reader.calls) != 0 {
				t.Fatal("latest original source required fresh observation", got, err, reader.calls)
			}
			evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), got.EvidenceID)
			if err != nil || evidence.Capture.Hosted == nil || evidence.Capture.Hosted.StandingSummary == nil || evidence.AttemptID != f.a.ID || evidence.OperationID != f.a.OperationID || evidence.Capture.CaptureID != ref.ID || evidence.Capture.CaptureSHA256 != ref.SHA256 || evidence.Capture.CaptureBytes != ref.Bytes || evidence.Capture.FinishedAt != original.Snapshot.IssueComments[2].UpdatedAt.Format(time.RFC3339Nano) || evidence.Capture.Hosted.StandingSummary.UpdatedAt != original.Snapshot.IssueComments[1].UpdatedAt.Format(time.RFC3339Nano) || evidence.Capture.Hosted.ObservedBaseSHA != f.f.baseSHA || evidence.Capture.Hosted.StandingSummary.ProviderRunID != runtimeStandingRun || len(evidence.Capture.Review.Findings) != 0 {
				t.Fatal("recovery replaced original source identity or finish time", evidence, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || recoveryRuntimeCount(t, f.f, "review_hosted_observations") != 2 || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 0 {
				t.Fatal("offline recovery rewrote source, duplicated evidence or kept reservation", err)
			}
			zero := int64(0)
			if _, err = f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "retained-source-finalize", got.EvidenceID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
				t.Fatal(err)
			}
			if err = f.f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.f.store, err = core.Open(t.Context(), f.f.storeDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.f.store.Close() })
			f.f.engine, err = reviewexec.New(f.f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
			if err != nil {
				t.Fatal(err)
			}
			if err = f.f.engine.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			events := recoveryRuntimeCount(t, f.f, "events")
			again, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
			if err != nil || again.Reason != "already_completed" || again.EvidenceID != got.EvidenceID || len(reader.calls) != 0 || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f, "events") != events {
				t.Fatal("restart replay repeated source or completion work", again, err)
			}
			if _, err = f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "retained-source-finalize", got.EvidenceID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil || recoveryRuntimeCount(t, f.f, "events") != events {
				t.Fatal("finalization replay duplicated credit", err)
			}
		})
	}
}

func TestRecoverHostedLatestRetainedIncompleteNeverFallsBackOlder(t *testing.T) {
	f, record, _, reader := retainedHostedRecoveryFixture(t)
	record.Snapshot.IssueComments[1].Body = "Unsupported latest provider inventory."
	record.FinishedAt = record.FinishedAt.Add(time.Nanosecond)
	if retained, _, err := retainHostedObservation(t.Context(), f.captures, f.f.engine, f.a, record); err != nil || retained.Reason != "capture_retained" {
		t.Fatal(retained, err)
	}
	reader.failure = errors.New("fresh observation unavailable")
	got, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
	if err == nil || got.Reason != "source_unavailable" || len(reader.calls) != 1 || got.EvidenceID != "" || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 1 {
		t.Fatal("newer unsupported source used older complete result or skipped normal observation", got, err, reader.calls)
	}
}

func TestRecoverHostedLatestRetainedCorruptionAndBindingRefuseBeforeProvider(t *testing.T) {
	for _, change := range []string{"corrupt bytes", "wrong actor", "wrong earliest", "indexed time mismatch", "wrong trigger", "wrong capability"} {
		t.Run(change, func(t *testing.T) {
			f, record, ref, reader := retainedHostedRecoveryFixture(t)
			if change == "corrupt bytes" {
				path := filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, ref))
				if err := os.WriteFile(path, []byte("changed immutable source"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if change == "wrong capability" {
				reader.failure = errors.New("provider must not be reached")
			} else {
				switch change {
				case "wrong actor":
					record.PostingActor.ID++
				case "wrong earliest":
					record.EarliestPosting = record.EarliestPosting.Add(-time.Second)
				case "wrong trigger":
					record.Snapshot.IssueComments[0].ID++
				}
				record.FinishedAt = record.FinishedAt.Add(time.Nanosecond)
				var err error
				ref, err = f.captures.Save(t.Context(), f.a.ID, record)
				if err != nil {
					t.Fatal(err)
				}
				in := reviewexec.HostedObservationInput{CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: record.StartedAt.Format(time.RFC3339Nano), FinishedAt: record.FinishedAt.Format(time.RFC3339Nano), ObservedAt: record.Snapshot.ObservedAt.Format(time.RFC3339Nano), ExpectedAnchor: *f.a.Anchor, Intent: f.a.Intent}
				if change == "indexed time mismatch" {
					in.StartedAt = record.StartedAt.Add(-time.Nanosecond).Format(time.RFC3339Nano)
				}
				if _, err = f.f.engine.RecordHostedObservation(t.Context(), f.f.project, f.a.ID, in); err != nil {
					t.Fatal(err)
				}
			}
			capability := "runtime-capability"
			if change == "wrong capability" {
				capability = "missing-capability"
			}
			got, err := RecoverHosted(t.Context(), reader, f.captures, f.f.engine, f.f.project, f.a.ID, capability, reader.record.PostingActor, reader.record.ProviderActor)
			if err == nil && got.Reason != "trigger_identity_changed" || len(reader.calls) != 0 || got.EvidenceID != "" || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 1 {
				t.Fatal("invalid latest source bypassed binding or used older valid source", change, got, err, reader.calls)
			}
		})
	}
}

func TestRecoverHostedRetainedShortcutRequiresExactUncertaintyReason(t *testing.T) {
	f, _, _, reader := retainedHostedRecoveryFixture(t)
	active, err := f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "active", "trigger_recovered", f.a.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, active.Revision, "uncertain", "another_uncertainty", f.a.ExternalID); err != nil {
		t.Fatal(err)
	}
	reader.failure = errors.New("fresh source unavailable")
	got, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
	if err == nil || !strings.HasPrefix(got.Reason, "source_") || len(reader.calls) != 1 || got.EvidenceID != "" {
		t.Fatal("retained shortcut escaped its exact uncertainty state", got, err, reader.calls)
	}
}
