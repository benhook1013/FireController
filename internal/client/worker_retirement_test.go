package client_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func retirementPreview() client.WorkerRetirement {
	empty := client.WorkerRetirementBlocker{IDs: []string{}}
	return client.WorkerRetirement{WorkerID: "worker-target", CanRetire: true, Blockers: client.WorkerRetirementBlockers{Jobs: empty, Notes: empty, Messages: empty}}
}
func TestWorkerRetirementStrictBoundTransportAndSingleShotReceipt(t *testing.T) {
	preview := retirementPreview()
	receipt := client.WorkerRetirementReceipt{ID: preview.WorkerID, Revision: 2, Outcome: "retired"}
	var reads, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("resource selection changed actor")
		}
		switch r.Method {
		case "GET":
			reads.Add(1)
			if r.URL.Path != "/api/v1/projects/project-one/workers/worker-target/retirement" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("preview routing")
			}
			write(w, 200, preview)
		case "POST":
			writes.Add(1)
			body, _ := io.ReadAll(r.Body)
			if r.URL.Path != "/api/v1/projects/project-one/workers/worker-target/retire" || string(body) != "{}" || r.Header.Get("Idempotency-Key") != "explicit-retirement" {
				t.Error("retirement request identity changed", string(body))
			}
			write(w, 200, receipt)
		default:
			t.Error("unexpected request")
			w.WriteHeader(500)
		}
	}))
	defer service.Close()
	c := newClient(t, service)
	if got, err := c.WorkerRetirement(t.Context(), preview.WorkerID); err != nil || !reflect.DeepEqual(got, preview) {
		t.Fatal(got, err)
	}
	for _, retired := range []bool{false, true} {
		preview = retirementPreview()
		preview.Retired = retired
		preview.CanRetire = !retired
		if _, err := c.WorkerRetirement(t.Context(), preview.WorkerID); err != nil {
			t.Fatal(err)
		}
	}
	for name, corrupt := range map[string]func(*client.WorkerRetirement){
		"other worker":     func(v *client.WorkerRetirement) { v.WorkerID = "foreign" },
		"retired eligible": func(v *client.WorkerRetirement) { v.Retired = true },
		"blocked eligible": func(v *client.WorkerRetirement) {
			v.Blockers.Jobs = client.WorkerRetirementBlocker{Count: 1, IDs: []string{"job-one"}}
		},
		"missing IDs": func(v *client.WorkerRetirement) { v.Blockers.Notes.Count = 1 },
		"nil IDs":     func(v *client.WorkerRetirement) { v.Blockers.Notes.IDs = nil },
		"negative":    func(v *client.WorkerRetirement) { v.Blockers.Messages.Count = -1 },
		"duplicate": func(v *client.WorkerRetirement) {
			v.CanRetire = false
			v.Blockers.Messages = client.WorkerRetirementBlocker{Count: 2, IDs: []string{"message-one", "message-one"}}
		},
		"unsafe ID": func(v *client.WorkerRetirement) {
			v.CanRetire = false
			v.Blockers.Jobs = client.WorkerRetirementBlocker{Count: 1, IDs: []string{"../private"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			preview = retirementPreview()
			corrupt(&preview)
			_, err := c.WorkerRetirement(t.Context(), "worker-target")
			checkError(t, err, "invalid_response")
		})
	}
	preview = retirementPreview()
	preview.CanRetire = false
	preview.Blockers.Jobs.Count = 21
	for i := 0; i < 20; i++ {
		preview.Blockers.Jobs.IDs = append(preview.Blockers.Jobs.IDs, "job-"+strings.Repeat("a", i+1))
	}
	if _, err := c.WorkerRetirement(t.Context(), preview.WorkerID); err != nil {
		t.Fatal("bounded count larger than IDs rejected", err)
	}
	for _, outcome := range []string{"retired", "unchanged"} {
		receipt.Outcome = outcome
		if got, err := c.RetireWorker(t.Context(), "explicit-retirement", "worker-target"); err != nil || got != receipt {
			t.Fatal(got, err)
		}
	}
	receipt.ID = "foreign"
	_, err := c.RetireWorker(t.Context(), "explicit-retirement", "worker-target")
	failure, ok := err.(*client.Error)
	if !ok {
		t.Fatal(err)
	}
	if failure.Code != "invalid_response" || !failure.OutcomeUnknown || writes.Load() != 3 {
		t.Fatal("invalid receipt retried or treated as known", err, writes.Load())
	}
	if reads.Load() != 12 {
		t.Fatal("preview retry/fanout", reads.Load())
	}
}

func TestWorkerArchiveAndSelectedAcknowledgementPreserveDefaultWire(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		requests.Add(1)
		if r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("selected ACK changed actor")
		}
		if r.Method == "GET" {
			retired := r.URL.Query().Get("include_retired") == "true"
			if !retired && r.URL.Query().Has("include_retired") {
				t.Error("default list changed cursor query")
			}
			write(w, 200, client.Page[client.Worker]{Items: []client.Worker{{ID: "worker-target", ProjectID: "project-one", Name: "Target", Retired: retired}}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		want := "{}"
		if r.Header.Get("Idempotency-Key") == "selected" {
			want = `{"worker_id":"worker-target"}`
		}
		if string(b) != want || r.URL.Path != "/api/v1/projects/project-one/messages/message-one/ack" {
			t.Error("ACK body/binding", string(b))
		}
		write(w, 200, client.MessageReceipt{ID: "message-one", ConversationID: "conversation-one", Outcome: "acknowledged"})
	}))
	defer service.Close()
	c := newClient(t, service)
	if page, err := c.Workers(t.Context(), client.PageOptions{}); err != nil || page.Items[0].Retired {
		t.Fatal(page, err)
	}
	if page, err := c.WorkersWithRetired(t.Context(), client.PageOptions{}, true); err != nil || !page.Items[0].Retired {
		t.Fatal(page, err)
	}
	if _, err := c.Acknowledge(t.Context(), "default", "message-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acknowledge(t.Context(), "selected", "message-one", "worker-target"); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	for _, id := range []string{"", "../escape"} {
		_, err := c.RetireWorker(t.Context(), "key", id)
		checkError(t, err, "validation_failed")
	}
	if _, err := c.Acknowledge(t.Context(), "selected", "message-one", "../escape"); err == nil {
		t.Fatal("invalid selected recipient")
	}
	if requests.Load() != before {
		t.Fatal("malformed operation sent")
	}
}

func longRetirementBlockers(fill string) client.WorkerRetirementBlockers {
	var groups [3]client.WorkerRetirementBlocker
	for i := range groups {
		groups[i].Count = (1 << 53) - 1
		for j := 0; j < 20; j++ {
			groups[i].IDs = append(groups[i].IDs, strings.Repeat(fill, 125/len(fill))+strings.Repeat("x", 125%len(fill))+fmt.Sprintf("%03d", j))
		}
	}
	return client.WorkerRetirementBlockers{Jobs: groups[0], Notes: groups[1], Messages: groups[2]}
}

func TestWorkerRetirementLongBlockersRemainDefinitive(t *testing.T) {
	for _, fill := range []string{"a", "<", "\"", "é"} {
		t.Run(fmt.Sprintf("fill-%x", fill), func(t *testing.T) {
			blocks := longRetirementBlockers(fill)
			details, err := json.Marshal(blocks)
			if err != nil || len(details) <= 4096 || len(details) > 48<<10 {
				t.Fatal("fixture must exceed the ordinary detail budget within the bounded blocker envelope", len(details), err)
			}
			var calls atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				if r.Header.Get("X-FC-Worker") != "worker-one" {
					t.Error("retirement selection changed actor")
				}
				if r.Method == "POST" {
					body, _ := io.ReadAll(r.Body)
					if string(body) != "{}" || r.Header.Get("Idempotency-Key") != "blocked-long" || r.URL.Path != "/api/v1/projects/project-one/workers/worker-target/retire" {
						t.Error("retirement request identity changed")
					}
				}
				write(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": "worker_retirement_blocked", "message": "Clear blockers first.", "details": blocks}, "request_id": "request-long-blockers"})
			}))
			defer service.Close()
			c := newClient(t, service)
			for _, mutate := range []bool{false, true} {
				var err error
				if mutate {
					_, err = c.RetireWorker(t.Context(), "blocked-long", "worker-target")
				} else {
					_, err = c.WorkerRetirement(t.Context(), "worker-target")
				}
				checkError(t, err, "worker_retirement_blocked")
				failure := err.(*client.Error)
				if failure.Status != http.StatusConflict || failure.OutcomeUnknown || failure.RequestID != "request-long-blockers" || !reflect.DeepEqual(failure.Details, blocks) {
					t.Fatal("definitive refusal lost bounded blocker handles", failure)
				}
			}
			if calls.Load() != 2 {
				t.Fatal("blocked retirement was retried or fanned out", calls.Load())
			}
		})
	}
}

func TestWorkerRetirementErrorDetailBoundsStayStrict(t *testing.T) {
	blocks := longRetirementBlockers("a")
	encode := func(value any) string {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	valid := encode(blocks)
	duplicate := longRetirementBlockers("a")
	duplicate.Jobs.IDs[1] = duplicate.Jobs.IDs[0]
	oversizedID := longRetirementBlockers("a")
	oversizedID.Jobs.IDs[0] += "b"
	extraID := longRetirementBlockers("a")
	extraID.Jobs.IDs = append(extraID.Jobs.IDs, "extra")
	sensitive := longRetirementBlockers("a")
	sensitive.Jobs.IDs[0] = testToken
	for _, test := range []struct {
		name, code, details string
		valid               bool
	}{
		{"ordinary-at-limit", "conflict", `"` + strings.Repeat("a", 4094) + `"`, true},
		{"ordinary-over-limit", "conflict", `"` + strings.Repeat("a", 4095) + `"`, false},
		{"other-code-blocker-shape", "conflict", valid, false},
		{"blocker-over-envelope", "worker_retirement_blocked", `"` + strings.Repeat("a", 48<<10) + `"`, false},
		{"transport-over-limit", "worker_retirement_blocked", `"` + strings.Repeat("a", 64<<10) + `"`, false},
		{"unknown-blocker-field", "worker_retirement_blocked", strings.Replace(valid, `"jobs":{`, `"jobs":{"unknown":0,`, 1), false},
		{"duplicate-blocker-field", "worker_retirement_blocked", strings.Replace(valid, `"jobs":{`, `"jobs":{"count":1,`, 1), false},
		{"null-count", "worker_retirement_blocked", strings.Replace(valid, `"count":9007199254740991`, `"count":null`, 1), false},
		{"unsafe-count", "worker_retirement_blocked", strings.Replace(valid, `9007199254740991`, `9007199254740992`, 1), false},
		{"duplicate-id", "worker_retirement_blocked", encode(duplicate), false},
		{"oversized-id", "worker_retirement_blocked", encode(oversizedID), false},
		{"extra-id", "worker_retirement_blocked", encode(extraID), false},
		{"sensitive-id", "worker_retirement_blocked", encode(sensitive), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				write(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": test.code, "message": "Request refused.", "details": json.RawMessage(test.details)}, "request_id": "request-bounds"})
			}))
			defer service.Close()
			c := newClient(t, service)
			for _, mutate := range []bool{false, true} {
				var err error
				if mutate {
					_, err = c.RetireWorker(t.Context(), "bounded-error", "worker-target")
				} else {
					_, err = c.WorkerRetirement(t.Context(), "worker-target")
				}
				want := "invalid_response"
				if test.valid {
					want = test.code
				} else if mutate && test.name != "transport-over-limit" {
					want = "unexpected_status"
				}
				checkError(t, err, want)
				failure := err.(*client.Error)
				if failure.OutcomeUnknown != (mutate && !test.valid) || failure.Details != nil {
					t.Fatal("invalid detail or outcome escaped its bounds", failure)
				}
			}
			if calls.Load() != 2 {
				t.Fatal("error was retried or fanned out", calls.Load())
			}
		})
	}
}
