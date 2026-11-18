package reviewruntime

import (
	"bytes"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCLIReparseRecoversOriginalInvalidProjectionWithoutProvider(t *testing.T) {
	f := newCLIDriverFixture(t)
	_, a := f.prepareAdmit(t, 2, 1, "reparse-long-title")
	w := f.driver.prepared[a.ID].workspace
	paths, err := f.manager.ChangedFiles(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	capture := makeCompleteCLICapture(t, w, paths)
	lines := bytes.Split(bytes.TrimSpace(capture.Stdout), []byte("\n"))
	var finding map[string]any
	for i, l := range lines {
		var x map[string]any
		if err = json.Unmarshal(l, &x); err != nil {
			t.Fatal(err)
		}
		if x["type"] == "finding" {
			x["title"] = strings.Repeat("Long title ", 60)
			finding = x
			lines[i], _ = json.Marshal(x)
		}
	}
	if finding == nil {
		t.Fatal("fixture lacks finding")
	}
	capture.Stdout = append(bytes.Join(lines, []byte("\n")), '\n')
	start, finish := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
	ref, err := f.captures.Save(t.Context(), a.ID, CaptureRecord{Capture: capture, StartedAt: start, FinishedAt: finish})
	if err != nil {
		t.Fatal(err)
	}
	old := reviewexec.ReviewEvidenceInput{CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: start.Format(time.RFC3339Nano), FinishedAt: finish.Format(time.RFC3339Nano), ExpectedAnchor: *a.Anchor, Review: reviewprovider.ParsedReview{Outcome: "uncertain", Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{}}}
	rec, err := f.engine.RecordReviewEvidence(t.Context(), a.ProjectID, a.ID, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.engine.PublishEvidence(t.Context(), a.ProjectID, rec.ID, nil); err != nil {
		t.Fatal(err)
	}
	a, err = f.engine.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "capture_invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.driver.recoverRawCLI(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the engine after the re-projection transaction committed,
	// before the normal completion transition: recovery must survive this crash window.
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	f.driver.engine = f.engine
	if err = f.engine.RecoverRetainedCLI(t.Context(), f.driver.capability.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), rec.ID)
	if err != nil || !got.Capture.Review.Complete || got.Recovery == nil || got.Capture.Review.Findings[0].Title != finding["title"] {
		t.Fatal(got, err)
	}
	_, after, err := f.captures.Read(t.Context(), a.ID)
	if err != nil || ref != after {
		t.Fatal("capture changed", err)
	}
	op, err := f.engine.ReadOperation(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), a.OperationID)
	if err != nil || op.State != "completed" {
		t.Fatal(op, err)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
		t.Fatal("slot held")
	}
	if err = f.driver.recoverRawCLI(t.Context()); err != nil {
		t.Fatal("replay", err)
	}
}

func TestRetainedCLIExitProofReleasesCapacityBeforeWorkspaceRecovery(t *testing.T) {
	f := newCLIDriverFixture(t)
	_, a := f.prepareAdmit(t, 2, 1, "exit-proof-retry")
	w := f.driver.prepared[a.ID].workspace
	paths, err := f.manager.ChangedFiles(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	capture := makeCompleteCLICapture(t, w, paths)
	capture.Exited = true
	capture.Stdout = []byte("malformed provider output")
	start, finish := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
	if _, err = f.captures.Save(t.Context(), a.ID, CaptureRecord{Capture: capture, StartedAt: start, FinishedAt: finish}); err != nil {
		t.Fatal(err)
	}
	a, err = f.engine.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "execution_interrupted", "")
	if err != nil {
		t.Fatal(err)
	}
	// Represents a successful durable capture after immediate exit-proof DB write
	// failed. Only fixture-owned temporary workspace is removed.
	if err = os.RemoveAll(w.Directory); err != nil {
		t.Fatal(err)
	}
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	f.driver.engine = f.engine
	if err = f.driver.recoverRawCLI(t.Context()); err == nil {
		t.Fatal("expected workspace failure")
	}
	var ended string
	if err = f.store.DB().QueryRow(`SELECT execution_ended_at FROM review_execution_reservations WHERE attempt_id=?`, a.ID).Scan(&ended); err != nil || ended == "" {
		t.Fatal("retained proof lost", ended, err)
	}
	op, err := f.engine.ReadOperation(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), a.OperationID)
	if err != nil || op.State != "uncertain" {
		t.Fatal("invented review completion", op, err)
	}
}
