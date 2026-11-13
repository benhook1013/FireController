package publicsite

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func validatePublicationSnapshot(t *testing.T, f *publicationFixture) error {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return ValidateSnapshot(t.Context(), tx)
}

func rewriteCurrentPublicContent(t *testing.T, f *publicationFixture, change func(*Content)) {
	t.Helper()
	var project, generation, raw string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT project_id,generation,public_revision FROM project_publications WHERE project_id=?`, f.project).Scan(&project, &generation, &revision); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT content_json FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision=?`, project, generation, revision).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var content Content
	if err := json.Unmarshal([]byte(raw), &content); err != nil {
		t.Fatal(err)
	}
	change(&content)
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`UPDATE public_project_snapshots SET content_json=? WHERE project_id=? AND generation=? AND revision=?`, string(encoded), project, generation, revision); err != nil {
		t.Fatal(err)
	}
}

func TestPublicLocationUsesPublishedAddressWithoutPrivateFields(t *testing.T) {
	f := newPublicationFixture(t)
	input := Approved{0, true, testPublicManifest()}
	if w := f.apply(t, input, "publish-location"); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, address := range []string{"one", f.project, "showcase"} {
		w := f.call(t, "GET", "/api/v1/public/project-locations/"+address, "", "", nil)
		var out map[string]any
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out) != 1 || out["slug"] != "showcase" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(address, w.Code, w.Body.String())
		}
	}
	for _, address := range []string{"two", f.other, "unpublished"} {
		w := f.call(t, "GET", "/api/v1/public/project-locations/"+address, "", "", nil)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), f.other) {
			t.Fatal("unpublished identity leaked", w.Code, w.Body.String())
		}
	}
	if w := f.call(t, "GET", "/api/v1/public/project-locations/one?private=1", "", "", nil); w.Code != 400 {
		t.Fatal("unexpected query admitted", w.Code)
	}
	if w := f.call(t, "POST", f.base()+"/publication/unpublish", f.admin, "unpublish-location", map[string]any{"expected_revision": 1}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, address := range []string{"one", f.project, "showcase"} {
		if w := f.call(t, "GET", "/api/v1/public/project-locations/"+address, "", "", nil); w.Code != 404 {
			t.Fatal("unpublished route retained", address, w.Code)
		}
	}
}

func TestLinkedJobWritesRefreshOnlyApprovedFacts(t *testing.T) {
	f := newPublicationFixture(t)
	job, _ := installLinkedSourceFixture(t, f)
	m := testPublicManifest()
	m.JobRefs = []JobRef{{Key: "selected-job", JobID: job, SummaryRevision: 1}}
	m.WorkerRefs = []WorkerRef{{Key: "selected-worker", WorkerID: f.worker, Label: "Builder"}}
	if w := f.apply(t, Approved{0, true, m}, "linked-publication"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	initial := f.public(t)
	hints, release, err := f.store.SubscribePublicProjection(f.project, "linked-write-proof")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	quiet := func() {
		t.Helper()
		if !reflect.DeepEqual(f.public(t), initial) {
			t.Fatal("private change altered public bytes, revision or time")
		}
		select {
		case <-hints:
			t.Fatal("private change emitted public activity")
		default:
		}
	}
	patch := func(id, key string, body any) {
		t.Helper()
		if w := f.call(t, "PATCH", f.base()+"/jobs/"+id, f.admin, key, body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	patch(job, "private-prose", map[string]any{"expected_revision": 2, "title": "PRIVATE_CHANGED_TITLE", "brief": "PRIVATE_CHANGED_BRIEF"})
	quiet()
	if w := f.call(t, "PUT", f.base()+"/jobs/"+job+"/summary", f.admin, "clear-later-summary", map[string]any{"expected_job_revision": 3, "expected_revision": 2, "title": "", "body": ""}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	quiet()
	w := f.call(t, "POST", f.base()+"/jobs", f.admin, "unselected-job", map[string]string{"title": "PRIVATE_UNSELECTED"})
	var created struct {
		ID string `json:"id"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	f.setFacts(nil, true)
	patch(created.ID, "unselected-status", map[string]any{"expected_revision": 1, "status": "parked"})
	quiet()
	readConfiguration := func() Configuration {
		t.Helper()
		w := f.call(t, "GET", f.base()+"/publication", f.admin, "", nil)
		var out Configuration
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return out
	}
	configuration := readConfiguration()
	if configuration.RefreshError != "" {
		t.Fatal("unselected job rebuilt optional publication", configuration.RefreshError)
	}
	f.setFacts(nil, false)
	patch(job, "selected-completed", map[string]any{"expected_revision": 3, "status": "completed"})
	next := f.public(t)
	if next.Revision != initial.Revision+1 || next.Generation != initial.Generation || next.Content.Progress[0].Title != "Approved job title" || next.Content.Progress[0].Description != "Approved job body" || next.Content.Progress[0].Status != "completed" || next.Content.WorkerLanes[0].Status != "completed" {
		t.Fatal("selected safe facts did not update while retaining approved prose", next)
	}
	select {
	case <-hints:
	default:
		t.Fatal("changed public facts lacked committed hint")
	}
	initial = next
	patch(job, "selected-completed", map[string]any{"expected_revision": 3, "status": "completed"})
	quiet()
	f.setFacts(nil, true)
	patch(job, "selected-projection-failure", map[string]any{"expected_revision": 4, "status": "blocked"})
	quiet()
	configuration = readConfiguration()
	if configuration.RefreshError != "projection_unavailable" {
		t.Fatal("projection failure was not retained privately", configuration.RefreshError)
	}
	if w := f.call(t, "GET", f.base()+"/jobs/"+job, f.admin, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"blocked"`) {
		t.Fatal("optional projection failure lost authoritative job update", w.Code)
	}
}

func TestPublicationCannotTakeAnotherProjectHandle(t *testing.T) {
	f := newPublicationFixture(t)
	m := testPublicManifest()
	m.Slug = "two"
	w := f.apply(t, Approved{0, true, m}, "handle-conflict")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "publication_slug_conflict") || strings.Contains(w.Body.String(), f.other) {
		t.Fatal(w.Code, w.Body.String())
	}
	m.Slug = "one"
	if w = f.apply(t, Approved{0, true, m}, "same-handle"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestLinkedPublicationSnapshotValidationAndStaleDynamicAllowlist(t *testing.T) {
	t.Run("current exact linked projection", func(t *testing.T) {
		f := newPublicationFixture(t)
		job, mapEntry := installLinkedSourceFixture(t, f)
		manifest := testPublicManifest()
		manifest.Progress = []Card{{Key: "manual", Title: "Manual title", Description: "Manual body", Status: "manual", Links: []Link{}}}
		manifest.JobRefs = []JobRef{{Key: "selected-job", JobID: job, SummaryRevision: 1}}
		manifest.MapRefs = []MapRef{{Key: "selected-map", EntryID: mapEntry, Revision: 1}}
		manifest.WorkerRefs = []WorkerRef{{Key: "selected-worker", WorkerID: f.worker, Label: "Public worker"}}
		if w := f.apply(t, Approved{0, true, manifest}, "linked-snapshot"); w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := validatePublicationSnapshot(t, f); err != nil {
			t.Fatal("valid linked publication rejected", err)
		}
	})

	t.Run("pinned and static corruption", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			change func(*Content)
		}{
			{"pinned summary", func(content *Content) { content.Progress[1].Title = "PRIVATE_CHANGED" }},
			{"manual card", func(content *Content) { content.Progress[0].Description = "PRIVATE_CHANGED" }},
			{"map narrative", func(content *Content) { content.ProjectMap[0].Description = "PRIVATE_CHANGED" }},
			{"worker label", func(content *Content) { content.WorkerLanes[0].Label = "PRIVATE_CHANGED" }},
		} {
			t.Run(test.name, func(t *testing.T) {
				f := newPublicationFixture(t)
				job, mapEntry := installLinkedSourceFixture(t, f)
				manifest := testPublicManifest()
				manifest.Progress = []Card{{Key: "manual", Title: "Manual title", Description: "Manual body", Links: []Link{}}}
				manifest.JobRefs = []JobRef{{Key: "selected-job", JobID: job, SummaryRevision: 1}}
				manifest.MapRefs = []MapRef{{Key: "selected-map", EntryID: mapEntry, Revision: 1}}
				manifest.WorkerRefs = []WorkerRef{{Key: "selected-worker", WorkerID: f.worker, Label: "Public worker"}}
				if w := f.apply(t, Approved{0, true, manifest}, "linked-corruption"); w.Code != http.StatusOK {
					t.Fatal(w.Code, w.Body.String())
				}
				rewriteCurrentPublicContent(t, f, test.change)
				if err := validatePublicationSnapshot(t, f); err == nil {
					t.Fatal("corrupted linked publication accepted")
				}
			})
		}
		t.Run("private source identity", func(t *testing.T) {
			f := newPublicationFixture(t)
			job, _ := installLinkedSourceFixture(t, f)
			manifest := testPublicManifest()
			manifest.JobRefs = []JobRef{{Key: "selected-job", JobID: job, SummaryRevision: 1}}
			if w := f.apply(t, Approved{0, true, manifest}, "linked-private-field"); w.Code != http.StatusOK {
				t.Fatal(w.Code, w.Body.String())
			}
			var generation, raw string
			var revision int64
			if err := f.store.DB().QueryRow(`SELECT generation,public_revision FROM project_publications WHERE project_id=?`, f.project).Scan(&generation, &revision); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB().QueryRow(`SELECT content_json FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision=?`, f.project, generation, revision).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			raw = strings.Replace(raw, `"progress":[{`, `"progress":[{"private_job_id":"`+job+`",`, 1)
			if _, err := f.store.DB().Exec(`UPDATE public_project_snapshots SET content_json=? WHERE project_id=? AND generation=? AND revision=?`, raw, f.project, generation, revision); err != nil {
				t.Fatal(err)
			}
			if err := validatePublicationSnapshot(t, f); err == nil {
				t.Fatal("private source identity accepted in anonymous content")
			}
		})
	})

	t.Run("refresh failure retains bounded prior dynamics", func(t *testing.T) {
		f := newPublicationFixture(t)
		job, _ := installLinkedSourceFixture(t, f)
		manifest := testPublicManifest()
		manifest.JobRefs = []JobRef{{Key: "selected-job", JobID: job, SummaryRevision: 1}}
		manifest.WorkerRefs = []WorkerRef{{Key: "selected-worker", WorkerID: f.worker, Label: "Public worker"}}
		if w := f.apply(t, Approved{0, true, manifest}, "linked-stale"); w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		f.setFacts(nil, true)
		if w := f.call(t, "PATCH", f.base()+"/jobs/"+job, f.admin, "linked-stale-change", map[string]any{"expected_revision": 2, "status": "blocked", "worker_id": ""}); w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := validatePublicationSnapshot(t, f); err != nil {
			t.Fatal("bounded stale last-good projection rejected", err)
		}
		rewriteCurrentPublicContent(t, f, func(content *Content) { content.Progress[0].Status = "PRIVATE_STATUS" })
		if err := validatePublicationSnapshot(t, f); err == nil {
			t.Fatal("noncanonical stale job status accepted")
		}
	})

	t.Run("missing exact retained source", func(t *testing.T) {
		f := newPublicationFixture(t)
		job, _ := installLinkedSourceFixture(t, f)
		manifest := testPublicManifest()
		manifest.JobRefs = []JobRef{{Key: "selected-job", JobID: job, SummaryRevision: 1}}
		if w := f.apply(t, Approved{0, true, manifest}, "linked-missing"); w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		if _, err := f.store.DB().Exec(`DELETE FROM job_summary_history WHERE project_id=? AND job_id=? AND revision=1`, f.project, job); err != nil {
			t.Fatal(err)
		}
		if err := validatePublicationSnapshot(t, f); err == nil {
			t.Fatal("missing pinned source accepted")
		}
	})
}
