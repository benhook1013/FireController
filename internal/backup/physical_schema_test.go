package backup

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestDomainPhysicalSchemaExactInventory(t *testing.T) {
	canonical := []string{
		`CREATE TABLE owned (id INTEGER PRIMARY KEY, value TEXT);`,
		`CREATE INDEX owned_value ON owned(value);`,
		`CREATE UNIQUE INDEX owned_unique ON owned(value, id);`,
		`CREATE TRIGGER owned_retain BEFORE DELETE ON owned BEGIN SELECT RAISE(ABORT,'retain'); END;`,
	}
	for _, tc := range []struct {
		name, mutation string
		valid          bool
	}{
		{"canonical", "", true},
		{"unrelated objects", `CREATE TABLE unrelated (id INTEGER); CREATE INDEX unrelated_id ON unrelated(id);`, true},
		{"missing index", `DROP INDEX owned_value;`, false},
		{"changed definition", `DROP INDEX owned_value; CREATE INDEX owned_value ON owned(id);`, false},
		{"extra index", `CREATE INDEX owned_extra ON owned(id);`, false},
		{"missing table", `DROP TABLE owned;`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, statement := range canonical {
				if _, err := db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mutation != "" {
				if _, err := db.ExecContext(t.Context(), tc.mutation); err != nil {
					t.Fatal(err)
				}
			}
			err = validateDomainPhysicalSchema(t.Context(), db, canonical, "owned")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestSchedulingPhysicalSchemaExactInventory(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			canonical := reviewexec.SchedulingSchemaSQL()
			if version == 2 {
				canonical = reviewexec.SchedulingSchemaV2SQL()
			}
			for _, tc := range []struct {
				name, mutation string
				valid          bool
			}{
				{"canonical", "", true},
				{"unrelated objects", `CREATE TABLE unrelated (id INTEGER); CREATE INDEX unrelated_id ON unrelated(id);`, true},
				{"missing table", `DROP TABLE review_pull_scheduling;`, false},
				{"missing index", `DROP INDEX review_automatic_selections_pull;`, false},
				{"changed index", `DROP INDEX review_automatic_selections_pull; CREATE INDEX review_automatic_selections_pull ON review_automatic_selections(project_id);`, false},
				{"weakened table", `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'revision>0','revision>=0') WHERE name='review_scheduling'; PRAGMA writable_schema=OFF;`, false},
				{"extra index", `CREATE INDEX scheduling_extra ON review_scheduling(revision);`, false},
				{"oversized name", `CREATE INDEX ` + strings.Repeat("x", 129) + ` ON review_scheduling(revision);`, false},
				{"oversized definition", `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=sql||' /*` + strings.Repeat("x", 16384) + `*/' WHERE name='review_scheduling'; PRAGMA writable_schema=OFF;`, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.sqlite"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					for _, statement := range canonical {
						if _, err := db.ExecContext(t.Context(), statement); err != nil {
							t.Fatal(err)
						}
					}
					if tc.mutation != "" {
						if _, err := db.ExecContext(t.Context(), tc.mutation); err != nil {
							t.Fatal(err)
						}
					}
					err = validateSchedulingPhysicalSchema(t.Context(), db, version)
					if tc.valid {
						if err != nil {
							t.Fatal("canonical scheduling schema rejected", err)
						}
						backupCode(t, validateSchedulingPhysicalSchema(t.Context(), db, 3-version), "backup_invalid")
					} else {
						backupCode(t, err, "backup_invalid")
					}
				})
			}
		})
	}
}

func TestDomainPhysicalSchemaRejectsInvalidCanonicalInputs(t *testing.T) {
	const table = `CREATE TABLE owned (id INTEGER);`
	for _, tc := range []struct {
		name       string
		statements []string
		names      []string
	}{
		{"empty", nil, nil},
		{"duplicate object", []string{table, table}, []string{"owned"}},
		{"duplicate owner", []string{table}, []string{"owned", "owned"}},
		{"unexpected owner", []string{table}, []string{"other"}},
		{"missing owner", []string{table}, []string{"owned", "missing"}},
		{"noncreate", []string{`ALTER TABLE owned ADD value TEXT;`}, []string{"owned"}},
		{"unsupported object", []string{`CREATE VIEW owned AS SELECT 1;`}, []string{"owned"}},
		{"malformed unique", []string{`CREATE UNIQUE TABLE owned (id INTEGER);`}, []string{"owned"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Invalid repository inputs must fail before accessing the database.
			if err := validateDomainPhysicalSchema(t.Context(), nil, tc.statements, tc.names...); err == nil {
				t.Fatal("accepted an invalid physical schema specification")
			}
		})
	}
}
