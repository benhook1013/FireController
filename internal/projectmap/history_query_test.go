package projectmap

import (
	"os"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
)

func TestHistoryContinuationUsesRevisionIndexRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = jobs.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if err = EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	for name, queryFor := range map[string]func(string, string, int64, int) (string, []any){
		"entry": entryHistoryQuery, "association": associationHistoryQuery,
	} {
		t.Run(name, func(t *testing.T) {
			// Explain the exact construction used by the HTTP handler, against
			// the installed domain schema. A continuation must seek below its
			// cursor rather than visit all newer rows in the scoped history.
			query, args := queryFor("project", "record", 50, 26)
			rows, err := store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plans []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plans = append(plans, detail)
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(plans, "\n")
			t.Log(plan)
			if !strings.Contains(plan, "revision<?") || strings.Contains(plan, "USE TEMP B-TREE") {
				t.Fatalf("history continuation lacks a revision index range: %s", plan)
			}
		})
	}
}
