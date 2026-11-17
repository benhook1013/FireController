package client_test

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func progressUpdate() client.Update {
	return client.Update{ID: "update-one", ProjectID: "project-one", JobID: "job-one", JobRevision: 2,
		Sequence: 8, ActorID: "principal-one", WorkerID: "", Body: "  completed <literal>\n", Evidence: []string{"proof-one"}, CreatedAt: "2026-10-15T00:00:00Z"}
}

func progressChecklist() client.Checklist {
	return client.Checklist{JobID: "job-one", JobRevision: 2, Revision: 3, Items: []client.ChecklistItem{{ID: "plan_1", Text: "  preserve text\t", Done: false}}}
}

func progressJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func progressResponseClient(t *testing.T, body string) (*client.Client, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return newClient(t, s), calls
}

func TestJobProgressClientStrictResponsesScopeOrderAndBudgets(t *testing.T) {
	update := progressJSON(t, client.Page[client.Update]{Items: []client.Update{progressUpdate()}})
	checklist := progressJSON(t, progressChecklist())
	history := progressJSON(t, client.Page[client.ChecklistHistoryEntry]{Items: []client.ChecklistHistoryEntry{{Revision: 3, JobRevision: 2, ActorID: "principal-one", WorkerID: "", Items: progressChecklist().Items, CreatedAt: "2026-10-15T00:00:00Z"}}})
	for _, test := range []struct{ name, route, body string }{
		{"update-project", "updates", strings.Replace(update, "project-one", "project-two", 1)},
		{"update-job", "updates", strings.Replace(update, "job-one", "job-two", 1)},
		{"update-null-evidence", "updates", strings.Replace(update, `["proof-one"]`, "null", 1)},
		{"update-missing-worker", "updates", strings.Replace(update, `"worker_id":"",`, "", 1)},
		{"update-null-body", "updates", strings.Replace(update, `"body":"  completed \u003cliteral\u003e\n"`, `"body":null`, 1)},
		{"update-unknown", "updates", strings.Replace(update, `"sequence":8`, `"sequence":8,"extra":1`, 1)},
		{"update-null-items", "updates", `{"items":null,"has_more":false}`},
		{"update-zero-time", "updates", strings.Replace(update, `2026-10-15T00:00:00Z`, `0001-01-01T00:00:00Z`, 1)},
		{"checklist-wrong-job", "checklist", strings.Replace(checklist, "job-one", "job-two", 1)},
		{"checklist-zero-with-items", "checklist", strings.Replace(checklist, `"revision":3`, `"revision":0`, 1)},
		{"checklist-null-items", "checklist", `{"job_id":"job-one","job_revision":2,"revision":0,"items":null}`},
		{"checklist-unknown", "checklist", strings.Replace(checklist, `"done":false`, `"done":false,"extra":1`, 1)},
		{"checklist-missing-false", "checklist", strings.Replace(checklist, `,"done":false`, "", 1)},
		{"checklist-null-false", "checklist", strings.Replace(checklist, `"done":false`, `"done":null`, 1)},
		{"checklist-alias", "checklist", strings.Replace(checklist, `"done":false`, `"Done":false`, 1)},
		{"checklist-duplicate", "checklist", strings.Replace(checklist, `"revision":3`, `"revision":3,"revision":3`, 1)},
		{"checklist-budget", "checklist", strings.Repeat(" ", 128<<10) + checklist},
		{"archive-budget", "updates", strings.Repeat(" ", 4<<20) + update},
		{"history-budget", "history", strings.Repeat(" ", 4<<20) + history},
		{"history-zero", "history", strings.Replace(history, `"revision":3`, `"revision":0`, 1)},
		{"history-unknown", "history", strings.Replace(history, `"actor_id":"principal-one"`, `"actor_id":"principal-one","project_id":"other"`, 1)},
		{"history-missing-false", "history", strings.Replace(history, `,"done":false`, "", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, calls := progressResponseClient(t, test.body)
			var err error
			switch test.route {
			case "updates":
				_, err = c.JobUpdates(t.Context(), "job-one", client.PageOptions{})
			case "checklist":
				_, err = c.JobChecklist(t.Context(), "job-one")
			case "history":
				_, err = c.JobChecklistHistory(t.Context(), "job-one", client.PageOptions{})
			}
			checkError(t, err, "invalid_response")
			if calls.Load() != 1 {
				t.Fatal("invalid response caused automatic followup")
			}
		})
	}
	for _, duplicateID := range []bool{false, true} {
		one, two := progressUpdate(), progressUpdate()
		two.Sequence = 7
		if !duplicateID {
			two.ID = "update-two"
			two.Sequence = 9
		}
		c, _ := progressResponseClient(t, progressJSON(t, client.Page[client.Update]{Items: []client.Update{one, two}}))
		_, err := c.JobUpdates(t.Context(), "job-one", client.PageOptions{})
		checkError(t, err, "invalid_response")
	}
	entry := client.ChecklistHistoryEntry{Revision: 2, JobRevision: 1, ActorID: "actor", Items: []client.ChecklistItem{}, CreatedAt: "2026-10-15T00:00:00Z"}
	c, _ := progressResponseClient(t, progressJSON(t, client.Page[client.ChecklistHistoryEntry]{Items: []client.ChecklistHistoryEntry{entry, entry}}))
	_, err := c.JobChecklistHistory(t.Context(), "job-one", client.PageOptions{})
	checkError(t, err, "invalid_response")
}

func TestJobProgressClientLocalValidationAndExplicitChecklistDecode(t *testing.T) {
	for _, raw := range []string{"null", "{}", `[{"id":"one","text":"task"}]`, `[{"id":"one","text":"task","done":null}]`, `[{"id":"one","text":"task","Done":false}]`, `[{"id":"one","text":"task","done":false,"done":false}]`, `[{"id":"one","text":"task","done":false,"other":1}]`, `[{"id":"one","text":"\ud800","done":false}]`, `[{"id":"é","text":"task","done":false}]`, `[{"id":"one","text":"task","done":false},{"id":"one","text":"again","done":true}]`, strings.Repeat(" ", 256<<10) + "[]"} {
		if _, err := client.DecodeJobChecklistItems([]byte(raw)); err == nil {
			t.Fatalf("malformed checklist accepted: %.80s", raw)
		}
	}
	for _, raw := range []string{"[]", `[{"id":"item_One-2","text":"  task\n","done":false}]`} {
		items, err := client.DecodeJobChecklistItems([]byte(raw))
		if err != nil || items == nil {
			t.Fatal("valid explicit array refused", err)
		}
	}
}

func TestJobProgressClientChecklistAggregateAndEscapedReadBoundaries(t *testing.T) {
	items := make([]client.ChecklistItem, 16)
	for i := range items {
		items[i] = client.ChecklistItem{ID: string(rune('a' + i)), Text: strings.Repeat("&", 1023)}
	}
	request := client.ReplaceChecklistRequest{ExpectedJobRevision: 1, Items: items}
	if err := client.ValidateJobChecklistRequest(request); err != nil {
		t.Fatal("exact16KiB plan refused", err)
	}
	body := progressJSON(t, client.Checklist{JobID: "job-one", JobRevision: 1, Revision: math.MaxInt64, Items: items})
	if len(body) < 90<<10 || len(body) >= 128<<10 {
		t.Fatal("escaped fixture does not exercise the current response bound")
	}
	c, _ := progressResponseClient(t, body)
	got, err := c.JobChecklist(t.Context(), "job-one")
	if err != nil || got.Revision != math.MaxInt64 || !reflect.DeepEqual(got.Items, items) {
		t.Fatal("bounded escaped plan was truncated or writable guard applied to read", err)
	}
	items[0].Text += "&"
	if client.ValidateJobChecklistRequest(request) == nil {
		t.Fatal("one-byte aggregate overflow accepted")
	}
	many := make([]client.ChecklistItem, 101)
	for i := range many {
		many[i] = client.ChecklistItem{ID: string(rune('a'+i/26)) + string(rune('a'+i%26)), Text: "task"}
	}
	request.Items = many[:100]
	if err := client.ValidateJobChecklistRequest(request); err != nil {
		t.Fatal("100 distinct items refused", err)
	}
	request.Items = many
	if client.ValidateJobChecklistRequest(request) == nil {
		t.Fatal("101 item plan accepted")
	}
}
