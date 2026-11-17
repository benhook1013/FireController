package reviews

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// Seed canonical tables directly in one disposable transaction. Repeated bodies
// are deliberately reused; validation must scale with intervals, not snapshots
// multiplied by the lifetime archive. No provider or service workload is used.
func historyIntegrityGrowthFixture(t *testing.T, count int) *sql.Tx {
	t.Helper()
	f := newAPIFixture(t)
	pull := nativePull(7)
	pull.Runs = []RunInput{nativeRun("integrity-growth")}
	pull.Attempts = []Attempt{nativeAttempt("integrity-growth", "prepared")}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "integrity-growth")
	tx := historyWriterTx(t, f)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`WITH RECURSIVE revisions(n) AS (SELECT 2 UNION ALL SELECT n+1 FROM revisions WHERE n<?)
 INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256)
 SELECT ?, 'integrity-growth-'||n,n,1,'integrity-growth','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z',printf('%064d',0) FROM revisions`, count, f.project)
	for _, spec := range []historyRecordSpec{historyPullRecord, historyRunRecord, historyAttemptRecord} {
		exec("UPDATE "+spec.memberships+" SET end_revision=2 WHERE project_id=?", f.project)
		columns := append([]string{"project_id"}, spec.keys...)
		columns = append(columns, "start_revision", "end_revision", "version_revision")
		columns = append(columns, spec.placement...)
		selectColumns := []string{"m.project_id"}
		for _, key := range spec.keys {
			selectColumns = append(selectColumns, "m."+key)
		}
		selectColumns = append(selectColumns, "s.revision", "CASE WHEN s.revision<? THEN s.revision+1 END", "m.version_revision")
		for _, place := range spec.placement {
			selectColumns = append(selectColumns, "m."+place)
		}
		exec("INSERT INTO "+spec.memberships+"("+strings.Join(columns, ",")+") SELECT "+strings.Join(selectColumns, ",")+" FROM "+spec.memberships+" m JOIN review_snapshots s ON s.project_id=m.project_id WHERE m.project_id=? AND m.start_revision=1 AND s.revision>=2", count, f.project)
		exec("UPDATE "+spec.current+" SET start_revision=? WHERE project_id=?", count, f.project)
	}
	exec("UPDATE review_workspace SET revision=?,snapshot_id=? WHERE project_id=?", count, fmt.Sprintf("integrity-growth-%d", count), f.project)
	rows, err := tx.QueryContext(t.Context(), "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("growth fixture violates canonical foreign keys")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestHistoryReferencesGrowthAndCancellation(t *testing.T) {
	for _, count := range []int{1000, 3000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			tx := historyIntegrityGrowthFixture(t, count)
			var before, after int64
			if err := tx.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&before); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if err := ValidateHistoryReferences(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			t.Logf("validated %d intervals in each of three canonical membership tables in %s", count, time.Since(start))
			if err := tx.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("validation wrote retained state")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := ValidateHistoryReferences(ctx, tx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
			if err := ValidateHistoryReferences(ctx, tx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline lost: %v", err)
			}
		})
	}
}

// Isolate the logical SQL predicates from physical FK validation (which has
// separate canonical-schema fixtures above). The oracle directly evaluates
// half-open intervals, including the maximum representable revision.
type historyTestInterval struct {
	project           string
	key, place, start int64
	end               *int64
}

func historyEnd(n int64) *int64 { return &n }
func historyIntervalsOverlap(a, b historyTestInterval) bool {
	return (a.end == nil || b.start < *a.end) && (b.end == nil || a.start < *b.end)
}

func TestHistoryIntervalOverlapBoundaries(t *testing.T) {
	cases := []struct {
		name string
		rows []historyTestInterval
	}{
		{"adjacent", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}, {"p", 7, 0, 2, nil}}},
		{"gap", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}, {"p", 7, 0, 3, nil}}},
		{"nested", []historyTestInterval{{"p", 7, 0, 1, historyEnd(9)}, {"p", 7, 0, 2, historyEnd(3)}, {"p", 7, 0, 4, nil}}},
		{"same start different key", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}, {"p", 8, 0, 1, nil}}},
		{"open predecessor", []historyTestInterval{{"p", 7, 0, 1, nil}, {"p", 7, 0, 2, historyEnd(3)}}},
		{"different placement", []historyTestInterval{{"p", 7, 0, 1, nil}, {"p", 8, 1, 1, nil}}},
		{"project isolated", []historyTestInterval{{"p", 7, 0, 1, nil}, {"q", 7, 0, 1, nil}}},
		{"maximum touching", []historyTestInterval{{"p", 7, 0, 1, historyEnd(math.MaxInt64)}, {"p", 7, 0, math.MaxInt64, nil}}},
		{"maximum open overlap", []historyTestInterval{{"p", 7, 0, 1, nil}, {"p", 7, 0, math.MaxInt64, nil}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err = db.Exec(`CREATE TABLE intervals(project_id TEXT,number INTEGER,position INTEGER,start_revision INTEGER,end_revision INTEGER)`); err != nil {
				t.Fatal(err)
			}
			for _, r := range tc.rows {
				if _, err = db.Exec(`INSERT INTO intervals VALUES(?,?,?,?,?)`, r.project, r.key, r.place, r.start, r.end); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{"number", "position"} {
				want := false
				for i, a := range tc.rows {
					for _, b := range tc.rows[i+1:] {
						same := a.key == b.key
						if key == "position" {
							same = a.place == b.place
						}
						if a.project == b.project && same && historyIntervalsOverlap(a, b) {
							want = true
						}
					}
				}
				var found int
				err = db.QueryRowContext(t.Context(), historyIntervalOverlapQuery("intervals", []string{key})).Scan(&found)
				if (err == nil) != want || (err != nil && !errors.Is(err, sql.ErrNoRows)) {
					t.Fatalf("%s overlap=%v, err=%v", key, want, err)
				}
			}
		})
	}
}

func TestHistoryOwnerCoverageBoundaries(t *testing.T) {
	cases := []struct {
		name              string
		parents, children []historyTestInterval
		invalid           bool
	}{
		{"adjacent versions", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}, {"p", 7, 0, 2, nil}}, []historyTestInterval{{"p", 7, 0, 1, nil}}, false},
		{"ends with owner", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}}, []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}}, false},
		{"starts after owner ends", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}}, []historyTestInterval{{"p", 7, 0, 2, nil}}, true},
		{"open child finite owner", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}}, []historyTestInterval{{"p", 7, 0, 1, nil}}, true},
		{"child before owner", []historyTestInterval{{"p", 7, 0, 2, nil}}, []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}}, true},
		{"real historical gap", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}, {"p", 7, 0, 3, nil}}, []historyTestInterval{{"p", 7, 0, 1, nil}}, true},
		{"separate child islands", []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}, {"p", 7, 0, 3, nil}}, []historyTestInterval{{"p", 7, 0, 1, historyEnd(2)}, {"p", 7, 0, 3, nil}}, false},
		{"project isolated", []historyTestInterval{{"p", 7, 0, 1, nil}}, []historyTestInterval{{"q", 7, 0, 1, nil}}, true},
		{"PR isolated", []historyTestInterval{{"p", 7, 0, 1, nil}}, []historyTestInterval{{"p", 8, 0, 1, nil}}, true},
		{"maximum open boundary", []historyTestInterval{{"p", 7, 0, 1, historyEnd(math.MaxInt64)}, {"p", 7, 0, math.MaxInt64, nil}}, []historyTestInterval{{"p", 7, 0, math.MaxInt64, nil}}, false},
		{"maximum uncovered", []historyTestInterval{{"p", 7, 0, 1, historyEnd(math.MaxInt64)}}, []historyTestInterval{{"p", 7, 0, math.MaxInt64, nil}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := historyOwnerTestDB(t)
			for _, r := range tc.parents {
				historyOwnerTestInsert(t, db, "review_pull_memberships", r)
			}
			for _, r := range tc.children {
				historyOwnerTestInsert(t, db, "children", r)
			}
			historyOwnerTestCheck(t, db, tc.invalid)
		})
	}
}

func historyOwnerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for _, q := range []string{`CREATE TABLE review_pull_memberships(project_id TEXT,number INTEGER,start_revision INTEGER,end_revision INTEGER)`, `CREATE TABLE children(project_id TEXT,pull_number INTEGER,start_revision INTEGER,end_revision INTEGER)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func historyOwnerTestInsert(t *testing.T, db *sql.DB, table string, r historyTestInterval) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO "+table+" VALUES(?,?,?,?)", r.project, r.key, r.start, r.end); err != nil {
		t.Fatal(err)
	}
}
func historyOwnerTestCheck(t *testing.T, db *sql.DB, invalid bool) {
	t.Helper()
	var found int
	err := db.QueryRowContext(t.Context(), historyOwnerCoverageQuery("children")).Scan(&found)
	if (err == nil) != invalid || (err != nil && !errors.Is(err, sql.ErrNoRows)) {
		t.Fatalf("uncovered child=%v, err=%v", invalid, err)
	}
}

func TestHistoryOwnerCoverageManySeparatedIslands(t *testing.T) {
	db := historyOwnerTestDB(t)
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, table := range []string{"review_pull_memberships", "children"} {
		if _, err := tx.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<3000) INSERT INTO ` + table + ` SELECT 'p',7,2*i,2*i+1 FROM n`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	historyOwnerTestCheck(t, db, false)
	t.Logf("validated 3000 separated parent/child islands in %s", time.Since(start))
	// A child spanning the first gap must fail, even though all its later owner
	// islands and its start are individually covered.
	historyOwnerTestInsert(t, db, "children", historyTestInterval{"p", 7, 0, 2, historyEnd(5)})
	historyOwnerTestCheck(t, db, true)
}
