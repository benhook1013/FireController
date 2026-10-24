package reviewruntime

import (
	"context"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestCLIRecoveryWorkerCompletesRetainedEvidenceAndJoinsWithoutDispatch(t *testing.T) {
	f := newCLIDriverFixture(t)
	_, a := f.prepareAdmit(t, 2, 1, "retained-cli-recovery")
	w := f.driver.prepared[a.ID].workspace
	paths, err := f.manager.ChangedFiles(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	start, finish := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
	retained, err := RetainCLI(t.Context(), f.captures, f.engine, f.manager, a, w, makeCompleteCLICapture(t, w, paths), start, finish)
	if err != nil {
		t.Fatal(err)
	}
	_, before, err := f.captures.Read(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "uncertain", "capture_retention_failed", ""); err != nil {
		t.Fatal(err)
	}
	f.driver.review = func(context.Context, reviewworkspace.Workspace) (reviewprocess.Capture, error) {
		panic("recovery must not launch a review")
	}
	waits := make(chan time.Duration)
	f.driver.recoveryWait = func(ctx context.Context, delay time.Duration) error {
		select {
		case waits <- delay:
		case <-ctx.Done():
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.driver.StartCLIRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.driver.StartCLIRecovery(ctx); err == nil {
		t.Fatal("duplicate local recovery worker started")
	}
	select {
	case delay := <-waits:
		if delay != 15*time.Second {
			t.Fatal(delay)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("local recovery did not finish its first bounded pass")
	}
	op, err := f.engine.ReadOperation(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), a.OperationID)
	if err != nil || op.State != "completed" || op.Reason != "cli_evidence_recovered" || op.AttemptID != a.ID {
		t.Fatal(op, err)
	}
	_, after, err := f.captures.Read(t.Context(), a.ID)
	if err != nil || after != before || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE id=?`, retained.Receipt.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
		t.Fatal("recovery replaced original evidence or retained reservation", err)
	}
	cancel()
	schedulerDone(t, done, false)
}
