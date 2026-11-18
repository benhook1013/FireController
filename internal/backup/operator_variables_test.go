package backup

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/opvars"
)

func operatorVariableRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t)
	if err := opvars.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	opvars.Register(f.handler.(*http.ServeMux), f.store)
	base := "/api/v1/projects/" + f.project + "/vars/provider_mode"
	f.request(t, "PUT", base, map[string]any{"scope": "instance", "value": "both"})
	f.request(t, "PUT", base, map[string]any{"scope": "instance", "value": "codex-low"})
	f.request(t, "PUT", base, map[string]any{"scope": "project", "value": "claude-low"})
	f.request(t, "PUT", base, map[string]any{"scope": "project", "value": nil})
	return f
}

func TestOperatorVariableBundlePreservesHistory(t *testing.T) {
	f := operatorVariableRestoreFixture(t)
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(t.Context(), bundle.Path, target); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var instance, project, unset int
	if err = db.QueryRow(`SELECT (SELECT count(*) FROM operator_variable_history WHERE project_id=''),(SELECT count(*) FROM operator_variable_history WHERE project_id=?),(SELECT count(*) FROM operator_variable_history WHERE value IS NULL)`, f.project).Scan(&instance, &project, &unset); err != nil {
		t.Fatal(err)
	}
	if instance != 2 || project != 2 || unset != 1 {
		t.Fatal("history changed", instance, project, unset)
	}
}

func TestOperatorVariableBundleRejectsCorruption(t *testing.T) {
	for _, change := range []string{
		`DELETE FROM operator_variable_history WHERE project_id='' AND revision=1`,
		`UPDATE operator_variable_history SET value='Bad Value ' WHERE project_id='' AND revision=2`,
		`UPDATE operator_variable_history SET key='Upper' WHERE project_id=''`,
		`UPDATE operator_variable_history SET project_id='missing-project' WHERE project_id<>''`,
		`UPDATE operator_variable_history SET value=NULL WHERE project_id='' AND revision=1`,
	} {
		t.Run(change, func(t *testing.T) {
			f := operatorVariableRestoreFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, change)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}
