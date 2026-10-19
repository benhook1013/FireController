package jobs_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type deliveryWorkerValue struct {
	ID                   string `json:"id"`
	ProjectID            string `json:"project_id"`
	Name                 string `json:"name"`
	PrimaryJobID         string `json:"primary_job_id"`
	Paused               bool   `json:"paused"`
	PauseReason          string `json:"pause_reason"`
	PauseReasonTruncated bool   `json:"pause_reason_truncated"`
}

type deliveryCheckpointValue struct {
	Sequence         int64  `json:"sequence"`
	JobRevision      int64  `json:"job_revision"`
	Done             string `json:"done"`
	Next             string `json:"next"`
	Blocker          string `json:"blocker"`
	DoneTruncated    bool   `json:"done_truncated"`
	NextTruncated    bool   `json:"next_truncated"`
	BlockerTruncated bool   `json:"blocker_truncated"`
	CreatedAt        string `json:"created_at"`
}

type deliveryUpdateValue struct {
	Sequence      int64  `json:"sequence"`
	JobRevision   int64  `json:"job_revision"`
	Body          string `json:"body"`
	BodyTruncated bool   `json:"body_truncated"`
	CreatedAt     string `json:"created_at"`
}
type deliveryChecklistValue struct {
	Revision    int64  `json:"revision"`
	JobRevision int64  `json:"job_revision"`
	Total       int    `json:"total"`
	Done        int    `json:"done"`
	CreatedAt   string `json:"created_at"`
}
type deliveryJobValue struct {
	BriefPreview          string                   `json:"brief_preview"`
	BriefPreviewTruncated bool                     `json:"brief_preview_truncated"`
	UpdatedAt             string                   `json:"updated_at"`
	Update                *deliveryUpdateValue     `json:"update"`
	Checklist             *deliveryChecklistValue  `json:"checklist"`
	ID                    string                   `json:"id"`
	Title                 string                   `json:"title"`
	WorkerID              string                   `json:"worker_id"`
	WorkerName            string                   `json:"worker_name"`
	Status                string                   `json:"status"`
	Revision              int64                    `json:"revision"`
	Checkpoint            *deliveryCheckpointValue `json:"checkpoint"`
}

type deliveryPageValue[T any] struct {
	Items   []T    `json:"items"`
	Cursor  string `json:"cursor"`
	HasMore bool   `json:"has_more"`
}

type deliveryValue struct {
	Workers deliveryPageValue[deliveryWorkerValue] `json:"workers"`
	Jobs    deliveryPageValue[deliveryJobValue]    `json:"jobs"`
}

func TestDeliveryReadIsBoundedPrivateAndIncludesLatestCheckpointSummary(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("delivery-read", "synthetic/delivery-read")
	foreignProject := a.createProject("delivery-other", "synthetic/delivery-other")
	token := a.grant(project, "delivery-read-client", "read", "write")
	readOnly := a.grant(project, "delivery-read-only", "read")
	worker := a.createWorker(project, "Builder")
	otherWorker := a.createWorker(project, "Reviewer")
	foreignWorker := a.createWorker(foreignProject, "Foreign")
	createJob := func(title, workerID string) string {
		t.Helper()
		r := a.call(http.MethodPost, "/api/v1/projects/"+project+"/jobs", token, "delivery-create-"+title, map[string]any{
			"title": title, "brief": "PRIVATE_BRIEF_" + title, "worker_id": workerID,
		})
		if r.Code != http.StatusCreated {
			t.Fatalf("create job: %d %s", r.Code, r.Body.String())
		}
		var receipt struct {
			ID string `json:"id"`
		}
		decodeResponse(t, r, &receipt)
		return receipt.ID
	}
	jobA := createJob("assigned-a", worker)
	jobB := createJob("assigned-b", worker)
	jobC := createJob("other-worker", otherWorker)
	jobU := createJob("unassigned", "")
	completed := createJob("completed", "")
	progressOK(t, a.call(http.MethodPatch, "/api/v1/projects/"+project+"/jobs/"+jobB, token, "delivery-block-job", map[string]any{"expected_revision": 1, "status": "blocked"}), 200)
	progressOK(t, a.call(http.MethodPatch, "/api/v1/projects/"+project+"/jobs/"+jobC, token, "delivery-park-job", map[string]any{"expected_revision": 1, "status": "parked"}), 200)
	progressOK(t, a.call(http.MethodPatch, "/api/v1/projects/"+project+"/jobs/"+completed, token, "delivery-complete-job", map[string]any{"expected_revision": 1, "status": "completed"}), 200)
	progressOK(t, a.call(http.MethodPut, primaryPath(project, worker), token, "delivery-primary", primaryRequest(0, jobA)), 200)
	reason := strings.Repeat("x", 238) + "雪pause reason"
	progressOK(t, a.call(http.MethodPut, pausePath(project, worker), token, "delivery-pause", pauseRequest(0, true, reason)), 200)
	done := strings.Repeat("d", 239) + "雪trailing detail"
	next := strings.Repeat("n", 238) + "雪next detail"
	blocker := strings.Repeat("b", 238) + "雪blocker detail"
	progressOK(t, a.callWorker(http.MethodPost, "/api/v1/projects/"+project+"/jobs/"+jobA+"/checkpoints", token, worker, "delivery-checkpoint", map[string]any{
		"expected_job_revision": 1, "done": done, "next": next, "blocker": blocker, "evidence": []string{"PRIVATE_EVIDENCE"},
	}), http.StatusCreated)
	// This update deliberately makes the latest checkpoint's captured job
	// revision stale while leaving the checkpoint itself as the latest one.
	progressOK(t, a.call(http.MethodPatch, "/api/v1/projects/"+project+"/jobs/"+jobA, token, "delivery-after-checkpoint", map[string]any{"expected_revision": 1, "title": "assigned-a-renamed"}), 200)

	path := "/api/v1/projects/" + project + "/delivery"
	if r := a.call(http.MethodGet, path, "", "", nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("missing credential status=%d", r.Code)
	}
	if r := a.call(http.MethodGet, path, foreignWorker, "", nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("foreign worker id is not a credential, status=%d", r.Code)
	}
	if r := a.call(http.MethodGet, "/api/v1/projects/"+foreignProject+"/delivery", token, "", nil); r.Code != http.StatusForbidden {
		t.Fatalf("cross-project status=%d body=%s", r.Code, r.Body.String())
	}
	response := a.call(http.MethodGet, path, readOnly, "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read-only delivery status=%d body=%s", response.Code, response.Body.String())
	}
	var value deliveryValue
	decodeResponse(t, response, &value)
	if len(value.Workers.Items) != 2 || !value.Workers.HasMore && value.Workers.Cursor != "" || len(value.Jobs.Items) != 4 {
		t.Fatalf("unexpected default page: %+v", value)
	}
	if strings.Contains(response.Body.String(), `"brief":`) || strings.Contains(response.Body.String(), "PRIVATE_EVIDENCE") || strings.Contains(response.Body.String(), "evidence_json") {
		t.Fatal("delivery response expanded full job brief or checkpoint evidence")
	}
	var selectedWorker *deliveryWorkerValue
	for i := range value.Workers.Items {
		if value.Workers.Items[i].ID == worker {
			selectedWorker = &value.Workers.Items[i]
		}
	}
	if selectedWorker == nil || selectedWorker.PrimaryJobID != jobA || !selectedWorker.Paused || !selectedWorker.PauseReasonTruncated || selectedWorker.PauseReason != strings.Repeat("x", 238) || len(selectedWorker.PauseReason) > 240 || !utf8.ValidString(selectedWorker.PauseReason) {
		t.Fatalf("worker summary missing/default state or unsafe preview: %+v", selectedWorker)
	}
	for _, item := range value.Workers.Items {
		if item.ID == otherWorker && (item.PrimaryJobID != "" || item.Paused || item.PauseReason != "" || item.PauseReasonTruncated) {
			t.Fatalf("worker with no state row did not get defaults: %+v", item)
		}
	}
	const sessionID = "0123456789abcdef0123456789abcdef"
	login := httptest.NewRequest(http.MethodPost, "http://localhost/api/v1/projects/"+project+"/browser-session", strings.NewReader(`{"session_id":"`+sessionID+`"}`))
	login.Header.Set("Authorization", "Bearer "+readOnly)
	login.Header.Set("Origin", "http://localhost")
	login.Header.Set("X-FC-Instance", a.store.InstanceID())
	loginResponse := httptest.NewRecorder()
	a.mux.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK || len(loginResponse.Result().Cookies()) != 1 {
		t.Fatalf("read-only browser login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	browserRead := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
	browserRead.Header.Set("X-FC-Browser-Session", sessionID)
	browserRead.Header.Set("X-FC-Instance", a.store.InstanceID())
	browserRead.AddCookie(loginResponse.Result().Cookies()[0])
	browserResponse := httptest.NewRecorder()
	a.mux.ServeHTTP(browserResponse, browserRead)
	if browserResponse.Code != http.StatusOK {
		t.Fatalf("read-only browser delivery status=%d body=%s", browserResponse.Code, browserResponse.Body.String())
	}
	var foundCheckpoint bool
	var foundUnassigned bool
	var foundParked bool
	for _, item := range value.Jobs.Items {
		if item.ID == jobA {
			foundCheckpoint = item.Checkpoint != nil && item.Checkpoint.Sequence > 0 && item.Checkpoint.JobRevision == 1 && item.Revision == 2 && item.Title == "assigned-a-renamed" && item.WorkerName == "Builder" && item.Checkpoint.DoneTruncated && item.Checkpoint.NextTruncated && item.Checkpoint.BlockerTruncated && item.Checkpoint.Done == strings.Repeat("d", 239) && item.Checkpoint.Next == strings.Repeat("n", 238) && item.Checkpoint.Blocker == strings.Repeat("b", 238) && len(item.Checkpoint.Done) <= 240 && len(item.Checkpoint.Next) <= 240 && len(item.Checkpoint.Blocker) <= 240 && utf8.ValidString(item.Checkpoint.Done) && utf8.ValidString(item.Checkpoint.Next) && utf8.ValidString(item.Checkpoint.Blocker)
		}
		if item.ID == jobU {
			foundUnassigned = item.WorkerID == "" && item.WorkerName == "" && item.Checkpoint == nil
		}
		if item.ID == jobC {
			foundParked = item.Status == "parked" && item.WorkerName == "Reviewer"
		}
		if item.ID == completed {
			t.Fatal("completed job entered open delivery page")
		}
	}
	if !foundCheckpoint || !foundUnassigned || !foundParked {
		t.Fatalf("missing latest/stale, unassigned, or owner-outside-page facts: %+v", value.Jobs.Items)
	}
	if value.Workers.Items[0].ID >= value.Workers.Items[1].ID {
		t.Fatalf("workers not ID ordered: %+v", value.Workers.Items)
	}
	if value.Jobs.Items[0].Status != "active" || value.Jobs.Items[1].Status != "active" || value.Jobs.Items[2].Status != "blocked" || value.Jobs.Items[3].Status != "parked" {
		t.Fatalf("open jobs not status/ID ordered: %+v", value.Jobs.Items)
	}
	outsideWorkerPage := a.call(http.MethodGet, path+"?worker_limit=1&job_limit=4", readOnly, "", nil)
	if outsideWorkerPage.Code != http.StatusOK {
		t.Fatal(outsideWorkerPage.Code, outsideWorkerPage.Body.String())
	}
	var partial deliveryValue
	decodeResponse(t, outsideWorkerPage, &partial)
	if len(partial.Workers.Items) != 1 {
		t.Fatalf("worker page limit was not applied: %+v", partial.Workers.Items)
	}
	foundOwnerOutsidePage := false
	for _, item := range partial.Jobs.Items {
		if item.WorkerID != "" && item.WorkerID != partial.Workers.Items[0].ID {
			foundOwnerOutsidePage = item.WorkerName != ""
		}
	}
	if !foundOwnerOutsidePage {
		t.Fatalf("jobs did not retain owner names beyond the worker page: %+v", partial.Jobs.Items)
	}

	// Independent cursors each continue exactly their own collection.
	first := a.call(http.MethodGet, path+"?worker_limit=1&job_limit=1", readOnly, "", nil)
	if first.Code != http.StatusOK {
		t.Fatal(first.Code, first.Body.String())
	}
	var page1 deliveryValue
	decodeResponse(t, first, &page1)
	if !page1.Workers.HasMore || page1.Workers.Cursor == "" || !page1.Jobs.HasMore || page1.Jobs.Cursor == "" || page1.Jobs.Items[0].Status != "active" {
		t.Fatalf("first independent page incomplete: %+v", page1)
	}
	continued := a.call(http.MethodGet, path+"?worker_limit=1&job_limit=2&worker_cursor="+url.QueryEscape(page1.Workers.Cursor)+"&job_cursor="+url.QueryEscape(page1.Jobs.Cursor), readOnly, "", nil)
	if continued.Code != http.StatusOK {
		t.Fatalf("continuation status=%d body=%s", continued.Code, continued.Body.String())
	}
	var page2 deliveryValue
	decodeResponse(t, continued, &page2)
	if len(page2.Workers.Items) != 1 || page2.Workers.Items[0].ID <= page1.Workers.Items[0].ID || len(page2.Jobs.Items) != 2 || page2.Jobs.Items[0].Status != "active" || page2.Jobs.Items[1].Status != "blocked" {
		t.Fatalf("independent cursors did not continue ordered sets: %+v", page2)
	}
	if bad := a.call(http.MethodGet, path+"?worker_cursor="+url.QueryEscape(page1.Workers.Cursor), foreignWorker, "", nil); bad.Code != http.StatusUnauthorized {
		t.Fatal("cursor read without scope authorization", bad.Code)
	}
	foreignToken := a.grant(foreignProject, "delivery-foreign-client", "read")
	if bad := a.call(http.MethodGet, "/api/v1/projects/"+foreignProject+"/delivery?worker_cursor="+url.QueryEscape(page1.Workers.Cursor), foreignToken, "", nil); bad.Code != http.StatusBadRequest {
		t.Fatalf("cross-project worker cursor status=%d body=%s", bad.Code, bad.Body.String())
	}
	if bad := a.call(http.MethodGet, "/api/v1/projects/"+foreignProject+"/delivery?job_cursor="+url.QueryEscape(page1.Jobs.Cursor), foreignToken, "", nil); bad.Code != http.StatusBadRequest {
		t.Fatalf("cross-project job cursor status=%d body=%s", bad.Code, bad.Body.String())
	}

	for _, query := range []string{
		"ignored=true", "worker_limit=1&worker_limit=2", "job_limit=1&job_limit=2", "worker_limit=0", "job_limit=101", "worker_cursor=bad", "job_cursor=bad", "worker_limit=1;job_limit=1", "worker_cursor=%zz",
	} {
		bad := a.call(http.MethodGet, path+"?"+query, readOnly, "", nil)
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("invalid selector accepted %q: %d %s", query, bad.Code, bad.Body.String())
		}
	}
}

func TestDeliveryReadUsesLatestCheckpointIndex(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("delivery-index", "synthetic/delivery-index")
	for _, lookup := range []struct{ query, index string }{
		{`SELECT sequence FROM job_checkpoints WHERE project_id=? AND job_id=? ORDER BY sequence DESC LIMIT 1`, "job_checkpoints_project_job_sequence"},
		{`SELECT sequence FROM job_updates WHERE project_id=? AND job_id=? ORDER BY sequence DESC LIMIT 1`, "job_updates_project_job_sequence"},
		{`SELECT items_json FROM job_checklists WHERE project_id=? AND job_id=?`, "sqlite_autoindex_job_checklists_1"},
	} {
		planRows, err := a.store.DB().Query("EXPLAIN QUERY PLAN "+lookup.query, project, "job_sample")
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for planRows.Next() {
			var id, parent, notused int
			var detail string
			if err := planRows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
			plan.WriteByte('\n')
		}
		err = planRows.Err()
		planRows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), lookup.index) || strings.Contains(plan.String(), "SCAN ") || strings.Contains(plan.String(), "TEMP B-TREE") {
			t.Fatalf("delivery lookup not bounded/index-backed: %s", plan.String())
		}
	}
}

func TestDeliveryReadHasNoProviderLookup(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("delivery-no-provider", "synthetic/delivery-no-provider")
	token := a.grant(project, "delivery-no-provider-client", "read")
	before := a.lookups
	r := a.call(http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/delivery", project), token, "", nil)
	if r.Code != http.StatusOK || a.lookups != before {
		t.Fatalf("stored delivery read status=%d lookups=%d->%d body=%s", r.Code, before, a.lookups, r.Body.String())
	}
	var shape map[string]json.RawMessage
	decodeResponse(t, r, &shape)
	if len(shape) != 2 || shape["workers"] == nil || shape["jobs"] == nil {
		t.Fatalf("unexpected delivery response shape: %+v", shape)
	}
	var empty deliveryValue
	if err := json.Unmarshal(r.Body.Bytes(), &empty); err != nil || empty.Workers.Items == nil || empty.Jobs.Items == nil {
		t.Fatalf("empty collections are not arrays: %v %+v", err, empty)
	}
}

func TestDeliveryQueryCursorFilterBinding(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("delivery-cursor", "synthetic/delivery-cursor")
	token := a.grant(project, "delivery-cursor-client", "read", "write")
	for i := 0; i < 2; i++ {
		a.createWorker(project, fmt.Sprintf("cursor-worker-%d", i))
	}
	first := a.call(http.MethodGet, "/api/v1/projects/"+project+"/delivery?worker_limit=1", token, "", nil)
	if first.Code != http.StatusOK {
		t.Fatal(first.Code, first.Body.String())
	}
	var page deliveryValue
	decodeResponse(t, first, &page)
	if page.Workers.Cursor == "" {
		t.Fatal("expected worker cursor")
	}
	for _, query := range []string{"worker_cursor=" + url.QueryEscape(page.Workers.Cursor) + "&job_limit=0"} {
		r := a.call(http.MethodGet, "/api/v1/projects/"+project+"/delivery?"+query, token, "", nil)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("bad cursor/query accepted %s: %d %s", query, r.Code, r.Body.String())
		}
	}
	otherPrincipal := a.grant(project, "delivery-cursor-other-principal", "read")
	wrongPrincipal := a.call(http.MethodGet, "/api/v1/projects/"+project+"/delivery?worker_cursor="+url.QueryEscape(page.Workers.Cursor), otherPrincipal, "", nil)
	if wrongPrincipal.Code != http.StatusBadRequest {
		t.Fatalf("cross-principal cursor status=%d body=%s", wrongPrincipal.Code, wrongPrincipal.Body.String())
	}
}

func TestDeliveryCurrentContextPreviewsAndRecordedTimes(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("delivery-context", "synthetic/delivery-context")
	token := a.grant(project, "delivery-context-client", "read", "write")
	path := "/api/v1/projects/" + project
	brief := strings.Repeat("b", 239) + "雪PRIVATE_FULL_BRIEF_TAIL"
	create := func(key, brief string) string {
		t.Helper()
		r := a.call(http.MethodPost, path+"/jobs", token, key, map[string]any{"title": key, "brief": brief})
		progressOK(t, r, 201)
		var receipt struct {
			ID string `json:"id"`
		}
		decodeResponse(t, r, &receipt)
		return receipt.ID
	}
	rich := create("rich", brief)
	missing := create("missing", "")
	cleared := create("cleared", "")
	jobPath := path + "/jobs/" + rich
	// Only the sequence-latest update is exposed, even after wall-clock rollback.
	for i, body := range []string{"PRIVATE_OLDER_UPDATE", strings.Repeat("u", 238) + "雪PRIVATE_UPDATE_TAIL"} {
		progressOK(t, a.call(http.MethodPost, jobPath+"/updates", token, fmt.Sprint("update", i), map[string]any{"expected_job_revision": 1, "body": body, "evidence": []string{"PRIVATE_UPDATE_EVIDENCE"}}), 201)
	}
	progressOK(t, a.call(http.MethodPost, jobPath+"/checkpoints", token, "context-checkpoint", map[string]any{"expected_job_revision": 1, "done": "done", "next": "next"}), 201)
	progressOK(t, a.call(http.MethodPut, jobPath+"/checklist", token, "context-checklist", map[string]any{"expected_job_revision": 1, "expected_revision": 0, "items": []map[string]any{{"id": "one", "text": "PRIVATE_CHECKLIST_TEXT", "done": true}, {"id": "two", "text": "other", "done": false}}}), 200)
	for revision, items := range [][]map[string]any{{{"id": "one", "text": "temporary", "done": false}}, {}} {
		progressOK(t, a.call(http.MethodPut, path+"/jobs/"+cleared+"/checklist", token, fmt.Sprint("clear", revision), map[string]any{"expected_job_revision": 1, "expected_revision": revision, "items": items}), 200)
	}
	progressOK(t, a.call(http.MethodPatch, jobPath, token, "context-edit", map[string]any{"expected_revision": 1, "title": "rich edited"}), 200)
	const editedAt = "2024-02-03T04:05:06+13:00"
	const latestAt = "2020-01-01T00:00:00Z"
	const checkpointAt = "2021-01-01T00:00:00Z"
	const checklistAt = "2099-01-01T00:00:00Z"
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE jobs SET updated_at=? WHERE project_id=? AND id=?`, []any{editedAt, project, rich}},
		{`UPDATE job_updates SET created_at='2030-01-01T00:00:00Z' WHERE project_id=? AND job_id=?`, []any{project, rich}},
		{`UPDATE job_updates SET created_at=? WHERE project_id=? AND job_id=? AND sequence=(SELECT max(sequence) FROM job_updates WHERE project_id=? AND job_id=?)`, []any{latestAt, project, rich, project, rich}},
		{`UPDATE job_checkpoints SET created_at=? WHERE project_id=? AND job_id=?`, []any{checkpointAt, project, rich}},
		{`UPDATE job_checklists SET created_at=? WHERE project_id=? AND job_id=?`, []any{checklistAt, project, rich}},
	} {
		if _, err := a.store.DB().Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	read := func() (*httptest.ResponseRecorder, deliveryValue) {
		r := a.call(http.MethodGet, path+"/delivery?job_limit=100", token, "", nil)
		progressOK(t, r, 200)
		var value deliveryValue
		decodeResponse(t, r, &value)
		return r, value
	}
	response, value := read()
	var richValue, missingValue, clearedValue deliveryJobValue
	for _, item := range value.Jobs.Items {
		switch item.ID {
		case rich:
			richValue = item
		case missing:
			missingValue = item
		case cleared:
			clearedValue = item
		}
	}
	if richValue.BriefPreview != strings.Repeat("b", 239) || !richValue.BriefPreviewTruncated || richValue.UpdatedAt != editedAt || richValue.Revision != 2 {
		t.Fatalf("brief/edit facts changed: %+v", richValue)
	}
	if richValue.Update == nil || richValue.Update.Sequence < 2 || richValue.Update.JobRevision != 1 || richValue.Update.Body != strings.Repeat("u", 238) || !richValue.Update.BodyTruncated || richValue.Update.CreatedAt != latestAt {
		t.Fatalf("sequence-latest update facts: %+v", richValue.Update)
	}
	if richValue.Checklist == nil || !reflect.DeepEqual(*richValue.Checklist, deliveryChecklistValue{Revision: 1, JobRevision: 1, Total: 2, Done: 1, CreatedAt: checklistAt}) {
		t.Fatalf("checklist facts: %+v", richValue.Checklist)
	}
	if richValue.Checkpoint == nil || richValue.Checkpoint.CreatedAt != checkpointAt || richValue.Checkpoint.JobRevision != 1 {
		t.Fatalf("checkpoint time/revision: %+v", richValue.Checkpoint)
	}
	if missingValue.Update != nil || missingValue.Checklist != nil || missingValue.Checkpoint != nil || missingValue.BriefPreview != "" || missingValue.BriefPreviewTruncated {
		t.Fatalf("invented missing context: %+v", missingValue)
	}
	if clearedValue.Checklist == nil || clearedValue.Checklist.Total != 0 || clearedValue.Checklist.Done != 0 || clearedValue.Checklist.Revision != 2 {
		t.Fatalf("known zero lost: %+v", clearedValue.Checklist)
	}
	for _, secret := range []string{"PRIVATE_FULL_BRIEF_TAIL", "PRIVATE_UPDATE_TAIL", "PRIVATE_OLDER_UPDATE", "PRIVATE_CHECKLIST_TEXT", "PRIVATE_UPDATE_EVIDENCE", `"brief":`, `"evidence":`, `"items_json":`, `"last_activity_at":`} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("expanded private content: %s", secret)
		}
	}
	// Repeating a read cannot rewrite imported/clock-rollback facts.
	_, repeated := read()
	if !reflect.DeepEqual(value, repeated) {
		t.Fatal("delivery read mutated context")
	}
}

func TestDeliveryRefusesMalformedCurrentContext(t *testing.T) {
	for name, change := range map[string]struct {
		query string
		value any
	}{
		"brief bound":            {`UPDATE jobs SET brief=? WHERE project_id=? AND id=?`, strings.Repeat("x", 32769)},
		"edit time":              {`UPDATE jobs SET updated_at=? WHERE project_id=? AND id=?`, "PRIVATE_INVALID_TIME"},
		"update time":            {`UPDATE job_updates SET created_at=? WHERE project_id=? AND job_id=?`, "PRIVATE_INVALID_TIME"},
		"update future revision": {`UPDATE job_updates SET job_revision=? WHERE project_id=? AND job_id=?`, 2},
		"checklist content":      {`UPDATE job_checklists SET items_json=? WHERE project_id=? AND job_id=?`, `[{"id":"one","text":"PRIVATE_BAD_CHECKLIST","done":"yes"}]`},
		"checklist time":         {`UPDATE job_checklists SET created_at=? WHERE project_id=? AND job_id=?`, "PRIVATE_INVALID_TIME"},
	} {
		t.Run(name, func(t *testing.T) {
			a := setupAPI(t)
			project := a.createProject("delivery-corrupt", "synthetic/delivery-corrupt")
			token := a.grant(project, "delivery-corrupt-client", "read", "write")
			base := "/api/v1/projects/" + project
			r := a.call(http.MethodPost, base+"/jobs", token, "create", map[string]any{"title": "Current", "brief": "brief"})
			progressOK(t, r, 201)
			var job struct {
				ID string `json:"id"`
			}
			decodeResponse(t, r, &job)
			path := base + "/jobs/" + job.ID
			progressOK(t, a.call(http.MethodPost, path+"/updates", token, "update", map[string]any{"expected_job_revision": 1, "body": "progress"}), 201)
			progressOK(t, a.call(http.MethodPut, path+"/checklist", token, "checklist", map[string]any{"expected_job_revision": 1, "expected_revision": 0, "items": []map[string]any{{"id": "one", "text": "task", "done": false}}}), 200)
			if _, err := a.store.DB().Exec(change.query, change.value, project, job.ID); err != nil {
				t.Fatal(err)
			}
			response := a.call(http.MethodGet, base+"/delivery", token, "", nil)
			if response.Code != 500 || !strings.Contains(response.Body.String(), "storage_unavailable") || strings.Contains(response.Body.String(), "PRIVATE_") {
				t.Fatalf("malformed summary admitted or exposed: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
