package reviewexec

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCIWatchHTTPScopeReadContentionIsBounded(t *testing.T) {
	for _, mode := range []string{"zero-wait", "caller-deadline", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			f, s, _, reader, _ := ciWatchFixture(t)
			release := holdCIWatchReadPool(t, f)
			defer release()
			ctx := t.Context()
			if mode == "shutdown" {
				lifetime, stop := context.WithCancel(t.Context())
				f.e.resultWaitLifetime = lifetime
				stop()
			}
			if mode == "caller-deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			req := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(`{"targets":[{"pull_number":42,"head_sha":"`+strings.Repeat("a", 40)+`"}]}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+f.reader)
			req.Header.Set("Prefer", "wait=0")
			w := &ciWatchScopeDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			done := make(chan struct{})
			started := time.Now()
			go func() { defer close(done); f.handler.ServeHTTP(w, req) }()
			select {
			case <-done:
			case <-time.After(7 * time.Second):
				release()
				<-done
				t.Fatal("scope read exceeded bounded HTTP allowance")
			}
			elapsed := time.Since(started)
			if mode == "zero-wait" && (elapsed < 4500*time.Millisecond || elapsed > 6500*time.Millisecond) {
				t.Fatal("wrong scope deadline", elapsed)
			}
			if (mode == "caller-deadline" || mode == "shutdown") && elapsed > time.Second {
				t.Fatal("ignored caller deadline", elapsed)
			}
			reason := "ci_watch_scope_read"
			if mode == "shutdown" {
				reason = "result_wait_lifetime"
			}
			if w.Code != 503 || !strings.Contains(w.Body.String(), reason) {
				t.Fatal("unscoped storage refusal", w.Code, w.Body.String())
			}
			if w.deadline.Before(started.Add(10*time.Second)) || w.deadline.After(started.Add(11*time.Second)) {
				t.Fatal("scope refusal lacked bounded response-write allowance", w.deadline.Sub(started))
			}
			ciWatchRegistered(t, s, 0)
			if reader.calls != 0 {
				t.Fatal("scope refusal acquired provider work")
			}
		})
	}
}

type ciWatchScopeDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline  time.Time
	deadlines []time.Time
}

func (w *ciWatchScopeDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestCIWatchHTTPRetainsResponseHeadroomAfterAuthentication(t *testing.T) {
	for _, wait := range []int{0, 1} {
		t.Run(strconv.Itoa(wait), func(t *testing.T) {
			f, s, _, reader, _ := ciWatchFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			req := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(`{"targets":[{"pull_number":42,"head_sha":"`+strings.Repeat("a", 40)+`"}]}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+f.reader)
			req.Header.Set("Prefer", "wait="+strconv.Itoa(wait))
			w := &ciWatchScopeDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			started := time.Now()
			f.handler.ServeHTTP(w, req)
			if len(w.deadlines) != 2 {
				t.Fatal("did not reach authenticated watch deadline", w.deadlines, w.Body.String())
			}
			if wait > 0 {
				if !w.deadlines[1].Equal(w.deadlines[0]) {
					t.Fatal("positive wait shortened refusal-write headroom", w.deadlines)
				}
			} else if d := w.deadline.Sub(started); d < 5*time.Second || d > 6*time.Second {
				t.Fatal("zero wait changed ordinary response allowance", d)
			}
			ciWatchRegistered(t, s, 0)
			if reader.calls != 0 {
				t.Fatal("deadline proof acquired provider work", reader.calls)
			}
		})
	}
}
