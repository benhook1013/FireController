package backup

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAttemptPrimaryKeyRejectsRehashedIdentitySubstitution(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "source")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	for _, ensure := range []func(context.Context, *core.Store) error{jobs.EnsureSchema, inbox.EnsureSchema, reviews.EnsureSchema, reviewexec.EnsureSchema} {
		if err := ensure(t.Context(), store); err != nil {
			t.Fatal(err)
		}
	}
	f := &restoreFixture{dir: dir, store: store}
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("canonical baseline", err)
	}
	var attached string
	rows, err := store.DB().Query("SELECT sql FROM sqlite_schema WHERE tbl_name='review_execution_attempts' AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sql string
		if err := rows.Scan(&sql); err != nil {
			t.Fatal(err)
		}
		attached += "; " + sql
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var canonical string
	if err := f.store.DB().QueryRow("SELECT sql FROM sqlite_schema WHERE name='review_execution_attempts'").Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"integer-with-operation-unique", "blob-primary-key", "nocase-primary-key"} {
		t.Run(mode, func(t *testing.T) {
			altered := canonical
			switch mode {
			case "integer-with-operation-unique":
				altered = strings.Replace(altered, "id TEXT PRIMARY KEY", "id INTEGER PRIMARY KEY", 1)
				altered = strings.Replace(altered, "operation_id TEXT NOT NULL", "operation_id TEXT NOT NULL UNIQUE", 1)
			case "blob-primary-key":
				altered = strings.Replace(altered, "id TEXT PRIMARY KEY", "id BLOB PRIMARY KEY", 1)
			case "nocase-primary-key":
				altered = strings.Replace(altered, "id TEXT PRIMARY KEY", "id TEXT PRIMARY KEY COLLATE NOCASE", 1)
			}
			if altered == canonical {
				t.Fatal("fixture did not alter schema")
			}
			damaged := restoreModifiedBundle(t, bundle, "DROP TABLE review_execution_attempts; "+altered+attached)
			before := restoreTree(t, damaged)

			if _, err := Inspect(t.Context(), damaged); err == nil {
				t.Error("Inspect accepted substituted attempt identity")
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			if _, err := Restore(t.Context(), damaged, destination); err == nil {
				t.Error("Restore accepted substituted attempt identity")
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatal("rejected restore created destination", err)
			}
			if !reflect.DeepEqual(before, restoreTree(t, damaged)) {
				t.Fatal("validation changed source bundle")
			}
		})
	}
}
