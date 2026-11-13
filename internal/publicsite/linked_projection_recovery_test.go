package publicsite

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// Only deliberately pinned prose may become public, even when source records
// change between preview/apply and exact recovery happens after withdrawal.
func TestLinkedProjectionPrivateEditsAndWithdrawnReplay(t *testing.T) {
	f := newPublicationFixture(t)
	write := func(method, path, key string, input any) []byte {
		t.Helper()
		w := f.call(t, method, path, f.admin, key, input)
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("synthetic write failed: status=%d", w.Code)
		}
		return append([]byte(nil), w.Body.Bytes()...)
	}
	var job struct{ ID string }
	if json.Unmarshal(write("POST", f.base()+"/jobs", "linked-recovery-create", map[string]any{"title": "PRIVATE original title", "brief": "PRIVATE original brief", "worker_id": f.worker}), &job) != nil || job.ID == "" {
		t.Fatal("job setup failed")
	}
	path := f.base() + "/jobs/" + job.ID
	write("PUT", path+"/summary", "linked-recovery-summary", map[string]any{"expected_job_revision": 1, "expected_revision": 0, "title": "Approved headline", "body": "Approved narrative"})
	input := Approved{Enabled: true, Manifest: testPublicManifest()}
	input.Manifest.JobRefs = []JobRef{{Key: "selected_job", JobID: job.ID, SummaryRevision: 1}}
	input.Manifest.WorkerRefs = []WorkerRef{{Key: "selected_worker", WorkerID: f.worker, Label: "Approved worker"}}
	preview := f.prepare(t, input)
	write("PATCH", path, "linked-recovery-private-edit", map[string]any{"expected_revision": 1, "title": "PRIVATE newer title", "brief": "PRIVATE newer brief", "progress": map[string]any{"summary": "PRIVATE progress"}})
	write("PUT", path+"/summary", "linked-recovery-new-summary", map[string]any{"expected_job_revision": 2, "expected_revision": 1, "title": "PRIVATE unapproved summary", "body": "PRIVATE unapproved narrative"})
	apply := map[string]any{"expected_revision": 0, "enabled": true, "manifest": input.Manifest, "preview_sha256": preview.SHA256}
	original := write("PUT", f.base()+"/publication", "linked-recovery-apply", apply)
	first := f.public(t)
	assertApproved := func(snapshot Snapshot) {
		t.Helper()
		encoded, err := json.Marshal(snapshot)
		if err != nil || bytes.Contains(encoded, []byte("PRIVATE")) || bytes.Contains(encoded, []byte(job.ID)) || bytes.Contains(encoded, []byte(f.worker)) {
			t.Fatal("private source leaked into public projection")
		}
		if len(snapshot.Content.Progress) != 1 || snapshot.Content.Progress[0].Title != "Approved headline" || snapshot.Content.Progress[0].Description != "Approved narrative" {
			t.Fatal("pinned approved source was replaced")
		}
	}
	assertApproved(first)
	write("PATCH", path, "linked-recovery-private-only", map[string]any{"expected_revision": 2, "brief": "PRIVATE latest brief"})
	write("PUT", path+"/summary", "linked-recovery-summary-clear", map[string]any{"expected_job_revision": 3, "expected_revision": 2, "title": "", "body": ""})
	if after := f.public(t); !reflect.DeepEqual(after, first) {
		t.Fatal("private edit or current-summary clear changed approved public state or freshness")
	}
	write("PATCH", path, "linked-recovery-assignment", map[string]any{"expected_revision": 3, "worker_id": "", "status": "blocked"})
	after := f.public(t)
	assertApproved(after)
	if after.Revision != first.Revision+1 || after.Content.Progress[0].Status != "blocked" || len(after.Content.WorkerLanes) != 1 || after.Content.WorkerLanes[0].Status != "idle" || after.Content.WorkerLanes[0].Headline != "" || len(after.Content.WorkerLanes[0].Milestones) != 0 {
		t.Fatal("approved live status/assignment projection was not updated atomically")
	}
	write("POST", f.base()+"/publication/unpublish", "linked-recovery-withdraw", map[string]any{"expected_revision": 1})
	if replay := write("PUT", f.base()+"/publication", "linked-recovery-apply", apply); !bytes.Equal(replay, original) {
		t.Fatal("withdrawn exact replay did not recover original receipt")
	}
	if w := f.call(t, "GET", "/api/v1/public/projects/showcase", "", "", nil); w.Code != http.StatusNotFound {
		t.Fatal("historical apply recovery republished withdrawn content")
	}
}
