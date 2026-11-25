package reviewexec

import (
	"context"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRepositoryMismatchLeavesConcurrentRoundGrantUnsettled(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	r := schedulingLatest(t, f)
	read := false
	reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		read = true
		roundGrant(t, f, 2, "hosted", 0, 1, 2, "new-local-grant-during-read")
		return reviewprovider.Repository{ID: 802, FullName: "owner/alternate", DefaultBranch: "main"}, nil
	}}
	selectionNext(t, f, reader)
	actual := selectionRead(t, f, r.ID)
	if !read || actual.State != "held" || actual.Reason != "repository_changed" || actual.OperationID != "" {
		t.Fatal("repository fixture failed", read, actual)
	}
	var settled string
	if err := f.store.DB().QueryRow(`SELECT settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?`, r.ID).Scan(&settled); err != nil || settled != "" {
		t.Fatal("unverified source read settled a later basis", settled, err)
	}
	schedulingGenerate(t, f, true)
	if next := schedulingLatest(t, f); next.ID == r.ID || next.State != "queued" {
		t.Fatal("new legitimate round basis cannot receive a fresh source read", next)
	}
}
