package backup

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/tracking"
)

func trackingRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := newRestoreFixture(t)
	if err := tracking.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	tracking.Register(f.handler.(*http.ServeMux), f.store)
	path := "/api/v1/projects/" + f.project + "/tracking"
	f.request(t, "PUT", path+"/milestones/v1", map[string]any{"milestone": map[string]any{"title": "First release"}})
	f.request(t, "PUT", path+"/items/P", map[string]any{"item": map[string]any{"title": "Parent", "milestone": "v1"}})
	f.request(t, "PUT", path+"/items/P.1", map[string]any{"item": map[string]any{"title": "Child", "parent": "P", "milestone": "v1"}})
	f.request(t, "POST", path+"/items/P.1/links", map[string]any{"add": []map[string]string{{"type": "pr", "ref": "12", "role": "delivers"}, {"type": "item", "ref": "P", "role": "related"}}})
	f.request(t, "PATCH", path+"/items/P.1", map[string]any{"changes": map[string]any{"status": "done"}})
	return f
}

func TestTrackingBundleRestoresRecordsAndHistory(t *testing.T) {
	f := trackingRestoreFixture(t)
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
	var revisions, links int
	if err = db.QueryRow(`SELECT count(*) FROM tracking_item_history WHERE project_id=? AND item_key='P.1'`, f.project).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM tracking_item_links WHERE project_id=? AND item_key='P.1'`, f.project).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if revisions != 3 || links != 2 {
		t.Fatalf("restored %d revisions and %d links", revisions, links)
	}
}

func TestTrackingBundleRejectsInconsistentRetainedState(t *testing.T) {
	for _, change := range []string{
		`DELETE FROM tracking_item_history WHERE revision=1`,
		`UPDATE tracking_items SET status='active' WHERE key='P.1'`,
		`UPDATE tracking_item_history SET record_json='{}' WHERE revision=1`,
		`DELETE FROM tracking_item_links WHERE target_type='pr'`,
		`INSERT INTO tracking_item_links(project_id,item_key,target_type,target_ref,role) SELECT project_id,key,'url','https://example.invalid/x','proof' FROM tracking_items WHERE key='P'`,
		`UPDATE tracking_milestones SET state='closed'`,
		`DROP INDEX tracking_items_parent`,
	} {
		t.Run(change, func(t *testing.T) {
			f := trackingRestoreFixture(t)
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, change)
			_, err := Inspect(t.Context(), bundle.Path)
			backupCode(t, err, "backup_invalid")
		})
	}
}
