package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func attemptClockRows(t *testing.T, f *attemptFixture) string {
	t.Helper()
	result := ""
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_transitions", "review_execution_reservations", "events", "receipts"} {
		rows, err := f.store.DB().Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			result += table + string(b)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return result
}
func assertAttemptClock(t *testing.T, f *attemptFixture, a Attempt) {
	t.Helper()
	var created, updated, first, current, state string
	if err := f.store.DB().QueryRow(`SELECT created_at,updated_at FROM review_operations WHERE id=?`, a.OperationID).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=1`, a.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT created_at,state FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&current, &state); err != nil {
		t.Fatal(err)
	}
	c, ce := time.Parse(time.RFC3339Nano, created)
	s, se := time.Parse(time.RFC3339Nano, first)
	last, le := time.Parse(time.RFC3339Nano, current)
	if ce != nil || se != nil || le != nil || state != a.State || s.Before(c) || last.Before(s) || updated != current {
		t.Fatalf("local writer produced invalid or mixed journal timing: created=%s start=%s current=%s operation_updated=%s state=%s", created, first, current, updated, state)
	}
}

func TestAttemptClockRollbackStartAdmissionAndSettlement(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	r := f.enqueue(t, attemptProject, "attempt-clock", "cli", 2, 1)
	var created string
	if err := f.store.DB().QueryRow(`SELECT created_at FROM review_operations WHERE id=?`, r.ID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	original, _ := time.Parse(time.RFC3339Nano, created)
	clock := original.Add(-250 * time.Millisecond)
	samples := 0
	f.e.attemptNow = func() time.Time { samples++; return clock }
	a := f.start(t, attemptProject, r)
	assertAttemptClock(t, f, a)
	if samples != 1 {
		t.Fatal("start sampled recording clock more than once", samples)
	}
	anchor := attemptAnchor(attemptProject, 2)
	clock = clock.Add(-time.Second)
	samples = 0
	var err error
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	assertAttemptClock(t, f, a)
	if samples != 1 || a.Anchor == nil || *a.Anchor != anchor {
		t.Fatal("admission resampled or rewrote observed source", samples)
	}
	clock = clock.Add(-time.Second)
	samples = 0
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "clock-completed", "")
	if err != nil {
		t.Fatal(err)
	}
	assertAttemptClock(t, f, a)
	if samples != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
		t.Fatal("terminal settlement sampled twice or retained reservation", samples)
	}
}

func TestAttemptClockSingleSampleWithinMutation(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	r := f.enqueue(t, attemptProject, "single-clock", "cli", 2, 1)
	base := time.Now().UTC().Add(time.Second)
	samples := 0
	f.e.attemptNow = func() time.Time { samples++; return base.Add(-time.Duration(samples-1) * 250 * time.Millisecond) }
	a := f.start(t, attemptProject, r)
	assertAttemptClock(t, f, a)
	if samples != 1 {
		t.Fatal("start independently sampled operation and transition", samples)
	}
	samples = 0
	var err error
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	assertAttemptClock(t, f, a)
	if samples != 1 {
		t.Fatal("admission independently sampled operation and transition", samples)
	}
}

func TestAttemptClockInterruptedRecoveryAfterReopen(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "preparing", true: "active"}[admitted], func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			r := f.enqueue(t, attemptProject, "reopen-clock", "cli", 2, 1)
			clock := time.Now().UTC().Add(time.Second)
			f.e.attemptNow = func() time.Time { return clock }
			a := f.start(t, attemptProject, r)
			var err error
			if admitted {
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
				if err != nil {
					t.Fatal(err)
				}
			}
			var path string
			if err = f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			if err = f.store.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := core.Open(t.Context(), filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			f.store = s
			f.e, err = New(s, attemptCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			samples := 0
			clock = clock.Add(-2 * time.Second)
			f.e.attemptNow = func() time.Time { samples++; return clock }
			if err = f.e.RecoverInterrupted(t.Context()); err != nil {
				t.Fatal(err)
			}
			err = s.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var e error
				a, e = readAttempt(ctx, tx, attemptProject, a.ID)
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			assertAttemptClock(t, f, a)
			want := "cancelled"
			reservations := 0
			if admitted {
				want = "uncertain"
				reservations = 1
			}
			if samples != 1 || a.State != want || attemptCount(t, s, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != reservations {
				t.Fatal("restart changed settlement/reservation or sampled twice", samples, a.State)
			}
		})
	}
}

func TestAttemptClockCorruptRetainedFactsRollback(t *testing.T) {
	for _, kind := range []string{"operation-created", "operation-reversed", "missing-start", "missing-current", "bad-current", "reversed-current"} {
		t.Run(kind, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "corrupt-clock", "cli", 2, 1))
			var err error
			a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "operation-created":
				_, err = f.store.DB().Exec(`UPDATE review_operations SET created_at='private broken timestamp' WHERE id=?`, a.OperationID)
			case "operation-reversed":
				_, err = f.store.DB().Exec(`UPDATE review_operations SET updated_at='2000-01-01T00:00:00Z' WHERE id=?`, a.OperationID)
			case "missing-start":
				_, err = f.store.DB().Exec(`DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=1`, a.ID)
			case "missing-current":
				_, err = f.store.DB().Exec(`DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, a.ID)
			case "bad-current":
				_, err = f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at='private broken timestamp' WHERE attempt_id=? AND revision=2`, a.ID)
			case "reversed-current":
				_, err = f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at='2000-01-01T00:00:00Z' WHERE attempt_id=? AND revision=2`, a.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := attemptClockRows(t, f)
			_, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "clock-completed", "")
			attemptCode(t, err, "storage_unavailable")
			if strings.Contains(err.Error(), "private broken") || before != attemptClockRows(t, f) {
				t.Fatal("corruption leaked or partially mutated retained state", err)
			}
		})
	}
}
