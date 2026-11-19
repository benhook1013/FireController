package retention

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	_ "modernc.org/sqlite"
)

func TestJobHistoryEligibility(t *testing.T) {
	cutoff := time.Date(2026, 10, 20, 12, 8, 9, 500, time.UTC)
	for _, spec := range jobHistoryPolicies {
		t.Run(spec.table, func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			columns := fmt.Sprintf("project_id TEXT, %s TEXT, %s INTEGER, %s TEXT", spec.owner, spec.revision, spec.timestamp)
			if spec.table == "job_progress_history" {
				columns += ", sequence INTEGER PRIMARY KEY"
			}
			if _, err := db.Exec("CREATE TABLE " + spec.table + "(" + columns + ")"); err != nil {
				t.Fatal(err)
			}
			put := func(project, owner string, revision int, at time.Time) {
				t.Helper()
				values := []any{project, owner, revision, at.Format(time.RFC3339Nano)}
				placeholders := "?,?,?,?"
				if spec.table == "job_progress_history" {
					placeholders += ",NULL"
				}
				if _, err := db.Exec("INSERT INTO "+spec.table+" VALUES("+placeholders+")", values...); err != nil {
					t.Fatal(err)
				}
			}
			zone := time.FixedZone("ahead", 3600)
			for i := 1; i <= 28; i++ {
				at := cutoff.Add(-time.Hour)
				switch i {
				case 3:
					at = cutoff
				case 4:
					at = cutoff.Add(time.Nanosecond)
				case 5:
					at = cutoff.Add(-time.Nanosecond).In(zone)
				case 6:
					at = cutoff.Add(time.Hour)
				}
				put("one", "owner", i, at)
			}
			put("other", "owner", 2, cutoff.Add(-time.Hour))
			put("one", "other", 2, cutoff.Add(-time.Hour))
			want := "2,5,7,8"
			if spec.table == "job_progress_history" {
				// Rev 9 is the oldest of twenty distinct revisions, while
				// rev 8 is immediately outside. Append repeats after newer
				// revisions to test ordering by owning revision, not sequence.
				for range 4 {
					put("one", "owner", 1, cutoff.Add(-time.Hour))
					put("one", "owner", 8, cutoff.Add(-time.Hour))
					put("one", "owner", 9, cutoff.Add(-time.Hour))
				}
				put("one", "owner", 2, cutoff.Add(time.Nanosecond))
				// The append head can refer to a nonmonotonic revision
				// and remains the evidence of the live current projection.
				put("one", "owner", 7, cutoff.Add(-time.Hour))
				want = "5,8,8,8,8,8"
			}
			match := func() string {
				t.Helper()
				var ids string
				q := "SELECT COALESCE(group_concat(r, ','),'') FROM (SELECT " + spec.revision +
					" r FROM " + spec.table + " WHERE " + spec.where() + " ORDER BY " + spec.revision + ")"
				if err := db.QueryRow(q, cutoff.Unix(), cutoff.Nanosecond()).Scan(&ids); err != nil {
					t.Fatal(err)
				}
				return ids
			}
			if got := match(); got != want {
				t.Fatalf("eligible revisions = %q; want %q", got, want)
			}
			if _, err := db.Exec("DELETE FROM "+spec.table+" WHERE "+spec.where(), cutoff.Unix(), cutoff.Nanosecond()); err != nil {
				t.Fatal(err)
			}
			if got := match(); got != "" {
				t.Fatalf("second pass still has eligible revisions: %s", got)
			}
		})
	}
}

// Exercise the exact worker helper against a database large enough to cross a
// batch boundary; live schema and restore compatibility are tested separately.
func TestJobHistoryPassDryRunAndIdempotence(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const rowCount = BatchRows + 25
	cutoff := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, spec := range jobHistoryPolicies {
		columns := fmt.Sprintf("project_id TEXT, %s TEXT, %s INTEGER, %s TEXT", spec.owner, spec.revision, spec.timestamp)
		fields, values := "project_id,"+spec.owner+","+spec.revision+","+spec.timestamp, "'one','owner',n,?"
		if spec.table == "job_progress_history" {
			columns += ", sequence INTEGER PRIMARY KEY"
		}
		db := store.DB()
		if _, err := db.ExecContext(ctx, "CREATE TABLE "+spec.table+"("+columns+")"); err != nil {
			t.Fatal(err)
		}
		populate := "WITH RECURSIVE revisions(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM revisions WHERE n<?) INSERT INTO " +
			spec.table + "(" + fields + ") SELECT " + values + " FROM revisions"
		if _, err := db.ExecContext(ctx, populate, rowCount, cutoff.Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		dry            bool
		deleted, alive int
	}{
		{true, rowCount - retainedJobHistoryRevisions - 1, rowCount},
		{false, rowCount - retainedJobHistoryRevisions - 1, retainedJobHistoryRevisions + 1},
		{false, 0, retainedJobHistoryRevisions + 1},
	} {
		pass := &pass{store: store, dryRun: test.dry, deleted: map[string]int64{}, yield: time.Millisecond}
		if err := pruneJobHistory(ctx, pass, cutoff); err != nil {
			t.Fatal(err)
		}
		for _, spec := range jobHistoryPolicies {
			var n int
			if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+spec.table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if pass.deleted[spec.table] != int64(test.deleted) || n != test.alive {
				t.Fatalf("dryRun=%t %s: selected/deleted=%d retained=%d; want %d/%d", test.dry,
					spec.table, pass.deleted[spec.table], n, test.deleted, test.alive)
			}
		}
	}
}
