package jobs_test

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestExactJobNotesIgnoreWorkerScopeAndContinueDefaultJobPage(t *testing.T) {
	a, project, token, firstWorker, jobPath := progressFixture(t)
	secondWorker := a.createWorker(project, "receiving-worker")
	jobID := strings.TrimPrefix(jobPath, "/api/v1/projects/"+project+"/jobs/")
	base := "/api/v1/projects/" + project + "/notes"
	noteOK(t, a.call("PATCH", jobPath, token, "exact-job-assign", map[string]any{"expected_revision": 1, "worker_id": firstWorker}), 200)
	workers := []string{firstWorker, secondWorker, ""}
	for i := 0; i < 8; i++ {
		created := noteCreate(t, a, base, token, workers[i%2], fmt.Sprint("exact-job-note-", i), workers[i%3], jobID, "proof", strings.Repeat("x", 260)+"PRIVATE_FULL_BODY")
		if i == 0 {
			noteOK(t, noteCall(a, "PATCH", base+"/"+created.ID, token, firstWorker, "consume-exact-note", map[string]any{"expected_revision": 1, "status": "consumed"}), 200)
		}
	}
	personal := noteCreate(t, a, base, token, firstWorker, "unrelated-personal-note", firstWorker, "", "", "Personal reminder outside the job")
	beforeWorker := noteList(t, a, base+"?worker_id="+firstWorker+"&include_assigned_jobs=true", token)
	if len(beforeWorker.Items) != 8 {
		t.Fatal("worker discovery did not expand assigned job before transfer", len(beforeWorker.Items))
	}
	beforeTransfer := readWorking(t, a, jobPath, token).Notes
	noteOK(t, a.call("PATCH", jobPath, token, "exact-job-transfer", map[string]any{"expected_revision": 2, "worker_id": secondWorker}), 200)
	before, lookups := noteCounts(t, a), a.lookups
	job := readWorking(t, a, jobPath, token)
	if len(job.Notes.Items) != 5 || !job.Notes.HasMore || job.Notes.Cursor == "" {
		t.Fatal("default job did not return its bounded pending-note page")
	}
	if !reflect.DeepEqual(beforeTransfer, job.Notes) {
		t.Fatal("job transfer changed the exact-job note selection or cursor")
	}
	for _, workerScope := range []string{"", "&worker_id=" + firstWorker, "&worker_id=" + firstWorker + "&include_assigned_jobs=true", "&worker_id=" + secondWorker, "&worker_id=worker-missing&include_assigned_jobs=true"} {
		t.Run(workerScope, func(t *testing.T) {
			query := base + "?job_id=" + jobID + "&limit=5" + workerScope
			first := noteList(t, a, query, token)
			if !reflect.DeepEqual(first, job.Notes) {
				t.Fatal("exact-job selection or signed cursor depended on worker scope")
			}
			rest := noteList(t, a, query+"&cursor="+url.QueryEscape(job.Notes.Cursor), token)
			if len(rest.Items) != 2 || rest.HasMore || rest.Cursor != "" {
				t.Fatal("default job cursor did not continue all exact-job notes", rest)
			}
			seen := map[string]bool{}
			for _, item := range append(first.Items, rest.Items...) {
				if seen[item.ID] || item.JobID != jobID || item.Status != "pending" || item.ID == personal.ID || item.Body != "" || !item.BodyTruncated || strings.Contains(item.BodyPreview, "PRIVATE_FULL_BODY") {
					t.Fatal("exact-job page lost scope, completeness or private-body bounds", item)
				}
				seen[item.ID] = true
			}
		})
	}
	// Worker discovery still follows direct targets and current assignments.
	afterWorker := noteList(t, a, base+"?worker_id="+firstWorker+"&include_assigned_jobs=true", token)
	if len(afterWorker.Items) != 3 {
		t.Fatal("exact-job selection changed ordinary worker discovery", len(afterWorker.Items))
	}
	if noteCounts(t, a) != before || a.lookups != lookups {
		t.Fatal("exact-job reads changed durable records or contacted the provider")
	}
}

func TestExactJobNotesKeepStatusFilterCursorAndAuthorizationBoundaries(t *testing.T) {
	a, project, token, worker, jobPath := progressFixture(t)
	jobID := strings.TrimPrefix(jobPath, "/api/v1/projects/"+project+"/jobs/")
	base := "/api/v1/projects/" + project + "/notes"
	otherReader := a.grant(project, "exact-job-other-reader", "read")
	otherProject := a.createProject("exact-job-other-project", "owner/exact-job-other-project")
	foreignToken := a.grant(otherProject, "exact-job-foreign-token", "read")
	for i := 0; i < 3; i++ {
		created := noteCreate(t, a, base, token, worker, fmt.Sprint("exact-boundary-", i), worker, jobID, "proof", fmt.Sprint("Selected note ", i))
		if i == 0 {
			noteOK(t, noteCall(a, "PATCH", base+"/"+created.ID, token, worker, "exact-boundary-consume", map[string]any{"expected_revision": 1, "status": "consumed"}), 200)
		}
	}
	before, lookups := noteCounts(t, a), a.lookups
	query := base + "?job_id=" + jobID
	pending := noteList(t, a, query+"&limit=1&worker_id="+worker+"&include_assigned_jobs=true", token)
	if len(pending.Items) != 1 || !pending.HasMore {
		t.Fatal("pending exact-job page was not bounded", pending)
	}
	all := noteList(t, a, query+"&status=all", token)
	if len(all.Items) != 3 {
		t.Fatal("all status did not include consumed notes", all)
	}
	consumed := noteList(t, a, query+"&status=consumed&phase=proof&kind=reminder", token)
	if len(consumed.Items) != 1 || consumed.Items[0].Status != "consumed" {
		t.Fatal("exact-job selection lost remaining explicit filters", consumed)
	}
	cursor := "&cursor=" + url.QueryEscape(pending.Cursor)
	for _, changed := range []string{base + "?worker_id=" + worker, base + "?job_id=job-missing", query + "&status=all", query + "&phase=proof", query + "&kind=source"} {
		noteError(t, noteCall(a, "GET", changed+cursor, token, worker, "", nil), 400, "validation_failed")
	}
	noteError(t, noteCall(a, "GET", query+cursor, otherReader, worker, "", nil), 400, "validation_failed")
	noteError(t, noteCall(a, "GET", query+cursor, foreignToken, "", "", nil), 403, "forbidden")
	foreignBase := "/api/v1/projects/" + otherProject + "/notes?job_id=" + jobID
	if page := noteList(t, a, foreignBase, foreignToken); len(page.Items) != 0 {
		t.Fatal("exact-job lookup exposed another project's notes", page)
	}
	for _, invalid := range []string{"&worker_id=", "&worker_id=../bad", "&include_assigned_jobs=bad", "&worker_id=" + worker + "&worker_id=" + worker} {
		noteError(t, noteCall(a, "GET", query+invalid, token, worker, "", nil), 400, "invalid_query")
	}
	noteOK(t, noteCall(a, "GET", query+"&limit=101", token, worker, "", nil), 400)
	if noteCounts(t, a) != before || a.lookups != lookups {
		t.Fatal("scoped reads changed durable records or made provider calls")
	}
}
