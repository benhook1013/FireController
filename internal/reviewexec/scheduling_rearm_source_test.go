package reviewexec

import (
	"context"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestAutomaticNoChildSourceDoesNotSettleRearmedChannel(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	old := schedulingLatest(t, f)
	reader := &refreshTestReader{repository: func(_ context.Context, id int64) (reviewprovider.Repository, error) {
		schedulingEnable(t, f, 1, true)
		return reviewprovider.Repository{ID: id + 1, FullName: "owner/repo", DefaultBranch: "main"}, nil
	}}
	selectionNext(t, f, reader)
	got := selectionRead(t, f, old.ID)
	if got.State != "held" || got.Reason != "repository_changed" || got.OperationID != "" {
		t.Fatalf("expected ordinary no-child hold: %+v", got)
	}
	var settled string
	if err := f.store.DB().QueryRow("SELECT settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?", old.ID).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled != "" {
		t.Fatal("older source result consumed a fresh semantic basis")
	}
	schedulingGenerate(t, f, true)
	fresh := schedulingLatest(t, f)
	if fresh.ID == old.ID {
		t.Fatal("rearm failed to acquire new source identity")
	}
	selectionNext(t, f, &refreshTestReader{})
	if got := selectionRead(t, f, fresh.ID); got.State != "selected" || got.OperationID == "" {
		t.Fatalf("fresh selection did not obtain its own source: %+v", got)
	}
}

func TestAutomaticNoChildSourceSettlesWhenChannelHasNotRearmed(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	old := schedulingLatest(t, f)
	reader := &refreshTestReader{repository: func(_ context.Context, id int64) (reviewprovider.Repository, error) {
		revision, enabled := int64(1), true
		if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "enable-other-channel-only", SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &enabled, AutomaticCLI: &enabled}); err != nil {
			t.Fatal(err)
		}
		return reviewprovider.Repository{ID: id + 1, FullName: "owner/repo", DefaultBranch: "main"}, nil
	}}
	selectionNext(t, f, reader)
	var settled string
	if err := f.store.DB().QueryRow("SELECT settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?", old.ID).Scan(&settled); err != nil || len(settled) != 64 {
		t.Fatalf("other-channel change discarded valid settlement: %q %v", settled, err)
	}
	schedulingGenerate(t, f, false)
}
