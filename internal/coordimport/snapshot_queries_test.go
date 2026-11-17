package coordimport

import (
	"context"
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"testing"
)

func TestSnapshotQueriesProgramsNotValuesAndLifetime(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("CREATE TABLE values_test(value INTEGER); INSERT INTO values_test VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	var previous *sql.Stmt
	for pass := 0; pass < 2; pass++ {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		q := newSnapshotQueries(tx)
		const query = "SELECT value FROM values_test"
		var got int
		if err = q.QueryRowContext(t.Context(), query).Scan(&got); err != nil || got != 1 {
			t.Fatal(got, err)
		}
		first := q.statements[query]
		if first == nil || first == previous {
			t.Fatal("statement not transaction local")
		}
		previous = first
		if _, err = tx.ExecContext(t.Context(), "UPDATE values_test SET value=2"); err != nil {
			t.Fatal(err)
		}
		if err = q.QueryRowContext(t.Context(), query).Scan(&got); err != nil || got != 2 || q.statements[query] != first {
			t.Fatal("cached result", got, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err = q.QueryRowContext(ctx, query).Scan(&got); err == nil {
			t.Fatal("cancelled read succeeded")
		}
		if rows, err := q.QueryContext(ctx, query); err == nil {
			rows.Close()
			t.Fatal("cancelled rows succeeded")
		}
		if err = q.QueryRowContext(t.Context(), "SELECT absent_column FROM values_test").Scan(&got); err == nil {
			t.Fatal("prepare error hidden")
		}
		if rows, err := q.QueryContext(t.Context(), "SELECT absent_column FROM values_test"); err == nil {
			rows.Close()
			t.Fatal("rows prepare error hidden")
		}
		for i := 0; i < 40; i++ {
			if err = q.QueryRowContext(t.Context(), fmt.Sprintf("SELECT %d", i)).Scan(&got); err != nil || got != i {
				t.Fatal(got, err)
			}
		}
		if len(q.statements) != 32 {
			t.Fatal("unbounded programs", len(q.statements))
		}
		q.close()
		if err = first.QueryRowContext(t.Context()).Scan(&got); err == nil {
			t.Fatal("statement outlived validation")
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}
