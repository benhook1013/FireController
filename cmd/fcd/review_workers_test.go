package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/server"
)

// Synthetic drivers exercise ownership without invoking a provider or executor.
type observerTestDriver struct {
	exit       chan error
	started    chan struct{}
	cancelled  chan struct{}
	joined     chan struct{}
	joinGate   <-chan struct{}
	prepared   chan reviewexec.Operation
	startError error
	posts      atomic.Int64
}

func newObserverTestDriver() *observerTestDriver {
	return &observerTestDriver{exit: make(chan error, 1), started: make(chan struct{}), cancelled: make(chan struct{}), joined: make(chan struct{}), prepared: make(chan reviewexec.Operation, 1)}
}
func (d *observerTestDriver) StartRecovery(ctx context.Context) (<-chan error, error) {
	if d.startError != nil {
		return nil, d.startError
	}
	done := make(chan error, 1)
	go func() {
		defer close(d.joined)
		defer close(done)
		close(d.started)
		select {
		case err := <-d.exit:
			done <- err
		case <-ctx.Done():
			close(d.cancelled)
			if d.joinGate != nil {
				<-d.joinGate
			}
			done <- nil
		}
	}()
	return done, nil
}
func (d *observerTestDriver) Prepare(_ context.Context, o reviewexec.Operation, _ reviewexec.Configuration, _ reviewexec.Attempt) (reviewexec.Preparation, error) {
	d.prepared <- o
	return reviewexec.Preparation{}, core.Fail("review_held", "Synthetic preparation hold.", 409)
}
func (d *observerTestDriver) Dispatch(context.Context, reviewexec.Operation, reviewexec.Attempt, func(context.Context, string) error) (reviewexec.Result, error) {
	d.posts.Add(1)
	return reviewexec.Result{}, errors.New("unexpected synthetic dispatch")
}

type observerTestFixture struct {
	store  *core.Store
	engine *reviewexec.Engine
	admin  string
	http   *httptest.Server
}

func newObserverTestFixture(t *testing.T) *observerTestFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, ensure := range []func(context.Context, *core.Store) error{reviewexec.EnsureSchema, jobs.EnsureSchema, inbox.EnsureSchema} {
		if err := ensure(t.Context(), store); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &observerTestFixture{store: store, admin: strings.TrimSpace(string(data))}
	capabilities := []reviewexec.Capability{}
	for i, id := range []string{"failed", "sibling"} {
		capabilities = append(capabilities, reviewexec.Capability{ID: id, Provider: "coderabbit", Account: id, Channels: []string{"hosted"}})
		if _, err := store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,?)`, id, id, i+801, "owner/"+id); err != nil {
			t.Fatal(err)
		}
		for _, worker := range []string{"sender", "recipient"} {
			if _, err := store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,?)`, id, worker, worker); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.engine, err = reviewexec.New(store, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"failed", "sibling"} {
		zero := int64(0)
		scope := f.scope(t, project)
		if _, err := f.engine.Configure(t.Context(), scope, "configure", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: project, Mode: "independent", Queue: []int64{1}, Channels: []string{"hosted"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.ConfigurePolicy(t.Context(), scope, "policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"hosted"}, PausedChannels: []string{}}); err != nil {
			t.Fatal(err)
		}
	}
	mux := server.New(store)
	jobs.Register(mux, store)
	inbox.Register(mux, store)
	f.http = httptest.NewServer(mux)
	t.Cleanup(f.http.Close)
	return f
}
func (f *observerTestFixture) scope(t *testing.T, project string) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
func (f *observerTestFixture) enqueue(t *testing.T, project, key string) reviewexec.Receipt {
	t.Helper()
	raw, err := f.engine.Enqueue(t.Context(), f.scope(t, project), key, reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 1, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}
func (f *observerTestFixture) uncertain(t *testing.T) reviewexec.Attempt {
	t.Helper()
	r := f.enqueue(t, "failed", "issued")
	a, err := f.engine.StartAttempt(t.Context(), "failed", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	anchor := reviewexec.Anchor{RepositoryID: 801, Repository: "owner/failed", PullNumber: 1, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("c", 40), BaseRef: "main", MergeBaseSHA: strings.Repeat("d", 40), PatchSHA256: strings.Repeat("e", 64), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	a, err = f.engine.Admit(t.Context(), "failed", a.ID, a.Revision, anchor, "synthetic issued intent")
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.engine.Advance(t.Context(), "failed", a.ID, a.Revision, "uncertain", "post_response_lost", "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func (f *observerTestFixture) retained(t *testing.T, a reviewexec.Attempt) string {
	t.Helper()
	var state, anchor, intent, opState, reason string
	var revision, opRevision, transitions, reservations int64
	err := f.store.DB().QueryRow(`SELECT a.state,a.revision,a.anchor_json,a.intent,o.state,o.revision,o.reason,(SELECT count(*) FROM review_execution_transitions WHERE attempt_id=a.id),(SELECT count(*) FROM review_execution_reservations WHERE attempt_id=a.id) FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE a.id=?`, a.ID).Scan(&state, &revision, &anchor, &intent, &opState, &opRevision, &reason, &transitions, &reservations)
	if err != nil || state != "uncertain" || reservations != 1 {
		t.Fatalf("uncertain reservation unavailable: %s/%d %v", state, reservations, err)
	}
	return fmt.Sprintf("%s/%d/%s/%s/%s/%d/%s/%d/%d", state, revision, anchor, intent, opState, opRevision, reason, transitions, reservations)
}
func observerSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("worker lifecycle deadline")
	}
}
func (f *observerTestFixture) coordination(t *testing.T, project string) {
	t.Helper()
	for _, request := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/readyz", "", 200},
		{"POST", "/api/v1/projects/" + project + "/jobs", `{"title":"still usable","brief":"coordination survives observer failure","worker_id":"sender"}`, 201},
		{"POST", "/api/v1/projects/" + project + "/messages", `{"recipient_worker_id":"recipient","body":"still usable"}`, 201},
		{"GET", "/api/v1/projects/" + project + "/jobs", "", 200},
		{"GET", "/api/v1/projects/" + project + "/inbox/recipient", "", 200},
	} {
		r, err := http.NewRequestWithContext(t.Context(), request.method, f.http.URL+request.path, strings.NewReader(request.body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+f.admin)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		r.Header.Set("X-FC-Worker", "sender")
		r.Header.Set("Idempotency-Key", "coordination-"+request.path)
		response, err := (&http.Client{Timeout: time.Second}).Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != request.status {
			t.Fatalf("%s %s: %d %s %v", request.method, request.path, response.StatusCode, body, err)
		}
	}
}

func TestReviewWorkersObserverExitDoesNotStopCoordinationOrSupervisor(t *testing.T) {
	for _, terminal := range []struct {
		name string
		err  error
	}{{"error", errors.New("private provider body /private/token")}, {"unexpected clean exit", nil}} {
		t.Run(terminal.name, func(t *testing.T) {
			f := newObserverTestFixture(t)
			uncertain := f.uncertain(t)
			before := f.retained(t, uncertain)
			failed, sibling := newObserverTestDriver(), newObserverTestDriver()
			gate := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate) }) }
			sibling.joinGate = gate
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			defer release()
			diagnostics := &output{ready: make(chan struct{})}
			done, err := startReviewWorkers(ctx, f.engine, map[string]reviewexec.Driver{"failed": failed, "sibling": sibling}, diagnostics)
			if err != nil {
				t.Fatal(err)
			}
			observerSignal(t, failed.started)
			observerSignal(t, sibling.started)
			failed.exit <- terminal.err
			observerSignal(t, failed.joined)
			observerSignal(t, diagnostics.ready)
			if diagnostic := diagnostics.String(); strings.Count(diagnostic, "\n") != 1 || !strings.Contains(diagnostic, `"code":"review_recovery_stopped"`) || !strings.Contains(diagnostic, "capability failed stopped") || strings.Contains(diagnostic, "private") || strings.Contains(diagnostic, "token") {
				t.Fatalf("unsafe or repeated observer diagnostic: %s", diagnostic)
			}
			select {
			case <-sibling.cancelled:
				t.Fatal("terminal recovery observer cancelled its healthy sibling")
			case err := <-done:
				t.Fatalf("terminal recovery observer stopped supervisor: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			queued := f.enqueue(t, "sibling", "after-observer-exit")
			select {
			case prepared := <-sibling.prepared:
				if prepared.ID != queued.ID {
					t.Fatal("wrong operation reached supervisor")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("supervisor stopped processing independent capability")
			}
			for _, project := range []string{"failed", "sibling"} {
				f.coordination(t, project)
			}
			if after := f.retained(t, uncertain); after != before {
				t.Fatal("observer exit changed uncertain journal or reservation")
			}
			if failed.posts.Load()+sibling.posts.Load() != 0 {
				t.Fatal("observer exit dispatched or reposted work")
			}
			cancel()
			observerSignal(t, sibling.cancelled)
			select {
			case <-done:
				t.Fatal("owner returned before sibling cleanup joined")
			default:
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not join workers")
			}
			observerSignal(t, sibling.joined)
			if strings.Count(diagnostics.String(), "\n") != 1 {
				t.Fatal("cancellation emitted another observer diagnostic")
			}
		})
	}
}

func TestReviewWorkersObserverStartupFailureRemainsFatal(t *testing.T) {
	f := newObserverTestFixture(t)
	failed, sibling := newObserverTestDriver(), newObserverTestDriver()
	startupFailure := core.Fail("configuration_invalid", "Synthetic observer binding rejected.", 500)
	failed.startError = startupFailure
	diagnostics := &output{ready: make(chan struct{})}
	done, err := startReviewWorkers(t.Context(), f.engine, map[string]reviewexec.Driver{"failed": failed, "sibling": sibling}, diagnostics)
	if !errors.Is(err, startupFailure) || done == nil {
		t.Fatalf("observer startup failure lost fatal ownership: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup failure did not cancel and join started workers")
	}
	select {
	case <-sibling.started:
		observerSignal(t, sibling.cancelled)
		observerSignal(t, sibling.joined)
	default:
		// Map order may encounter the refused binding before this observer.
	}
	if diagnostics.String() != "" {
		t.Fatal("startup cancellation emitted an asynchronous observer diagnostic")
	}
}

func TestReviewWorkersSupervisorFailureRemainsFatalAndJoinsObservers(t *testing.T) {
	f := newObserverTestFixture(t)
	first, second := newObserverTestDriver(), newObserverTestDriver()
	diagnostics := &output{ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := startReviewWorkers(ctx, f.engine, map[string]reviewexec.Driver{"failed": first, "sibling": second}, diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	observerSignal(t, first.started)
	observerSignal(t, second.started)
	// A real failed supervisor read must still terminate the aggregate. The
	// synthetic observers remain healthy until their owner cancels them.
	if _, err := f.store.DB().Exec(`ALTER TABLE review_operations RENAME TO broken_review_operations`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "review_persistence_failed" {
			t.Fatalf("supervisor storage failure was hidden: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor failure did not terminate owner")
	}
	for _, driver := range []*observerTestDriver{first, second} {
		observerSignal(t, driver.cancelled)
		observerSignal(t, driver.joined)
	}
	if diagnostics.String() != "" {
		t.Fatal("fatal supervisor cancellation emitted an observer diagnostic")
	}
}
