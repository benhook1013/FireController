package backup

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/projectmap"
)

func mapRestoreFixture(t *testing.T) (*restoreFixture, string, string) {
	t.Helper()
	f := newRestoreFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	projectmap.Register(f.handler.(*http.ServeMux), f.store)
	path := "/api/v1/projects/" + f.project
	entry := projectmap.Editable{Kind: "workstream", Title: "Original editorial title", State: "Ready when needed", Description: "Private map description", Where: "A retained position", Next: "Next milestone", Phases: []projectmap.Phase{{ID: "foundation", Name: "Original phase", State: "Waiting"}}, Links: []projectmap.Link{{Label: "Source", URL: "https://example.invalid/source"}}}
	created := f.request(t, "POST", path+"/map/entries", entry)
	id := created["id"].(string)
	var job string
	if err := f.store.DB().QueryRow(`SELECT id FROM jobs WHERE project_id=? LIMIT 1`, f.project).Scan(&job); err != nil {
		t.Fatal(err)
	}
	f.request(t, "PUT", path+"/jobs/"+job+"/workstream", map[string]any{"expected_revision": 0, "workstream_id": id})
	entry.Title = "Renamed editorial title"
	entry.Description = "Changed description must not rewrite original history"
	entry.Phases[0].Name = "Renamed phase"
	entry.Phases[0].Archived = true
	entry.Archived = true
	f.request(t, "PUT", path+"/map/entries/"+id, map[string]any{"expected_revision": 1, "entry": entry})
	return f, id, job
}

func TestProjectMapBundleRetainsEditorialHistoryAndJobAssociation(t *testing.T) {
	f, id, job := mapRestoreFixture(t)
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("map inspection mutated the bundle")
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
	var first, last projectmap.Entry
	for rev, dest := range map[int]*projectmap.Entry{1: &first, 2: &last} {
		var raw string
		if err = db.QueryRow(`SELECT state_json FROM project_map_entry_history WHERE project_id=? AND entry_id=? AND revision=?`, f.project, id, rev).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(raw), dest); err != nil {
			t.Fatal(err)
		}
	}
	if first.Title != "Original editorial title" || first.Description != "Private map description" || first.Phases[0].Name != "Original phase" || first.Archived || last.Title != "Renamed editorial title" || !last.Archived || !last.Phases[0].Archived {
		t.Fatal("restore lost complete editorial snapshots")
	}
	var retainedID, title string
	var revision int
	if err = db.QueryRow(`SELECT workstream_id,workstream_title,revision FROM job_workstreams WHERE project_id=? AND job_id=?`, f.project, job).Scan(&retainedID, &title, &revision); err != nil {
		t.Fatal(err)
	}
	if retainedID != id || title != "Original editorial title" || revision != 1 {
		t.Fatal("rename/archive rewrote retained association meaning")
	}
}

func TestProjectMapBundleRejectsInconsistentRetainedState(t *testing.T) {
	for _, change := range []string{
		`DELETE FROM project_map_entry_history WHERE revision=1`,
		`UPDATE project_map_entries SET title='Scalar disagrees with full snapshot'`,
		`UPDATE project_map_entry_history SET state_json='{}' WHERE revision=1`,
		`UPDATE job_workstreams SET workstream_title='Current disagrees with captured title'`,
		`DELETE FROM job_workstream_history`,
		`UPDATE job_workstream_history SET job_revision=99999`,
		`UPDATE project_map_entry_history SET revision=4 WHERE revision=1`,
	} {
		t.Run(change, func(t *testing.T) {
			f, _, _ := mapRestoreFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, change)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}
