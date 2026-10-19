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
}

type deliveryJobValue struct {
	ID         string                   `json:"id"`
	Title      string                   `json:"title"`
	WorkerID   string                   `json:"worker_id"`
	WorkerName string                   `json:"worker_name"`
	Status     string                   `json:"status"`
	Revision   int64                    `json:"revision"`
	Checkpoint *deliveryCheckpointValue `json:"checkpoint"`
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

func TestDeliveryWhitespaceOnlyTruncatedCheckpointPreviewsRetainFullText(t *testing.T) {
	for _, mode := range []string{"ASCII", "Unicode"} {
		t.Run(mode, func(t *testing.T) {
			a := setupAPI(t)
			project := a.createProject("whitespace-preview", "synthetic/whitespace-preview")
			token := a.grant(project, "whitespace-preview-reader-writer", "read", "write")
			worker := a.createWorker(project, "Builder")
			job := createCheckpointTestJob(t, a, project, token)
			base := "/api/v1/projects/" + project + "/jobs/" + job.ID
			donePrefix, nextPrefix, blocker := strings.Repeat(" ", 241), strings.Repeat("\t", 241), ""
			wantDone, wantNext := strings.Repeat(" ", 240), strings.Repeat("\t", 240)
			if mode == "Unicode" {
				donePrefix, nextPrefix = strings.Repeat("\u2003", 81), strings.Repeat("\u3000", 81)
				wantDone, wantNext = strings.Repeat("\u2003", 80), strings.Repeat("\u3000", 80)
				blocker = strings.Repeat("\n", 241) + "Waiting for a decision."
			}
			done, next := donePrefix+"Completed the stored work.", nextPrefix+"Continue the next step."
			progressOK(t, a.callWorker(http.MethodPost, base+"/checkpoints", token, worker, "leading-whitespace-checkpoint", map[string]any{
				"expected_job_revision": 1, "done": done, "next": next, "blocker": blocker,
			}), http.StatusCreated)

			// Retain every value in the affected durable tables, including the
			// untrimmed full checkpoint text and existing receipts/events.
			state := func() map[string]string {
				t.Helper()
				out := map[string]string{}
				for _, table := range []string{"jobs", "job_history", "job_checkpoints", "events", "receipts"} {
					rows, err := a.store.DB().Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
					if err != nil {
						t.Fatal(err)
					}
					columns, err := rows.Columns()
					if err != nil {
						t.Fatal(err)
					}
					values := [][]any{}
					for rows.Next() {
						row := make([]any, len(columns))
						dest := make([]any, len(columns))
						for i := range row {
							dest[i] = &row[i]
						}
						if err := rows.Scan(dest...); err != nil {
							t.Fatal(err)
						}
						values = append(values, row)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					rows.Close()
					raw, err := json.Marshal(values)
					if err != nil {
						t.Fatal(err)
					}
					out[table] = string(raw)
				}
				return out
			}
			before, lookups := state(), a.lookups
			response := a.call(http.MethodGet, "/api/v1/projects/"+project+"/delivery", token, "", nil)
			progressOK(t, response, http.StatusOK)
			var delivery deliveryValue
			decodeResponse(t, response, &delivery)
			if len(delivery.Jobs.Items) != 1 || delivery.Jobs.Items[0].Checkpoint == nil {
				t.Fatal("missing Delivery checkpoint", delivery)
			}
			preview := delivery.Jobs.Items[0].Checkpoint
			if preview.Done != wantDone || preview.Next != wantNext || !preview.DoneTruncated || !preview.NextTruncated || len(preview.Done) != 240 || len(preview.Next) != 240 || strings.TrimSpace(preview.Done) != "" || strings.TrimSpace(preview.Next) != "" || !utf8.ValidString(preview.Done) || !utf8.ValidString(preview.Next) {
				t.Fatal("valid full text did not produce exact bounded whitespace previews", preview)
			}
			if mode == "Unicode" && (preview.Blocker != strings.Repeat("\n", 240) || !preview.BlockerTruncated) || mode == "ASCII" && (preview.Blocker != "" || preview.BlockerTruncated) {
				t.Fatal("optional blocker preview changed", preview)
			}
			latest := a.call(http.MethodGet, base+"/checkpoints/latest", token, "", nil)
			progressOK(t, latest, http.StatusOK)
			var full struct {
				Checkpoint checkpointAPIValue `json:"checkpoint"`
			}
			decodeResponse(t, latest, &full)
			if full.Checkpoint.Done != done || full.Checkpoint.Next != next || full.Checkpoint.Blocker != blocker {
				t.Fatal("checkpoint storage or full read trimmed accepted text")
			}
			for _, field := range []string{"done", "next"} {
				for i, invalid := range []string{"", strings.Repeat(" ", 241), strings.Repeat("\u2003", 81)} {
					request := map[string]any{"expected_job_revision": 1, "done": "Completed", "next": "Continue"}
					request[field] = invalid
					progressOK(t, a.callWorker(http.MethodPost, base+"/checkpoints", token, worker, fmt.Sprintf("blank-full-%s-%d", field, i), request), http.StatusBadRequest)
				}
			}
			if !reflect.DeepEqual(before, state()) || a.lookups != lookups {
				t.Fatal("stored Delivery/full reads or invalid appends changed rows, receipts/events, or provider calls")
			}
		})
	}
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
	if strings.Contains(response.Body.String(), "PRIVATE_BRIEF") || strings.Contains(response.Body.String(), "PRIVATE_EVIDENCE") || strings.Contains(response.Body.String(), "evidence_json") {
		t.Fatal("delivery response leaked job brief or checkpoint evidence")
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
	planRows, err := a.store.DB().Query(`EXPLAIN QUERY PLAN SELECT sequence FROM job_checkpoints WHERE project_id=? AND job_id=? ORDER BY sequence DESC LIMIT 1`, project, "job_sample")
	if err != nil {
		t.Fatal(err)
	}
	defer planRows.Close()
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
	if err := planRows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "job_checkpoints_project_job_sequence") {
		t.Fatalf("latest checkpoint lookup is not index-backed: %s", plan.String())
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

func TestDeliveryPreservesAPIAcceptedControlCharactersInNamesAndTitles(t *testing.T) {
	for _, chars := range []struct{ name, value string }{{"C0", "\x00\x01\x1f"}, {"DEL", "\x7f"}} {
		for _, operation := range []string{"job-create", "job-edit", "worker-create"} {
			t.Run(chars.name+"/"+operation, func(t *testing.T) {
				a := setupAPI(t)
				project := a.createProject("delivery-control", "synthetic/delivery-control")
				token := a.grant(project, "delivery-control-client", "read", "write")
				base := "/api/v1/projects/" + project
				name, title := "Builder", "Ship work"
				if operation == "worker-create" {
					name = "Builder" + chars.value + "Lane"
				}
				workerResponse := a.call(http.MethodPost, base+"/workers", token, "control-worker", map[string]any{"name": name})
				progressOK(t, workerResponse, http.StatusOK)
				var workerReceipt receipt
				decodeResponse(t, workerResponse, &workerReceipt)
				if operation == "job-create" {
					title = "Ship" + chars.value + "work"
				}
				created := a.call(http.MethodPost, base+"/jobs", token, "control-job", map[string]any{"title": "  " + title + "  ", "worker_id": workerReceipt.ID})
				progressOK(t, created, http.StatusCreated)
				var jobReceipt receipt
				decodeResponse(t, created, &jobReceipt)
				revision := int64(1)
				if operation == "job-edit" {
					title = "Updated" + chars.value + "work"
					progressOK(t, a.call(http.MethodPatch, base+"/jobs/"+jobReceipt.ID, token, "control-edit", map[string]any{"expected_revision": 1, "title": "  " + title + "  "}), http.StatusOK)
					revision = 2
				}
				state := func() map[string]string {
					t.Helper()
					snapshot := map[string]string{}
					for _, table := range []string{"workers", "jobs", "job_history", "events", "receipts"} {
						rows, err := a.store.DB().Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
						if err != nil {
							t.Fatal(err)
						}
						columns, err := rows.Columns()
						if err != nil {
							t.Fatal(err)
						}
						values := [][]any{}
						for rows.Next() {
							row := make([]any, len(columns))
							dest := make([]any, len(columns))
							for i := range row {
								dest[i] = &row[i]
							}
							if err := rows.Scan(dest...); err != nil {
								t.Fatal(err)
							}
							values = append(values, row)
						}
						if err := rows.Err(); err != nil {
							t.Fatal(err)
						}
						rows.Close()
						raw, err := json.Marshal(values)
						if err != nil {
							t.Fatal(err)
						}
						snapshot[table] = string(raw)
					}
					return snapshot
				}
				before, lookups := state(), a.lookups
				delivery := a.call(http.MethodGet, base+"/delivery", token, "", nil)
				progressOK(t, delivery, http.StatusOK)
				var board deliveryValue
				decodeResponse(t, delivery, &board)
				if len(board.Workers.Items) != 1 || board.Workers.Items[0].ID != workerReceipt.ID || board.Workers.Items[0].Name != name || len(board.Jobs.Items) != 1 || board.Jobs.Items[0].ID != jobReceipt.ID || board.Jobs.Items[0].Title != title || board.Jobs.Items[0].WorkerName != name || board.Jobs.Items[0].WorkerID != workerReceipt.ID || board.Jobs.Items[0].Revision != revision {
					t.Fatal("Delivery changed API-admitted text or identity")
				}
				jobDetail := a.call(http.MethodGet, base+"/jobs/"+jobReceipt.ID, token, "", nil)
				progressOK(t, jobDetail, http.StatusOK)
				var job jobValue
				decodeResponse(t, jobDetail, &job)
				if job.Title != title || job.WorkerID != workerReceipt.ID || job.Revision != revision {
					t.Fatal("job detail changed admitted title")
				}
				jobList := a.call(http.MethodGet, base+"/jobs", token, "", nil)
				progressOK(t, jobList, http.StatusOK)
				var jobs struct {
					Items []jobValue `json:"items"`
				}
				decodeResponse(t, jobList, &jobs)
				if len(jobs.Items) != 1 || jobs.Items[0].Title != title {
					t.Fatal("job list changed admitted title")
				}
				workerDetail := a.call(http.MethodGet, base+"/workers/"+workerReceipt.ID, token, "", nil)
				progressOK(t, workerDetail, http.StatusOK)
				var worker struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				}
				decodeResponse(t, workerDetail, &worker)
				if worker.ID != workerReceipt.ID || worker.Name != name {
					t.Fatal("worker detail changed admitted name")
				}
				workerList := a.call(http.MethodGet, base+"/workers", token, "", nil)
				progressOK(t, workerList, http.StatusOK)
				var workers struct {
					Items []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"items"`
				}
				decodeResponse(t, workerList, &workers)
				if len(workers.Items) != 1 || workers.Items[0].Name != name {
					t.Fatal("worker list changed admitted name")
				}
				if !reflect.DeepEqual(before, state()) || a.lookups != lookups {
					t.Fatal("Delivery/detail/list reads mutated durable rows or called a provider")
				}
			})
		}
	}
}

func TestDeliveryStillRejectsMalformedStoredNamesAndTitles(t *testing.T) {
	for _, field := range []string{"title", "name"} {
		for _, bad := range []struct{ name, value string }{{"empty", ""}, {"blank", " \t "}, {"unnormalized", " padded "}, {"overlong", strings.Repeat("x", 241)}, {"invalid-utf8", string([]byte{0xff})}} {
			t.Run(field+"/"+bad.name, func(t *testing.T) {
				a := setupAPI(t)
				project := a.createProject("delivery-malformed", "synthetic/delivery-malformed")
				token := a.grant(project, "delivery-malformed-client", "read", "write")
				base := "/api/v1/projects/" + project
				a.createWorker(project, "First")
				a.createWorker(project, "Second")
				first := a.call(http.MethodGet, base+"/delivery?worker_limit=1", token, "", nil)
				progressOK(t, first, http.StatusOK)
				var page deliveryValue
				decodeResponse(t, first, &page)
				if len(page.Workers.Items) != 1 || !page.Workers.HasMore || page.Workers.Cursor == "" {
					t.Fatal("bounded fixture worker page unavailable")
				}
				owner := page.Workers.Items[0].ID
				created := a.call(http.MethodPost, base+"/jobs", token, "malformed-job", map[string]any{"title": "Valid title", "worker_id": owner})
				progressOK(t, created, http.StatusCreated)
				var job receipt
				decodeResponse(t, created, &job)
				// Corrupt only this disposable fixture after successful public admission.
				table, id := "jobs", job.ID
				if field == "name" {
					table, id = "workers", owner
				}
				value := bad.value
				if field == "name" && bad.name == "overlong" {
					value = strings.Repeat("x", 201)
				}
				if _, err := a.store.DB().Exec(`UPDATE `+table+` SET `+field+`=? WHERE project_id=? AND id=?`, value, project, id); err != nil {
					t.Fatal(err)
				}
				var bytesBefore string
				if err := a.store.DB().QueryRow(`SELECT hex(CAST(`+field+` AS BLOB)) FROM `+table+` WHERE project_id=? AND id=?`, project, id).Scan(&bytesBefore); err != nil {
					t.Fatal(err)
				}
				lookups := a.lookups
				paths := []string{base + "/delivery"}
				if field == "name" {
					paths = append(paths, base+"/delivery?worker_limit=1&worker_cursor="+url.QueryEscape(page.Workers.Cursor))
				}
				for _, path := range paths {
					r := a.call(http.MethodGet, path, token, "", nil)
					progressOK(t, r, http.StatusInternalServerError)
					var failure struct {
						Error struct {
							Code string `json:"code"`
						} `json:"error"`
					}
					decodeResponse(t, r, &failure)
					if failure.Error.Code != "storage_unavailable" {
						t.Fatal("malformed storage lost safe refusal")
					}
				}
				var bytesAfter string
				if err := a.store.DB().QueryRow(`SELECT hex(CAST(`+field+` AS BLOB)) FROM `+table+` WHERE project_id=? AND id=?`, project, id).Scan(&bytesAfter); err != nil || bytesAfter != bytesBefore || a.lookups != lookups {
					t.Fatal("invalid stored read changed bytes or provider calls", err)
				}
			})
		}
	}
}
