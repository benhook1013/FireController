package reviews

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func publishStatus(t *testing.T, f *apiFixture, pins map[int64]SourceMetadataPin, p PullInput, ci, merge string) (Receipt, error) {
	t.Helper()
	var receipt Receipt
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		receipt, err = PublishSourceStatus(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "service-lifecycle-sync"}, 701, pins, []SourceStatus{{Pull: p, CIState: ci, Mergeability: merge}})
		return err
	})
	return receipt, err
}

func TestSourceStatusCASPreservesHistoryConcurrentEvidenceAndOrder(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.State = "open"
	p.Runs = []RunInput{nativeRun("retained")}
	old := f.observe(t, reviewRequest(0, []PullInput{p, nativePull(2)}), "start")["id"].(string)
	oldBytes := nativeSnapshotBytes(t, f, old)
	pins := sourcePins(t, f, 1)
	a := nativeAttempt("later", "prepared")
	metadata := p
	metadata.Runs = nil
	mustNativePublish(t, f, NativePublication{Pull: metadata, Attempt: &a})
	before := metadataRead(t, f, 1, "").Pull
	order := queueEditRequest(2, nil, nil)
	order.Order = []int64{2, 1}
	if w := queueEditHTTP(f, order, "concurrent-order"); w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	pull := sourcePull(1, p.Title)
	if _, err := publishStatus(t, f, pins, pull, "pending", "conflicting"); err != nil {
		t.Fatal(err)
	}
	got := metadataRead(t, f, 1, "").Pull
	if got.CIState != "pending" || got.Mergeability != "conflicting" || got.RunCount != before.RunCount || got.AttemptCount != before.AttemptCount || !reflect.DeepEqual(got.RecentRuns, before.RecentRuns) {
		t.Fatal(got)
	}
	if nativeSnapshotBytes(t, f, old) != oldBytes {
		t.Fatal("old history changed")
	}
	var position int
	if err := f.store.DB().QueryRow(`SELECT position FROM review_current_pulls WHERE project_id=? AND number=1`, f.project).Scan(&position); err != nil || position != 1 {
		t.Fatal("concurrent order changed", position, err)
	}
	counts := historyTableCounts(t, f)
	if r, err := publishStatus(t, f, sourcePins(t, f, 1), pull, "pending", "conflicting"); err != nil || r.ID != "" {
		t.Fatal(r, err)
	}
	if !reflect.DeepEqual(counts, historyTableCounts(t, f)) {
		t.Fatal("no-op created history")
	}
	if _, err := publishStatus(t, f, pins, sourcePull(1, "stale"), "success", "mergeable"); err != nil {
		t.Fatal(err)
	}
	if metadataRead(t, f, 1, "").Pull.Title != p.Title {
		t.Fatal("stale status pin overwrote concurrent display")
	}
}

func TestSourceStatusChangedComparisonClearsCreditAndOrdinaryPathRefusesStatus(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.State = "open"
	p.Complete = true
	f.observe(t, reviewRequest(0, []PullInput{p}), "start")
	changed := sourcePull(1, p.Title)
	changed.HeadSHA = strings.Repeat("c", 40)
	if _, err := publishStatus(t, f, sourcePins(t, f, 1), changed, "", "unknown"); err != nil {
		t.Fatal(err)
	}
	got := metadataRead(t, f, 1, "").Pull
	if got.CIState != "unknown" || got.Mergeability != "unknown" || got.Complete {
		t.Fatal("inherited credit", got)
	}
	changed.CIState = "success"
	if _, err := sourcePublish(f, sourcePins(t, f, 1), changed); err == nil {
		t.Fatal("ordinary metadata path accepted CI")
	}
	changed.CIState = ""
	if _, err := publishStatus(t, f, sourcePins(t, f, 1), changed, "optional", "mergeable"); err == nil {
		t.Fatal("invented passing CI state")
	}
}
