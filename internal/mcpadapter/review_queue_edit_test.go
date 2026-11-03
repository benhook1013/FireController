package mcpadapter_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewQueueEditMCPActualAPIAdminLostReceiptAndReplay(t *testing.T) {
	f := mutationFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	reviews.Register(mux, f.store)
	var lose atomic.Bool
	lose.Store(true)
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews/queue/edits") {
			posts.Add(1)
			if r.Method != "POST" || r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("Idempotency-Key") == "" {
				t.Error("queue edit binding changed")
			}
			record := httptest.NewRecorder()
			mux.ServeHTTP(record, r)
			if record.Code == 201 && lose.CompareAndSwap(true, false) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"id":`))
				return
			}
			for k, vs := range record.Header() {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(record.Code)
			_, _ = w.Write(record.Body.Bytes())
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	entries := []any{}
	for n := 1; n <= 100; n++ {
		entries = append(entries, map[string]any{"number": n, "title": fmt.Sprintf("Merged retained %d ", n) + strings.Repeat("x", 200), "state": "merged", "ci_state": "not_required", "mergeability": "unknown"})
	}
	document := map[string]any{"expected_revision": 0, "repository_id": 1, "source": "selected admin queue editor", "queue": entries}
	input := map[string]any{"document": document, "idempotency_key": "original-queue-edit"}
	encoded, _ := json.Marshal(input)
	if len(encoded) <= 16<<10 {
		t.Fatal("fixture did not exercise full native document budget")
	}
	diagnosticResult(t, call(t, session, "review_queue_edit", input), "forbidden", false)
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE is_admin=0`); err != nil {
		t.Fatal(err)
	}
	diagnosticResult(t, call(t, session, "review_queue_edit", input), "invalid_response", true)
	var first, replay client.ReviewQueueEditReceipt
	decodeResult(t, call(t, session, "review_queue_edit", input), &first)
	if first.Revision != 1 || first.Outcome != "queue_edited" || posts.Load() != 3 {
		t.Fatal("committed lost receipt retried automatically or changed", first, posts.Load())
	}
	later := map[string]any{"document": map[string]any{"expected_revision": 1, "repository_id": 1, "source": "explicit superseded cleanup", "retired": []any{map[string]any{"number": 2, "reason": "superseded"}}}, "idempotency_key": "retire-two"}
	var second client.ReviewQueueEditReceipt
	decodeResult(t, call(t, session, "review_queue_edit", later), &second)
	decodeResult(t, call(t, session, "review_queue_edit", input), &replay)
	if replay != first || second.Revision != 2 {
		t.Fatal("old original key did not replay", first, replay, second)
	}
	var queue client.ReviewQueue
	decodeResult(t, call(t, session, "review_queue", map[string]any{"limit": 100}), &queue)
	if len(queue.Items) != 99 {
		t.Fatal("omitted merged history was lost", len(queue.Items))
	}
	for _, p := range queue.Items {
		if p.Number == 2 || p.State != "merged" || p.CIState != "not_required" {
			t.Fatal("retirement changed omitted metadata", p)
		}
	}
	var snapshots, receipts int
	if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_snapshots),(SELECT count(*) FROM receipts WHERE operation='reviews.queue.edit')`).Scan(&snapshots, &receipts); err != nil || snapshots != 2 || receipts != 2 || f.providers.Load() != 1 {
		t.Fatal("replay or queue edit performed extra work", snapshots, receipts, err)
	}
}

func TestReviewQueueEditMCPStrictArgumentsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": binding}, "missing-queue-edit-credential"), "alpha"), "")
	base := `{"document":{"expected_revision":0,"repository_id":1,"source":"selected","retired":[{"number":2,"reason":"defunct"}]},"idempotency_key":"chosen"}`
	for _, raw := range []string{`{}`, `null`, strings.Replace(base, `"document":{`, `"document":null,"extra":{`, 1), strings.Replace(base, `"idempotency_key":"chosen"`, `"idempotency_key":null`, 1), strings.Replace(base, `"expected_revision":0`, `"expected_revision":null`, 1), strings.Replace(base, `"number":2`, `"number":2.5`, 1), strings.Replace(base, `"number":2`, `"number":2,"number":3`, 1), strings.Replace(base, `"retired":`, `"runs":[],"retired":`, 1), strings.Replace(base, `"reason":"defunct"`, `"reason":"merged"`, 1), strings.Replace(base, `"idempotency_key":"chosen"`, `"idempotency_key":"chosen","idempotency_key":"other"`, 1)} {
		errorResult(t, call(t, session, "review_queue_edit", json.RawMessage(raw)), "invalid_arguments")
	}
	if requests.Load() != 0 {
		t.Fatal("invalid document contacted service")
	}
}
