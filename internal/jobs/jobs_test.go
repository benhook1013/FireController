package jobs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/server"
)

type testAPI struct {
	t       *testing.T
	store   *core.Store
	mux     *http.ServeMux
	admin   string
	lookups int
}

func TestJobsPublicAPI(t *testing.T) {
	// Full fixture body is initialized in setupAPI, which keeps all setup and
	// mutation requests on the same public HTTP surface as clients use.
	a := setupAPI(t)
	projectA := a.createProject("jobs-alpha", "benhook1013/jobs-alpha")
	projectB := a.createProject("jobs-beta", "benhook1013/jobs-beta")
	clientA := a.grant(projectA, "client-token-alpha", "read", "write")
	clientB := a.grant(projectB, "client-token-beta", "read", "write")
	readOnly := a.grant(projectA, "client-token-readonly", "read")
	worker := a.createWorker(projectA, "builder-alpha")

	missing := a.call(http.MethodGet, "/api/v1/projects/"+projectA+"/jobs", "", "", nil)
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing credential status=%d, want 401", missing.Code)
	}

	created := a.call(http.MethodPost, "/api/v1/projects/"+projectA+"/jobs", clientA, "job-create-1", map[string]any{
		"title": "Ship core", "brief": "Full private task brief", "worker_id": worker,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var initial receipt
	decodeResponse(t, created, &initial)
	if initial.ID == "" || initial.Revision != 1 || initial.Outcome != "created" {
		t.Fatalf("unexpected create receipt: %+v", initial)
	}

	readonly := a.call(http.MethodPost, "/api/v1/projects/"+projectA+"/jobs", readOnly, "readonly-create", map[string]any{"title": "Forbidden"})
	if readonly.Code != http.StatusForbidden {
		t.Fatalf("read-only mutation status=%d, want 403", readonly.Code)
	}
	cross := a.call(http.MethodGet, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID, clientB, "", nil)
	if cross.Code != http.StatusForbidden {
		t.Fatalf("cross-project read status=%d, want 403", cross.Code)
	}

	detail := a.call(http.MethodGet, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID, clientA, "", nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var full jobValue
	decodeResponse(t, detail, &full)
	if full.Brief != "Full private task brief" || full.ProjectID != projectA {
		t.Fatalf("detail did not return explicit full job: %+v", full)
	}

	// The first edit receipt can be replayed after a later edit without
	// re-running its callback or exposing the current brief in the receipt.
	firstEditBody := map[string]any{"expected_revision": 1, "title": "Ship core v2", "brief": "Updated private brief"}
	firstEdit := a.call(http.MethodPatch, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID, clientA, "edit-first", firstEditBody)
	if firstEdit.Code != http.StatusOK {
		t.Fatalf("first edit status=%d body=%s", firstEdit.Code, firstEdit.Body.String())
	}
	var firstReceipt receipt
	decodeResponse(t, firstEdit, &firstReceipt)
	if firstReceipt.Revision != 2 || firstReceipt.ID != initial.ID {
		t.Fatalf("unexpected edit receipt: %+v", firstReceipt)
	}
	second := a.call(http.MethodPatch, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID, clientA, "edit-second", map[string]any{"expected_revision": 2, "status": "blocked"})
	if second.Code != http.StatusOK {
		t.Fatalf("second edit status=%d body=%s", second.Code, second.Body.String())
	}
	var historyBefore, eventsBefore, receiptsBefore int
	if err := a.store.DB().QueryRow("SELECT count(*) FROM job_history WHERE project_id=? AND job_id=?", projectA, initial.ID).Scan(&historyBefore); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow("SELECT count(*) FROM events WHERE project_id=? AND resource_id=? AND event_type IN ('job.created','job.updated')", projectA, initial.ID).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow("SELECT count(*) FROM receipts WHERE project_id=? AND operation='jobs.edit' AND target=?", projectA, initial.ID).Scan(&receiptsBefore); err != nil {
		t.Fatal(err)
	}
	staleResponse := a.call(http.MethodPatch, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID, clientA, "edit-stale", map[string]any{"expected_revision": 1, "title": "Stale write"})
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("stale edit status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}
	var staleFailure struct {
		Error struct {
			Code    string           `json:"code"`
			Details map[string]int64 `json:"details"`
		} `json:"error"`
	}
	decodeResponse(t, staleResponse, &staleFailure)
	if staleFailure.Error.Code != "revision_conflict" || staleFailure.Error.Details["current_revision"] != 3 {
		t.Fatalf("stale conflict lacks fresh revision: %+v", staleFailure.Error)
	}
	var historyAfter, eventsAfter, receiptsAfter int
	if err := a.store.DB().QueryRow("SELECT count(*) FROM job_history WHERE project_id=? AND job_id=?", projectA, initial.ID).Scan(&historyAfter); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow("SELECT count(*) FROM events WHERE project_id=? AND resource_id=? AND event_type IN ('job.created','job.updated')", projectA, initial.ID).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow("SELECT count(*) FROM receipts WHERE project_id=? AND operation='jobs.edit' AND target=?", projectA, initial.ID).Scan(&receiptsAfter); err != nil {
		t.Fatal(err)
	}
	if historyBefore != historyAfter || eventsBefore != eventsAfter || receiptsBefore != receiptsAfter {
		t.Fatalf("stale edit changed durable counts: history %d->%d events %d->%d receipts %d->%d",
			historyBefore, historyAfter, eventsBefore, eventsAfter, receiptsBefore, receiptsAfter)
	}
	replay := a.call(http.MethodPatch, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID, clientA, "edit-first", firstEditBody)
	if replay.Code != http.StatusOK {
		t.Fatalf("lost-response replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	var replayReceipt receipt
	decodeResponse(t, replay, &replayReceipt)
	if replayReceipt != firstReceipt {
		t.Fatalf("replay changed receipt: first=%+v replay=%+v", firstReceipt, replayReceipt)
	}
	conflict := a.call(http.MethodPatch, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID, clientA, "edit-first", map[string]any{"expected_revision": 1, "title": "Different body"})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same key with changed body status=%d, want 409", conflict.Code)
	}

	// Competing clients with the same expected revision have one winner.
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i, title := range []string{"Winner one", "Winner two"} {
		wg.Add(1)
		go func(i int, title string) {
			defer wg.Done()
			<-barrier
			response := a.call(http.MethodPatch, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID, clientA,
				[]string{"race-one", "race-two"}[i], map[string]any{"expected_revision": 3, "title": title})
			statuses <- response.Code
		}(i, title)
	}
	close(barrier)
	wg.Wait()
	close(statuses)
	var successes, stale int
	for status := range statuses {
		switch status {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			stale++
		default:
			t.Fatalf("unexpected CAS race status=%d", status)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("CAS results: successes=%d conflicts=%d", successes, stale)
	}

	// Create a second item and verify the compact list uses a scoped bounded
	// cursor while detail alone exposes the full brief.
	secondJob := a.call(http.MethodPost, "/api/v1/projects/"+projectA+"/jobs", clientA, "job-create-2", map[string]any{"title": "Another job", "brief": "Should not appear in list"})
	if secondJob.Code != http.StatusCreated {
		t.Fatalf("second create status=%d", secondJob.Code)
	}
	list := a.call(http.MethodGet, "/api/v1/projects/"+projectA+"/jobs?limit=1", clientA, "", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var listing struct {
		Items   []map[string]any `json:"items"`
		Cursor  string           `json:"cursor"`
		HasMore bool             `json:"has_more"`
	}
	decodeResponse(t, list, &listing)
	if len(listing.Items) != 1 || !listing.HasMore || listing.Cursor == "" {
		t.Fatalf("unexpected compact first page: %+v", listing)
	}
	if _, exists := listing.Items[0]["brief"]; exists {
		t.Fatal("list leaked a full brief")
	}
	page2 := a.call(http.MethodGet, "/api/v1/projects/"+projectA+"/jobs?limit=1&cursor="+url.QueryEscape(listing.Cursor), clientA, "", nil)
	if page2.Code != http.StatusOK {
		t.Fatalf("second page status=%d body=%s", page2.Code, page2.Body.String())
	}
	wrongFilter := a.call(http.MethodGet, "/api/v1/projects/"+projectA+"/jobs?limit=1&status=active&cursor="+url.QueryEscape(listing.Cursor), clientA, "", nil)
	if wrongFilter.Code != http.StatusBadRequest {
		t.Fatalf("cursor reused with a different filter status=%d, want 400", wrongFilter.Code)
	}
	wrongProject := a.call(http.MethodGet, "/api/v1/projects/"+projectB+"/jobs?limit=1&cursor="+url.QueryEscape(listing.Cursor), clientB, "", nil)
	if wrongProject.Code != http.StatusBadRequest {
		t.Fatalf("cursor reused in another project status=%d, want 400", wrongProject.Code)
	}

	history := a.call(http.MethodGet, "/api/v1/projects/"+projectA+"/jobs/"+initial.ID+"/history", clientA, "", nil)
	if history.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", history.Code, history.Body.String())
	}
	var historyResult struct {
		Items []json.RawMessage `json:"items"`
	}
	decodeResponse(t, history, &historyResult)
	if len(historyResult.Items) != 4 {
		t.Fatalf("history has %d entries, want create + two edits + race winner", len(historyResult.Items))
	}
	var historyCount, eventCount int
	if err := a.store.DB().QueryRow("SELECT count(*) FROM job_history WHERE project_id=? AND job_id=?", projectA, initial.ID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow("SELECT count(*) FROM events WHERE project_id=? AND resource_id=? AND event_type IN ('job.created','job.updated')", projectA, initial.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 4 || eventCount != 4 {
		t.Fatalf("atomic counts history=%d events=%d, want 4 each", historyCount, eventCount)
	}
	if a.lookups != 2 {
		t.Fatalf("jobs invoked repository resolver: calls=%d want only 2 project setups", a.lookups)
	}
}

type receipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type jobValue struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	Brief     string `json:"brief"`
	WorkerID  string `json:"worker_id"`
	Status    string `json:"status"`
	Revision  int64  `json:"revision"`
}

func setupAPI(t *testing.T) *testAPI {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := jobs.EnsureSchema(ctx, store); err != nil {
		t.Fatal(err)
	}
	adminBytes, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	a := &testAPI{t: t, store: store, admin: string(bytes.TrimSpace(adminBytes))}
	base := server.New(store, server.WithRepositoryResolver(func(_ context.Context, repository string) (server.RepositoryIdentity, error) {
		a.lookups++
		return server.RepositoryIdentity{ID: int64(a.lookups), FullName: repository}, nil
	}))
	a.mux = http.NewServeMux()
	// Mounting server's exact patterns onto one shared mux preserves all public
	// admin/bootstrap routes while keeping the jobs API composition under test.
	a.mux.Handle("/", base)
	jobs.Register(a.mux, store)
	return a
}

func (a *testAPI) call(method, path, token, key string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	a.mux.ServeHTTP(response, request)
	return response
}

func (a *testAPI) createProject(name, repository string) string {
	a.t.Helper()
	response := a.call(http.MethodPost, "/api/v1/projects", a.admin, "project-"+name, map[string]string{"name": name, "repository": repository})
	if response.Code != http.StatusOK {
		a.t.Fatalf("create project status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		ID string `json:"id"`
	}
	decodeResponse(a.t, response, &result)
	return result.ID
}

func (a *testAPI) createWorker(project, name string) string {
	a.t.Helper()
	response := a.call(http.MethodPost, "/api/v1/projects/"+project+"/workers", a.admin, "worker-"+name, map[string]string{"name": name})
	if response.Code != http.StatusOK {
		a.t.Fatalf("create worker status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		ID string `json:"id"`
	}
	decodeResponse(a.t, response, &result)
	return result.ID
}

func (a *testAPI) grant(project, token string, permissions ...string) string {
	a.t.Helper()
	hash := sha256.Sum256([]byte(token))
	response := a.call(http.MethodPost, "/api/v1/projects/"+project+"/credentials", a.admin, "grant-"+token, map[string]any{
		"token_sha256": hex.EncodeToString(hash[:]), "permissions": permissions,
	})
	if response.Code != http.StatusOK {
		a.t.Fatalf("grant status=%d body=%s", response.Code, response.Body.String())
	}
	return token
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("invalid response JSON: %v body=%s", err, response.Body.String())
	}
}

func TestMalformedJobQueryEncodingIsRejectedBeforeReturningData(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("query-proof", "example/query-proof")
	created := a.call(http.MethodPost, "/api/v1/projects/"+project+"/jobs", a.admin, "create", map[string]any{"title": "Synthetic job"})
	var job receipt
	decodeResponse(t, created, &job)
	before := a.lookups
	for _, path := range []string{"/jobs", "/jobs/" + job.ID + "/history"} {
		for _, query := range []string{"status=%zz", "cursor=%zz", "limit=1;status=blocked", "%zz=value", "status=blocked&ignored=%zz"} {
			response := a.call(http.MethodGet, "/api/v1/projects/"+project+path+"?"+query, a.admin, "", nil)
			var value struct{ Error struct{ Code string } }
			decodeResponse(t, response, &value)
			if response.Code != http.StatusBadRequest || value.Error.Code != "invalid_query" {
				t.Fatalf("malformed query returned data: %s?%s: %d %s", path, query, response.Code, response.Body.String())
			}
		}
	}
	if a.lookups != before {
		t.Fatal("invalid local query performed provider work")
	}
}

func TestOpenJobsGlobalPagingAndIsolation(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("open-jobs", "example/open-jobs")
	other := a.createProject("other-open-jobs", "example/other-open-jobs")
	token := a.grant(project, "open-jobs-reader-and-writer", "read", "write")
	otherToken := a.grant(other, "other-open-jobs-reader", "read", "write")
	worker := a.createWorker(project, "Selected")
	otherWorker := a.createWorker(project, "Other")
	prefix := "/api/v1/projects/" + project + "/jobs"
	var expected []jobValue
	var all []jobValue
	for _, status := range []string{"parked", "completed", "blocked", "active"} {
		for i := 0; i < 3; i++ {
			assigned := worker
			if i == 1 {
				assigned = otherWorker
			}
			response := a.call(http.MethodPost, prefix, token, fmt.Sprintf("create-%s-%d", status, i), map[string]any{"title": status, "status": status, "worker_id": assigned})
			if response.Code != http.StatusCreated {
				t.Fatal("create", response.Code, response.Body.String())
			}
			var receipt receipt
			decodeResponse(t, response, &receipt)
			item := jobValue{ID: receipt.ID, Status: status, WorkerID: assigned}
			all = append(all, item)
			if status != "completed" {
				expected = append(expected, item)
			}
		}
	}
	completed := a.call(http.MethodPost, "/api/v1/projects/"+other+"/jobs", otherToken, "complete-only", map[string]any{"title": "Completed elsewhere", "status": "completed"})
	if completed.Code != http.StatusCreated {
		t.Fatal(completed.Code)
	}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].Status != expected[j].Status {
			return expected[i].Status < expected[j].Status
		}
		return expected[i].ID < expected[j].ID
	})
	digest := func() string {
		// All persisted effects of job calls remain fixed across the read workload.
		h := sha256.New()
		names, err := a.store.DB().Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for names.Next() {
			var name string
			if err := names.Scan(&name); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, name)
		}
		if err := names.Err(); err != nil {
			t.Fatal(err)
		}
		names.Close()
		for _, table := range tables {
			h.Write([]byte(table + "\n"))
			rows, err := a.store.DB().Query(`SELECT * FROM "` + table + `" ORDER BY rowid`)
			if err != nil {
				t.Fatal(err)
			}
			cols, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				values := make([]any, len(cols))
				targets := make([]any, len(cols))
				for i := range values {
					targets[i] = &values[i]
				}
				if err := rows.Scan(targets...); err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				h.Write(body)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	plan, err := a.store.DB().Query(`EXPLAIN QUERY PLAN SELECT id,title,COALESCE(worker_id,''),status,revision FROM jobs WHERE project_id=? AND status=? AND id>? ORDER BY id LIMIT ?`, project, "blocked", "job_cursor", 3)
	if err != nil {
		t.Fatal(err)
	}
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log("open continuation branch:", detail)
		if strings.Contains(detail, "SCAN jobs") || !strings.Contains(detail, "id>?") {
			t.Fatal("continuation branch does not seek beyond the prior ID", detail)
		}
	}
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	plan.Close()
	workerPlan, err := a.store.DB().Query(`EXPLAIN QUERY PLAN SELECT id,title,COALESCE(worker_id,''),status,revision FROM jobs WHERE project_id=? AND status=? AND id>? AND worker_id=? ORDER BY id LIMIT ?`, project, "blocked", "job_cursor", worker, 3)
	if err != nil {
		t.Fatal(err)
	}
	for workerPlan.Next() {
		var id, parent, unused int
		var detail string
		if err := workerPlan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log("assigned open continuation branch:", detail)
		if strings.Contains(detail, "SCAN jobs") || !strings.Contains(detail, "id>?") {
			t.Fatal("assigned continuation scanned the prior ID prefix", detail)
		}
	}
	if err := workerPlan.Err(); err != nil {
		t.Fatal(err)
	}
	workerPlan.Close()
	before := digest()
	read := func(path, credential string) (items []jobValue, cursor string, more bool) {
		t.Helper()
		response := a.call(http.MethodGet, path, credential, "", nil)
		if response.Code != 200 {
			t.Fatal("read", response.Code, response.Body.String())
		}
		var page struct {
			Items   []jobValue `json:"items"`
			Cursor  string     `json:"cursor"`
			HasMore bool       `json:"has_more"`
		}
		decodeResponse(t, response, &page)
		if page.Items == nil {
			t.Fatal("null page")
		}
		return page.Items, page.Cursor, page.HasMore
	}
	var got []jobValue
	cursor := ""
	firstCursor := ""
	for page := 0; page < 10; page++ {
		items, next, more := read(prefix+"?status=open&limit=2&cursor="+url.QueryEscape(cursor), token)
		if len(items) > 2 {
			t.Fatal("unbounded page")
		}
		for _, item := range items {
			got = append(got, jobValue{ID: item.ID, Status: item.Status, WorkerID: item.WorkerID})
		}
		if page == 0 {
			firstCursor = next
		}
		if !more {
			break
		}
		if next == "" || next == cursor {
			t.Fatal("invalid continuation")
		}
		cursor = next
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("open global order/coverage mismatch: got %v want %v", got, expected)
	}
	var assigned []jobValue
	for _, item := range expected {
		if item.WorkerID == worker {
			assigned = append(assigned, item)
		}
	}
	var selected []jobValue
	cursor = ""
	for page := 0; page < 10; page++ {
		items, next, more := read(prefix+"?status=open&worker_id="+worker+"&limit=2&cursor="+url.QueryEscape(cursor), token)
		for _, item := range items {
			selected = append(selected, jobValue{ID: item.ID, Status: item.Status, WorkerID: item.WorkerID})
		}
		if !more {
			break
		}
		cursor = next
	}
	if !reflect.DeepEqual(selected, assigned) {
		t.Fatal("assigned open pages differ")
	}
	items, cursor, more := read("/api/v1/projects/"+other+"/jobs?status=open", otherToken)
	if len(items) != 0 || cursor != "" || more {
		t.Fatal("completed-only project did not yield empty open page")
	}
	for _, path := range []string{prefix + "?status=active&cursor=" + url.QueryEscape(firstCursor), prefix + "?cursor=" + url.QueryEscape(firstCursor), prefix + "?status=open&worker_id=" + worker + "&cursor=" + url.QueryEscape(firstCursor), "/api/v1/projects/" + other + "/jobs?status=open&cursor=" + url.QueryEscape(firstCursor), prefix + "?status=open&cursor=malformed"} {
		credential := token
		if strings.Contains(path, other) {
			credential = otherToken
		}
		if response := a.call(http.MethodGet, path, credential, "", nil); response.Code != 400 {
			t.Fatal("foreign/malformed cursor accepted", response.Code)
		}
	}
	// A valid signature cannot make an invalid open-list position admissible.
	request := httptest.NewRequest(http.MethodGet, prefix, nil)
	request.SetPathValue("project_id", project)
	request.Header.Set("Authorization", "Bearer "+token)
	scope, err := a.store.Scope(request, "read")
	if err != nil {
		t.Fatal(err)
	}
	filter, _ := json.Marshal([]string{project, "", "open"})
	for _, position := range []string{`["completed","job_one"]`, `["active"]`, `["active",""]`, `["active","job_one","extra"]`, `null`} {
		signed := a.store.Cursor(scope, "jobs-open-v1:"+string(filter), position)
		if response := a.call(http.MethodGet, prefix+"?status=open&cursor="+url.QueryEscape(signed), token, "", nil); response.Code != 400 {
			t.Fatal("malformed signed position accepted", position, response.Code)
		}
	}
	// Existing no-filter and exact-state lists keep their original global ID order.
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	items, _, _ = read(prefix, token)
	for i, item := range items {
		if item.ID != all[i].ID {
			t.Fatal("old ID ordering changed")
		}
	}
	items, _, _ = read(prefix+"?status=blocked", token)
	last := ""
	for _, item := range items {
		if item.Status != "blocked" || item.ID <= last {
			t.Fatal("specific-status ordering changed")
		}
		last = item.ID
	}
	if digest() != before {
		t.Fatal("job reads changed retained rows")
	}
	// Open remains an invalid stored/mutated status.
	if response := a.call(http.MethodPost, prefix, token, "refuse-open-create", map[string]any{"title": "Invalid", "status": "open"}); response.Code != 400 {
		t.Fatal("open accepted as job state")
	}
	if response := a.call(http.MethodPatch, prefix+"/"+all[0].ID, token, "refuse-open-edit", map[string]any{"expected_revision": 1, "status": "open"}); response.Code != 400 {
		t.Fatal("open accepted as edited state")
	}
}
