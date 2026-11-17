package mcpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewSelectionBusyMCPExposesReadableOriginalHandle(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, srv.URL), "chosen")
	var first client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_start", map[string]any{"channel": "cli", "idempotency_key": "accepted-start", "wait_seconds": 0}), &first)
	counts := func() [3]int64 {
		var out [3]int64
		for i, table := range []string{"review_selections", "review_operations", "receipts"} {
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&out[i]); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before, providers := counts(), f.providers.Load()
	for range 2 {
		failure := diagnosticResult(t, call(t, session, "review_start", map[string]any{"channel": "cli", "idempotency_key": "refused-start", "expected_pull_number": 3386, "wait_seconds": 0}), "review_busy", false)
		details, ok := failure.Details.(map[string]any)
		if !ok || details["selection_id"] != first.ID || details["channel"] != "cli" || details["state"] != "queued" || !strings.Contains(failure.Message, "not accepted") || !strings.Contains(failure.Action, "review_selection_read selection_id="+first.ID) || counts() != before {
			t.Fatal("MCP busy response hid selection or changed pending work", failure)
		}
	}
	var state client.ReviewRunState
	decodeResult(t, call(t, session, "review_selection_read", map[string]any{"selection_id": first.ID}), &state)
	if state.Selection.ID != first.ID || state.Selection.State != "queued" || state.Operation != nil || counts() != before || posts.Load() != 3 || f.providers.Load() != providers {
		t.Fatal("MCP recovery read started extra work", state)
	}
}
