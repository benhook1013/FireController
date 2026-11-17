package projectmap

import (
	"os"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
)

func TestEditorialPageUsesPositionIndexWithoutSortingBodies(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = jobs.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if err = EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		name, sql, seek string
		args            []any
	}{
		{"same position", editorialSamePositionQuery, "id>?", []any{maxEntryBytes, "project", 7, "map_entry_00000000000000000000000000000000", 6}},
		{"later positions", editorialLaterPositionQuery, "<expr>>?", []any{maxEntryBytes, "project", 7, 6}},
	} {
		t.Run(query.name, func(t *testing.T) {
			rows, err := store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.sql, query.args...)
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
			if !strings.Contains(plan, "project_map_entries_editorial_order") || !strings.Contains(plan, query.seek) || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatal("editorial continuation lacks an indexed position/ID seek", plan)
			}
		})
	}
}
