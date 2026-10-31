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
