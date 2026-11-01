package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	domain "github.com/benhook1013/FireController/internal/reviews"
)

func transferDocument() json.RawMessage {
	zero := int64(0)
	pull := domain.PullInput{Number: 7, Title: "Selected retained history", State: "open", CIState: "unknown", Mergeability: "unknown", Runs: []domain.RunInput{{SourceRunID: "shared-run", Channel: "manual", Scope: "selected history", Outcome: "unknown", Found: 1, Findings: []domain.Finding{{SourceFindingID: "shared-finding", Title: "Retained finding", Detail: "PRIVATE_TRANSFER_DETAIL", Severity: "unknown", Disposition: "unadjudicated"}}}}, Attempts: []domain.Attempt{{SourceAttemptID: "shared-attempt", Channel: "manual", State: "unknown", SourceRunID: "shared-run"}}}
	req := domain.TransferRequest{Version: 1, ExpectedRevision: &zero, RepositoryID: 701, Publisher: "prepared transfer", Groups: []domain.TransferGroup{{Source: "original-a", Pulls: []domain.PullInput{pull}}, {Source: "original-b", Pulls: []domain.PullInput{pull}}}}
	raw, _ := json.Marshal(req)
	return raw
}
func transferPreview() client.ReviewTransferPreview {
	return client.ReviewTransferPreview{Version: 1, ProjectID: "project-one", RepositoryID: 701, Publisher: "prepared transfer", ExpectedRevision: 0, PayloadSHA256: strings.Repeat("a", 64), PreviewSHA256: strings.Repeat("b", 64), Counts: client.ReviewTransferCounts{Pulls: 1, Runs: 2, Attempts: 2, Findings: 2}, ImportedHistory: true, Coverage: "partial"}
}

func TestReviewTransferClientRealPreviewApplyAndReplay(t *testing.T) {
	f := newReviewClientFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='review-client-credential'`); err != nil {
		t.Fatal(err)
	}
	count := func() string {
		var s string
		if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_snapshots)||':'||(SELECT count(*) FROM receipts)||':'||(SELECT count(*) FROM events)`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := count()
	preview, err := f.client.PreviewReviewTransfer(t.Context(), transferDocument())
	if err != nil {
		t.Fatal(err)
	}
	if preview.Counts != transferPreview().Counts || preview.Coverage != "partial" || preview.NativeCoverage || !preview.ImportedHistory || count() != before {
		t.Fatal("preview boundary changed", preview)
	}
	receipt, err := f.client.ApplyReviewTransfer(t.Context(), "transfer-once", preview.PreviewSHA256, transferDocument())
	if err != nil || receipt.Revision != 1 || receipt.Outcome != "transferred" {
		t.Fatal(receipt, err)
	}
	var next domain.TransferRequest
	_ = json.Unmarshal(transferDocument(), &next)
	*next.ExpectedRevision = 1
	for i := range next.Groups {
		next.Groups[i].Pulls[0].Runs = nil
		next.Groups[i].Pulls[0].Attempts = nil
	}
	raw, _ := json.Marshal(next)
	second, err := f.client.PreviewReviewTransfer(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt, err := f.client.ApplyReviewTransfer(t.Context(), "transfer-next", second.PreviewSHA256, raw)
	if err != nil || secondReceipt.Revision != 2 || secondReceipt.Outcome != "transferred" {
		t.Fatal("nonzero expected revision did not advance once", secondReceipt, err)
	}
	saved := count()
	secondReplay, err := f.client.ApplyReviewTransfer(t.Context(), "transfer-next", second.PreviewSHA256, raw)
	if err != nil || secondReplay != secondReceipt || count() != saved {
		t.Fatal("nonzero-revision exact retry changed receipt or state", secondReplay, err)
	}
	replay, err := f.client.ApplyReviewTransfer(t.Context(), "transfer-once", preview.PreviewSHA256, transferDocument())
	if err != nil || replay != receipt || count() != saved {
		t.Fatal("old exact retry changed state", replay, err)
	}
	_, err = f.client.ApplyReviewTransfer(t.Context(), "transfer-wrong", strings.Repeat("f", 64), transferDocument())
	checkError(t, err, "import_preview_mismatch")
	var failure *client.Error
	if !errors.As(err, &failure) || failure.OutcomeUnknown || failure.Action != "Preview the selected batch again and inspect the new preview before applying." {
		t.Fatal("known mismatch not actionable", err)
	}
}

func TestReviewTransferClientBindsReceiptRevision(t *testing.T) {
	for _, tc := range []struct {
		name            string
		expected, reply int64
		valid           bool
	}{
		{"wrong-positive-at-zero", 0, 2, false},
		{"wrong-positive-at-nonzero", 7, 1, false},
		{"valid-nonzero", 7, 8, true},
		{"last-representable-revision", math.MaxInt64 - 1, math.MaxInt64, true},
		{"overflow-cannot-succeed", math.MaxInt64, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var batch domain.TransferRequest
			if err := json.Unmarshal(transferDocument(), &batch); err != nil {
				t.Fatal(err)
			}
			batch.ExpectedRevision = &tc.expected
			document, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				var wrapper struct {
					Preview string          `json:"preview_sha256"`
					Batch   json.RawMessage `json:"batch"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/project-one/reviews/transfer/apply" || r.URL.RawQuery != "" ||
					r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Idempotency-Key") != "original-transfer-key" ||
					json.NewDecoder(r.Body).Decode(&wrapper) != nil || wrapper.Preview != strings.Repeat("b", 64) || string(wrapper.Batch) != string(document) {
					t.Error("apply changed selected binding, key, preview or batch")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(client.ReviewTransferReceipt{ID: "review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Revision: tc.reply, Outcome: "transferred"})
			}))
			defer server.Close()
			c := newClient(t, server)
			receipt, err := c.ApplyReviewTransfer(t.Context(), "original-transfer-key", strings.Repeat("b", 64), document)
			if calls.Load() != 1 {
				t.Fatal("apply was not a single dispatched request", calls.Load())
			}
			if tc.valid {
				if err != nil || receipt.Revision != tc.reply {
					t.Fatal("valid bound receipt refused", receipt, err)
				}
				return
			}
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != "invalid_response" || !failure.OutcomeUnknown || failure.Status != http.StatusCreated || receipt != (client.ReviewTransferReceipt{}) {
				t.Fatal("unbound successful response lost uncertain-outcome recovery", receipt, err)
			}
		})
	}
}

func TestReviewTransferClientStrictPreviewAndCompactReceipt(t *testing.T) {
	var response atomic.Value
	response.Store(`{}`)
	var status, calls atomic.Int64
	status.Store(200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != "POST" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("transfer binding/transport changed")
		}
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		if strings.HasSuffix(r.URL.Path, "/preview") {
			if r.Header.Get("Idempotency-Key") != "" || string(raw) != string(transferDocument()) {
				t.Error("preview altered raw selected batch or mutation identity")
			}
		} else {
			var wrapper struct {
				Preview string          `json:"preview_sha256"`
				Batch   json.RawMessage `json:"batch"`
			}
			if json.Unmarshal(raw, &wrapper) != nil || string(wrapper.Batch) != string(transferDocument()) || wrapper.Preview != strings.Repeat("b", 64) || r.Header.Get("Idempotency-Key") != "retained-transfer" {
				t.Error("apply did not preserve exact batch/hash/key")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, response.Load().(string))
	}))
	defer server.Close()
	c := newClient(t, server)
	base, _ := json.Marshal(transferPreview())
	edits := []func(map[string]any){func(m map[string]any) { delete(m, "native_coverage") }, func(m map[string]any) { m["imported_history"] = nil }, func(m map[string]any) { m["native_coverage"] = true }, func(m map[string]any) { m["coverage"] = "complete" }, func(m map[string]any) { m["project_id"] = "foreign" }, func(m map[string]any) { m["repository_id"] = 702 }, func(m map[string]any) { m["publisher"] = "original-a" }, func(m map[string]any) { m["expected_revision"] = 1 }, func(m map[string]any) { m["version"] = 2 }, func(m map[string]any) { m["payload_sha256"] = strings.Repeat("A", 64) }, func(m map[string]any) { m["counts"].(map[string]any)["pulls"] = 2 }, func(m map[string]any) { m["counts"].(map[string]any)["runs"] = 1 }, func(m map[string]any) { m["counts"].(map[string]any)["attempts"] = 1 }, func(m map[string]any) { m["counts"].(map[string]any)["findings"] = 1 }, func(m map[string]any) { m["groups"] = []any{} }}
	for _, edit := range edits {
		var m map[string]any
		_ = json.Unmarshal(base, &m)
		edit(m)
		raw, _ := json.Marshal(m)
		response.Store(string(raw))
		_, err := c.PreviewReviewTransfer(t.Context(), transferDocument())
		checkError(t, err, "invalid_response")
		var failure *client.Error
		if !errors.As(err, &failure) || failure.OutcomeUnknown {
			t.Fatal("preview became mutation", err)
		}
	}
	response.Store(strings.TrimSuffix(string(base), "}") + `,"coverage":"partial"}`)
	_, err := c.PreviewReviewTransfer(t.Context(), transferDocument())
	checkError(t, err, "invalid_response")
	response.Store(string(base))
	if _, err = c.PreviewReviewTransfer(t.Context(), transferDocument()); err != nil {
		t.Fatal(err)
	}
	status.Store(201)
	for _, raw := range []string{`{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":1,"outcome":"observed"}`, `{"id":"foreign","revision":1,"outcome":"transferred"}`, `{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":0,"outcome":"transferred"}`, `{"id":`} {
		response.Store(raw)
		before := calls.Load()
		_, err := c.ApplyReviewTransfer(t.Context(), "retained-transfer", strings.Repeat("b", 64), transferDocument())
		var failure *client.Error
		if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls.Load() != before+1 {
			t.Fatal("invalid/lost apply reply retried or lost uncertainty", err)
		}
	}
	response.Store(`{"id":"review_snapshot_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","revision":1,"outcome":"transferred"}`)
	if got, err := c.ApplyReviewTransfer(t.Context(), "retained-transfer", strings.Repeat("b", 64), transferDocument()); err != nil || got.Revision != 1 {
		t.Fatal(got, err)
	}
}

func TestReviewTransferClientAdmissionAndCancellation(t *testing.T) {
	doc := transferDocument()
	bad := []json.RawMessage{nil, []byte(`null`), []byte(`[]`), []byte(strings.Replace(string(doc), `"version":1`, `"version":1,"version":1`, 1)), []byte(strings.Replace(string(doc), `"expected_revision":0`, `"expected_revision":null`, 1)), []byte(strings.Replace(string(doc), `"complete":false`, `"complete":true`, 1)), []byte(strings.Replace(string(doc), `"complete":false`, `"complete":null`, 1)), []byte(strings.Replace(string(doc), `"publisher":"prepared transfer"`, `"Publisher":"prepared transfer"`, 1)), []byte(strings.Replace(string(doc), `"original-b"`, `"original-a"`, 1)), []byte(strings.Replace(string(doc), `"groups":[`, `"groups":null,"ignored":[`, 1)), []byte(strings.Replace(string(doc), `"title":"Selected retained history"`, `"title":"\ud800"`, 1)), []byte(strings.Repeat(" ", 256<<10) + string(doc))}
	for _, raw := range bad {
		if client.ValidateReviewTransferDocument(raw) == nil {
			t.Fatal("invalid batch admitted before credentials")
		}
	}
	var batch domain.TransferRequest
	_ = json.Unmarshal(doc, &batch)
	for i := 0; i < 101; i++ {
		batch.Groups = append(batch.Groups, domain.TransferGroup{Source: "extra"})
	}
	raw, _ := json.Marshal(batch)
	if client.ValidateReviewTransferDocument(raw) == nil {
		t.Fatal("unbounded groups admitted")
	}
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "apply"}[apply], func(t *testing.T) {
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
			invoke := func(ctx context.Context) error {
				if apply {
					_, err := c.ApplyReviewTransfer(ctx, "key", strings.Repeat("b", 64), doc)
					return err
				}
				_, err := c.PreviewReviewTransfer(ctx, doc)
				return err
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			err := invoke(canceled)
			var failure *client.Error
			if !errors.As(err, &failure) || failure.OutcomeUnknown || calls.Load() != 0 {
				t.Fatal("unsent cancellation became ambiguous", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- invoke(ctx) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("request did not dispatch")
			}
			cancel()
			select {
			case err = <-done:
				if !errors.As(err, &failure) || failure.OutcomeUnknown != apply || calls.Load() != 1 {
					t.Fatal("dispatch cancellation/retry boundary changed", err)
				}
			case <-time.After(time.Second):
				t.Fatal("request cancellation blocked")
			}
		})
	}
}

func TestReviewTransferClientSelectedBatchBounds(t *testing.T) {
	request := func() domain.TransferRequest {
		var req domain.TransferRequest
		_ = json.Unmarshal(transferDocument(), &req)
		req.Groups = req.Groups[:1]
		return req
	}
	for _, tc := range []struct {
		name string
		edit func(*domain.TransferRequest)
	}{
		{"empty-groups", func(req *domain.TransferRequest) { req.Groups = nil }},
		{"empty-pulls", func(req *domain.TransferRequest) { req.Groups[0].Pulls = nil }},
		{"pull-appearances", func(req *domain.TransferRequest) {
			pull := req.Groups[0].Pulls[0]
			pull.Runs = nil
			pull.Attempts = nil
			req.Groups = make([]domain.TransferGroup, 100)
			for i := range req.Groups {
				req.Groups[i] = domain.TransferGroup{Source: fmt.Sprintf("producer-%d", i), Pulls: []domain.PullInput{pull}}
			}
			pull.Number = 8
			req.Groups[0].Pulls = append(req.Groups[0].Pulls, pull)
		}},
		{"runs-per-pull", func(req *domain.TransferRequest) {
			run := req.Groups[0].Pulls[0].Runs[0]
			run.Findings = nil
			run.Found = 0
			req.Groups[0].Pulls[0].Runs = nil
			for i := 0; i < 101; i++ {
				run.SourceRunID = fmt.Sprintf("run-%d", i)
				req.Groups[0].Pulls[0].Runs = append(req.Groups[0].Pulls[0].Runs, run)
			}
		}},
		{"attempts-per-pull", func(req *domain.TransferRequest) {
			attempt := req.Groups[0].Pulls[0].Attempts[0]
			attempt.SourceRunID = ""
			req.Groups[0].Pulls[0].Attempts = nil
			for i := 0; i < 101; i++ {
				attempt.SourceAttemptID = fmt.Sprintf("attempt-%d", i)
				req.Groups[0].Pulls[0].Attempts = append(req.Groups[0].Pulls[0].Attempts, attempt)
			}
		}},
		{"total-runs", func(req *domain.TransferRequest) {
			pull := req.Groups[0].Pulls[0]
			run := pull.Runs[0]
			run.Findings = nil
			run.Found = 0
			pull.Attempts = nil
			req.Groups[0].Pulls = nil
			for i := 0; i < 3; i++ {
				pull.Number = int64(i + 1)
				pull.Runs = nil
				for j := 0; j < 67; j++ {
					run.SourceRunID = fmt.Sprintf("run-%d-%d", i, j)
					pull.Runs = append(pull.Runs, run)
				}
				req.Groups[0].Pulls = append(req.Groups[0].Pulls, pull)
			}
		}},
		{"total-attempts", func(req *domain.TransferRequest) {
			pull := req.Groups[0].Pulls[0]
			attempt := pull.Attempts[0]
			attempt.SourceRunID = ""
			pull.Runs = nil
			req.Groups[0].Pulls = nil
			for i := 0; i < 3; i++ {
				pull.Number = int64(i + 1)
				pull.Attempts = nil
				for j := 0; j < 67; j++ {
					attempt.SourceAttemptID = fmt.Sprintf("attempt-%d-%d", i, j)
					pull.Attempts = append(pull.Attempts, attempt)
				}
				req.Groups[0].Pulls = append(req.Groups[0].Pulls, pull)
			}
		}},
		{"findings-per-run", func(req *domain.TransferRequest) {
			finding := req.Groups[0].Pulls[0].Runs[0].Findings[0]
			req.Groups[0].Pulls[0].Runs[0].Findings = nil
			for i := 0; i < 101; i++ {
				finding.SourceFindingID = fmt.Sprintf("finding-%d", i)
				req.Groups[0].Pulls[0].Runs[0].Findings = append(req.Groups[0].Pulls[0].Runs[0].Findings, finding)
			}
		}},
		{"total-findings", func(req *domain.TransferRequest) {
			run := req.Groups[0].Pulls[0].Runs[0]
			run.Findings = nil
			req.Groups[0].Pulls[0].Attempts = nil
			req.Groups[0].Pulls[0].Runs = nil
			for i := 0; i < 11; i++ {
				run.SourceRunID = fmt.Sprintf("run-%d", i)
				run.Findings = nil
				n := 100
				if i == 10 {
					n = 1
				}
				run.Found = int64(n)
				for j := 0; j < n; j++ {
					run.Findings = append(run.Findings, domain.Finding{SourceFindingID: fmt.Sprintf("f%d", j), Title: "f", Severity: "unknown", Disposition: "unadjudicated"})
				}
				req.Groups[0].Pulls[0].Runs = append(req.Groups[0].Pulls[0].Runs, run)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := request()
			tc.edit(&req)
			raw, _ := json.Marshal(req)
			if len(raw) > 256<<10 {
				t.Fatal("count fixture exceeded byte budget before isolated count admission", len(raw))
			}
			if client.ValidateReviewTransferDocument(raw) == nil {
				t.Fatal("excess selected batch admitted")
			}
		})
	}
	req := request()
	req.Groups[0].Pulls[0].Runs = nil
	req.Groups[0].Pulls[0].Attempts = nil
	raw, _ := json.Marshal(req)
	if err := client.ValidateReviewTransferDocument(raw); err != nil {
		t.Fatal("metadata-only partial transfer refused", err)
	}
}

func TestReviewTransferClientMetadataRealPreviewApplyReplay(t *testing.T) {
	for _, tc := range []struct {
		name, state, mergedAt string
		diff                  *domain.PullDiff
		draft                 *bool
	}{
		{"known-nonzero", "open", "", &domain.PullDiff{Files: 3, Additions: 12, Deletions: 4}, func() *bool { v := true; return &v }()},
		{"known-zero-false", "open", "", &domain.PullDiff{}, func() *bool { v := false; return &v }()},
		{"merged-instant", "merged", "2026-10-19T22:10:11.123Z", &domain.PullDiff{Files: 1}, nil},
		{"unknown", "open", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewClientFixture(t)
			if _, err := f.store.DB().Exec(`UPDATE credentials SET is_admin=1 WHERE id='review-client-credential'`); err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			request := domain.TransferRequest{Version: 1, ExpectedRevision: &zero, RepositoryID: 701, Publisher: "metadata transfer", Groups: []domain.TransferGroup{{Source: "original-metadata", Pulls: []domain.PullInput{{Number: 7, Title: "Observed display facts", State: tc.state, HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "unknown", Mergeability: "unknown", Diff: tc.diff, IsDraft: tc.draft, MergedAt: tc.mergedAt, Runs: []domain.RunInput{}, Attempts: []domain.Attempt{}}}}}}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			original := string(raw)
			counts := func() string {
				var value string
				if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM review_snapshots)||':'||(SELECT count(*) FROM receipts)||':'||(SELECT count(*) FROM events)`).Scan(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := counts()
			preview, err := f.client.PreviewReviewTransfer(t.Context(), raw)
			if err != nil {
				t.Fatal("actual metadata preview refused", err)
			}
			if counts() != before || preview.NativeCoverage || preview.Coverage != "partial" || preview.Counts != (client.ReviewTransferCounts{Pulls: 1}) {
				t.Fatal("metadata-only preview changed state or coverage", preview)
			}
			receipt, err := f.client.ApplyReviewTransfer(t.Context(), "metadata-once", preview.PreviewSHA256, raw)
			if err != nil || receipt.Revision != 1 {
				t.Fatal(receipt, err)
			}
			pull, err := f.client.ReviewPull(t.Context(), 7, receipt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (pull.Pull.Diff == nil) != (tc.diff == nil) || tc.diff != nil && (pull.Pull.Diff.Files != tc.diff.Files || pull.Pull.Diff.Additions != tc.diff.Additions || pull.Pull.Diff.Deletions != tc.diff.Deletions) || (pull.Pull.IsDraft == nil) != (tc.draft == nil) || tc.draft != nil && *pull.Pull.IsDraft != *tc.draft || pull.Pull.MergedAt != tc.mergedAt {
				t.Fatal("metadata lost known/unknown values", pull.Pull)
			}
			saved := counts()
			replay, err := f.client.ApplyReviewTransfer(t.Context(), "metadata-once", preview.PreviewSHA256, raw)
			if err != nil || replay != receipt || counts() != saved || string(raw) != original {
				t.Fatal("exact metadata replay changed retained state or input", replay, err)
			}
		})
	}
}

func TestReviewTransferClientMalformedMetadataRefusedBeforeHTTP(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		http.Error(w, "unexpected", 500)
	}))
	defer server.Close()
	c := newClient(t, server)
	for _, fields := range []string{
		`"diff":{"files":-1,"additions":0,"deletions":0}`,
		`"diff":{"files":2147483648,"additions":0,"deletions":0}`,
		`"diff":{"files":0,"additions":0}`,
		`"diff":{"files":null,"additions":0,"deletions":0}`,
		`"diff":{"files":0,"additions":0,"deletions":0,"extra":0}`,
		`"diff":{"files":0,"additions":0,"deletions":0},"diff":{}`,
		`"is_draft":"false"`, `"is_draft":1`,
		`"merged_at":"2026-10-19T22:00:00Z"`, `"merged_at":"not-an-instant"`,
		`"unknown_metadata":false`,
	} {
		raw := json.RawMessage(strings.ReplaceAll(string(transferDocument()), `"title":"Selected retained history"`, `"title":"Selected retained history",`+fields))
		if fields == `"merged_at":"not-an-instant"` {
			raw = json.RawMessage(strings.ReplaceAll(string(raw), `"state":"open"`, `"state":"merged"`))
		}
		if client.ValidateReviewTransferDocument(raw) == nil {
			t.Fatal("malformed metadata admitted", fields)
		}
		_, err := c.PreviewReviewTransfer(t.Context(), raw)
		checkError(t, err, "validation_failed")
		_, err = c.ApplyReviewTransfer(t.Context(), "metadata-invalid", strings.Repeat("b", 64), raw)
		checkError(t, err, "validation_failed")
		if calls.Load() != 0 {
			t.Fatal("malformed metadata dispatched HTTP")
		}
	}
}
