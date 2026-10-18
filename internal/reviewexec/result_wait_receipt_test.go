package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResultWaitLifetimePOSTReturnsKnownSourceAndCheckpointReceipts(t *testing.T) {
	for _, kind := range []string{"source", "checkpoint"} {
		t.Run(kind, func(t *testing.T) {
			var f *attemptFixture
			var path, body string
			if kind == "source" {
				f = sourceHTTPFixture(t)
				path = "/api/v1/projects/" + attemptProject + "/reviews/source-refreshes"
				body = encoded(refreshRequest())
			} else {
				var evidence string
				f, evidence, path = checkpointHTTPFixture(t)
				body = encoded(CheckpointPublicationRequest{EvidenceID: evidence, ExpectedRevision: adjudicationRevision(1)})
			}
			lifetime, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.e.resultWaitLifetime = lifetime
			srv := httptest.NewServer(f.handler)
			defer srv.Close()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+f.writer)
			request.Header.Set("X-FC-Instance", f.store.InstanceID())
			request.Header.Set("Idempotency-Key", "shutdown-known-receipt")
			request.Header.Set("Prefer", "wait=60")
			type result struct {
				response *http.Response
				err      error
			}
			done := make(chan result, 1)
			go func() {
				response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
				done <- result{response, err}
			}()
			count := func() int {
				if kind == "source" {
					f.e.sourceMu.Lock()
					defer f.e.sourceMu.Unlock()
					return f.e.sourceWaiterCount
				}
				f.e.checkpointMu.Lock()
				defer f.e.checkpointMu.Unlock()
				return f.e.checkpointWaiterCount
			}
			deadline := time.Now().Add(time.Second)
			for count() != 1 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if count() != 1 {
				t.Fatal("result wait was not registered")
			}
			state := func() string {
				if kind == "checkpoint" {
					return checkpointSQLState(t, f)
				}
				return fmt.Sprint(attemptCount(t, f.store, `SELECT count(*) FROM review_source_refreshes WHERE state='queued'`), attemptCount(t, f.store, `SELECT count(*) FROM receipts`), attemptCount(t, f.store, `SELECT count(*) FROM events`))
			}
			before := state()
			cancel()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatal(got.err)
				}
				defer got.response.Body.Close()
				data, err := io.ReadAll(got.response.Body)
				if err != nil {
					t.Fatal(err)
				}
				var receipt Receipt
				if got.response.StatusCode != http.StatusAccepted || json.Unmarshal(data, &receipt) != nil || receipt.State != "queued" || receipt.Revision < 1 || strings.Contains(string(data), "outcome_unknown") {
					t.Fatal("shutdown did not preserve known accepted receipt", got.response.StatusCode, string(data))
				}
				if kind == "source" {
					read := refreshRead(t, f, receipt.ID)
					if read.State != "queued" || read.Revision != receipt.Revision {
						t.Fatal("source operation changed", read)
					}
				} else {
					read := checkpointRead(t, f, receipt.ID)
					if read.State != "queued" || read.Revision != receipt.Revision {
						t.Fatal("publication changed", read)
					}
				}
				if count() != 0 || state() != before {
					t.Fatal("shutdown changed durable work or leaked wait capacity")
				}
			case <-time.After(time.Second):
				t.Fatal("lifetime cancellation did not detach HTTP POST")
			}
		})
	}
}

func TestResultWaitLifecycleReasonDoesNotMaskOtherFailures(t *testing.T) {
	f := sourceHTTPFixture(t)
	scope := evidenceReadScope(t, f, attemptProject, f.writer)
	caller, cancel := context.WithCancel(t.Context())
	cancel()
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	cancelLifetime()
	f.e.resultWaitLifetime = lifetime
	ctx, cleanup := f.e.resultWaitContext(context.Background())
	defer cleanup()
	reason := resultWaitCanceled(ctx, "detached")
	if detached, _ := f.e.acceptedWaitDetached(caller, scope, reason); detached {
		t.Fatal("client cancellation became successful receipt")
	}
	admissionSQL(t, f, `UPDATE grants SET can_read=0 WHERE credential_id='exec-writer-cred'`)
	detached, err := f.e.acceptedWaitDetached(t.Context(), scope, reason)
	if detached {
		t.Fatal("revoked access exposed a receipt")
	}
	attemptCode(t, err, "forbidden")
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	detached, err = f.e.acceptedWaitDetached(t.Context(), scope, reason)
	if detached {
		t.Fatal("failed fresh read became a successful receipt")
	}
	attemptCode(t, err, "storage_unavailable")
}

func TestResultWaitLifetimeHTTPDoesNotMaskFailedFreshRead(t *testing.T) {
	f := sourceHTTPFixture(t)
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.e.resultWaitLifetime = lifetime
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	path := "/api/v1/projects/" + attemptProject + "/reviews/requests"
	body := encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.writer)
	request.Header.Set("X-FC-Instance", f.store.InstanceID())
	request.Header.Set("Idempotency-Key", "failed-fresh-read")
	request.Header.Set("Prefer", "wait=900")
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		done <- result{response, err}
	}()
	operationWaitRegistered(t, f.e, 1)
	var sequence int
	var name, filename string
	if err = f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &filename); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.response.Body.Close()
		data, err := io.ReadAll(got.response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got.response.StatusCode != http.StatusInternalServerError || !strings.Contains(string(data), `"outcome_unknown":true`) || strings.Contains(string(data), "review_operation_") {
			t.Fatal("failed fresh read was masked", got.response.StatusCode, string(data))
		}
	case <-time.After(time.Second):
		t.Fatal("failed-read POST did not detach")
	}
	operationWaitRegistered(t, f.e, 0)
	// A separate read-only connection proves a failed observer did not undo the
	// action or its idempotency receipt after the owning store has been closed.
	readback, err := sql.Open("sqlite", "file:"+filename+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer readback.Close()
	var queued, receipts int
	if err = readback.QueryRow(`SELECT count(*) FROM review_operations WHERE state='queued' AND revision=1`).Scan(&queued); err != nil || queued != 1 {
		t.Fatal("accepted operation changed", queued, err)
	}
	if err = readback.QueryRow(`SELECT count(*) FROM receipts WHERE operation='reviews.request'`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("accepted receipt changed", receipts, err)
	}
}
