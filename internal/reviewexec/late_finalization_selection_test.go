package reviewexec

import (
	"context"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// A changed provider head and archived external Hosted review must not erase
// old-head native adjudication obligations or invent native round credit.
func TestLateFinalizationSelectionRemainsChannelLocalAcrossHeadMove(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	roundGrant(t, f, 2, "cli", 0, 1, 1, "audit-exact-cli")
	roundGrant(t, f, 2, "hosted", 0, 1, 1, "audit-exact-hosted")
	a, evidence := roundCapture(t, f, 710, false)
	revision := int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "audit-close-pending-cli", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	options, _ := lifecycleOptions(nil)
	schedule := newLifecycleSchedule(options)
	reader := externalReader(externalSnapshot(1, strings.Repeat("d", 40)))
	reader.pull = func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		p := lifecyclePull(number)
		p.HeadSHA = strings.Repeat("c", 40)
		p.ChangedFiles = 1
		return p, nil
	}
	if !lifecycleTick(t, f, schedule, reader) || externalArchiveCount(t, f) != 1 {
		t.Fatal("external source was not independently archived")
	}
	if got := lifecycleRead(t, f); got.HeadSHA != strings.Repeat("c", 40) {
		t.Fatal("source head not updated", got.HeadSHA)
	}
	cli, _ := selectionEnqueue(t, f, "audit-closed-cli-selection", "cli", nil)
	selectionNext(t, f, reader)
	if got := selectionRead(t, f, cli.ID); got.State != "held" || got.Reason != "review_evidence_pending" || got.OperationID != "" {
		t.Fatal("head movement/closure bypassed pending old-head native capture", got)
	}
	hosted, _ := selectionEnqueue(t, f, "audit-independent-hosted-selection", "hosted", nil)
	selectionNext(t, f, reader)
	selected := selectionRead(t, f, hosted.ID)
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), selected.OperationID)
	if err != nil || selected.State != "selected" || op.PullNumber != 2 || op.HeadSHA != strings.Repeat("c", 40) {
		t.Fatal("external archive manufactured native coverage or other channel blocked selection", selected, op, err)
	}
	progress := readProgress(t, f, 2).Items[0].Channels
	if progress[0].Used != 0 || progress[0].Pending != 1 || progress[1].Used != 0 || progress[1].Pending != 0 {
		t.Fatal("archive/pending evidence supplied native completion credit", progress)
	}
	roundFinalize(t, f, a, evidence, 710, false)
	if got := roundRead(t, f, 2, "cli"); !got.Closed || got.Used != 1 {
		t.Fatal("late original-grant finalization not credited under closure", got)
	}
	after, _ := selectionEnqueue(t, f, "audit-cli-after-finalization", "cli", nil)
	selectionNext(t, f, reader)
	result := selectionRead(t, f, after.ID)
	op, err = f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), result.OperationID)
	if err != nil || result.State != "selected" || op.PullNumber != 4 {
		t.Fatal("finalized closed front did not advance independently", result, op, err)
	}
}
