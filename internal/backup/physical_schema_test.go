package backup

import (
	"database/sql"
	"path/filepath"
	"testing"
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
