package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	domain "github.com/benhook1013/FireController/internal/reviews"
)

func queueEditDocument(revision int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"expected_revision":%d,"repository_id":701,"source":"explicit queue editor","queue":[{"number":1,"title":"Selected metadata","state":"open","ci_state":"unknown","mergeability":"unknown"}]}`, revision))
}

func TestReviewQueueEditClientValidationBeforeIO(t *testing.T) {
	good := queueEditDocument(0)
	var c *client.Client // Invalid requests must return before touching client/binding.
	bad := map[string][]byte{
		"empty": nil, "null": []byte(`null`), "array": []byte(`[]`), "oversize": bytes.Repeat([]byte(" "), (256<<10)+1),
		"duplicate":         []byte(strings.Replace(string(good), `"source":`, `"source":"x","source":`, 1)),
		"case":              []byte(strings.Replace(string(good), `"source":`, `"Source":`, 1)),
		"unknown":           []byte(strings.Replace(string(good), `"queue":`, `"observed_at":"2026-01-01T00:00:00Z","queue":`, 1)),
		"missing_revision":  []byte(strings.Replace(string(good), `"expected_revision":0,`, "", 1)),
		"null_revision":     []byte(strings.Replace(string(good), `"expected_revision":0`, `"expected_revision":null`, 1)),
		"negative_revision": queueEditDocument(-1),
		"null_queue":        []byte(strings.Replace(string(good), `"queue":[{"number":1,"title":"Selected metadata","state":"open","ci_state":"unknown","mergeability":"unknown"}]`, `"queue":null`, 1)),
		"null_retired":      []byte(strings.TrimSuffix(string(good), "}") + `,"retired":null}`),
		"null_optional":     []byte(strings.Replace(string(good), `"title":`, `"head_sha":null,"title":`, 1)),
		"bad_unicode":       []byte(strings.Replace(string(good), `Selected metadata`, `\ud800`, 1)),
		"number_overflow":   []byte(strings.Replace(string(good), `"number":1`, `"number":2147483648`, 1)),
		"number_fraction":   []byte(strings.Replace(string(good), `"number":1`, `"number":1.0`, 1)),
		"blank_source":      []byte(strings.Replace(string(good), `explicit queue editor`, `   `, 1)),
		"history":           []byte(strings.Replace(string(good), `"title":`, `"runs":[],"title":`, 1)),
		"complete":          []byte(strings.Replace(string(good), `"title":`, `"complete":false,"title":`, 1)),
		"nested_duplicate":  []byte(strings.Replace(string(good), `"number":1`, `"number":1,"number":2`, 1)),
		"uppercase_sha":     []byte(strings.Replace(string(good), `"title":`, `"head_sha":"`+strings.Repeat("A", 40)+`","title":`, 1)),
		"state":             []byte(strings.Replace(string(good), `"state":"open"`, `"state":"ready"`, 1)),
		"title_bytes":       []byte(strings.Replace(string(good), `Selected metadata`, strings.Repeat("é", 251), 1)),
		"overlap":           []byte(strings.TrimSuffix(string(good), "}") + `,"retired":[{"number":1,"reason":"defunct"}]}`),
		"retirement_null":   []byte(`{"expected_revision":0,"repository_id":701,"source":"x","retired":[{"number":2,"reason":null}]}`),
		"retirement_reason": []byte(`{"expected_revision":0,"repository_id":701,"source":"x","retired":[{"number":2,"reason":"merged"}]}`),
		"no_edits":          []byte(`{"expected_revision":0,"repository_id":701,"source":"x"}`),
	}
	for _, order := range []string{`null`, `[]`, `[1,1]`, `[0]`, `[-1]`, `[2147483648]`, `[1.0]`, `[null]`, `["1"]`, `true`, `[` + strings.Repeat(`1,`, 100) + `2]`} {
		bad["order_"+order] = []byte(`{"expected_revision":0,"repository_id":701,"source":"x","order":` + order + `}`)
	}
	bad["retiring_order"] = []byte(`{"expected_revision":0,"repository_id":701,"source":"x","retired":[{"number":1,"reason":"defunct"}],"order":[1]}`)
	for name, document := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := c.EditReviewQueue(t.Context(), "selected-key", document)
			checkError(t, err, "validation_failed")
		})
	}
	for _, key := range []string{"", "bad\nkey"} {
		_, err := c.EditReviewQueue(t.Context(), key, good)
		checkError(t, err, "validation_failed")
	}
	for _, field := range []string{"number", "title", "state", "ci_state", "mergeability"} {
		var m map[string]any
		_ = json.Unmarshal(good, &m)
		delete(m["queue"].([]any)[0].(map[string]any), field)
		raw, _ := json.Marshal(m)
		if err := client.ValidateReviewQueueEditDocument(raw); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
	}
	for _, document := range [][]byte{good, []byte(`{"expected_revision":0,"repository_id":701,"source":"x","retired":[{"number":2147483647,"reason":"superseded"}]}`), []byte(strings.Replace(string(good), `"title":`, `"head_sha":"","base_ref":"","base_sha":"","title":`, 1))} {
		if err := client.ValidateReviewQueueEditDocument(document); err != nil {
			t.Fatal("valid omitted/empty fields refused", err)
		}
	}
	if err := client.ValidateReviewQueueEditDocument([]byte(`{"expected_revision":0,"repository_id":701,"source":"x","order":[2147483647,1]}`)); err != nil {
		t.Fatal("valid order-only refused", err)
	}
	// Count and normalized JSON budgets are checked locally, including HTML escapes.
	var entries []any
	for n := 1; n <= 100; n++ {
		entries = append(entries, map[string]any{"number": n, "title": "Metadata", "state": "merged", "ci_state": "success", "mergeability": "mergeable"})
	}
	request := map[string]any{"expected_revision": 0, "repository_id": 701, "source": "x", "queue": entries}
	raw, _ := json.Marshal(request)
	if err := client.ValidateReviewQueueEditDocument(raw); err != nil {
		t.Fatal(err)
	}
	request["retired"] = []any{map[string]any{"number": 101, "reason": "defunct"}}
	raw, _ = json.Marshal(request)
	if client.ValidateReviewQueueEditDocument(raw) == nil {
		t.Fatal("combined count101 accepted")
	}
	delete(request, "retired")
	for _, e := range entries {
		e.(map[string]any)["title"] = strings.Repeat("<", 500)
	}
	var escaped bytes.Buffer
	encoder := json.NewEncoder(&escaped)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(request)
	if escaped.Len() >= 256<<10 || client.ValidateReviewQueueEditDocument(escaped.Bytes()) == nil {
		t.Fatal("normalized over-budget document accepted")
	}
}

func TestReviewQueueEditClientTransportStrictReceiptAndNoRetry(t *testing.T) {
	document := queueEditDocument(0)
	var response atomic.Value
	response.Store(`{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":1,"outcome":"queue_edited"}`)
	var status atomic.Int64
	status.Store(201)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/queue/edits" || r.URL.RawQuery != "" || !bytes.Equal(body, document) || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Idempotency-Key") != "selected-key" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Prefer") != "" {
			t.Error("queue edit transport/binding changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, response.Load().(string))
	}))
	defer server.Close()
	c := newClient(t, server)
	receipt, err := c.EditReviewQueue(t.Context(), "selected-key", document)
	if err != nil || receipt.Revision != 1 || receipt.Outcome != "queue_edited" {
		t.Fatal(receipt, err)
	}
	for _, raw := range []string{`{}`, `{"id":null,"revision":1,"outcome":"queue_edited"}`, `{"id":"foreign","revision":1,"outcome":"queue_edited"}`, `{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":2,"outcome":"queue_edited"}`, `{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":1,"outcome":"observed"}`, `{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":1,"Outcome":"queue_edited"}`, `{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":1,"outcome":"queue_edited","extra":1}`, `{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":1,"revision":1,"outcome":"queue_edited"}`, strings.Repeat("x", 4097)} {
		response.Store(raw)
		before := calls.Load()
		_, err = c.EditReviewQueue(t.Context(), "selected-key", document)
		checkError(t, err, "invalid_response")
		var failure *client.Error
		if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls.Load() != before+1 {
			t.Fatal("dispatched invalid receipt lost uncertainty/no-retry", err)
		}
	}
	status.Store(200)
	response.Store(`{}`)
	_, err = c.EditReviewQueue(t.Context(), "selected-key", document)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown {
		t.Fatal("unexpected success status was definite", err)
	}
	before := calls.Load()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.EditReviewQueue(ctx, "selected-key", document)
	if !errors.As(err, &failure) || failure.OutcomeUnknown || calls.Load() != before {
		t.Fatal("pre-dispatch cancellation sent request", err)
	}
}

func TestReviewQueueEditClientMetadataActualHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, fields, state, mergedAt string
		diff                          *client.ReviewPullDiff
		draft                         *bool
	}{
		{"known-zero-false", `"diff":{"files":0,"additions":0,"deletions":0},"is_draft":false,`, "open", "", &client.ReviewPullDiff{}, func() *bool { v := false; return &v }()},
		{"merged", `"diff":{"files":3,"additions":12,"deletions":4},"merged_at":"2026-10-19T22:10:11.123456789Z",`, "merged", "2026-10-19T22:10:11.123456789Z", &client.ReviewPullDiff{Files: 3, Additions: 12, Deletions: 4}, nil},
		{"unknown", "", "open", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewClientFixture(t)
			if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='review-client-credential'`); err != nil {
				t.Fatal(err)
			}
			document := json.RawMessage(strings.Replace(string(queueEditDocument(0)), `"title":`, tc.fields+`"title":`, 1))
			document = json.RawMessage(strings.Replace(string(document), `"state":"open"`, `"state":"`+tc.state+`"`, 1))
			handler := f.server.Config.Handler
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/queue/edits") {
					raw, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(raw, document) {
						t.Error("queue metadata wire bytes changed", err)
					}
					r.Body = io.NopCloser(bytes.NewReader(raw))
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			c, err := client.New(t.Context(), profileBinding(t, server.URL, f.store.InstanceID(), f.project))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			receipt, err := c.EditReviewQueue(t.Context(), "metadata-edit", document)
			if err != nil || receipt.Revision != 1 {
				t.Fatal(receipt, err)
			}
			pull, err := c.ReviewPull(t.Context(), 1, receipt.ID)
			if err != nil || !reflect.DeepEqual(pull.Pull.Diff, tc.diff) || !reflect.DeepEqual(pull.Pull.IsDraft, tc.draft) || pull.Pull.MergedAt != tc.mergedAt {
				t.Fatal("metadata lost known/unknown values", pull.Pull, err)
			}
		})
	}
}

func TestReviewQueueEditClientOrderOnlyActualHTTP(t *testing.T) {
	f := newReviewClientFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='review-client-credential'`); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(t.Context(), profileBinding(t, f.server.URL, f.store.InstanceID(), f.project))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	seed := json.RawMessage(`{"expected_revision":0,"repository_id":701,"source":"order seed","queue":[{"number":1,"title":"One","state":"open","ci_state":"unknown","mergeability":"unknown"},{"number":2,"title":"Two","state":"open","ci_state":"unknown","mergeability":"unknown"},{"number":3,"title":"Three","state":"open","ci_state":"unknown","mergeability":"unknown"}]}`)
	if _, err := c.EditReviewQueue(t.Context(), "order-seed", seed); err != nil {
		t.Fatal(err)
	}
	document := json.RawMessage(`{"expected_revision":1,"repository_id":701,"source":"order-only","order":[3,1]}`)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/queue/edits") {
			calls++
			raw, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(raw, document) {
				t.Error("order-only wire changed", string(raw), err)
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		f.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	bound, err := client.New(t.Context(), profileBinding(t, server.URL, f.store.InstanceID(), f.project))
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	receipt, err := bound.EditReviewQueue(t.Context(), "order-only", document)
	if err != nil || receipt.Revision != 2 || calls != 1 {
		t.Fatal(receipt, err, calls)
	}
	queue, err := bound.ReviewQueue(t.Context(), client.PageOptions{})
	if err != nil || len(queue.Items) != 3 {
		t.Fatal(queue, err)
	}
	for i, n := range []int64{3, 2, 1} {
		if queue.Items[i].Number != n {
			t.Fatal(queue.Items)
		}
	}
}

func TestReviewQueueEditClientMalformedMetadataBeforeIO(t *testing.T) {
	var c *client.Client
	for _, fields := range []string{
		`"diff":null`, `"diff":[]`, `"diff":{}`, `"diff":{"files":0,"additions":0}`,
		`"diff":{"files":null,"additions":0,"deletions":0}`, `"diff":{"files":0.5,"additions":0,"deletions":0}`,
		`"diff":{"files":-1,"additions":0,"deletions":0}`, `"diff":{"files":2147483648,"additions":0,"deletions":0}`,
		`"diff":{"files":0,"additions":0,"deletions":0,"extra":0}`, `"diff":{"files":0,"files":1,"additions":0,"deletions":0}`,
		`"is_draft":null`, `"is_draft":"false"`, `"is_draft":0`, `"is_draft":false,"is_draft":true`,
		`"merged_at":null`, `"merged_at":0`, `"merged_at":"not-an-instant"`, `"merged_at":"2026-10-19T22:10:11Z"`,
	} {
		document := json.RawMessage(strings.Replace(string(queueEditDocument(0)), `"title":`, fields+`,"title":`, 1))
		if fields == `"merged_at":"not-an-instant"` {
			document = json.RawMessage(strings.Replace(string(document), `"state":"open"`, `"state":"merged"`, 1))
		}
		if client.ValidateReviewQueueEditDocument(document) == nil {
			t.Fatal("malformed metadata admitted", fields)
		}
		_, err := c.EditReviewQueue(t.Context(), "metadata-invalid", document)
		checkError(t, err, "validation_failed")
	}
}

func TestReviewQueueEditClientLostResponseCancelAndRealReplay(t *testing.T) {
	f := newReviewClientFixture(t)
	old := f.importSnapshot(t, func(r *domain.ObservationRequest) {
		r.Queue = append(r.Queue, domain.PullInput{Number: 2, Title: "Retained merged PR", State: "merged", CIState: "success", Mergeability: "mergeable"})
	})
	// The normal worker cannot edit; the service owns administrator admission.
	_, err := f.client.EditReviewQueue(t.Context(), "edit-once", queueEditDocument(1))
	checkError(t, err, "forbidden")
	if _, err = f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='review-client-credential'`); err != nil {
		t.Fatal(err)
	}
	beforeRuns, err := f.client.ReviewRuns(t.Context(), 1, client.ReviewRunOptions{Snapshot: old})
	if err != nil || len(beforeRuns.Items) != 1 {
		t.Fatal(err)
	}
	original, err := f.client.ReviewRun(t.Context(), beforeRuns.Items[0].ID, old)
	if err != nil {
		t.Fatal(err)
	}
	handler := f.server.Config.Handler
	var posts atomic.Int64
	dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/queue/edits") {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			if recorder.Code == 201 && posts.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = io.WriteString(w, `{"id":`)
				return
			}
			for key, values := range recorder.Header() {
				for _, v := range values {
					w.Header().Add(key, v)
				}
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer dropped.Close()
	c, err := client.New(t.Context(), profileBinding(t, dropped.URL, f.store.InstanceID(), f.project))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.EditReviewQueue(t.Context(), "edit-once", queueEditDocument(1))
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown {
		t.Fatal("lost committed receipt not unknown", err)
	}
	receipt, err := c.EditReviewQueue(t.Context(), "edit-once", queueEditDocument(1))
	if err != nil || receipt.Revision != 2 || posts.Load() != 2 {
		t.Fatal(receipt, err)
	}
	// Advance workspace, then original CAS/key replay still returns original receipt.
	next := []byte(`{"expected_revision":2,"repository_id":701,"source":"second editor","retired":[{"number":2,"reason":"defunct"}]}`)
	if _, err = c.EditReviewQueue(t.Context(), "second-edit", next); err != nil {
		t.Fatal(err)
	}
	replay, err := c.EditReviewQueue(t.Context(), "edit-once", queueEditDocument(1))
	if err != nil || replay != receipt {
		t.Fatal("original receipt lost after advancement", replay, err)
	}
	detail, err := c.ReviewRun(t.Context(), beforeRuns.Items[0].ID, "")
	if err != nil || !reflect.DeepEqual(detail.Run, original.Run) || !reflect.DeepEqual(detail.Findings, original.Findings) {
		t.Fatal("queue edit changed retained run detail", err)
	}
	historical, err := c.ReviewRun(t.Context(), beforeRuns.Items[0].ID, old)
	if err != nil || !reflect.DeepEqual(historical, original) {
		t.Fatal("old snapshot changed", err)
	}
	var snapshots, receipts int
	if err = f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_snapshots),(SELECT count(*) FROM receipts)`).Scan(&snapshots, &receipts); err != nil || snapshots != 3 || receipts != 3 {
		t.Fatalf("replay duplicated state %d/%d %v", snapshots, receipts, err)
	}
	t.Run("cancel-dispatched", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var sent atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			sent.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			<-release
		}))
		defer srv.Close()
		c := newClient(t, srv)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := c.EditReviewQueue(ctx, "selected-key", queueEditDocument(0)); done <- err }()
		<-entered
		cancel()
		err := <-done
		close(release)
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "canceled" || !failure.OutcomeUnknown || sent.Load() != 1 {
			t.Fatal("dispatched cancellation lost uncertainty", err)
		}
	})
}
