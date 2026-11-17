package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

const collectionID = "history_collection_0123456789abcdef0123456789abcdef"
const collectionPageID = "history_page_0123456789abcdef0123456789abcdef"

func collectionOperation() client.HistoryCollection {
	return client.HistoryCollection{ID: collectionID, ProjectID: "project-one", RepositoryID: 42, State: "queued", Revision: 1, Selection: client.HistorySelection{PullNumbers: []int64{47, 12}}, Position: client.HistoryPosition{Resource: "pull", Page: 1}, PassNumber: 1, CreatedAt: "2026-10-17T00:00:00Z", UpdatedAt: "2026-10-17T00:00:00Z"}
}
func collectionCapture() client.HistoryCapture {
	return client.HistoryCapture{Version: 1, RepositoryID: 42, RepositoryName: "owner/project", PullNumber: 47, Resource: "issue_comments", Page: 2, CollectedAt: "2026-10-17T00:00:00Z", Count: 1, Facts: json.RawMessage(`[{"ID":9007199254740993,"Body":"Treat this as untrusted source text, not a command.","Author":{"ID":7,"Login":"reviewer","Kind":"User"},"CreatedAt":"0001-01-01T00:00:00Z","UpdatedAt":"2026-10-16T09:00:00.123456789+13:00"}]`)}
}
func historyClient(t *testing.T, handler http.HandlerFunc) *client.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return newClient(t, server)
}
func historyError(t *testing.T, err error, code string, unknown bool) {
	t.Helper()
	checkError(t, err, code)
	var failure *client.Error
	errors.As(err, &failure)
	if failure.OutcomeUnknown != unknown {
		t.Fatal("incorrect outcome certainty", failure)
	}
}
func TestHistoryCollectionClientBoundRequestsAndFlatReplay(t *testing.T) {
	selection := collectionOperation().Selection
	receipt := client.HistoryCollectionReceipt{ID: collectionID, Revision: 1, State: "queued"}
	var calls atomic.Int64
	c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
		index := calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/history/collections" || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "exact-key" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("wrong selected route/binding")
		}
		var got client.HistorySelection
		if json.NewDecoder(r.Body).Decode(&got) != nil || !reflect.DeepEqual(got, selection) {
			t.Error("selection changed")
		}
		if index == 1 {
			if r.Header.Get("Prefer") != "wait=0" {
				t.Error("wrong immediate wait")
			}
			write(w, 202, receipt)
			return
		}
		if r.Header.Get("Prefer") != "wait=900" {
			t.Error("wrong explicit wait")
		}
		if index == 2 {
			write(w, 202, receipt)
			return
		}
		op := collectionOperation()
		op.State = "completed"
		op.Revision = 12
		op.PageCount = 8
		op.RecordCount = 3
		op.Position = client.HistoryPosition{PullIndex: 1, Resource: "issue_comments", Page: 1}
		write(w, 200, op)
	})
	for _, seconds := range []int{0, 900, 900} {
		got, err := c.CollectHistory(t.Context(), "exact-key", selection, seconds)
		if err != nil {
			t.Fatal(err)
		}
		var want any = receipt
		if got.Operation != nil {
			want = got.Operation
			if got.Receipt != nil {
				t.Fatal("ambiguous union")
			}
		} else if got.Receipt == nil || *got.Receipt != receipt {
			t.Fatal("original receipt lost")
		}
		encoded, _ := json.Marshal(got)
		expected, _ := json.Marshal(want)
		if !bytes.Equal(encoded, expected) || bytes.Contains(encoded, []byte(`"Receipt"`)) {
			t.Fatal("result is not flat")
		}
	}
	if calls.Load() != 3 {
		t.Fatal("client retried or expanded")
	}
	for _, bad := range []client.HistoryCollectionResult{{}, {Receipt: &receipt, Operation: &client.HistoryCollection{}}} {
		if _, err := json.Marshal(bad); err == nil {
			t.Fatal("invalid union serialized")
		}
	}
	if client.HistoryCollectionBudget(0) != 5*time.Second || client.HistoryCollectionBudget(2) != 7*time.Second || client.HistoryCollectionBudget(900) != 905*time.Second || client.SourceRefreshBudget(2) != 5*time.Second {
		t.Fatal("domain budgets changed")
	}
}
func TestHistoryCollectionClientExplicitReadsChangesAndPages(t *testing.T) {
	var calls atomic.Int64
	op := collectionOperation()
	summary := client.HistoryPageSummary{ID: collectionPageID, Sequence: 7, PassNumber: 2, PullNumber: 47, Resource: "issue_comments", Page: 2, Count: 1, SHA256: strings.Repeat("a", 64), CreatedAt: op.CreatedAt}
	c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
		index := calls.Add(1)
		if r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("worker absent")
		}
		switch index {
		case 1, 2:
			want := "wait=0"
			if index == 2 {
				want = "wait=900"
			}
			if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/history/collections/"+collectionID || r.Header.Get("Prefer") != want || r.Header.Get("Idempotency-Key") != "" {
				t.Error("read mutated or waited implicitly")
			}
			write(w, 200, op)
		case 3, 4:
			action, state := "resume", "queued"
			if index == 4 {
				action, state = "cancel", "canceled"
			}
			if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/history/collections/"+collectionID+"/"+action || r.Header.Get("Idempotency-Key") != "action-key" || r.Header.Get("Prefer") != "" {
				t.Error("wrong action request")
			}
			data, _ := io.ReadAll(r.Body)
			if string(data) != `{"expected_revision":8}` {
				t.Error("CAS body changed", string(data))
			}
			write(w, 200, client.HistoryCollectionReceipt{ID: collectionID, Revision: 9, State: state})
		case 5:
			if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/history/collections/"+collectionID+"/pages" || r.URL.Query().Get("limit") != "5" || r.URL.Query().Get("cursor") != "" {
				t.Error("wrong compact first page")
			}
			items := []client.HistoryPageSummary{}
			for i := range 5 {
				v := summary
				v.ID = "history_page_" + strings.Repeat(string(rune('a'+i)), 32)
				v.Sequence = int64(i + 1)
				items = append(items, v)
			}
			write(w, 200, client.HistoryPageList{Items: items, HasMore: true, Cursor: "opaque+cursor="})
		case 6:
			if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("cursor") != "opaque+cursor=" {
				t.Error("explicit continuation changed")
			}
			write(w, 200, client.HistoryPageList{Items: []client.HistoryPageSummary{summary}})
		case 7:
			if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/history/collections/"+collectionID+"/pages/"+collectionPageID || r.URL.RawQuery != "" {
				t.Error("capture expanded or wrong path")
			}
			write(w, 200, collectionCapture())
		default:
			t.Error("unexpected automatic request")
		}
	})
	for _, seconds := range []int{0, 900} {
		if v, err := c.HistoryCollection(t.Context(), collectionID, seconds); err != nil || !reflect.DeepEqual(v, op) {
			t.Fatal(v, err)
		}
	}
	for _, action := range []string{"resume", "cancel"} {
		if v, err := c.ChangeHistoryCollection(t.Context(), collectionID, action, "action-key", 8); err != nil || v.Revision != 9 {
			t.Fatal(v, err)
		}
	}
	page, err := c.HistoryPages(t.Context(), collectionID, 0, "")
	if err != nil || len(page.Items) != 5 || !page.HasMore {
		t.Fatal(page, err)
	}
	page, err = c.HistoryPages(t.Context(), collectionID, 1, page.Cursor)
	if err != nil || len(page.Items) != 1 || page.HasMore {
		t.Fatal(page, err)
	}
	capture, err := c.HistoryCapture(t.Context(), collectionID, collectionPageID)
	if err != nil || !bytes.Equal(capture.Facts, collectionCapture().Facts) {
		t.Fatal("source facts/unknown times changed", err)
	}
	if calls.Load() != 7 {
		t.Fatal("implicit read/provider expansion")
	}
}
func TestHistoryCollectionClientValidatesBeforeUse(t *testing.T) {
	var c *client.Client
	for _, s := range []client.HistorySelection{{}, {PullNumbers: []int64{}}, {PullNumbers: []int64{1, 1}}, {PullNumbers: []int64{0}}, {PullNumbers: []int64{2147483648}}, {PullNumbers: make([]int64, 101)}} {
		_, err := c.CollectHistory(t.Context(), "key", s, 0)
		historyError(t, err, "validation_failed", false)
	}
	for _, seconds := range []int{-1, 901} {
		_, err := c.CollectHistory(t.Context(), "key", collectionOperation().Selection, seconds)
		historyError(t, err, "validation_failed", false)
		_, err = c.HistoryCollection(t.Context(), collectionID, seconds)
		historyError(t, err, "validation_failed", false)
	}
	for _, id := range []string{"", "history_collection_" + strings.Repeat("A", 32), collectionPageID, collectionID + "/escape"} {
		_, err := c.HistoryCollection(t.Context(), id, 0)
		historyError(t, err, "validation_failed", false)
	}
	for _, key := range []string{"", " padded", strings.Repeat("k", 129)} {
		_, err := c.CollectHistory(t.Context(), key, collectionOperation().Selection, 0)
		historyError(t, err, "validation_failed", false)
	}
	for _, revision := range []int64{0, -1, math.MaxInt64} {
		_, err := c.ChangeHistoryCollection(t.Context(), collectionID, "cancel", "key", revision)
		historyError(t, err, "validation_failed", false)
	}
	_, err := c.ChangeHistoryCollection(t.Context(), collectionID, "restart", "key", 1)
	historyError(t, err, "validation_failed", false)
	for _, limit := range []int{-1, 101} {
		_, err = c.HistoryPages(t.Context(), collectionID, limit, "")
		historyError(t, err, "validation_failed", false)
	}
	_, err = c.HistoryPages(t.Context(), collectionID, 1, "bad\ncursor")
	historyError(t, err, "validation_failed", false)
	_, err = c.HistoryCapture(t.Context(), collectionID, collectionID)
	historyError(t, err, "validation_failed", false)
}
func TestHistoryCollectionClientRejectsInvalidOperationOrReceipt(t *testing.T) {
	for name, change := range map[string]func(*client.HistoryCollection){
		"scope":      func(v *client.HistoryCollection) { v.ProjectID = "other-project" },
		"id":         func(v *client.HistoryCollection) { v.ID = collectionID + "a" },
		"repository": func(v *client.HistoryCollection) { v.RepositoryID = 0 },
		"selection":  func(v *client.HistoryCollection) { v.Selection.PullNumbers = []int64{47, 47} },
		"position":   func(v *client.HistoryCollection) { v.Position.PullIndex = 2 },
		"page":       func(v *client.HistoryCollection) { v.Position.Page = 2 },
		"state":      func(v *client.HistoryCollection) { v.State = "cancelled" },
		"reason":     func(v *client.HistoryCollection) { v.Reason = "unexpected" },
		"pass":       func(v *client.HistoryCollection) { v.PassNumber = 2 },
		"counts":     func(v *client.HistoryCollection) { v.RecordCount = 1 },
		"time":       func(v *client.HistoryCollection) { v.UpdatedAt = "not-a-timestamp" },
	} {
		t.Run(name, func(t *testing.T) {
			v := collectionOperation()
			change(&v)
			c := historyClient(t, func(w http.ResponseWriter, r *http.Request) { write(w, 200, v) })
			_, err := c.HistoryCollection(t.Context(), collectionID, 0)
			historyError(t, err, "invalid_response", false)
		})
	}
	for name, body := range map[string]string{
		"queued200":        mustHistoryJSON(t, collectionOperation()),
		"receipt-revision": `{"id":"` + collectionID + `","state":"queued","revision":2}`,
		"receipt-state":    `{"id":"` + collectionID + `","state":"running","revision":1}`,
		"missing":          `{"id":"` + collectionID + `","revision":1}`,
		"unknown":          `{"id":"` + collectionID + `","state":"queued","revision":1,"extra":true}`,
		"null":             `{"id":"` + collectionID + `","state":null,"revision":1}`,
		"duplicate":        `{"id":"` + collectionID + `","state":"queued","revision":1,"revision":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if name != "queued200" {
					w.WriteHeader(202)
				}
				_, _ = io.WriteString(w, body)
			})
			_, err := c.CollectHistory(t.Context(), "original-key", collectionOperation().Selection, 2)
			historyError(t, err, "invalid_response", true)
		})
	}
	// A terminal result must preserve the exact selected PR order.
	v := collectionOperation()
	v.State = "paused"
	v.Reason = "rate_limited"
	v.Selection.PullNumbers = []int64{12, 47}
	c := historyClient(t, func(w http.ResponseWriter, r *http.Request) { write(w, 200, v) })
	_, err := c.CollectHistory(t.Context(), "key", collectionOperation().Selection, 2)
	historyError(t, err, "invalid_response", true)
}
func mustHistoryJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHistoryCollectionClientPreservesBackwardWallClock(t *testing.T) {
	op := collectionOperation()
	op.State, op.Reason, op.Revision = "paused", "source_unavailable", 2
	op.UpdatedAt = "2026-10-16T23:59:59.123456789Z"
	var calls atomic.Int64
	c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		write(w, http.StatusOK, op)
	})
	got, err := c.HistoryCollection(t.Context(), collectionID, 0)
	if err != nil || !reflect.DeepEqual(got, op) {
		t.Fatal("read refused or rewrote valid retained wall-clock times", err)
	}
	result, err := c.CollectHistory(t.Context(), "original-key", op.Selection, 2)
	if err != nil || result.Operation == nil || result.Receipt != nil || !reflect.DeepEqual(*result.Operation, op) {
		t.Fatal("terminal collection result refused or rewrote valid wall-clock times", err)
	}
	if calls.Load() != 2 {
		t.Fatal("clock handling retried or expanded reads")
	}
}
func TestHistoryCollectionClientCaptureBoundsAndUntrustedFacts(t *testing.T) {
	// The selected PR capture is object-valued; review/comment captures are
	// arrays. Preserve both rather than interpreting RawMessage as []byte JSON.
	pull := collectionCapture()
	pull.Resource, pull.Page = "pull", 1
	pull.Facts = json.RawMessage(`{"Title":"Merged history","Number":47,"RepositoryID":42,"State":"closed","Merged":true,"HeadSHA":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","BaseSHA":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","BaseRef":"main","ChangedFiles":2}`)
	pull.Next = &client.HistoryPosition{Resource: "reviews", Page: 1}
	pc := historyClient(t, func(w http.ResponseWriter, r *http.Request) { write(w, 200, pull) })
	gotPull, pullErr := pc.HistoryCapture(t.Context(), collectionID, collectionPageID)
	if pullErr != nil || !bytes.Equal(gotPull.Facts, pull.Facts) {
		t.Fatal("object-valued pull capture refused or changed", pullErr)
	}
	wrongPull := pull
	wrongPull.RepositoryID++
	wrongClient := historyClient(t, func(w http.ResponseWriter, r *http.Request) { write(w, 200, wrongPull) })
	_, pullErr = wrongClient.HistoryCapture(t.Context(), collectionID, collectionPageID)
	historyError(t, pullErr, "invalid_response", false)
	for name, change := range map[string]func(*client.HistoryCapture){
		"version":     func(v *client.HistoryCapture) { v.Version = 2 },
		"repository":  func(v *client.HistoryCapture) { v.RepositoryName = "https://elsewhere" },
		"null-array":  func(v *client.HistoryCapture) { v.Facts = json.RawMessage("null"); v.Count = 0 },
		"wrong-count": func(v *client.HistoryCapture) { v.Count = 0 },
		"string-id":   func(v *client.HistoryCapture) { v.Facts = json.RawMessage(`[{"ID":"7","Body":"a","Author":{}}]`) },
		"duplicate-id": func(v *client.HistoryCapture) {
			v.Count = 2
			v.Facts = json.RawMessage(`[{"ID":7,"Body":"a","Author":{}},{"ID":7,"Body":"a","Author":{}}]`)
		},
		"missing-body":        func(v *client.HistoryCapture) { v.Facts = json.RawMessage(`[{"ID":7,"Author":{}}]`) },
		"null-body":           func(v *client.HistoryCapture) { v.Facts = json.RawMessage(`[{"ID":7,"Body":null,"Author":{}}]`) },
		"next-skips-page":     func(v *client.HistoryCapture) { v.Next = &client.HistoryPosition{Resource: v.Resource, Page: 4} },
		"next-skips-resource": func(v *client.HistoryCapture) { v.Next = &client.HistoryPosition{Resource: "reviews", Page: 1} },
		"null-time":           func(v *client.HistoryCapture) { v.CollectedAt = "0001-01-01T00:00:00Z" },
		"private-token": func(v *client.HistoryCapture) {
			v.Facts = json.RawMessage(`[{"ID":7,"Body":"` + testToken + `","Author":{}}]`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := collectionCapture()
			change(&v)
			c := historyClient(t, func(w http.ResponseWriter, r *http.Request) { write(w, 200, v) })
			_, err := c.HistoryCapture(t.Context(), collectionID, collectionPageID)
			historyError(t, err, "invalid_response", false)
		})
	}
	for _, body := range []string{
		strings.Replace(mustHistoryJSON(t, collectionCapture()), `"count":1`, `"count":null`, 1),
		strings.TrimSuffix(mustHistoryJSON(t, collectionCapture()), "}") + `,"next":{"resource":"pull","page":1}}`,
	} {
		c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		})
		_, err := c.HistoryCapture(t.Context(), collectionID, collectionPageID)
		historyError(t, err, "invalid_response", false)
	}
	var calls atomic.Int64
	c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		v := collectionCapture()
		size := 5 << 20
		if r.URL.Query().Get("oversize") == "yes" {
			size = 8 << 20
		}
		v.Facts = json.RawMessage(`[{"ID":7,"Body":"` + strings.Repeat("x", size) + `","Author":{}}]`)
		write(w, 200, v)
	})
	v, err := c.HistoryCapture(t.Context(), collectionID, collectionPageID)
	if err != nil || len(v.Facts) < 5<<20 {
		t.Fatal("explicit capture did not use selected larger bound", err)
	}
	// The selected capture bound remains finite; raw facts are limited to8MiB.
	over := collectionCapture()
	over.Facts = json.RawMessage(`[{"ID":7,"Body":"` + strings.Repeat("x", 8<<20) + `","Author":{}}]`)
	oversized := historyClient(t, func(w http.ResponseWriter, r *http.Request) { write(w, 200, over) })
	_, err = oversized.HistoryCapture(t.Context(), collectionID, collectionPageID)
	historyError(t, err, "invalid_response", false)
	_, err = oversized.HistoryCollection(t.Context(), collectionID, 0)
	historyError(t, err, "invalid_response", false)
	if calls.Load() != 1 {
		t.Fatal("capture implicitly fetched operation")
	}
}
func TestHistoryCollectionClientRejectsMalformedPageAndAction(t *testing.T) {
	summary := client.HistoryPageSummary{ID: collectionPageID, Sequence: 1, PassNumber: 1, PullNumber: 47, Resource: "pull", Page: 1, Count: 1, SHA256: strings.Repeat("b", 64), CreatedAt: "2026-10-17T00:00:00Z"}
	for name, change := range map[string]func(*client.HistoryPageList){
		"null":           func(v *client.HistoryPageList) { v.Items = nil },
		"duplicate":      func(v *client.HistoryPageList) { v.Items = append(v.Items, summary) },
		"wrong-pull":     func(v *client.HistoryPageList) { v.Items[0].PullNumber = 0 },
		"wrong-digest":   func(v *client.HistoryPageList) { v.Items[0].SHA256 = "a" },
		"pull-count":     func(v *client.HistoryPageList) { v.Items[0].Count = 0 },
		"missing-cursor": func(v *client.HistoryPageList) { v.HasMore = true },
		"extra-cursor":   func(v *client.HistoryPageList) { v.Cursor = "cursor" },
	} {
		t.Run(name, func(t *testing.T) {
			v := client.HistoryPageList{Items: []client.HistoryPageSummary{summary}}
			change(&v)
			c := historyClient(t, func(w http.ResponseWriter, r *http.Request) { write(w, 200, v) })
			_, err := c.HistoryPages(t.Context(), collectionID, 2, "")
			historyError(t, err, "invalid_response", false)
		})
	}
	c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, client.HistoryCollectionReceipt{ID: collectionID, Revision: 8, State: "canceled"})
	})
	_, err := c.ChangeHistoryCollection(t.Context(), collectionID, "cancel", "key", 8)
	historyError(t, err, "invalid_response", true)
}
func TestHistoryCollectionClientCancellationAndAcceptedErrors(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		for _, received := range []bool{false, true} {
			name := map[bool]string{false: "read", true: "post"}[mutation] + map[bool]string{false: "-unsent", true: "-received"}[received]
			t.Run(name, func(t *testing.T) {
				started, release := make(chan struct{}, 1), make(chan struct{})
				defer close(release)
				var calls atomic.Int64
				c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					started <- struct{}{}
					select {
					case <-r.Context().Done():
					case <-release:
					}
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if !received {
					cancel()
				}
				done := make(chan error, 1)
				go func() {
					if mutation {
						_, err := c.CollectHistory(ctx, "original-key", collectionOperation().Selection, 900)
						done <- err
					} else {
						_, err := c.HistoryCollection(ctx, collectionID, 900)
						done <- err
					}
				}()
				if received {
					select {
					case <-started:
					case <-time.After(time.Second):
						t.Fatal("request absent")
					}
					cancel()
				}
				select {
				case err := <-done:
					historyError(t, err, "canceled", mutation && received)
				case <-time.After(time.Second):
					t.Fatal("detachment blocked")
				}
				want := int64(0)
				if received {
					want = 1
				}
				if calls.Load() != want {
					t.Fatal("retried or sent canceled request")
				}
			})
		}
	}
	var calls atomic.Int64
	c := historyClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"revoked after acceptance","details":{"outcome_unknown":true,"operation_id":"`+collectionID+`"}},"request_id":"req-one"}`)
	})
	_, err := c.CollectHistory(t.Context(), "original-key", collectionOperation().Selection, 2)
	historyError(t, err, "forbidden", true)
	_, err = c.HistoryCollection(t.Context(), collectionID, 2)
	historyError(t, err, "forbidden", false)
	if calls.Load() != 2 {
		t.Fatal("accepted error retried")
	}
}
