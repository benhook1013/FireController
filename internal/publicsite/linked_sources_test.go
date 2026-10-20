package publicsite

import (
	"context"
	"database/sql"
	"encoding/json"
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
