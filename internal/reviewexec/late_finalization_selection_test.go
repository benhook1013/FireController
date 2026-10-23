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
	cliSelected := selectionRead(t, f, cli.ID)
	cliOperation, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), cliSelected.OperationID)
	if err != nil || cliSelected.State != "selected" || cliOperation.PullNumber != 4 {
		t.Fatal("pending evidence blocked an independent later PR", cliSelected, cliOperation, err)
	}
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: lifecyclePull(2), ParentSHA: lifecyclePull(2).BaseSHA, ObservedAt: stamp()}
	plan.Pull.HeadSHA = strings.Repeat("c", 40)
	plan.Pull.ChangedFiles = 1
	state, err := f.e.Comparison(t.Context(), attemptProject, plan, "cli")
	if err != nil || !state.Pending {
		t.Fatal("independent selection erased the original PR obligation", state, err)
	}
	hosted, _ := selectionEnqueue(t, f, "audit-independent-hosted-selection", "hosted", nil)
	selectionNext(t, f, reader)
	selected := selectionRead(t, f, hosted.ID)
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), selected.OperationID)
	if err != nil || selected.State != "selected" || op.PullNumber != 2 || op.HeadSHA != strings.Repeat("c", 40) {
		t.Fatal("external archive manufactured native coverage or other channel blocked selection", selected, op, err)
	}
	progress := readProgress(t, f, 2).Items[0].Channels
	if progress[0].Used != 0 || progress[0].Pending != 1 || progress[1].Used != 0 || progress[1].Pending != 1 {
		t.Fatal("archive/pending evidence supplied native completion credit", progress)
	}
	roundFinalize(t, f, a, evidence, 710, false)
	if got := roundRead(t, f, 2, "cli"); !got.Closed || got.Used != 1 {
		t.Fatal("late original-grant finalization not credited under closure", got)
	}
	state, err = f.e.Comparison(t.Context(), attemptProject, plan, "cli")
	if err != nil || state.Pending || !state.RoundsComplete {
		t.Fatal("late finalization did not settle the original closed rule", state, err)
	}
	retained, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), cliSelected.OperationID)
	if err != nil || retained.ID != cliOperation.ID || retained.PullNumber != 4 || retained.HeadSHA != cliOperation.HeadSHA {
		t.Fatal("late finalization changed the independently selected operation", retained, err)
	}
}
