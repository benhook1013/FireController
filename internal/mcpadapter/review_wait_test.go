package mcpadapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReviewResultStdioWaitDoesNotBlockOtherToolsOrRetry(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var posts atomic.Int64
	preferences := make(chan string, 4)
	readStarted := make(chan string, 1)
	readDetached := make(chan struct{}, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews/requests") {
			posts.Add(1)
			preferences <- r.Header.Get("Prefer")
		}
		waiting := strings.Contains(r.URL.Path, "/reviews/operations/") && boundedReviewWait(r.Header.Get("Prefer"), 50)
		if waiting {
			readStarted <- r.Header.Get("Prefer")
		}
		handler.ServeHTTP(w, r)
		if waiting {
			readDetached <- struct{}{}
		}
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	args := executionArguments()
	delete(args, "wait_seconds")
	var first, replay client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_request", args), &first)
	if <-preferences != "wait=2" {
		t.Fatal("default review wait preference lost")
	}
	args["wait_seconds"] = 0
	decodeResult(t, call(t, session, "review_request", args), &replay)
	if <-preferences != "wait=0" || first != replay || posts.Load() != 2 {
		t.Fatal("wait selection changed action identity")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "review_operation_read", Arguments: map[string]any{"operation_id": first.ID, "wait_seconds": 50}})
		done <- err
	}()
	select {
	case preference := <-readStarted:
		seconds, err := strconv.Atoi(strings.TrimPrefix(preference, "wait="))
		if err != nil || seconds <= 5 || seconds > 50 {
			t.Fatal("forwarded wait lost the caller budget or ordinary-deadline coverage", preference, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long review read did not start")
	}
	var binding profile.Context
	decodeResult(t, call(t, session, "context", nil), &binding)
	if binding.ProjectID != f.project {
		t.Fatal("concurrent tool lost binding")
	}
	// Exercise the adapter's selected budget as well as the shared client's
	// owned transport: an ordinary five-second tool deadline must not win.
	select {
	case err := <-done:
		t.Fatal("review wait ended under ordinary tool budget", err)
	case <-time.After(5100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SDK cancellation did not return")
	}
	select {
	case <-readDetached:
	case <-time.After(2 * time.Second):
		t.Fatal("SDK cancellation did not detach forwarded wait")
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations WHERE state='queued'`).Scan(&count); err != nil || count != 1 || posts.Load() != 2 {
		t.Fatal("canceled read changed or retried provider request", count, err)
	}
}

// Setup consumes the original result-wait deadline, so the forwarded positive
// preference may shrink. These fast fixtures must still select a bounded wait.
func boundedReviewWait(preference string, selected int) bool {
	seconds, err := strconv.Atoi(strings.TrimPrefix(preference, "wait="))
	return err == nil && preference == "wait="+strconv.Itoa(seconds) && seconds <= selected && (seconds > 0 || seconds == 0 && selected == 0)
}
