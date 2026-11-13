package reviewexec

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

var cooldownProofReads atomic.Int64
var cooldownProofRegister sync.Once

// Capture the production selector so the plan assertion cannot drift to a
// separately hand-written query.
type cooldownQueryRecorder struct {
	*sql.DB
	selector string
}

func (q *cooldownQueryRecorder) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "FROM review_execution_attempts a") {
		q.selector = query
	}
	return q.DB.QueryRowContext(ctx, query, args...)
}

func cooldownQueryDB(t *testing.T) *sql.DB {
	t.Helper()
	cooldownProofRegister.Do(func() {
		sqlite.MustRegisterScalarFunction("test_cooldown_proof", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			cooldownProofReads.Add(1)
			return args[0], nil
		})
	})
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE review_execution_attempts(id TEXT PRIMARY KEY,operation_id TEXT,project_id TEXT,provider TEXT,account TEXT,state TEXT,revision INTEGER,anchor_json TEXT,intent TEXT,external_id TEXT)`,
		`CREATE INDEX review_attempts_operation ON review_execution_attempts(operation_id,id)`,
		`CREATE TABLE review_execution_transitions(attempt_id TEXT,revision INTEGER,state TEXT,reason TEXT,created_at TEXT,PRIMARY KEY(attempt_id,revision))`,
		`CREATE TABLE proof(attempt_id TEXT PRIMARY KEY,body_json TEXT)`,
		`CREATE VIEW review_hosted_preflight AS SELECT attempt_id,test_cooldown_proof(body_json) AS body_json FROM proof`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func cooldownQueryInsert(t *testing.T, q interface {
	Exec(string, ...any) (sql.Result, error)
}, id, at, body string) {
	t.Helper()
	if _, err := q.Exec(`INSERT INTO review_execution_attempts VALUES(?,'op','project','hosted','account','held',1,'{}','','')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(`INSERT INTO review_execution_transitions VALUES(?,1,'held','hosted_allowance_cooldown',?)`, id, at); err != nil {
		t.Fatal(err)
	}
	if body != "absent" {
		if _, err := q.Exec(`INSERT INTO proof VALUES(?,?)`, id, body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLatestHostedCooldownQueryHistoryCost(t *testing.T) {
	for _, n := range []int{1000, 10000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			db := cooldownQueryDB(t)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			body := `{"input":{"observation":{"RetryNotBefore":"2026-02-01T00:00:00Z"}},"padding":"` + strings.Repeat("x", 4096) + `"}`
			for i := 0; i < n; i++ {
				cooldownQueryInsert(t, tx, fmt.Sprintf("a%05d", i), start.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), body)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			cooldownProofReads.Store(0)
			at := time.Now()
			recorder := &cooldownQueryRecorder{DB: db}
			a, err := latestCooldownAttempt(context.Background(), recorder, Operation{ID: "op", ProjectID: "project", Channel: "hosted"})
			if err != nil || a.ID != fmt.Sprintf("a%05d", n-1) {
				t.Fatal(a, err)
			}
			t.Logf("history=%d selected=%s proof-scalar-reads=%d elapsed=%s", n, a.ID, cooldownProofReads.Load(), time.Since(at))
			if reads := cooldownProofReads.Load(); reads > 12 {
				t.Fatalf("historical proofs evaluated before newest transition narrowed: %d", reads)
			}
			rows, err := db.Query("EXPLAIN QUERY PLAN "+recorder.selector, "op", "", "hosted_allowance_cooldown")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				t.Log(detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}

		})
	}
}

func TestLatestHostedCooldownQueryTieSemantics(t *testing.T) {
	proof := func(at string) string { return `{"input":{"observation":{"RetryNotBefore":"` + at + `"}}}` }
	for _, test := range []struct{ name, bodyA, bodyZ, want string }{
		{"deadline wins despite ID", proof("2026-01-02T00:00:02Z"), proof("2026-01-02T00:00:01Z"), "a-tied"},
		{"equal deadline uses ID", proof("2026-01-02T00:00:02Z"), proof("2026-01-02T00:00:02Z"), "z-tied"},
		{"malformed wins", `{`, proof("2026-01-02T00:00:02Z"), "a-tied"},
		{"missing proof wins", "absent", proof("2026-01-02T00:00:02Z"), "a-tied"},
		{"oversized wins", strings.Repeat("x", 16385), proof("2026-01-02T00:00:02Z"), "a-tied"},
		{"missing scalar wins", `{}`, proof("2026-01-02T00:00:02Z"), "a-tied"},
		{"nontext scalar wins", `{"input":{"observation":{"RetryNotBefore":2}}}`, proof("2026-01-02T00:00:02Z"), "a-tied"},
		{"tied unreadable uses ID", `{`, `{`, "z-tied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := cooldownQueryDB(t)
			cooldownQueryInsert(t, db, "old-malformed", "2026-01-01T00:00:00Z", `{`)
			cooldownQueryInsert(t, db, "old-later-deadline", "2026-01-01T00:00:00Z", proof("2099-01-01T00:00:00Z"))
			// Timestamps may be clamped equal after rollback; provider deadline then
			// disambiguates order, except unreadable proof must win conservatively.
			cooldownQueryInsert(t, db, "a-tied", "2026-01-01T00:00:01Z", test.bodyA)
			cooldownQueryInsert(t, db, "z-tied", "2026-01-01T00:00:01Z", test.bodyZ)
			cooldownQueryInsert(t, db, "current", "2026-01-01T00:00:03Z", `{`)
			a, err := latestCooldownAttempt(t.Context(), db, Operation{ID: "op", ProjectID: "project", Channel: "hosted", AttemptID: "current"})
			if err != nil || a.ID != test.want {
				t.Fatal("latest-time or conservative tie ordering changed", a.ID, err, test.want)
			}
		})
	}
}
