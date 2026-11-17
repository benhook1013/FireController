package reviewprocess

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestRunReportsSuccessfulSpawnBeforeWaiting(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("shell unavailable")
	}
	r := &Runner{executable: shell, now: time.Now}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan time.Time, 1)
	finished := make(chan runResult, 1)
	go func() {
		finished <- r.run(ctx, t.TempDir(), []string{"-c", "sleep 30"}, 1024, 1024, func(at time.Time) { started <- at })
	}()
	var at time.Time
	select {
	case at = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn callback missing while running")
	}
	select {
	case <-finished:
		t.Fatal("process returned before callback could be observed")
	default:
	}
	cancel()
	result := <-finished
	if at.IsZero() || !result.capture.Started {
		t.Fatal("spawn timestamp lost", result.capture)
	}
	r.executable = "/nonexistent/firecontroller-test-executable"
	called := false
	failed := r.run(t.Context(), t.TempDir(), nil, 1024, 1024, func(time.Time) { called = true })
	if called || failed.capture.Started {
		t.Fatal("failed spawn invented start")
	}
}
