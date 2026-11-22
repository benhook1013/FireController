package jobs_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
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

func TestDeliveryCardsProjectsOnlyWireFieldsAndPreservesDefault(t *testing.T) {
	a, project, token, worker, jobPath := progressFixture(t)
	progressOK(t, a.call("PUT", jobPath+"/checklist", token, "cards-plan", progressReplace(1, 0, []progressItem{{ID: "done", Text: "PRIVATE checklist text", Done: true}})), http.StatusOK)
	current := readWorking(t, a, jobPath, token)
	progressOK(t, a.call("PATCH", jobPath, token, "cards-progress", map[string]any{
		"expected_revision": current.Revision, "worker_id": worker, "brief": strings.Repeat("PRIVATE standing brief ", 20),
		"progress": map[string]any{"summary": strings.Repeat("雪", 100), "next": "Run focused proof", "blocker": "Awaiting evidence", "evidence": []string{"PRIVATE evidence"}},
	}), http.StatusOK)
	current = readWorking(t, a, jobPath, token)
	progressOK(t, a.callWorker("PUT", jobPath+"/summary", token, worker, "cards-summary", summaryBody(current.Revision, 0, "PRIVATE audience title", "PRIVATE audience body")), http.StatusOK)
	base := "/api/v1/projects/" + project + "/delivery"
	type rawDelivery struct {
		Workers json.RawMessage `json:"workers"`
		Jobs    struct {
			Items   []map[string]json.RawMessage `json:"items"`
			Cursor  string                       `json:"cursor"`
			HasMore bool                         `json:"has_more"`
		} `json:"jobs"`
	}
	read := func(query string) rawDelivery {
		t.Helper()
		r := a.call(http.MethodGet, base+query, token, "", nil)
		progressOK(t, r, http.StatusOK)
		var value rawDelivery
		decodeResponse(t, r, &value)
		return value
	}
	before, lookups := contextState(t, a), a.lookups
	full, cards := read(""), read("?view=cards")
	if len(full.Jobs.Items) != 1 || len(cards.Jobs.Items) != 1 || !reflect.DeepEqual(full.Workers, cards.Workers) || cards.Jobs.HasMore || cards.Jobs.Cursor != "" {
		t.Fatal("cards changed workers, job selection or page envelope")
	}
	job := full.Jobs.Items[0]
	for _, field := range []string{"brief_preview", "brief_preview_truncated", "checklist", "summary"} {
		if job[field] == nil || cards.Jobs.Items[0][field] != nil {
			t.Fatalf("default/cards projection mismatch for %s", field)
		}
		delete(job, field)
	}
	if len(job) != 8 || !reflect.DeepEqual(job, cards.Jobs.Items[0]) {
		t.Fatal("cards changed retained fields or expanded beyond the exact card shape")
	}
	var progress struct {
		Summary          string `json:"summary"`
		Next             string `json:"next"`
		Blocker          string `json:"blocker"`
		SummaryTruncated bool   `json:"summary_truncated"`
	}
	if err := json.Unmarshal(job["progress"], &progress); err != nil || progress.Summary != strings.Repeat("雪", 80) || !progress.SummaryTruncated || progress.Next != "Run focused proof" || progress.Blocker != "Awaiting evidence" {
		t.Fatal("cards lost bounded UTF-8 progress", err)
	}
	// Collection selection composes with the projection, including empty arrays.
	for _, collection := range []string{"workers", "jobs"} {
		value := read("?view=cards&collection=" + collection)
		if collection == "workers" && (value.Jobs.Items == nil || len(value.Jobs.Items) != 0 || value.Jobs.HasMore || value.Jobs.Cursor != "") || collection == "jobs" && !reflect.DeepEqual(value.Jobs, cards.Jobs) {
			t.Fatal("cards changed collection selection")
		}
	}
	if !reflect.DeepEqual(before, contextState(t, a)) || lookups != a.lookups {
		t.Fatal("cards read mutated durable state or contacted a provider")
	}
	// A wire projection must not bypass validation of the omitted stored facts.
	if _, err := a.store.DB().Exec(`UPDATE job_checklists SET items_json='broken' WHERE project_id=? AND job_id=?`, project, current.ID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "?view=cards"} {
		progressOK(t, a.call(http.MethodGet, base+query, token, "", nil), http.StatusInternalServerError)
	}
}

func TestDeliveryCardsSelectorsCursorsAndScope(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("delivery-cards", "synthetic/delivery-cards")
	token := a.grant(project, "delivery-cards-client", "read", "write")
	for i := 0; i < 3; i++ {
		worker := a.createWorker(project, fmt.Sprintf("Cards worker %d", i))
		progressOK(t, a.call(http.MethodPost, "/api/v1/projects/"+project+"/jobs", token, fmt.Sprintf("cards-job-%d", i), map[string]any{"title": fmt.Sprintf("Cards job %d", i), "worker_id": worker}), http.StatusCreated)
	}
	base := "/api/v1/projects/" + project + "/delivery"
	read := func(query string) deliveryValue {
		t.Helper()
		r := a.call(http.MethodGet, base+query, token, "", nil)
		progressOK(t, r, http.StatusOK)
		var value deliveryValue
		decodeResponse(t, r, &value)
		return value
	}
	full, cards := read("?worker_limit=1&job_limit=1"), read("?view=cards&worker_limit=1&job_limit=1")
	if !full.Workers.HasMore || !full.Jobs.HasMore || full.Workers.Cursor == "" || full.Jobs.Cursor == "" || full.Workers.Cursor != cards.Workers.Cursor || full.Jobs.Cursor != cards.Jobs.Cursor {
		t.Fatal("view changed cursor identity or boundedness")
	}
	for _, view := range []string{"", "view=cards&"} {
		next := read("?" + view + "worker_limit=1&job_limit=1&worker_cursor=" + url.QueryEscape(cards.Workers.Cursor) + "&job_cursor=" + url.QueryEscape(cards.Jobs.Cursor))
		if len(next.Workers.Items) != 1 || len(next.Jobs.Items) != 1 || next.Workers.Items[0].ID == full.Workers.Items[0].ID || next.Jobs.Items[0].ID == full.Jobs.Items[0].ID {
			t.Fatal("cursor did not continue across views")
		}
		// The first worker is outside this slice, but its job retains its owner name.
		fallback := read("?" + view + "worker_limit=1&worker_cursor=" + url.QueryEscape(cards.Workers.Cursor))
		foundOwner := false
		for _, job := range fallback.Jobs.Items {
			if job.WorkerID == full.Workers.Items[0].ID {
				foundOwner = true
				if job.WorkerName != full.Workers.Items[0].Name {
					t.Fatal("cards lost off-page worker identity")
				}
			}
		}
		if !foundOwner {
			t.Fatal("off-page owner fixture job disappeared")
		}
	}
	for _, query := range []string{"view=", "view=full", "view=Cards", "view=cards&view=cards", "view=cards&view=", "view=%zz", "view=cards&unknown=1", "view=cards&job_limit=0", "view=cards&collection=jobs&worker_limit=1", "view=cards&collection=workers&job_cursor=foreign", "view=cards&job_cursor=foreign"} {
		progressOK(t, a.call(http.MethodGet, base+"?"+query, token, "", nil), http.StatusBadRequest)
	}
	otherPrincipal := a.grant(project, "cards-other-principal", "read")
	otherProject := a.createProject("delivery-cards-other", "synthetic/delivery-cards-other")
	otherToken := a.grant(otherProject, "cards-other-project", "read")
	for _, cursor := range []struct{ key, value string }{{"worker_cursor", cards.Workers.Cursor}, {"job_cursor", cards.Jobs.Cursor}} {
		query := "?view=cards&" + cursor.key + "=" + url.QueryEscape(cursor.value)
		progressOK(t, a.call(http.MethodGet, base+query, otherPrincipal, "", nil), http.StatusBadRequest)
		progressOK(t, a.call(http.MethodGet, "/api/v1/projects/"+otherProject+"/delivery"+query, otherToken, "", nil), http.StatusBadRequest)
	}
	for _, query := range []string{"", "?view=cards"} {
		progressOK(t, a.call(http.MethodGet, base+query, otherToken, "", nil), http.StatusForbidden)
	}
	empty := a.call(http.MethodGet, "/api/v1/projects/"+otherProject+"/delivery?view=cards", otherToken, "", nil)
	progressOK(t, empty, http.StatusOK)
	var emptyPage deliveryValue
	decodeResponse(t, empty, &emptyPage)
	if emptyPage.Workers.Items == nil || emptyPage.Jobs.Items == nil || len(emptyPage.Workers.Items) != 0 || len(emptyPage.Jobs.Items) != 0 || emptyPage.Workers.HasMore || emptyPage.Jobs.HasMore {
		t.Fatal("empty cards collections must remain bounded arrays")
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

func TestDeliveryReadsOnlySelectedCollection(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("delivery-selection", "synthetic/delivery-selection")
	token := a.grant(project, "delivery-selection-client", "read", "write")
	worker := a.createWorker(project, "Selected worker")
	base := "/api/v1/projects/" + project
	created := a.call(http.MethodPost, base+"/jobs", token, "selected-job", map[string]any{"title": "Selected job", "worker_id": worker})
	progressOK(t, created, http.StatusCreated)
	read := func(query string) deliveryValue {
		t.Helper()
		response := a.call(http.MethodGet, base+"/delivery"+query, token, "", nil)
		progressOK(t, response, http.StatusOK)
		var value deliveryValue
		decodeResponse(t, response, &value)
		return value
	}
	full := read("")
	workers := read("?collection=workers&worker_limit=1")
	jobs := read("?collection=jobs&job_limit=1")
	if !reflect.DeepEqual(workers.Workers, full.Workers) || !reflect.DeepEqual(workers.Jobs, deliveryPageValue[deliveryJobValue]{Items: []deliveryJobValue{}}) {
		t.Fatalf("unexpected workers-only delivery: %+v", workers)
	}
	if !reflect.DeepEqual(jobs.Jobs, full.Jobs) || !reflect.DeepEqual(jobs.Workers, deliveryPageValue[deliveryWorkerValue]{Items: []deliveryWorkerValue{}}) {
		t.Fatalf("unexpected jobs-only delivery: %+v", jobs)
	}
	for _, query := range []string{"collection=", "collection=all", "collection=jobs&collection=workers", "collection=jobs&worker_limit=1", "collection=workers&job_cursor=foreign"} {
		progressOK(t, a.call(http.MethodGet, base+"/delivery?"+query, token, "", nil), http.StatusBadRequest)
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
				cards := a.call(http.MethodGet, base+"/delivery?view=cards", token, "", nil)
				progressOK(t, cards, http.StatusOK)
				var cardBoard deliveryValue
				decodeResponse(t, cards, &cardBoard)
				if !reflect.DeepEqual(board, cardBoard) {
					t.Fatal("cards changed API-admitted names, titles or identity")
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
				for _, path := range append([]string(nil), paths...) {
					separator := "?"
					if strings.Contains(path, "?") {
						separator = "&"
					}
					paths = append(paths, path+separator+"view=cards")
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
