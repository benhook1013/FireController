package reviewexec

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestDeploymentDrainLetsCurrentDispatchFinishAndKeepsQueuedWork(t *testing.T) {
	f := supervisorFixture(t)
	first := f.enqueue(t, attemptProject, "drain-first", "cli", 2, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	driver := &controlledDriver{dispatch: func(ctx context.Context, o Operation, a Attempt, trigger func(context.Context, string) error) (Result, error) {
		close(entered)
		select {
		case <-release:
			return Result{State: "completed", Reason: "synthetic_capture"}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.e.processNext(ctx, map[string]Driver{"test-capability": driver}) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("dispatch did not start")
	}
	if got := f.e.deploymentDrain("begin"); !got.Draining || got.ActiveDispatches != 1 {
		t.Fatal(got)
	}
	queued := f.enqueue(t, attemptProject, "drain-new-request", "cli", 4, 1)
	if err := f.e.processNext(ctx, map[string]Driver{"test-capability": driver}); err != nil {
		t.Fatal(err)
	}
	if driver.calls.Load() != 1 || supervisorOperationState(t, f, queued.ID) != "queued" {
		t.Fatal("drain started or discarded queued work")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := f.e.deploymentDrain("status"); !got.Draining || got.ActiveDispatches != 0 {
		t.Fatal(got)
	}
	if supervisorOperationState(t, f, first.ID) != "completed" {
		t.Fatal("drain interrupted current work")
	}
	// New process state clears the drain; durable queued identity is untouched.
	next, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if got := next.deploymentDrain("status"); got.Draining || got.ActiveDispatches != 0 {
		t.Fatal(got)
	}
	resumed := &controlledDriver{}
	if err = next.processNext(ctx, map[string]Driver{"test-capability": resumed}); err != nil {
		t.Fatal(err)
	}
	if resumed.calls.Load() != 1 || supervisorOperationState(t, f, queued.ID) != "completed" {
		t.Fatal("restart lost or duplicated the queued request")
	}
}

func TestDeploymentDrainRequiresGlobalAdminAndHasExplicitCancellation(t *testing.T) {
	f := supervisorFixture(t)
	path := "/api/v1/operator/deployment-drain"
	for _, method := range []string{"GET", "POST", "DELETE"} {
		if w := f.request(method, path, f.writer, "", "{}"); w.Code != 403 {
			t.Fatal("worker changed global deployment state", method, w.Code, w.Body.String())
		}
	}
	if w := f.request("POST", path, f.admin, "", `{"unknown":true}`); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := f.request("POST", path+"?begin=true", f.admin, "", "{}"); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.e.deploymentDrain("status").Draining {
		t.Fatal("invalid request began drain")
	}
	for _, method := range []string{"POST", "POST", "GET", "DELETE", "DELETE", "GET"} {
		w := f.request(method, path, f.admin, "", "{}")
		var got DeploymentDrain
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		if method == "POST" && !got.Draining || method == "DELETE" && got.Draining {
			t.Fatal("wrong drain action", method, got)
		}
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE is_admin=1`); err != nil {
		t.Fatal(err)
	}
	if w := f.request("POST", path, f.admin, "", "{}"); w.Code != 401 {
		t.Fatal("revoked operator started drain", w.Code)
	}
}
