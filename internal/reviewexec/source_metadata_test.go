package reviewexec

import (
	"context"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestSourceMetadataRefreshAndSelectionReuseBoundedReads(t *testing.T) {
	for _, mode := range []string{"refresh", "selection"} {
		t.Run(mode, func(t *testing.T) {
			f := attemptProjectionFixture(t, true)
			// Source resolution requires at least one changed file for a selected
			// open review. Zero display counts are proved at the publisher boundary.
			reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				z := int64(0)
				d := false
				return reviewprovider.Pull{Number: n, RepositoryID: 801, Title: "Fresh observed title", State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1, Additions: &z, Deletions: &z, IsDraft: &d}, nil
			}}
			if mode == "refresh" {
				r := refreshEnqueue(t, f, "source-project-refresh", refreshRequest())
				worked, err := f.e.refreshNext(t.Context(), map[string]TargetReader{"test-capability": reader})
				if err != nil || !worked || refreshRead(t, f, r.ID).State != "completed" {
					t.Fatal("refresh", worked, err)
				}
			} else {
				admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
				r, _ := selectionEnqueue(t, f, "source-project-selection", "cli", nil)
				selectionNext(t, f, reader)
				if selectionRead(t, f, r.ID).State != "selected" {
					t.Fatal("selection changed")
				}
			}
			var got reviews.PullResponse
			projectionRead(t, f, "/pulls/2", &got)
			if got.Pull.Title != "Fresh observed title" || got.Pull.Diff == nil || got.Pull.Diff.Files != 1 || got.Pull.Diff.Additions != 0 || got.Pull.IsDraft == nil || *got.Pull.IsDraft {
				t.Fatal("fresh metadata did not reach ordinary stored read", got.Pull)
			}
			if reader.calls.Load() != 2 {
				t.Fatal("metadata publication added provider calls", reader.calls.Load())
			}
			var unchanged reviews.PullResponse
			projectionRead(t, f, "/pulls/4", &unchanged)
			if unchanged.Pull.Title != "Synthetic PR 4" || unchanged.Pull.Diff != nil {
				t.Fatal("unread suffix changed")
			}
		})
	}
}
