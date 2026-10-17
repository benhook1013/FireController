package projectmap_test

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/projectmap"
)

func (f *fixture) job() string {
	w := f.call("POST", "/api/v1/projects/"+f.project+"/jobs", f.token, "job", map[string]string{"title": "Job", "brief": "immutable job brief", "worker_id": f.worker})
	expect(f.t, w, 201)
	var r projectmap.Receipt
	decode(f.t, w, &r)
	return r.ID
}
func (f *fixture) jobPath(job string) string { return "/api/v1/projects/" + f.project + "/jobs/" + job }
func (f *fixture) association(job string) projectmap.JobWorkstream {
	w := f.call("GET", f.jobPath(job)+"/workstream", f.reader, "", nil)
	expect(f.t, w, 200)
	var a projectmap.JobWorkstream
	decode(f.t, w, &a)
	return a
}
func (f *fixture) set(job, key string, rev int64, workstream string) projectmap.JobReceipt {
	w := f.call("PUT", f.jobPath(job)+"/workstream", f.token, key, map[string]any{"expected_revision": rev, "workstream_id": workstream})
	expect(f.t, w, 200)
	var r projectmap.JobReceipt
	decode(f.t, w, &r)
	return r
}
func TestJobWorkstreamIndependentRevisionCapturedMeaningAndReplay(t *testing.T) {
	f := setup(t)
	job := f.job()
	beforeJob := f.call("GET", f.jobPath(job), f.reader, "", nil)
	expect(t, beforeJob, 200)
	initial := f.association(job)
	if initial != (projectmap.JobWorkstream{JobID: job}) {
		t.Fatal(initial)
	}
	counts := f.counts()
	clear := f.set(job, "initial-clear", 0, "")
	if clear != (projectmap.JobReceipt{JobID: job, Revision: 0, Outcome: "unchanged"}) {
		t.Fatal(clear)
	}
	after := f.counts()
	if !reflect.DeepEqual(counts[:5], after[:5]) || after[5] != counts[5]+1 {
		t.Fatal(counts, after)
	}
	entry := f.create("stream", workstream())
	selectBody := map[string]any{"expected_revision": 0, "workstream_id": entry.ID}
	selected := f.call("PUT", f.jobPath(job)+"/workstream", f.token, "select", selectBody)
	expect(t, selected, 200)
	if f.association(job).Revision != 1 {
		t.Fatal("association not selected")
	}
	// Editorial changes never rewrite captured title or association revision.
	v := workstream()
	v.Title = "new title"
	v.Archived = true
	expect(t, f.replace(entry.ID, "rename", 1, v), 200)
	current := f.association(job)
	if current.WorkstreamTitle != "Named stream" || current.WorkstreamID != entry.ID || current.Revision != 1 {
		t.Fatal(current)
	}
	unchanged := f.set(job, "unchanged-archived", 1, entry.ID)
	if unchanged.Revision != 1 || unchanged.Outcome != "unchanged" {
		t.Fatal(unchanged)
	}
	cleared := f.set(job, "clear", 1, "")
	if cleared.Revision != 2 || cleared.Outcome != "updated" {
		t.Fatal(cleared)
	}
	// A different new selection of the archived stream is refused.
	expect(t, f.call("PUT", f.jobPath(job)+"/workstream", f.token, "archived", map[string]any{"expected_revision": 2, "workstream_id": entry.ID}), 400)
	v.Archived = false
	expect(t, f.replace(entry.ID, "restore", 2, v), 200)
	f.set(job, "new-selection", 2, entry.ID)
	latest := f.association(job)
	if latest.WorkstreamTitle != "new title" || latest.Revision != 3 {
		t.Fatal(latest)
	}
	history := f.call("GET", f.jobPath(job)+"/workstream/history?limit=2", f.reader, "", nil)
	expect(t, history, 200)
	var p projectmap.Page[projectmap.JobWorkstreamHistory]
	decode(t, history, &p)
	if len(p.Items) != 2 || p.Items[0].Revision != 3 || p.Items[0].WorkstreamTitle != "new title" || p.Items[1].WorkstreamID != "" || !p.HasMore {
		t.Fatal(p)
	}
	for _, h := range p.Items {
		if h.JobRevision != 1 || h.ActorID == "" {
			t.Fatal(h)
		}
	}
	tail := f.call("GET", f.jobPath(job)+"/workstream/history?limit=2&cursor="+url.QueryEscape(p.Cursor), f.reader, "", nil)
	expect(t, tail, 200)
	p = projectmap.Page[projectmap.JobWorkstreamHistory]{}
	decode(t, tail, &p)
	if len(p.Items) != 1 || p.Items[0].WorkstreamTitle != "Named stream" || p.Items[0].Revision != 1 || p.HasMore {
		t.Fatal(p)
	}
	counts = f.counts()
	replay := f.call("PUT", f.jobPath(job)+"/workstream", f.token, "select", selectBody)
	expect(t, replay, 200)
	if replay.Body.String() != selected.Body.String() || !reflect.DeepEqual(counts, f.counts()) {
		t.Fatal("replay changed association")
	}
	stale := f.call("PUT", f.jobPath(job)+"/workstream", f.token, "stale", map[string]any{"expected_revision": 0, "workstream_id": ""})
	expect(t, stale, 409)
	if !strings.Contains(stale.Body.String(), `"job_workstream_revision_conflict"`) || !strings.Contains(stale.Body.String(), `"current_revision":3`) {
		t.Fatal(stale.Body.String())
	}
	afterJob := f.call("GET", f.jobPath(job), f.reader, "", nil)
	expect(t, afterJob, 200)
	if beforeJob.Body.String() != afterJob.Body.String() {
		t.Fatal("map mutated job", beforeJob.Body, afterJob.Body)
	}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestJobWorkstreamScopeBadTargetsAndAtomicity(t *testing.T) {
	f := setup(t)
	job := f.job()
	base := f.jobPath(job) + "/workstream"
	v := workstream()
	v.Kind = "return_point"
	v.Trigger = "later"
	v.Where = ""
	v.Next = ""
	v.Phases = nil
	rp := f.create("return", v)
	for name, body := range map[string]any{
		"null": map[string]any{"expected_revision": nil, "workstream_id": ""}, "omitted": map[string]any{"expected_revision": 0}, "null-target": map[string]any{"expected_revision": 0, "workstream_id": nil}, "negative": map[string]any{"expected_revision": -1, "workstream_id": ""}, "return": map[string]any{"expected_revision": 0, "workstream_id": rp.ID}, "extra": map[string]any{"expected_revision": 0, "workstream_id": "", "job_revision": 1},
	} {
		expect(t, f.call("PUT", base, f.token, name, body), 400)
	}
	expect(t, f.call("PUT", base, f.token, "missing", map[string]any{"expected_revision": 0, "workstream_id": "map_entry_00000000000000000000000000000000"}), 404)
	expect(t, f.call("PUT", base, f.reader, "reader", map[string]any{"expected_revision": 0, "workstream_id": ""}), 403)
	stale := f.call("PUT", base, f.token, "stale", map[string]any{"expected_revision": 1, "workstream_id": ""})
	expect(t, stale, 409)
	if !strings.Contains(stale.Body.String(), `"current_revision":0`) {
		t.Fatal(stale.Body.String())
	}
	// Same credential can access both projects, but a foreign ID is still scoped404.
	f.worker = ""
	otherBase := "/api/v1/projects/" + f.other + "/map/entries"
	w := f.call("POST", otherBase, f.admin, "foreign-stream", workstream())
	expect(t, w, 201)
	var foreign projectmap.Receipt
	decode(t, w, &foreign)
	expect(t, f.call("PUT", base, f.token, "foreign", map[string]any{"expected_revision": 0, "workstream_id": foreign.ID}), 404)
	expect(t, f.call("GET", "/api/v1/projects/"+f.other+"/jobs/"+job+"/workstream", f.admin, "", nil), 404)
	current := f.create("valid", workstream())
	counts := f.counts()
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_association_event BEFORE INSERT ON events WHEN NEW.event_type='job.workstream_changed' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	expect(t, f.call("PUT", base, f.token, "rollback", map[string]any{"expected_revision": 0, "workstream_id": current.ID}), 500)
	if !reflect.DeepEqual(counts, f.counts()) || f.association(job).Revision != 0 {
		t.Fatal("partial association")
	}
	// Current/history reads are effect-free; empty history is [] and no fabricated0 entry.
	w = f.call("GET", base+"/history", f.reader, "", nil)
	expect(t, w, 200)
	var p projectmap.Page[projectmap.JobWorkstreamHistory]
	decode(t, w, &p)
	if p.Items == nil || len(p.Items) != 0 || p.HasMore || p.Cursor != "" {
		t.Fatal(p)
	}
	if !reflect.DeepEqual(counts, f.counts()) {
		t.Fatal("reads changed state")
	}
}

func TestMapSnapshotRejectsDamagedLogicalHistory(t *testing.T) {
	cases := map[string]func(*fixture, string, string){
		"scalar": func(f *fixture, id, job string) {
			_, err := f.store.DB().Exec(`UPDATE project_map_entries SET title='changed' WHERE id=?`, id)
			if err != nil {
				t.Fatal(err)
			}
		},
		"missinghistory": func(f *fixture, id, job string) {
			_, err := f.store.DB().Exec(`DELETE FROM project_map_entry_history WHERE entry_id=? AND revision=1`, id)
			if err != nil {
				t.Fatal(err)
			}
		},
		"noncanonical": func(f *fixture, id, job string) {
			_, err := f.store.DB().Exec(`UPDATE project_map_entry_history SET state_json=state_json||' ' WHERE entry_id=? AND revision=1`, id)
			if err != nil {
				t.Fatal(err)
			}
		},
		"missingphase": func(f *fixture, id, job string) {
			var raw string
			if err := f.store.DB().QueryRow(`SELECT state_json FROM project_map_entry_history WHERE entry_id=? AND revision=1`, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var e projectmap.Entry
			if err := json.Unmarshal([]byte(raw), &e); err != nil {
				t.Fatal(err)
			}
			e.Phases = append(e.Phases, projectmap.Phase{ID: "retained_extra", Name: "Missing from later revision"})
			data, _ := json.Marshal(e)
			if _, err := f.store.DB().Exec(`UPDATE project_map_entry_history SET state_json=? WHERE entry_id=? AND revision=1`, string(data), id); err != nil {
				t.Fatal(err)
			}
		},
		"associationtitle": func(f *fixture, id, job string) {
			if _, err := f.store.DB().Exec(`UPDATE job_workstream_history SET workstream_title='changed' WHERE job_id=?`, job); err != nil {
				t.Fatal(err)
			}
		},
		"associationgap": func(f *fixture, id, job string) {
			if _, err := f.store.DB().Exec(`DELETE FROM job_workstream_history WHERE job_id=?`, job); err != nil {
				t.Fatal(err)
			}
		},
		"jobfuture": func(f *fixture, id, job string) {
			if _, err := f.store.DB().Exec(`UPDATE job_workstream_history SET job_revision=500 WHERE job_id=?`, job); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			id := f.create("original", workstream()).ID
			v := workstream()
			v.Title = "renamed"
			expect(t, f.replace(id, "rename", 1, v), 200)
			job := f.job()
			f.set(job, "select", 0, id)
			if err := f.validate(); err != nil {
				t.Fatal("valid fixture", err)
			}
			damage(f, id, job)
			if err := f.validate(); err == nil {
				t.Fatal("damaged logical snapshot accepted")
			}
		})
	}
}
