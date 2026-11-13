package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
)

func coordinationBatch() client.CoordinationImportBatch {
	zero := int64(0)
	return client.CoordinationImportBatch{Version: 1, RepositoryID: 701, ExpectedRevision: &zero, Source: "selected archive", SourceSHA256: strings.Repeat("a", 64), Jobs: []coordimportmodel.PreparedJob{{
		SourceKey: " original job ", Title: "Historical job", Brief: "PRIVATE_IMPORT_BRIEF", SourceRevision: 42, Status: "parked",
		CreatedAt: "2024-01-02T03:04:05+13:00", UpdatedAt: "2024-02-03T04:05:06Z", Checkpoints: []coordimportmodel.PreparedCheckpoint{{SourceKey: "checkpoint", ActorID: "original-actor", Done: "done", Next: "next", CreatedAt: "2024-02-03T04:05:06Z", Evidence: []string{}}},
	}}}
}
func coordinationDocument(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(coordinationBatch())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func coordinationPreview(t *testing.T) client.CoordinationImportPreview {
	digest := sha256.Sum256(coordinationDocument(t))
	return client.CoordinationImportPreview{ProjectID: "project-one", ExpectedRevision: 0, PayloadSHA256: hex.EncodeToString(digest[:]), PreviewSHA256: strings.Repeat("b", 64), Counts: client.CoordinationImportCounts{Jobs: 1, Checkpoints: 1, NewJobs: 1}}
}
func coordinationReceipt() client.CoordinationImportReceipt {
	return client.CoordinationImportReceipt{Revision: 1, Outcome: "imported", Counts: client.CoordinationImportCounts{Jobs: 1, Checkpoints: 1, NewJobs: 1}, Mappings: []client.CoordinationImportMapping{{SourceKey: " original job ", JobID: "job-one"}}}
}

func TestCoordinationImportClientBoundRequestsAndStrictReplies(t *testing.T) {
	var status atomic.Int64
	status.Store(200)
	var reply atomic.Value
	reply.Store([]byte(`{"revision":0}`))
	var requests atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		requests.Add(1)
		if r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer "+testToken || r.URL.RawQuery != "" {
			t.Error("request lost explicit binding")
		}
		if r.Method == "POST" {
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing content type")
			}
			if strings.HasSuffix(r.URL.Path, "/preview") && r.Header.Get("Idempotency-Key") != "" {
				t.Error("preview acquired mutation semantics")
			}
			if strings.HasSuffix(r.URL.Path, "/apply") && r.Header.Get("Idempotency-Key") != "exact-key" {
				t.Error("apply key changed")
			}
		} else if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/coordination/imports") {
			t.Error("unexpected route")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write(reply.Load().([]byte))
	}))
	defer s.Close()
	c := newClient(t, s)
	state, err := c.CoordinationImportState(t.Context())
	if err != nil || state.Revision != 0 {
		t.Fatal(state, err)
	}
	for _, raw := range []string{`{}`, `{"revision":null}`, `{"revision":-1}`, `{"revision":0,"extra":1}`, `{"revision":0,"revision":1}`} {
		reply.Store([]byte(raw))
		_, err := c.CoordinationImportState(t.Context())
		checkError(t, err, "invalid_response")
	}
	set := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		reply.Store(b)
	}
	set(coordinationPreview(t))
	p, err := c.PreviewCoordinationImport(t.Context(), coordinationDocument(t))
	if err != nil || p.ProjectID != "project-one" {
		t.Fatal(p, err)
	}
	for name, mutate := range map[string]func(*client.CoordinationImportPreview){
		"project":     func(v *client.CoordinationImportPreview) { v.ProjectID = "foreign" },
		"revision":    func(v *client.CoordinationImportPreview) { v.ExpectedRevision = 1 },
		"payload":     func(v *client.CoordinationImportPreview) { v.PayloadSHA256 = strings.Repeat("f", 64) },
		"fingerprint": func(v *client.CoordinationImportPreview) { v.PreviewSHA256 = "wrong" },
		"counts":      func(v *client.CoordinationImportPreview) { v.Counts.NewJobs = 2 },
	} {
		t.Run("preview "+name, func(t *testing.T) {
			v := coordinationPreview(t)
			mutate(&v)
			set(v)
			_, err := c.PreviewCoordinationImport(t.Context(), coordinationDocument(t))
			checkError(t, err, "invalid_response")
		})
	}
	set(coordinationReceipt())
	receipt, err := c.ApplyCoordinationImport(t.Context(), "exact-key", p.PreviewSHA256, coordinationDocument(t))
	if err != nil || receipt.Revision != 1 {
		t.Fatal(receipt, err)
	}
	for name, mutate := range map[string]func(*client.CoordinationImportReceipt){
		"revision":        func(v *client.CoordinationImportReceipt) { v.Revision = 2 },
		"outcome":         func(v *client.CoordinationImportReceipt) { v.Outcome = "unchanged" },
		"counts":          func(v *client.CoordinationImportReceipt) { v.Counts.Checkpoints = 2 },
		"mapping source":  func(v *client.CoordinationImportReceipt) { v.Mappings[0].SourceKey = "other" },
		"mapping id":      func(v *client.CoordinationImportReceipt) { v.Mappings[0].JobID = "bad/id" },
		"missing mapping": func(v *client.CoordinationImportReceipt) { v.Mappings = []client.CoordinationImportMapping{} },
	} {
		t.Run("receipt "+name, func(t *testing.T) {
			v := coordinationReceipt()
			mutate(&v)
			set(v)
			before := requests.Load()
			_, err := c.ApplyCoordinationImport(t.Context(), "exact-key", p.PreviewSHA256, coordinationDocument(t))
			checkError(t, err, "invalid_response", "PRIVATE_IMPORT_BRIEF")
			var failure *client.Error
			if !errors.As(err, &failure) || !failure.OutcomeUnknown || requests.Load() != before+1 {
				t.Fatal("invalid write result was not single-shot unknown", err)
			}
		})
	}
	for _, raw := range []string{`{}`, `{"revision":null}`, `{"revision":1,"outcome":"imported","counts":null,"mappings":[]}`} {
		reply.Store([]byte(raw))
		_, err := c.ApplyCoordinationImport(t.Context(), "exact-key", p.PreviewSHA256, coordinationDocument(t))
		checkError(t, err, "invalid_response")
	}
	unchanged := coordinationReceipt()
	unchanged.Outcome = "unchanged"
	unchanged.Counts.NewJobs, unchanged.Counts.UnchangedJobs = 0, 1
	set(unchanged)
	batch := coordinationBatch()
	one := int64(1)
	batch.ExpectedRevision = &one
	raw, _ := json.Marshal(batch)
	if _, err := c.ApplyCoordinationImport(t.Context(), "exact-key", p.PreviewSHA256, raw); err != nil {
		t.Fatal("valid unchanged receipt refused", err)
	}
	unchanged.Revision = 0
	set(unchanged)
	_, err = c.ApplyCoordinationImport(t.Context(), "exact-key", p.PreviewSHA256, coordinationDocument(t))
	checkError(t, err, "invalid_response")
}

func TestCoordinationImportClientLocalAdmissionAndWrapperBudget(t *testing.T) {
	valid := coordinationDocument(t)
	for name, raw := range map[string][]byte{
		"empty": nil, "null": []byte("null"), "unknown": append([]byte(`{"extra":1,`), valid[1:]...),
		"duplicate":      append([]byte(`{"version":1,`), valid[1:]...),
		"null revision":  []byte(strings.Replace(string(valid), `"expected_revision":0`, `"expected_revision":null`, 1)),
		"nested unknown": []byte(strings.Replace(string(valid), `"title":`, `"extra":true,"title":`, 1)),
		"huge":           []byte(strings.Repeat(" ", (256<<10)+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if client.ValidateCoordinationImportDocument(raw) == nil {
				t.Fatal("invalid local document accepted")
			}
		})
	}
	batch := coordinationBatch()
	batch.Jobs = append(batch.Jobs, batch.Jobs[0])
	batch.Jobs[1].SourceKey = "second"
	for i := range batch.Jobs {
		batch.Jobs[i].Brief = strings.Repeat("<", 25000)
	}
	// Disabling HTML escaping keeps the file small, while the normal apply
	// encoder expands it beyond 256KiB. Refuse before any network operation.
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(batch); err != nil {
		t.Fatal(err)
	}
	if b.Len() >= 256<<10 || client.ValidateCoordinationImportDocument([]byte(b.String())) == nil {
		t.Fatal("preview/apply escaped budget gap admitted")
	}
	if client.ValidateCoordinationImportApply("bad\nkey", strings.Repeat("b", 64), valid) == nil || client.ValidateCoordinationImportApply("key", "bad", valid) == nil {
		t.Fatal("invalid mutation identity accepted")
	}
}

func TestCoordinationImportClientAcceptsBoundedEscapedMappings(t *testing.T) {
	batch := coordinationBatch()
	prototype := batch.Jobs[0]
	batch.Jobs = make([]coordimportmodel.PreparedJob, 25)
	receipt := client.CoordinationImportReceipt{Revision: 1, Outcome: "imported", Counts: client.CoordinationImportCounts{Jobs: 25, Checkpoints: 25, NewJobs: 25}, Mappings: []client.CoordinationImportMapping{}}
	for i := range batch.Jobs {
		batch.Jobs[i] = prototype
		batch.Jobs[i].SourceKey = strings.Repeat("<", 197) + "-" + strconv.Itoa(i+10)
		receipt.Mappings = append(receipt.Mappings, client.CoordinationImportMapping{SourceKey: batch.Jobs[i].SourceKey, JobID: "job-" + strconv.Itoa(i)})
	}
	document, _ := json.Marshal(batch)
	encoded, _ := json.Marshal(receipt)
	if len(encoded) <= 16<<10 || len(encoded) > 64<<10 {
		t.Fatal("fixture does not exercise escaped bounded response")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		write(w, 200, receipt)
	}))
	defer s.Close()
	c := newClient(t, s)
	result, err := c.ApplyCoordinationImport(t.Context(), "exact-key", strings.Repeat("b", 64), document)
	if err != nil || len(result.Mappings) != 25 || result.Mappings[24] != receipt.Mappings[24] {
		t.Fatal("valid largest escaped source mappings refused", err)
	}
}

func TestCoordinationImportClientCancellationPreservesWriteUncertainty(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var requests atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer s.Close()
	defer close(release)
	c := newClient(t, s)
	for _, apply := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			var err error
			if apply {
				_, err = c.ApplyCoordinationImport(ctx, "exact-key", strings.Repeat("b", 64), coordinationDocument(t))
			} else {
				_, err = c.PreviewCoordinationImport(ctx, coordinationDocument(t))
			}
			result <- err
		}()
		<-entered
		cancel()
		err := <-result
		var failure *client.Error
		if !errors.As(err, &failure) || failure.OutcomeUnknown != apply {
			t.Fatal("cancellation lost read/write boundary", err)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("canceled operation automatically retried")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.ApplyCoordinationImport(ctx, "exact-key", strings.Repeat("b", 64), coordinationDocument(t))
	var failure *client.Error
	if !errors.As(err, &failure) || failure.OutcomeUnknown || requests.Load() != 2 {
		t.Fatal("unsent cancellation became unknown/dispatched", err)
	}
}
