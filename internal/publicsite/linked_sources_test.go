package publicsite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/projectmap"
)

func linkedID(prefix, digit string) string { return prefix + strings.Repeat(digit, 32) }

func installLinkedSourceFixture(t *testing.T, f *publicationFixture) (string, string) {
	t.Helper()
	if err := jobs.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	jobID := linkedID("job_", "a")
	mapID := linkedID("map_entry_", "b")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	state, _ := json.Marshal(projectmap.Entry{ID: mapID, ProjectID: f.project, Revision: 1, Kind: "workstream", Title: "Approved map title", Description: "Approved map narrative", State: "in progress", Where: "PRIVATE_WHERE", Next: "PRIVATE_NEXT", Phases: []projectmap.Phase{}, Links: []projectmap.Link{{Label: "PRIVATE_LINK", URL: "https://private.invalid"}}, CreatedAt: now, UpdatedAt: now, Position: 1})
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO jobs(project_id,id,title,brief,worker_id,status,revision,created_at,updated_at) VALUES(?,?,?,?,?,'active',2,?,?)`, []any{f.project, jobID, "PRIVATE_JOB_TITLE", "PRIVATE_BRIEF", f.worker, now, now}},
		{`INSERT INTO job_progress(project_id,job_id,job_revision,summary,next,blocker,evidence_json) VALUES(?,?,2,'','','','[]')`, []any{f.project, jobID}},
		{`INSERT INTO job_summary_history(project_id,job_id,revision,job_revision,title,body,actor_id,worker_id,created_at) VALUES(?,?,1,1,'Approved job title','Approved job body','principal_author',?,?),(?,?,2,2,'Later title','Later body','principal_author',?,?)`, []any{f.project, jobID, f.worker, now, f.project, jobID, f.worker, now}},
		{`INSERT INTO job_summaries(project_id,job_id,revision,job_revision,title,body,actor_id,worker_id,created_at) VALUES(?,?,2,2,'Later title','Later body','principal_author',?,?)`, []any{f.project, jobID, f.worker, now}},
		{`INSERT INTO project_map_entries(project_id,id,kind,title,state,archived,revision,state_json) VALUES(?,?,'workstream','PRIVATE_CURRENT_TITLE','private-current',0,1,?)`, []any{f.project, mapID, string(state)}},
		{`INSERT INTO project_map_entry_history(project_id,entry_id,revision,actor_id,state_json,created_at) VALUES(?,?,1,'principal_author',?,?)`, []any{f.project, mapID, string(state), now}},
	}
	for i, statement := range statements {
		if _, err := f.store.DB().Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("fixture statement %d: %v", i, err)
		}
	}
	return jobID, mapID
}

func resolveFixture(t *testing.T, f *publicationFixture, manifest Manifest) (Manifest, error) {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Manifest{}, err
	}
	defer tx.Rollback()
	out, err := resolveLinkedSources(context.Background(), tx, f.project, manifest)
	return out, err
}

func TestResolveLinkedSourcesUsesPinnedPublicProseAndLiveSafeState(t *testing.T) {
	f := newPublicationFixture(t)
	jobID, mapID := installLinkedSourceFixture(t, f)
	m := testPublicManifest()
	m.JobRefs = []JobRef{{Key: "approved_job", JobID: jobID, SummaryRevision: 1}}
	m.MapRefs = []MapRef{{Key: "approved_map", EntryID: mapID, Revision: 1}}
	m.WorkerRefs = []WorkerRef{{Key: "approved_worker", WorkerID: f.worker, Label: "Public worker label"}}
	resolved, err := resolveFixture(t, f, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Progress) != 1 || resolved.Progress[0].Title != "Approved job title" || resolved.Progress[0].Description != "Approved job body" || resolved.Progress[0].Status != "active" || len(resolved.Progress[0].Links) != 0 {
		t.Fatalf("job source did not retain pinned prose and live status: %#v", resolved.Progress)
	}
	if len(resolved.ProjectMap) != 1 || resolved.ProjectMap[0].Title != "Approved map title" || resolved.ProjectMap[0].Description != "Approved map narrative" || resolved.ProjectMap[0].Status != "in progress" || resolved.ProjectMap[0].Phase != "" || len(resolved.ProjectMap[0].Links) != 0 {
		t.Fatalf("map source exposed or changed selected narrative: %#v", resolved.ProjectMap)
	}
	if len(resolved.WorkerLanes) != 1 || resolved.WorkerLanes[0].Label != "Public worker label" || resolved.WorkerLanes[0].Headline != "Approved job title" || resolved.WorkerLanes[0].Status != "active" || len(resolved.WorkerLanes[0].Milestones) != 1 {
		t.Fatalf("worker source was not derived from selected assigned summaries: %#v", resolved.WorkerLanes)
	}
	if resolved.JobRefs != nil || resolved.MapRefs != nil || resolved.WorkerRefs != nil {
		t.Fatal("authenticated references remained in resolved manifest")
	}
	raw, _ := json.Marshal(resolved)
	for _, private := range []string{jobID, mapID, f.worker, "PRIVATE_JOB_TITLE", "PRIVATE_BRIEF", "PRIVATE_CURRENT_TITLE", "PRIVATE_WHERE", "PRIVATE_NEXT", "PRIVATE_LINK", "Later title", "Later body", "job_refs", "map_refs", "worker_refs"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("resolved public manifest exposed %q: %s", private, raw)
		}
	}
	if _, err = f.store.DB().Exec(`UPDATE jobs SET status='blocked',title='PRIVATE_CHANGED_TITLE',brief='PRIVATE_CHANGED_BRIEF' WHERE project_id=? AND id=?`, f.project, jobID); err != nil {
		t.Fatal(err)
	}
	changed, err := resolveFixture(t, f, m)
	if err != nil || changed.Progress[0].Title != "Approved job title" || changed.Progress[0].Description != "Approved job body" || changed.Progress[0].Status != "blocked" {
		t.Fatalf("private edit changed pinned prose or failed live status: %#v %v", changed.Progress, err)
	}
}

func TestResolveLinkedSourcesRejectsUnavailableSources(t *testing.T) {
	f := newPublicationFixture(t)
	jobID, mapID := installLinkedSourceFixture(t, f)
	otherWorker := linkedID("worker_", "c")
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,'Other worker')`, f.other, otherWorker); err != nil {
		t.Fatal(err)
	}
	base := testPublicManifest()
	cases := map[string]func() Manifest{
		"missing job": func() Manifest {
			m := base
			m.JobRefs = []JobRef{{Key: "x", JobID: linkedID("job_", "d"), SummaryRevision: 1}}
			return m
		},
		"foreign job": func() Manifest {
			m := base
			m.JobRefs = []JobRef{{Key: "x", JobID: linkedID("job_", "e"), SummaryRevision: 1}}
			return m
		},
		"cleared summary": func() Manifest {
			m := base
			m.JobRefs = []JobRef{{Key: "x", JobID: jobID, SummaryRevision: 3}}
			return m
		},
		"missing map": func() Manifest {
			m := base
			m.MapRefs = []MapRef{{Key: "x", EntryID: linkedID("map_entry_", "d"), Revision: 1}}
			return m
		},
		"foreign worker": func() Manifest {
			m := base
			m.WorkerRefs = []WorkerRef{{Key: "x", WorkerID: otherWorker, Label: "Other"}}
			return m
		},
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	foreignJob := linkedID("job_", "e")
	if _, err := f.store.DB().Exec(`INSERT INTO jobs(project_id,id,title,brief,status,revision,created_at,updated_at) VALUES(?,?, 'private','private','active',1,?,?);
 INSERT INTO job_summary_history(project_id,job_id,revision,job_revision,title,body,actor_id,worker_id,created_at) VALUES(?,?,1,1,'Foreign','Foreign','principal_author','',?);
 INSERT INTO job_summary_history(project_id,job_id,revision,job_revision,title,body,actor_id,worker_id,created_at) VALUES(?,?,3,2,'','','principal_author','',?)`, f.other, foreignJob, now, now, f.other, foreignJob, now, f.project, jobID, now); err != nil {
		t.Fatal(err)
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveFixture(t, f, manifest()); err == nil {
				t.Fatal("unavailable source accepted")
			}
		})
	}
	if _, err := f.store.DB().Exec(`UPDATE project_map_entry_history SET state_json='{"title":' WHERE project_id=? AND entry_id=?`, f.project, mapID); err != nil {
		t.Fatal(err)
	}
	malformed := base
	malformed.MapRefs = []MapRef{{Key: "x", EntryID: mapID, Revision: 1}}
	if _, err := resolveFixture(t, f, malformed); err == nil {
		t.Fatal("malformed retained map state accepted")
	}
}

func TestLinkedSummaryPublicationNormalizesLineEndingsWithoutChangingSource(t *testing.T) {
	for _, tc := range []struct{ name, body, public string }{
		{"crlf", "First paragraph.\r\n\r\n- A milestone\r\n- Tab\t雪", "First paragraph.\n\n- A milestone\n- Tab\t雪"},
		{"lone_cr", "First paragraph.\r\r- A milestone\r- Tab\t雪", "First paragraph.\n\n- A milestone\n- Tab\t雪"},
		{"lf_unicode_tabs", "First paragraph.\n\n- A milestone\n- Tab\t雪", "First paragraph.\n\n- A milestone\n- Tab\t雪"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			created := f.call(t, http.MethodPost, f.base()+"/jobs", f.admin, "summary-job", map[string]string{"title": "PRIVATE_JOB_TITLE", "brief": "PRIVATE_BRIEF"})
			var job struct {
				ID string `json:"id"`
			}
			if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &job) != nil || job.ID == "" {
				t.Fatal("job setup failed", created.Code)
			}
			path := f.base() + "/jobs/" + job.ID + "/summary"
			request := map[string]any{"expected_job_revision": 1, "expected_revision": 0, "title": "Approved summary", "body": tc.body}
			if w := f.call(t, http.MethodPut, path, f.admin, "original-summary", request); w.Code != 200 {
				t.Fatal("valid private line endings refused", w.Code, w.Body.String())
			}
			original := f.call(t, http.MethodGet, path+"/history/1", f.admin, "", nil)
			var source jobs.SummaryRevision
			if original.Code != 200 || json.Unmarshal(original.Body.Bytes(), &source) != nil || source.Body != tc.body || source.Revision != 1 {
				t.Fatal("summary handler changed original bytes/revision")
			}
			var stored []byte
			if err := f.store.DB().QueryRow(`SELECT CAST(body AS BLOB) FROM job_summary_history WHERE project_id=? AND job_id=? AND revision=1`, f.project, job.ID).Scan(&stored); err != nil || !bytes.Equal(stored, []byte(tc.body)) {
				t.Fatal("summary history did not retain exact authored bytes", err)
			}
			if strings.Contains(tc.body, "\r") {
				direct := testPublicManifest()
				direct.Progress = []Card{{Key: "direct", Title: "Approved summary", Description: tc.body, Links: []Link{}}}
				if _, err := NormalizeManifest(direct); err == nil {
					t.Fatal("linked-source normalization broadened direct public text validation")
				}
			}
			m := testPublicManifest()
			m.JobRefs = []JobRef{{Key: "approved-summary", JobID: job.ID, SummaryRevision: 1}}
			input := Approved{0, true, m}
			preview := f.prepare(t, input)
			if len(preview.Content.Progress) != 1 || preview.Content.Progress[0].Description != tc.public {
				t.Fatal("preview did not normalize only line endings", preview.Content.Progress)
			}
			applied := f.call(t, http.MethodPut, f.base()+"/publication", f.admin, "publish-summary", map[string]any{"expected_revision": 0, "enabled": true, "manifest": m, "preview_sha256": preview.SHA256})
			if applied.Code != 200 {
				t.Fatal("normalized preview could not apply", applied.Code, applied.Body.String())
			}
			initial := f.public(t)
			if len(initial.Content.Progress) != 1 || initial.Content.Progress[0].Description != tc.public {
				t.Fatal("published description differs from preview")
			}
			if err := validatePublicationSnapshot(t, f); err != nil {
				t.Fatal("canonical snapshot rejected normalized pinned summary", err)
			}
			f.refresh(t)
			if !reflect.DeepEqual(initial, f.public(t)) {
				t.Fatal("unchanged refresh changed normalized prose, revision or timestamp")
			}
			// A newer summary stays private; the selected revision remains authoritative.
			request["expected_revision"], request["title"], request["body"] = 1, "PRIVATE_LATER_TITLE", "PRIVATE_LATER_BODY\r\n"
			if w := f.call(t, http.MethodPut, path, f.admin, "later-summary", request); w.Code != 200 {
				t.Fatal("later private summary failed", w.Code)
			}
			if w := f.call(t, http.MethodPatch, f.base()+"/jobs/"+job.ID, f.admin, "refresh-job-status", map[string]any{"expected_revision": 1, "status": "blocked"}); w.Code != 200 {
				t.Fatal("job-triggered refresh failed", w.Code)
			}
			current := f.public(t)
			if len(current.Content.Progress) != 1 || current.Content.Progress[0].Description != tc.public || current.Content.Progress[0].Title != "Approved summary" || current.Content.Progress[0].Status != "blocked" {
				t.Fatal("refresh lost pinned prose or live status")
			}
			raw, _ := json.Marshal(current)
			for _, private := range []string{job.ID, f.worker, f.project, "PRIVATE_JOB_TITLE", "PRIVATE_BRIEF", "PRIVATE_LATER_TITLE", "PRIVATE_LATER_BODY", "job_refs", "summary_revision"} {
				if bytes.Contains(raw, []byte(private)) {
					t.Fatal("publication disclosed private source fields", private)
				}
			}
			after := f.call(t, http.MethodGet, path+"/history/1", f.admin, "", nil)
			if after.Code != 200 || !bytes.Equal(original.Body.Bytes(), after.Body.Bytes()) {
				t.Fatal("publication or refresh rewrote original history")
			}
			if err := f.store.DB().QueryRow(`SELECT CAST(body AS BLOB) FROM job_summary_history WHERE project_id=? AND job_id=? AND revision=1`, f.project, job.ID).Scan(&stored); err != nil || !bytes.Equal(stored, []byte(tc.body)) {
				t.Fatal("source bytes changed after publication/refresh", err)
			}
			if err := validatePublicationSnapshot(t, f); err != nil {
				t.Fatal("refreshed snapshot rejected original pinned revision", err)
			}
		})
	}
}
