package backup

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/presentation"
)

func presentationRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t)
	if err := presentation.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	presentation.Register(f.handler.(*http.ServeMux), f.store)
	path := "/api/v1/projects/" + f.project + "/presentation"
	f.request(t, "PUT", path, map[string]any{"expected_revision": 0, "presentation": map[string]any{"name": "First project name", "brand_name": "Private brand", "headings": map[string]string{"map": "Private roadmap"}}})
	f.request(t, "PUT", path, map[string]any{"expected_revision": 1, "presentation": map[string]any{"name": "Current project name"}})
	return f
}

func TestPresentationBundlePreservesNameAndFullHistory(t *testing.T) {
	f := presentationRestoreFixture(t)
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("inspection changed bundle")
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
	var name, currentRaw string
	if err = db.QueryRow(`SELECT p.name,c.state_json FROM projects p JOIN project_presentations c ON c.project_id=p.id WHERE p.id=?`, f.project).Scan(&name, &currentRaw); err != nil {
		t.Fatal(err)
	}
	var current presentation.State
	if json.Unmarshal([]byte(currentRaw), &current) != nil || name != "Current project name" || current.Name != name || current.Revision != 2 || current.BrandName != "FireController" || len(current.Headings) != 0 {
		t.Fatal("current settings/name lost", name, current)
	}
	var originalRaw, latestRaw string
	if err = db.QueryRow(`SELECT state_json FROM project_presentation_history WHERE project_id=? AND revision=1`, f.project).Scan(&originalRaw); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT state_json FROM project_presentation_history WHERE project_id=? AND revision=2`, f.project).Scan(&latestRaw); err != nil {
		t.Fatal(err)
	}
	var original presentation.State
	if json.Unmarshal([]byte(originalRaw), &original) != nil || original.Name != "First project name" || original.BrandName != "Private brand" || original.Headings["map"] != "Private roadmap" || latestRaw != currentRaw {
		t.Fatal("captured history changed", original)
	}
}

func TestPresentationBundleRejectsResealedBindingCorruption(t *testing.T) {
	for _, change := range []string{`UPDATE projects SET name='Changed outside presentation'`, `DELETE FROM project_presentation_history WHERE revision=1`} {
		t.Run(change, func(t *testing.T) {
			f := presentationRestoreFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, change)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}
