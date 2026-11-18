package reviews

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/core"
	"strings"
	"testing"
)

func captureFallback() RunInput {
	r := nativeUndecidedRun("review_evidence_fallback", 0)
	r.Outcome = "uncertain"
	r.Complete = false
	r.Attributable = false
	return r
}
func applyCaptureRecovery(f *apiFixture, b, a RunInput) (Receipt, error) {
	var r Receipt
	err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		var e error
		r, e = RecoverNativeCLIProjection(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "recovery-test"}, 1, b, a)
		return e
	})
	return r, err
}
func TestNativeCLIProjectionRecoveryPreservesHistory(t *testing.T) {
	f := newAPIFixture(t)
	before := captureFallback()
	old := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &before})
	oldBytes := nativeSnapshotBytes(t, f, old.ID)
	after := nativeUndecidedRun(before.SourceRunID, 2)
	after.Findings[0].Title = strings.Repeat("界", 1000)
	got, err := applyCaptureRecovery(f, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != old.Revision+1 {
		t.Fatal(got)
	}
	if oldBytes != nativeSnapshotBytes(t, f, old.ID) {
		t.Fatal("old snapshot mutated")
	}
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, before.SourceRunID), &detail)
	if !detail.Run.Complete || len(detail.Findings) != 2 || detail.Findings[0].Title != after.Findings[0].Title {
		t.Fatal("recovery lost findings", detail)
	}
	if _, err = applyCaptureRecovery(f, before, after); err == nil {
		t.Fatal("stale repeat allowed")
	}
}
func TestNativeCLIProjectionRecoveryRefusesChangedSource(t *testing.T) {
	for _, kind := range []string{"head", "time", "reviewer", "scope", "decided", "title-bound", "before-count", "before-limits"} {
		t.Run(kind, func(t *testing.T) {
			f := newAPIFixture(t)
			b := captureFallback()
			mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &b})
			a := nativeUndecidedRun(b.SourceRunID, 1)
			switch kind {
			case "head":
				a.HeadSHA = strings.Repeat("f", 40)
			case "time":
				a.FinishedAt = "2026-01-01T00:02:00Z"
			case "reviewer":
				a.Reviewer = "other"
			case "scope":
				a.Scope = "other"
			case "decided":
				a.Findings[0].Disposition = "accepted"
			case "title-bound":
				a.Findings[0].Title = strings.Repeat("x", 4097)
			case "before-count":
				b.Found = 1
			case "before-limits":
				b.CoverageLimits = []string{"other"}
			}
			counts := nativeProjectionCounts(t, f)
			if _, e := applyCaptureRecovery(f, b, a); e == nil {
				t.Fatal("accepted", kind)
			}
			for table, n := range counts {
				if countRows(t, f.store, table) != n {
					t.Fatal("mutation on failure", table)
				}
			}
		})
	}
}
