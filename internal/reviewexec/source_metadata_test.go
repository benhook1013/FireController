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

// These controlled readers exercise the optional-display boundary; the GitHub
// adapter rejects malformed provider responses before constructing these facts.
func TestSourceMetadataInvalidDisplayDoesNotStrandWorkers(t *testing.T) {
	for _, mode := range []string{"refresh", "selection"} {
		for _, field := range []string{"title", "merged_at", "diff"} {
			t.Run(mode+"/"+field, func(t *testing.T) {
				f := attemptProjectionFixture(t, true)
				reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					z := int64(0)
					p := reviewprovider.Pull{Number: n, RepositoryID: 801, Title: "Fresh title", State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1, Additions: &z, Deletions: &z}
					switch field {
					case "title":
						p.Title = "Unsafe\x00title"
					case "merged_at":
						p.MergedAt = "not-an-instant"
					case "diff":
						negative := int64(-1)
						p.Additions = &negative
					}
					return p, nil
				}}
				if mode == "refresh" {
					r := refreshEnqueue(t, f, "invalid-display-refresh", refreshRequest())
					worked, err := f.e.refreshNext(t.Context(), map[string]TargetReader{"test-capability": reader})
					if err != nil || !worked {
						t.Fatalf("optional display blocked refresh settlement: worked=%v err=%v", worked, err)
					}
					if got := refreshRead(t, f, r.ID); got.State != "completed" || got.Observation == nil {
						t.Fatalf("source comparison did not complete: %+v", got)
					}
				} else {
					admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
					r, _ := selectionEnqueue(t, f, "invalid-display-selection", "cli", nil)
					selectionNext(t, f, reader)
					if got := selectionRead(t, f, r.ID); got.State != "selected" || got.OperationID == "" {
						t.Fatalf("selection did not settle: %+v", got)
					}
				}
				var got reviews.PullResponse
				projectionRead(t, f, "/pulls/2", &got)
				if got.Pull.Title != "Synthetic PR 2" || got.Pull.Diff != nil || got.Pull.MergedAt != "" {
					t.Fatal("invalid display replaced retained facts", got.Pull)
				}
				if reader.calls.Load() != 2 {
					t.Fatal("display refusal caused extra provider reads", reader.calls.Load())
				}
			})
		}
	}
}

func TestSourceMetadataSelectionSkipsInvalidMergedDisplayAndPublishesSelected(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
	r, _ := selectionEnqueue(t, f, "merged-invalid-display", "cli", nil)
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		p := reviewprovider.Pull{Number: n, RepositoryID: 801, Title: "Fresh selected title", State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
		if n == 2 {
			// Merged-prefix comparison identity is valid; only its optional
			// display title cannot be published.
			p.State, p.Merged, p.Title = "closed", true, "invalid\x00display-title"
		}
		return p, nil
	}}
	selectionNext(t, f, reader)
	if got := selectionRead(t, f, r.ID); got.State != "selected" || got.OperationID == "" {
		t.Fatal("valid target blocked by invalid merged prefix display", got)
	}
	var closed, selected reviews.PullResponse
	projectionRead(t, f, "/pulls/2", &closed)
	projectionRead(t, f, "/pulls/4", &selected)
	if closed.Pull.Title != "Synthetic PR 2" || selected.Pull.Title != "Fresh selected title" {
		t.Fatal("per-entry optional refusal lost valid publication", closed.Pull, selected.Pull)
	}
	if reader.calls.Load() != 3 {
		t.Fatal("selection repeated source reads", reader.calls.Load())
	}
}

func TestSourceMetadataDoesNotOverrideMandatorySourceHold(t *testing.T) {
	f := attemptProjectionFixture(t, true)
	r := refreshEnqueue(t, f, "invalid-source-identity", refreshRequest())
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: n, RepositoryID: 999, Title: "bad\x00title", State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	worked, err := f.e.refreshNext(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || !worked {
		t.Fatal(worked, err)
	}
	if got := refreshRead(t, f, r.ID); got.State != "held" || got.Reason != "target_changed" || got.Observation != nil {
		t.Fatal("optional refusal bypassed mandatory source identity", got)
	}
}
