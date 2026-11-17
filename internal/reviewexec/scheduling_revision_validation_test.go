package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestAutomaticRetainedRevisionValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"text", "malformed"}, {"fraction", 1.5}, {"blob", []byte("1")}, {"null", nil}, {"zero", int64(0)}, {"negative", int64(-1)}, {"overflow", "9223372036854775808"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			r := schedulingLatest(t, f)
			// Remove physical constraints to model damaged retained data, including
			// values SQLite's normal column constraints would have rejected.
			admissionSQL(t, f, `ALTER TABLE review_automatic_selections RENAME TO saved_automatic_selections`)
			admissionSQL(t, f, `CREATE TABLE review_automatic_selections AS SELECT * FROM saved_automatic_selections`)
			admissionSQL(t, f, `UPDATE review_automatic_selections SET scheduling_revision=?`, tc.value)
			err := continuityGuard(t, f, r)
			if !retainedRevisionSchedulingError(err) {
				t.Fatalf("malformed revision did not retain scheduling error: %v", err)
			}
		})
	}
}

func TestAutomaticRetainedRevisionPreservesQueryErrors(t *testing.T) {
	for _, mode := range []string{"sql", "cancelled", "transaction"} {
		t.Run(mode, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			r := schedulingLatest(t, f)
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ctx := t.Context()
			switch mode {
			case "sql":
				if _, err := tx.Exec(`DROP TABLE review_automatic_selections`); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "transaction":
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			err = f.e.requireAutomaticRun(ctx, tx, r)
			if err == nil || retainedRevisionSchedulingError(err) {
				t.Fatalf("real failure suppressed: %v", err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if mode == "transaction" && !errors.Is(err, sql.ErrTxDone) {
				t.Fatal(err)
			}
		})
	}
}

// Assert the stable public scheduling error independently of the internal
// project-local refusal marker; dedicated tests cover supervisor isolation.
func retainedRevisionSchedulingError(err error) bool {
	var failure *core.Error
	return errors.As(err, &failure) && failure.Code == "storage_unavailable" && failure.Status == 503 && failure.Message == "Stored review scheduling is unavailable."
}
