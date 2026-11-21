package reviews

import (
	"reflect"
	"strings"
	"testing"
)

func TestReviewSnapshotReferenceIndexes(t *testing.T) {
	type probe struct {
		table, column, index string
	}
	var probes []probe
	for _, prefix := range []string{"pull", "run", "attempt", "retired"} {
		versions := "review_" + prefix + "_versions"
		memberships := "review_" + prefix + "_memberships"
		probes = append(probes,
			probe{versions, "version_revision", versions + "_snapshot"},
			probe{memberships, "start_revision", memberships + "_snapshot_start"},
			probe{memberships, "end_revision", memberships + "_snapshot_end"},
		)
	}

	for _, existing := range []bool{false, true} {
		name := "fresh"
		if existing {
			name = "existing-v13"
		}
		t.Run(name, func(t *testing.T) {
			var f *apiFixture
			retained := map[string]historyMigrationWire{}
			if existing {
				// Build the genuine pre-index v13 migration sequence with retained
				// versions and closed/open intervals, rather than rewind a stamp.
				f = lineageFixture(t, "integrated13")
				if err := EnsureNotificationSchema(t.Context(), f.store); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM metadata WHERE key='schema:review_retention_indexes'`).Scan(&count); err != nil || count != 0 {
					t.Fatal("pre-index fixture already has the additive domain", count, err)
				}
				for _, p := range probes {
					if err := f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE type='index' AND name=?`, p.index).Scan(&count); err != nil || count != 0 {
						t.Fatal("pre-index fixture already has index", p.index, count, err)
					}
				}
				for _, route := range []string{
					"/queue?limit=1",
					"/pulls/7?snapshot=legacy-one",
					"/pulls/8?snapshot=legacy-four",
					"/pulls/7?snapshot=legacy-nine",
					"/runs/" + runOpaqueID(f.project, "producer-a", "shared-run") + "?snapshot=legacy-one",
				} {
					wire := historyMigrationRead(t, f, route)
					if wire.Status != 200 {
						t.Fatalf("retained fixture read %s: %d %s", route, wire.Status, wire.Body)
					}
					retained[route] = wire
				}
				var version string
				if err := f.store.DB().QueryRowContext(t.Context(), `SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version); err != nil || version != "13" {
					t.Fatal("pre-index fixture is not reviews v13", version, err)
				}
				if err := EnsureSchema(t.Context(), f.store); err != nil {
					t.Fatal("add indexes to existing reviews v13", err)
				}
			} else {
				f = newAPIFixture(t)
			}

			for key, want := range map[string]string{"schema:reviews": "13", "schema:review_retention_indexes": "1"} {
				var got string
				if err := f.store.DB().QueryRowContext(t.Context(), `SELECT value FROM metadata WHERE key=?`, key).Scan(&got); err != nil || got != want {
					t.Fatalf("%s = %q, want %q: %v", key, got, want, err)
				}
			}
			for _, p := range probes {
				t.Run(p.index, func(t *testing.T) {
					var columns string
					if err := f.store.DB().QueryRowContext(t.Context(), `SELECT group_concat(name,',') FROM (SELECT name FROM pragma_index_info(?) ORDER BY seqno)`, p.index).Scan(&columns); err != nil || columns != "project_id,"+p.column {
						t.Fatalf("index columns = %q, want project_id,%s: %v", columns, p.column, err)
					}
					// Do not force INDEXED BY: the ordinary child-FK probe must
					// naturally constrain both columns, not scan a project's history.
					rows, err := f.store.DB().QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT rowid FROM `+p.table+` WHERE project_id=? AND `+p.column+`=?`, f.project, 1)
					if err != nil {
						t.Fatal(err)
					}
					defer rows.Close()
					var plan []string
					for rows.Next() {
						var id, parent, unused int
						var detail string
						if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
							t.Fatal(err)
						}
						plan = append(plan, detail)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					if len(plan) != 1 || !strings.HasPrefix(plan[0], "SEARCH "+p.table+" ") || !strings.Contains(plan[0], "INDEX "+p.index+" (") || !strings.Contains(plan[0], "(project_id=? AND "+p.column+"=?)") {
						t.Fatalf("expected full composite child-FK lookup, got %v", plan)
					}
				})
			}
			var workspaceIndexes int
			if err := f.store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE type='index' AND tbl_name='review_workspace' AND sql IS NOT NULL`).Scan(&workspaceIndexes); err != nil || workspaceIndexes != 0 {
				t.Fatal("workspace needs no additional index beyond its project primary key", workspaceIndexes, err)
			}
			for route, want := range retained {
				if got := historyMigrationRead(t, f, route); !reflect.DeepEqual(got, want) {
					t.Fatalf("additive indexes changed retained read %s\nbefore=%+v\nafter=%+v", route, want, got)
				}
			}
			before := historyMigrationDatabaseDigest(t, f)
			if err := EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal("repeat startup", err)
			}
			if after := historyMigrationDatabaseDigest(t, f); after != before {
				t.Fatal("repeat startup changed schema or persisted rows")
			}
		})
	}
}
