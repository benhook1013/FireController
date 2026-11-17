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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	domain "github.com/benhook1013/FireController/internal/reviews"
)

func importDocument() json.RawMessage {
	return json.RawMessage(`{"expected_revision":0,"repository_id":701,"source":"prepared export","observed_at":"2026-10-15T00:00:00Z","queue":[],"retired":[]}`)
}
func importPreview() client.ReviewImportPreview {
	return client.ReviewImportPreview{Version: 1, ProjectID: "project-one", RepositoryID: 701, Source: "prepared export", ObservedAt: "2026-10-15T00:00:00Z", PayloadSHA256: strings.Repeat("a", 64), PreviewSHA256: strings.Repeat("b", 64), ImportedHistory: true}
}

func TestReviewImportClientPublicPreviewApplyAndSharedReceipt(t *testing.T) {
	f := newReviewClientFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='review-client-credential'`); err != nil {
		t.Fatal(err)
	}
	var originalTables string
	count := func() string {
		var s string
		if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_snapshots)||':'||(SELECT count(*) FROM receipts)||':'||(SELECT count(*) FROM events)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	originalTables = count()
	preview, err := f.client.PreviewReviewImport(t.Context(), importDocument())
	if err != nil {
		t.Fatal(err)
	}
	if count() != originalTables || preview.NativeCoverage || !preview.ImportedHistory || preview.Counts != (client.ReviewImportCounts{}) {
		t.Fatal("preview changed state/credit", preview)
	}
	var equivalent map[string]any
	json.Unmarshal(importDocument(), &equivalent)
	equivalent["queue"] = nil
	delete(equivalent, "retired")
	formatted, _ := json.MarshalIndent(equivalent, "", "  ")
	normalized, err := f.client.PreviewReviewImport(t.Context(), formatted)
	if err != nil || normalized.PreviewSHA256 != preview.PreviewSHA256 || normalized.PayloadSHA256 != preview.PayloadSHA256 {
		t.Fatal("format/null normalization changed identity", normalized, err)
	}
	receipt, err := f.client.ApplyReviewImport(t.Context(), "apply-once", preview.PreviewSHA256, importDocument())
	if err != nil || receipt.Revision != 1 {
		t.Fatal(receipt, err)
	}
	// The old observation endpoint and prepared apply share one receipt namespace.
	request := httptest.NewRequest("POST", "/api/v1/projects/project-one/reviews/observations", strings.NewReader(string(importDocument())))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-FC-Instance", f.store.InstanceID())
	request.Header.Set("X-FC-Worker", "worker-one")
	request.Header.Set("Idempotency-Key", "apply-once")
	w := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(w, request)
	var old domain.Receipt
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &old) != nil || old.ID != receipt.ID || old.Revision != receipt.Revision {
		t.Fatal("receipt namespace diverged", w.Code, w.Body.String())
	}
	saved := count()
	replay, err := f.client.ApplyReviewImport(t.Context(), "apply-once", preview.PreviewSHA256, formatted)
	if err != nil || replay != receipt || count() != saved {
		t.Fatal("exact semantic replay changed import", replay, err)
	}
	_, err = f.client.ApplyReviewImport(t.Context(), "other-key", strings.Repeat("c", 64), importDocument())
	checkError(t, err, "import_preview_mismatch")
	var failure *client.Error
	if !errors.As(err, &failure) || failure.OutcomeUnknown || failure.Status != 409 || failure.Action != "Preview the selected document again and inspect the new preview before applying." {
		t.Fatal("prewrite refusal guidance/status became unknown", err)
	}
}

func TestReviewImportClientStrictPreviewAndUnknownApply(t *testing.T) {
	var response atomic.Value
	response.Store(`{}`)
	var status atomic.Int64
	status.Store(200)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.URL.RawQuery != "" {
			t.Error("import transport changed")
		}
		if strings.HasSuffix(r.URL.Path, "preview") && r.Header.Get("Idempotency-Key") != "" {
			t.Error("preview marked as mutation")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, response.Load().(string))
	}))
	defer server.Close()
	c := newClient(t, server)
	base, _ := json.Marshal(importPreview())
	edits := []func(map[string]any){func(m map[string]any) { delete(m, "native_coverage") }, func(m map[string]any) { m["imported_history"] = nil }, func(m map[string]any) { m["native_coverage"] = true }, func(m map[string]any) { m["project_id"] = "other" }, func(m map[string]any) { m["source"] = "other" }, func(m map[string]any) { m["repository_id"] = 702 }, func(m map[string]any) { m["preview_sha256"] = strings.Repeat("B", 64) }, func(m map[string]any) { m["counts"].(map[string]any)["runs"] = 201 }, func(m map[string]any) { m["counts"].(map[string]any)["findings"] = 1 }, func(m map[string]any) { m["queue"] = []any{} }, func(m map[string]any) { m["Version"] = 1 }}
	for _, edit := range edits {
		var m map[string]any
		json.Unmarshal(base, &m)
		edit(m)
		raw, _ := json.Marshal(m)
		response.Store(string(raw))
		_, err := c.PreviewReviewImport(t.Context(), importDocument())
		checkError(t, err, "invalid_response")
		var failure *client.Error
		if !errors.As(err, &failure) || failure.OutcomeUnknown {
			t.Fatal("preview failure became mutation")
		}
	}
	response.Store(strings.TrimSuffix(string(base), "}") + `,"native_coverage":false}`)
	_, err := c.PreviewReviewImport(t.Context(), importDocument())
	checkError(t, err, "invalid_response")
	response.Store(string(base))
	if _, err = c.PreviewReviewImport(t.Context(), importDocument()); err != nil {
		t.Fatal(err)
	}
	response.Store(`{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":1,"outcome":"observed"}`)
	status.Store(201)
	got, err := c.ApplyReviewImport(t.Context(), "retained-key", strings.Repeat("b", 64), importDocument())
	if err != nil || got.Revision != 1 {
		t.Fatal(got, err)
	}
	response.Store(`{"id":"foreign","revision":1,"outcome":"observed"}`)
	before := calls.Load()
	_, err = c.ApplyReviewImport(t.Context(), "retained-key", strings.Repeat("b", 64), importDocument())
	checkError(t, err, "invalid_response")
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls.Load() != before+1 {
		t.Fatal("apply ambiguity not single-shot", err)
	}
}

func TestReviewImportClientLocalValidationAndPreviewCancellation(t *testing.T) {
	var calls atomic.Int64
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	c := newClient(t, server)
	bad := []json.RawMessage{nil, []byte(`null`), []byte(`[]`), []byte(`{"expected_revision":0,"expected_revision":0}`), []byte(`{"expected_revision":null,"repository_id":701,"source":"x","observed_at":"2026-10-15T00:00:00Z"}`), []byte(strings.Replace(string(importDocument()), `"queue":[]`, `"queue":{}`, 1)), []byte(strings.Replace(string(importDocument()), `"queue":[]`, `"queue":[{"x":1,"x":2}]`, 1)), []byte(strings.Replace(string(importDocument()), `"source":"prepared export"`, `"source":"\ud800"`, 1)), []byte(strings.Repeat(" ", 256<<10) + string(importDocument()))}
	for _, document := range bad {
		if _, err := c.PreviewReviewImport(t.Context(), document); err == nil {
			t.Fatal("invalid local document accepted")
		}
	}
	for _, key := range []string{"", "bad\nkey"} {
		if _, err := c.ApplyReviewImport(t.Context(), key, strings.Repeat("b", 64), importDocument()); err == nil {
			t.Fatal("invalid apply key accepted")
		}
	}
	if _, err := c.ApplyReviewImport(t.Context(), "key", strings.Repeat("B", 64), importDocument()); err == nil {
		t.Fatal("bad digest accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("local validation called HTTP")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := c.PreviewReviewImport(ctx, importDocument()); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("preview did not start")
	}
	cancel()
	select {
	case err := <-done:
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "canceled" || failure.OutcomeUnknown {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("preview cancellation blocked")
	}
	if calls.Load() != 1 {
		t.Fatal("preview retried")
	}
}

// Unicode escapes remain in RawMessage's compact wire encoding, while the
// service decodes them before normalizing its observation. Every source field
// here is domain-valid; only the local same-file apply wire size is at issue.
func importEscapedBudgetDocument(t *testing.T, target int) json.RawMessage {
	t.Helper()
	zero := int64(0)
	req := domain.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 701, Source: "prepared export", ObservedAt: "2026-10-15T00:00:00Z", Queue: []domain.PullInput{{Number: 1, Title: "Bounded imported history", State: "open", CIState: "unknown", Mergeability: "unknown", Runs: []domain.RunInput{{SourceRunID: "budget-run", Channel: "manual", Scope: "selected history", Outcome: "unknown", Found: 3}}}}}
	encode := func(n int) []byte {
		findings := make([]domain.Finding, 3)
		for i := range findings {
			size := 16 << 10
			if i == 2 {
				size = n
			}
			findings[i] = domain.Finding{SourceFindingID: fmt.Sprintf("budget-finding-%d", i), Title: "Retained finding", Detail: strings.Repeat("z", size), Severity: "unknown", Disposition: "unadjudicated"}
		}
		req.Queue[0].Runs[0].Findings = findings
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.ReplaceAll(raw, []byte("z"), []byte(`\u007a`))
	}
	low, high := 0, 16<<10
	for low < high {
		mid := (low + high + 1) / 2
		if len(encode(mid)) <= target {
			low = mid
		} else {
			high = mid - 1
		}
	}
	body := encode(low)
	if len(body) > target || len(body) < target-6 {
		t.Fatal("boundary fixture did not reach requested size", len(body), target)
	}
	return body
}

func TestReviewImportClientPreviewApplyWireBudget(t *testing.T) {
	f := newReviewClientFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='review-client-credential'`); err != nil {
		t.Fatal(err)
	}
	document := importEscapedBudgetDocument(t, (256<<10)-32)
	wrapper, _ := json.Marshal(struct {
		Preview     string          `json:"preview_sha256"`
		Observation json.RawMessage `json:"observation"`
	}{strings.Repeat("a", 64), document})
	if len(document) > 256<<10 || len(wrapper) <= 256<<10 {
		t.Fatal("fixture did not isolate wrapper overhead", len(document), len(wrapper))
	}
	// The unchanged public service accepts this syntactically larger document:
	// its decoded/normalized representation and apply wrapper fit comfortably.
	request := httptest.NewRequest("POST", "/api/v1/projects/project-one/reviews/import/preview", bytes.NewReader(document))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-FC-Instance", f.store.InstanceID())
	request.Header.Set("X-FC-Worker", "worker-one")
	w := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(w, request)
	if w.Code != 200 {
		t.Fatal("boundary source is not an admissible server preview", w.Code, w.Body.String())
	}
	calls := f.calls.Load()
	_, err := f.client.PreviewReviewImport(t.Context(), document)
	if err == nil || f.calls.Load() != calls {
		t.Fatalf("unapplyable same-file preview dispatched: error=%v domain_calls=%d", err, f.calls.Load()-calls)
	}
	checkError(t, err, "validation_failed")
	var failure *client.Error
	if !errors.As(err, &failure) || failure.OutcomeUnknown || !strings.Contains(failure.Action, "smaller") {
		t.Fatal("wire-size refusal lost safe guidance", err)
	}
	// Nearby valid escaped input still previews and applies without rewriting it.
	within := importEscapedBudgetDocument(t, (256<<10)-256)
	preview, err := f.client.PreviewReviewImport(t.Context(), within)
	if err != nil {
		t.Fatal("valid near-bound file refused", err)
	}
	receipt, err := f.client.ApplyReviewImport(t.Context(), "near-wire-bound", preview.PreviewSHA256, within)
	if err != nil || receipt.Revision != 1 {
		t.Fatal("same near-bound file could not apply", receipt, err)
	}
}
