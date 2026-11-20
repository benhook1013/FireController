package retention

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"modernc.org/sqlite"
)

func TestScheduledRetentionSnapshotFailureDiagnostics(t *testing.T) {
	const marker = "private-fixture-marker"
	name := "test_retention_wait_" + strings.ReplaceAll(core.NewID("fixture"), "-", "_")
	if err := sqlite.RegisterScalarFunction(name, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		time.Sleep(2250 * time.Millisecond)
		return int64(0), nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, expr, class string }{
		{"storage", "RAISE(ABORT,'" + marker + "')", "storage"},
		{"deadline", name + "()", "deadline_or_busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lease := filepath.Join(dir, "phase.lock")
			if err := os.WriteFile(lease, nil, 0600); err != nil {
				t.Fatal(err)
			}
			db, err := core.Open(t.Context(), filepath.Join(dir, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = reviews.EnsureSchema(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			a, b, c := core.NewID("review_snapshot"), core.NewID("review_snapshot"), core.NewID("review_snapshot")
			old := stamp(time.Now().Add(-31 * 24 * time.Hour))
			now := stamp(time.Now())
			put := func(q string, args ...any) {
				t.Helper()
				if _, err := db.DB().ExecContext(t.Context(), q, args...); err != nil {
					t.Fatal(err)
				}
			}
			put("INSERT INTO projects(id,name,github_repository_id,repository) VALUES('p-ret','Fixture',1,'test/retention')")
			put("INSERT INTO review_snapshots VALUES('p-ret',?,1,1,'fixture',?,?,printf('%064d',1)),('p-ret',?,2,1,'fixture',?,?,printf('%064d',2)),('p-ret',?,3,1,'fixture',?,?,printf('%064d',3))", a, old, old, b, old, old, c, now, now)
			put("INSERT INTO review_workspace VALUES('p-ret',3,?)", c)
			put("INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) VALUES('p-ret','review.observed',?,1,'fixture','{}',?)", a, old)
			put("CREATE TRIGGER test_snapshot_failure AFTER DELETE ON review_snapshots WHEN OLD.revision=1 BEGIN SELECT " + tc.expr + "; END")
			cfg := Config{Interval: 10 * time.Millisecond, ReviewHistory: DefaultHistory, JobHistory: DefaultHistory, Receipts: DefaultHistory, Diagnostics: DefaultDiagnostics, PhaseLease: lease}
			ctx, cancel := context.WithCancel(t.Context())
			reports := make(chan Report, 1)
			failures := make(chan error, 1)
			done := Run(ctx, db, cfg, func(r Report) {
				select {
				case reports <- r:
				default:
				}
			}, func(err error) {
				select {
				case failures <- err:
				default:
				}
			})
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("retention worker did not stop")
				}
			}()
			var report Report
			select {
			case report = <-reports:
				cancel()
			case err := <-failures:
				t.Fatalf("unexpected generic failure: %v", err)
			case <-time.After(9 * time.Second):
				t.Fatal("no scheduled report")
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("retention worker did not stop")
			}
			if report.Error != "review_history_failed" || report.HistoryFailurePhase != "snapshot_batch" || report.HistoryFailureClass != tc.class {
				t.Fatalf("unexpected report: %+v", report)
			}
			select {
			case err := <-failures:
				t.Fatalf("unexpected generic failure: %v", err)
			default:
			}
			var fence, raw string
			if err = db.DB().QueryRow("SELECT value FROM metadata WHERE key=?", core.ReviewHistoryFloorPrefix+"p-ret").Scan(&fence); err != nil || fence != "2" {
				t.Fatalf("floor=%q err=%v", fence, err)
			}
			if err = db.DB().QueryRow("SELECT value FROM metadata WHERE key=?", LastPassKey).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			expected, err := json.Marshal(report)
			var fields map[string]json.RawMessage
			if err != nil || raw != string(expected) || json.Unmarshal([]byte(raw), &fields) != nil || len(fields) != 5 || strings.Contains(raw, marker) {
				t.Fatalf("durable report changed: %s", raw)
			}
			for _, q := range []string{"SELECT count(*) FROM review_snapshots WHERE snapshot_id=?", "SELECT count(*) FROM events WHERE resource_id=? AND event_type='review.observed'"} {
				var present int
				if err = db.DB().QueryRow(q, a).Scan(&present); err != nil || present != 1 {
					t.Fatalf("snapshot/event rollback not intact: %d, %v", present, err)
				}
			}
		})
	}
}
