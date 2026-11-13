package reviewruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestCLIRawRecoveryVerifiesOriginalArtifactWithoutRerunning(t *testing.T) {
	for _, mode := range []string{"complete", "missing", "changed-workspace", "incomplete", "still-active", "no-reservation"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			_, a := f.prepareAdmit(t, 2, 1, "raw-cli-recovery")
			w := f.driver.prepared[a.ID].workspace
			paths, err := f.manager.ChangedFiles(t.Context(), w)
			if err != nil {
				t.Fatal(err)
			}
			start, finish := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
			capture := makeCompleteCLICapture(t, w, paths)
			if mode == "incomplete" {
				capture.Stdout = []byte("unfinished provider result")
			}
			var original CaptureReference
			if mode != "missing" {
				original, err = f.captures.Save(t.Context(), a.ID, CaptureRecord{Capture: capture, StartedAt: start, FinishedAt: finish})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode != "still-active" {
				a, err = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "uncertain", "capture_retention_failed", "")
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "changed-workspace" {
				if err = os.WriteFile(filepath.Join(w.Directory, "unexpected.txt"), []byte("unreviewed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "no-reservation" {
				if _, err = f.store.DB().Exec("DELETE FROM review_execution_reservations WHERE attempt_id=?", a.ID); err != nil {
					t.Fatal(err)
				}
			}
			providerReads := f.reader.repositoryCalls
			f.driver.review = func(context.Context, reviewworkspace.Workspace) (reviewprocess.Capture, error) {
				panic("raw recovery must not launch a review")
			}
			err = f.driver.recoverRawCLI(t.Context())
			if (mode == "missing" || mode == "changed-workspace") != (err != nil) {
				t.Fatal("unexpected original-artifact recovery result", mode, err)
			}
			if err = f.engine.RecoverRetainedCLI(t.Context(), f.driver.capability.ID); err != nil {
				t.Fatal(err)
			}
			op, err := f.engine.ReadOperation(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), a.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "complete" {
				if op.State != "completed" || op.AttemptID != a.ID || cliCount(t, f.store, "SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?", a.ID) != 1 || cliCount(t, f.store, "SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?", a.ID) != 0 {
					t.Fatal("original evidence did not settle exactly once", op)
				}
				if err = f.driver.recoverRawCLI(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if op.State == "completed" {
				t.Fatal("missing, changed, live or incomplete work gained completion", mode)
			}
			if f.reader.repositoryCalls != providerReads {
				t.Fatal("local recovery performed provider reads")
			}
			if mode != "missing" {
				record, ref, err := f.captures.Read(t.Context(), a.ID)
				if err != nil || ref != original || !record.StartedAt.Equal(start) || !record.FinishedAt.Equal(finish) {
					t.Fatal("recovery changed original capture identity/times", err)
				}
			}
		})
	}
}
