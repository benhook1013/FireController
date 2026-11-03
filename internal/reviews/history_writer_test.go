package reviews

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

var historyWriterRetiredSpec = historyRecordSpec{
	versions: "review_retired_versions", memberships: "review_retired_memberships", current: "review_current_retired",
	keys: []string{"number"}, body: []string{"reason"}, placement: []string{"position"},
}

func newHistoryWriterFixture(t *testing.T) *apiFixture {
	t.Helper()
	return newAPIFixture(t)
}

func historyWriterSnapshot(t *testing.T, tx *sql.Tx, project string, revision int64) string {
	t.Helper()
	id := fmt.Sprintf("writer_snapshot_%d", revision)
	_, err := tx.ExecContext(t.Context(), `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256)
 VALUES(?,?,?,1,'writer-fixture','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z',?)`, project, id, revision, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func historyWriterTx(t *testing.T, f *apiFixture) *sql.Tx {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func historyWriterStage(t *testing.T, tx *sql.Tx, write *historySnapshotWrite, number, position int64, reason string) int64 {
	t.Helper()
	version, err := write.version(t.Context(), tx, historyWriterRetiredSpec, []any{number}, []any{reason}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := write.membership(t.Context(), tx, historyWriterRetiredSpec, []any{number}, version, []any{position}); err != nil {
		t.Fatal(err)
	}
	return version
}

func historyWriterCurrent(t *testing.T, tx *sql.Tx, project string) string {
	t.Helper()
	var reason string
	err := tx.QueryRowContext(t.Context(), `SELECT v.reason FROM review_current_retired c
 JOIN review_retired_memberships m USING(project_id,number,start_revision,position)
 JOIN review_retired_versions v USING(project_id,number,version_revision) WHERE c.project_id=? AND c.number=7`, project).Scan(&reason)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func historyWriterAt(t *testing.T, tx *sql.Tx, project string, revision int64) string {
	t.Helper()
	var reason string
	err := tx.QueryRowContext(t.Context(), `SELECT v.reason FROM review_retired_memberships m
 JOIN review_retired_versions v USING(project_id,number,version_revision)
 WHERE m.project_id=? AND m.number=7 AND m.start_revision<=? AND (m.end_revision IS NULL OR ?<m.end_revision)`, project, revision, revision).Scan(&reason)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func TestHistoryWriterPreflightKeepsPreviousCurrentAndHistoricalBodies(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	first := historyWriterSnapshot(t, tx, f.project, 1)
	one := newHistorySnapshotWrite(f.project, 1)
	historyWriterStage(t, tx, one, 7, 0, "superseded")
	if err := one.applyCurrent(t.Context(), tx, first, 0); err != nil {
		t.Fatal(err)
	}
	second := historyWriterSnapshot(t, tx, f.project, 2)
	two := newHistorySnapshotWrite(f.project, 2)
	historyWriterStage(t, tx, two, 7, 0, "defunct")
	if historyWriterCurrent(t, tx, f.project) != "superseded" || historyWriterAt(t, tx, f.project, 1) != "superseded" || historyWriterAt(t, tx, f.project, 2) != "defunct" {
		t.Fatal("staging replaced old current or historical data")
	}
	if err := two.applyCurrent(t.Context(), tx, second, 1); err != nil {
		t.Fatal(err)
	}
	if historyWriterCurrent(t, tx, f.project) != "defunct" || historyWriterAt(t, tx, f.project, 1) != "superseded" {
		t.Fatal("current advancement rewrote the old snapshot")
	}
	var snapshot string
	if err := tx.QueryRowContext(t.Context(), "SELECT snapshot_id FROM review_workspace WHERE project_id=?", f.project).Scan(&snapshot); err != nil || snapshot != second {
		t.Fatal("references and workspace did not advance together", snapshot, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryWriterReusesBodiesAcrossOrderSwapsAndOmittedHistory(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	first := historyWriterSnapshot(t, tx, f.project, 1)
	one := newHistorySnapshotWrite(f.project, 1)
	historyWriterStage(t, tx, one, 7, 0, "superseded")
	historyWriterStage(t, tx, one, 8, 1, "defunct")
	if err := one.applyCurrent(t.Context(), tx, first, 0); err != nil {
		t.Fatal(err)
	}
	second := historyWriterSnapshot(t, tx, f.project, 2)
	two := newHistorySnapshotWrite(f.project, 2)
	if historyWriterStage(t, tx, two, 7, 1, "superseded") != 1 || historyWriterStage(t, tx, two, 8, 0, "defunct") != 1 {
		t.Fatal("position changes copied immutable bodies")
	}
	if err := two.applyCurrent(t.Context(), tx, second, 1); err != nil {
		t.Fatal("valid position swap collided with previous ordering", err)
	}
	third := historyWriterSnapshot(t, tx, f.project, 3)
	three := newHistorySnapshotWrite(f.project, 3)
	if err := three.remove(t.Context(), tx, historyWriterRetiredSpec, []any{int64(7)}); err != nil {
		t.Fatal(err)
	}
	if err := three.applyCurrent(t.Context(), tx, third, 2); err != nil {
		t.Fatal(err)
	}
	var gap, versions int
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM review_current_retired WHERE project_id=? AND number=7", f.project).Scan(&gap); err != nil || gap != 0 {
		t.Fatal("omitted history remained current", gap, err)
	}
	fourth := historyWriterSnapshot(t, tx, f.project, 4)
	four := newHistorySnapshotWrite(f.project, 4)
	if historyWriterStage(t, tx, four, 7, 1, "superseded") != 1 {
		t.Fatal("returning unchanged history copied its body")
	}
	if err := four.applyCurrent(t.Context(), tx, fourth, 3); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM review_retired_versions WHERE project_id=?", f.project).Scan(&versions); err != nil || versions != 2 {
		t.Fatal("unrelated snapshots duplicated old payloads", versions, err)
	}
	if historyWriterAt(t, tx, f.project, 1) != "superseded" || historyWriterAt(t, tx, f.project, 4) != "superseded" {
		t.Fatal("return rewrote original history")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryWriterRejectsReplacementAndRollsBackFailedPublication(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	first := historyWriterSnapshot(t, tx, f.project, 1)
	one := newHistorySnapshotWrite(f.project, 1)
	historyWriterStage(t, tx, one, 7, 0, "superseded")
	if _, err := one.version(t.Context(), tx, historyWriterRetiredSpec, []any{int64(7)}, []any{"defunct"}, false); err == nil {
		t.Fatal("replaced an already staged immutable body")
	}
	if err := one.applyCurrent(t.Context(), tx, first, 0); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = historyWriterTx(t, f)
	second := historyWriterSnapshot(t, tx, f.project, 2)
	two := newHistorySnapshotWrite(f.project, 2)
	historyWriterStage(t, tx, two, 7, 0, "defunct")
	historyWriterStage(t, tx, two, 8, 0, "superseded")
	if err := two.applyCurrent(t.Context(), tx, second, 1); err == nil {
		t.Fatal("duplicate current order was accepted")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	tx = historyWriterTx(t, f)
	if historyWriterCurrent(t, tx, f.project) != "superseded" || historyWriterAt(t, tx, f.project, 1) != "superseded" {
		t.Fatal("failed publication changed retained or current data")
	}
	var count int
	if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM review_snapshots WHERE project_id=?", f.project).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed snapshot survived rollback", count, err)
	}
}
