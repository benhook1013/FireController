package reviews

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func assertRecentCurrentRuns(t *testing.T, f *apiFixture, number int64) PullResponse {
	t.Helper()
	var pull PullResponse
	nativeRead(t, f, fmt.Sprintf("/pulls/%d", number), &pull)
	var archive RunsResponse
	nativeRead(t, f, fmt.Sprintf("/pulls/%d/runs?limit=100", number), &archive)
	want := archive.Items
	if len(want) > 3 {
		want = want[:3]
	}
	tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := newHistorySnapshotWrite(f.project, pull.Revision+1).recentCurrentRuns(t.Context(), tx, number)
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(pull.Pull.RecentRuns, want) {
		t.Fatalf("indexed/current compact recent runs differ from archive: got%+v compact%+v want%+v err%v", got, pull.Pull.RecentRuns, want, err)
	}
	return pull
}

func TestRecentCurrentRunsPublicationAndAdjudicationPreserveHistory(t *testing.T) {
	f := newAPIFixture(t)
	runs := make([]RunInput, 5)
	for i := range runs {
		runs[i] = nativeUndecidedRun(fmt.Sprintf("recent-native-%d", i), 1)
		mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &runs[i]})
		assertRecentCurrentRuns(t, f, 1)
	}
	// Another PR and its run must never enter the selected top three.
	other := nativeRun("unrelated-recent-run")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(2), Run: &other})
	before := assertRecentCurrentRuns(t, f, 1)
	oldDigest := nativeSnapshotBytes(t, f, before.SnapshotID)
	var oldPull PullResponse
	oldWire := nativeRead(t, f, "/pulls/1?snapshot="+before.SnapshotID, &oldPull)
	for _, index := range []int{0, 4} {
		input := nativeAdjudicationInput(runs[index])
		decideNativeFinding(&input.Findings[0], "rejected")
		receipt, changed, err := applyNativeAdjudication(f, input)
		if err != nil || !changed || receipt.ID == before.SnapshotID {
			t.Fatal("real adjudication did not advance", changed, err)
		}
		after := assertRecentCurrentRuns(t, f, 1)
		if after.Pull.RecentRuns[0].SourceRunID != runs[4].SourceRunID {
			t.Fatal("adjudication reordered immutable run positions", after.Pull.RecentRuns)
		}
	}
	var retained PullResponse
	if got := nativeRead(t, f, "/pulls/1?snapshot="+before.SnapshotID, &retained); string(got) != string(oldWire) || nativeSnapshotBytes(t, f, before.SnapshotID) != oldDigest {
		t.Fatal("recent summary update changed historical response or retained rows")
	}
}

func TestRecentCurrentRunsStagedOmissionReplacementAndAddition(t *testing.T) {
	f := newAPIFixture(t)
	runs := make([]RunInput, 5)
	var receipt Receipt
	for i := range runs {
		runs[i] = nativeUndecidedRun(fmt.Sprintf("staged-recent-%d", i), 0)
		receipt = mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &runs[i]})
	}
	before := historyMigrationDatabaseDigest(t, f)
	rollback := errors.New("intentional staged-read rollback")
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		next := receipt.Revision + 1
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) SELECT project_id,?, ?,repository_id,source,observed_at,recorded_at,payload_sha256 FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, "review_snapshot_staged_recent", next, f.project, receipt.ID); err != nil {
			return err
		}
		w := newHistorySnapshotWrite(f.project, next)
		// Omit the winner, replace a middle run, and append a new winner. Old
		// references must remain usable until every prospective read has finished.
		if err := w.remove(ctx, tx, historyRunRecord, []any{runOpaqueID(f.project, nativeSource, runs[4].SourceRunID)}); err != nil {
			return err
		}
		changed := runs[2]
		changed.Reviewer = "Updated retained reviewer"
		changed.CoverageLimits = []string{"Literal <limits>", "Preserved selected version"}
		if err := w.run(ctx, tx, nativeSource, 1, 2, changed); err != nil {
			return err
		}
		added := nativeUndecidedRun("new-staged-winner", 0)
		if err := w.run(ctx, tx, nativeSource, 1, 5, added); err != nil {
			return err
		}
		// A touched different PR must not leak into this prospective result.
		unrelated := nativeUndecidedRun("new-other-pr", 0)
		if err := w.run(ctx, tx, nativeSource, 2, 0, unrelated); err != nil {
			return err
		}
		got, err := w.recentCurrentRuns(ctx, tx, 1)
		if err != nil {
			return err
		}
		want := []RunSummary{summarizeRun(f.project, nativeSource, 1, added), summarizeRun(f.project, nativeSource, 1, runs[3]), summarizeRun(f.project, nativeSource, 1, changed)}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("staged recent reduction mismatch: got%+v want%+v", got, want)
		}
		var current int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_current_runs WHERE project_id=? AND id=?`, f.project, runOpaqueID(f.project, nativeSource, runs[4].SourceRunID)).Scan(&current); err != nil {
			return err
		}
		if current != 1 {
			t.Error("prospective read changed old references")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal("staging failed before intentional rollback", err)
	}
	if historyMigrationDatabaseDigest(t, f) != before {
		t.Fatal("prospective staged read escaped rollback")
	}
	assertRecentCurrentRuns(t, f, 1)
}

func TestRecentCurrentRunsUsesCurrentOrderingIndex(t *testing.T) {
	f := newAPIFixture(t)
	for _, touched := range []int{0, 1, 3} {
		args := []any{f.project, int64(1)}
		for i := 0; i < touched; i++ {
			args = append(args, fmt.Sprintf("touched-%d", i))
		}
		rows, err := f.store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+currentRecentRunsQuery(touched), args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan.WriteString(detail)
			plan.WriteByte('\n')
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		text := plan.String()
		if !strings.Contains(text, "SEARCH c USING INDEX") || !strings.Contains(text, "(project_id=? AND pull_number=?)") || !strings.Contains(text, "SEARCH m USING INDEX") || !strings.Contains(text, "SEARCH v USING INDEX") || strings.Contains(text, "TEMP B-TREE") || strings.Contains(text, "SCAN ") {
			t.Fatalf("recent current query lost indexed top-three shape (touched%d):\n%s", touched, text)
		}
	}
}
