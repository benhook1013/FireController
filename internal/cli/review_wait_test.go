package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
)

func TestReviewResultCLIWaitAndDetachUseOneRetainedAction(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var posts atomic.Int64
	var setupStarted atomic.Pointer[time.Time]
	preferences := make(chan string, 4)
	readStarted := make(chan string, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews/requests") {
			posts.Add(1)
			preferences <- r.Header.Get("Prefer")
		}
		if strings.Contains(r.URL.Path, "/reviews/operations/") {
			if !boundedReviewWait(r.Header.Get("Prefer"), 900, setupStarted.Load()) {
				t.Error("operation wait was shortened beyond elapsed setup time")
			}
			readStarted <- r.Header.Get("Prefer")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	args := reviewRequestArgs()
	// The existing enqueue fixture explicitly selects immediate return. Omit
	// that preference to exercise the real service's default bounded wait.
	started := time.Now()
	first := mutationSuccess(t, path, "builder", args[:len(args)-2]...)
	if <-preferences != "wait=2" || first["state"] != "queued" || time.Since(started) < 1800*time.Millisecond {
		t.Fatal("default did not collect within its bounded wait")
	}
	replay := mutationSuccess(t, path, "builder", args...)
	if <-preferences != "wait=0" || encodeString(first) != encodeString(replay) || posts.Load() != 2 {
		t.Fatal("detaching changed the durable request or original receipt")
	}
	readStartedAt := time.Now()
	setupStarted.Store(&readStartedAt)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct {
		code        int
		out, stderr string
	}, 1)
	go func() {
		var out, stderr bytes.Buffer
		code := cli.RunContext(ctx, []string{"--profiles-file", path, "--profile", "builder", "reviews", "operation", first["id"].(string), "--wait-seconds", "900"}, nil, &out, &stderr)
		done <- struct {
			code        int
			out, stderr string
		}{code, out.String(), stderr.String()}
	}()
	select {
	case preference := <-readStarted:
		seconds, err := strconv.Atoi(strings.TrimPrefix(preference, "wait="))
		if err != nil || seconds <= 5 || seconds > 900 {
			t.Fatal("forwarded wait lost the caller budget or ordinary-deadline coverage", preference, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("explicit wait did not reach operation read")
	}
	cancel()
	select {
	case got := <-done:
		if got.code != cli.ExitCancelled || got.out != "" || strings.Contains(got.stderr, `"outcome_unknown":true`) {
			t.Fatal("read cancellation became an uncertain write", got.code, got.stderr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read wait did not detach")
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations WHERE state='queued'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("detached read changed review action", count, err)
	}
	for _, command := range [][]string{args, {"reviews", "operation", first["id"].(string)}} {
		for _, invalid := range []string{"-1", "901", "01", "1.5", ""} {
			candidate := append([]string(nil), command...)
			if command[1] == "request" {
				candidate = candidate[:len(candidate)-2]
			}
			candidate = append(candidate, "--wait-seconds", invalid)
			code, out, stderr := mutationInvoke(t, path, "builder", candidate...)
			var failure map[string]any
			if code != cli.ExitUsage || out != "" || json.Unmarshal([]byte(stderr), &failure) != nil {
				t.Fatal("invalid wait reached service", code, stderr)
			}
		}
	}
	if posts.Load() != 2 {
		t.Fatal("invalid selection sent another request")
	}
}

// Setup consumes the original result-wait deadline. A forwarded wait may lose
// only that elapsed setup time plus the rounding to whole wire seconds.
func boundedReviewWait(preference string, selected int, started *time.Time) bool {
	if started == nil {
		return false
	}
	elapsed := time.Since(*started)
	if elapsed < 0 {
		return false
	}
	minimum := max(0, selected-int((elapsed+time.Second-1)/time.Second))
	seconds, err := strconv.Atoi(strings.TrimPrefix(preference, "wait="))
	return err == nil && preference == "wait="+strconv.Itoa(seconds) && seconds >= minimum && seconds <= selected
}
